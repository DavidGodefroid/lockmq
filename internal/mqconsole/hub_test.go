package mqconsole

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/DavidGodefroid/lockkit/console"
	"github.com/DavidGodefroid/lockkit/hubclient"
	"github.com/DavidGodefroid/lockkit/hubclient/hubtest"
	"github.com/DavidGodefroid/lockkit/ipc"
	"github.com/DavidGodefroid/lockkit/plans"
	"github.com/DavidGodefroid/lockkit/ticket"

	"github.com/DavidGodefroid/lockmq/internal/product"
)

const hubInstance = "queues"

// hubRig is a console in hub mode beside a fake lockhub.
func hubRig(t *testing.T, o rigOpts) (*rig, *hubtest.Hub, ed25519.PublicKey) {
	t.Helper()
	home := t.TempDir()
	key, err := hubclient.InstanceKey(home, hubInstance)
	if err != nil {
		t.Fatal(err)
	}
	pub := key.Public().(ed25519.PublicKey)
	hub := hubtest.New(t, hubInstance, pub)
	hubKey := filepath.Join(t.TempDir(), "hub.pub")
	if err := os.WriteFile(hubKey, []byte(base64.StdEncoding.EncodeToString(hub.Pub)+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	o.hub = &console.HubOptions{URL: hub.URL(), KeyFile: hubKey, Instance: hubInstance, Home: home}
	return newRig(t, o), hub, pub
}

type pendingOut struct{ Status, Ticket, Lock, Instance string }

func pendingOf(t *testing.T, resp ipc.Response) pendingOut {
	t.Helper()
	var p pendingOut
	if resp.Error != nil || json.Unmarshal(resp.Result, &p) != nil || p.Status != "pending" || !ticket.ValidID(p.Ticket) || p.Lock != "lockmq" || p.Instance != hubInstance {
		t.Fatalf("not pending: %+v %s", resp.Error, resp.Result)
	}
	return p
}

// runDecided runs p until its ticket's decision has arrived.
func (r *rig) runDecided(p Params) ipc.Response {
	r.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		resp := r.call(MethodRun, p)
		var po pendingOut
		if resp.Error != nil || json.Unmarshal(resp.Result, &po) != nil || po.Status != "pending" {
			return resp
		}
		if time.Now().After(deadline) {
			r.t.Fatal("the decision never arrived")
		}
		time.Sleep(30 * time.Millisecond)
	}
}

// In hub mode a read never runs without a decided plan: op.run makes the
// plan, pushes its masked ticket and answers pending, with no prompt
// here; the decided ticket runs it once.
func TestHubReadRunsOnlyOnceDecided(t *testing.T) {
	r, hub, pub := hubRig(t, rigOpts{})
	p := readParams()
	p.Requester = ipc.Requester{Agent: "dev-1", Token: "tok-1"}
	pd := pendingOf(t, r.call(MethodRun, p))
	if r.io.promptCount() != 0 || r.reader.reads != 0 {
		t.Fatalf("prompts %d reads %d", r.io.promptCount(), r.reader.reads)
	}
	raw := hub.Ticket(pd.Ticket)
	tk, err := ticket.VerifyTicket(pub, raw)
	if err != nil || tk.Source.Lock != "lockmq" || tk.Risk != ticket.RiskReadPII || !strings.Contains(strings.Join(tk.Screen.Body, "\n"), "READ stream order-events") {
		t.Fatalf("ticket %+v %v", tk, err)
	}
	run := p
	run.Ticket = pd.Ticket
	pendingOf(t, r.call(MethodRun, run))
	if r.reader.reads != 0 {
		t.Fatal("read before the decision")
	}
	hub.Decide(pd.Ticket, ticket.Approved, "")
	other := run
	other.Requester = ipc.Requester{Agent: "dev-2", Token: "tok-2"}
	if resp := r.runDecided(other); resp.Error == nil || resp.Error.Code != ipc.CodeWrongRequester {
		t.Fatalf("other agent: %+v %s", resp.Error, resp.Result)
	}
	out := mustOK(t, r.runDecided(run))
	if r.reader.reads != 1 || strings.Contains(out, "alice@example.com") || !hub.Consumed(pd.Ticket) {
		t.Fatalf("reads %d consumed %v out %s", r.reader.reads, hub.Consumed(pd.Ticket), out)
	}
	if resp := r.call(MethodRun, run); resp.Error == nil || r.reader.reads != 1 {
		t.Fatalf("ran twice: %+v", resp)
	}
	audit := r.auditLog()
	if !strings.Contains(audit, `"event":"pending"`) || !strings.Contains(audit, `"approval":"hub"`) || !strings.Contains(audit, `"facade":`) {
		t.Errorf("audit:\n%s", audit)
	}
}

// Catalogue reads need no approval in hub mode either; the dry run says
// the request is decided on the hub; a denial is a denial.
func TestHubCatalogDryRunAndDenial(t *testing.T) {
	r, hub, _ := hubRig(t, rigOpts{tier: "peek"})
	mustOK(t, r.call(MethodRun, Params{Op: "queues"}))
	var plan Plan
	if err := json.Unmarshal([]byte(mustOK(t, r.call(MethodPlan, peekParams("payments")))), &plan); err != nil || plan.Verdict != "hub" {
		t.Fatalf("dry run: %+v %v", plan, err)
	}
	p := peekParams("payments")
	pd := pendingOf(t, r.call(MethodRun, p))
	hub.Decide(pd.Ticket, ticket.Denied, "no")
	p.Ticket = pd.Ticket
	if resp := r.runDecided(p); resp.Error == nil || resp.Error.Code != ipc.CodeDenied || r.broker.peekCount() != 0 {
		t.Fatalf("denied: %+v peeks %d", resp.Error, r.broker.peekCount())
	}
}

// A peek approved on the hub runs once; a queue whose properties changed
// between the approval and the run is refused stale before any read.
func TestHubPeekApprovedThenStale(t *testing.T) {
	r, hub, _ := hubRig(t, rigOpts{tier: "peek"})
	p := peekParams("payments")
	pd := pendingOf(t, r.call(MethodRun, p))
	hub.Decide(pd.Ticket, ticket.Approved, "")
	run := p
	run.Ticket = pd.Ticket
	mustOK(t, r.runDecided(run))
	if r.broker.peekCount() != 1 {
		t.Fatalf("peeks %d", r.broker.peekCount())
	}
	// Message counts move all the time and change nothing; the queue's
	// arguments do.
	pd = pendingOf(t, r.call(MethodRun, p))
	r.broker.mu.Lock()
	r.broker.queues["%2F/payments"]["messages_ready"] = 9.0
	r.broker.mu.Unlock()
	hub.Decide(pd.Ticket, ticket.Approved, "")
	run.Ticket = pd.Ticket
	mustOK(t, r.runDecided(run))
	pd = pendingOf(t, r.call(MethodRun, p))
	r.broker.mu.Lock()
	r.broker.queues["%2F/payments"]["arguments"] = map[string]any{"x-delivery-limit": 50.0}
	r.broker.mu.Unlock()
	hub.Decide(pd.Ticket, ticket.Approved, "")
	run.Ticket = pd.Ticket
	mustRefused(t, r.runDecided(run), "E_STALE")
	if r.broker.peekCount() != 2 {
		t.Fatalf("a stale peek ran: peeks %d", r.broker.peekCount())
	}
}

// The same for a stream read whose arguments changed.
func TestHubReadStale(t *testing.T) {
	r, hub, _ := hubRig(t, rigOpts{})
	p := readParams()
	pd := pendingOf(t, r.call(MethodRun, p))
	r.broker.mu.Lock()
	r.broker.queues["%2F/order-events"]["arguments"] = map[string]any{"x-max-age": "1D"}
	r.broker.mu.Unlock()
	hub.Decide(pd.Ticket, ticket.Approved, "")
	p.Ticket = pd.Ticket
	mustRefused(t, r.runDecided(p), "E_STALE")
	if r.reader.reads != 0 {
		t.Fatalf("reads %d", r.reader.reads)
	}
}

// testClock is a console clock the test moves.
type testClock struct {
	mu sync.Mutex
	t  time.Time
}

func newClock() *testClock { return &testClock{t: time.Now().UTC().Truncate(time.Millisecond)} }

func (c *testClock) now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *testClock) add(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

// storedPlan is the plan behind ticket tk, made by the rig's agent.
func storedPlan(t *testing.T, r *rig, tk, token string) *plans.Plan {
	t.Helper()
	sp, err := r.srv.Hub().Store.ByTicket(tk, plans.Caller{UID: -1, Token: token})
	if err != nil {
		t.Fatal(err)
	}
	return sp
}

// The recompute of a renewal gives what the run's recheck gives: the
// policy fingerprint and plan hash bound at plan time, and the same
// plan-hash inputs; a queue that moved shows up by name, a message count
// does not; without the broker the queue keeps its stored digest.
func TestHubRecomputeMatchesRecheck(t *testing.T) {
	for _, c := range []struct {
		name, queue string
		tier        string
		params      Params
	}{
		{"read", "%2F/order-events", "", readParams()},
		{"peek", "%2F/payments", "peek", peekParams("payments")},
	} {
		t.Run(c.name, func(t *testing.T) {
			r, _, _ := hubRig(t, rigOpts{tier: c.tier})
			p := c.params
			p.Requester = ipc.Requester{Agent: "dev-1", Token: "tok-1"}
			pd := pendingOf(t, r.call(MethodRun, p))
			sp := storedPlan(t, r, pd.Ticket, "tok-1")
			if len(sp.Binding.Inputs) != 2 || sp.Binding.Inputs["params"] == "" || sp.Binding.Inputs["queue"] == "" {
				t.Fatalf("inputs at plan time: %v", sp.Binding.Inputs)
			}
			recompute := r.be.recompute(r.srv)
			fp, hash, inputs, err := recompute(sp)
			if err != nil || fp != sp.Binding.PolicyFP || hash != sp.Binding.PlanHash || len(ticket.DiffInputs(sp.Binding.Inputs, inputs)) != 0 {
				t.Fatalf("recompute %s %s %v %v; stored %+v", fp, hash, inputs, err, sp.Binding)
			}
			// The run's recheck hashes the same.
			var body hubBody
			if err := json.Unmarshal(sp.Body, &body); err != nil {
				t.Fatal(err)
			}
			pp, err := r.be.prepare(context.Background(), r.srv, body.Params)
			if err != nil {
				t.Fatal(err)
			}
			if rh, err := planHash(body.Params, pp.props); err != nil || rh != hash || policyFP(pp.pol) != fp {
				t.Fatalf("recheck %s %v, recompute %s", rh, err, hash)
			}

			r.broker.mu.Lock()
			r.broker.queues[c.queue]["messages"] = 99.0
			r.broker.queues[c.queue]["messages_ready"] = 99.0
			r.broker.mu.Unlock()
			if _, h2, in2, err := recompute(sp); err != nil || h2 != hash || len(ticket.DiffInputs(sp.Binding.Inputs, in2)) != 0 {
				t.Fatalf("message counts moved the plan: %s %v", h2, err)
			}
			r.broker.mu.Lock()
			r.broker.queues[c.queue]["arguments"] = map[string]any{"x-delivery-limit": 50.0, "x-max-age": "1D"}
			r.broker.mu.Unlock()
			_, h3, in3, err := recompute(sp)
			ch := ticket.DiffInputs(sp.Binding.Inputs, in3)
			if err != nil || h3 == hash || len(ch) != 1 || ch[0].Input != "queue" || ch[0].Change != ticket.ChangeChanged {
				t.Fatalf("moved queue: %s %v %+v", h3, err, ch)
			}

			// No session to read the queue on: its stored digest stays,
			// so does the plan hash; nothing reconnects.
			r.be.mu.Lock()
			cur := r.be.cur
			r.be.cur = nil
			r.be.mu.Unlock()
			prompts := r.io.promptCount()
			fp4, h4, in4, err := recompute(sp)
			if err != nil || fp4 != fp || h4 != hash || len(ticket.DiffInputs(sp.Binding.Inputs, in4)) != 0 || r.io.promptCount() != prompts {
				t.Fatalf("offline recompute %s %v %v", h4, in4, err)
			}
			r.be.mu.Lock()
			r.be.cur = cur
			r.be.mu.Unlock()

			// A request the broker now refuses is not renewed.
			r.broker.mu.Lock()
			delete(r.broker.queues, c.queue)
			r.broker.mu.Unlock()
			if _, _, _, err := recompute(sp); err == nil {
				t.Fatal("renewed a request for a queue that is gone")
			}
		})
	}
}

// An expired ticket is asked again (decision H1): its run answers pending
// with a new ticket that renews it and names the input that moved; once
// approved, the new ticket runs once.
//
// Read tickets (read-pii) have no TTL in lockkit, pending or approved: the
// expiry this lock meets is an approval its clock cannot use, here one
// dated ahead of it (the console's clock moved back), which Begin ends
// Expired like an aged approval.
func TestHubExpiredTicketRenewed(t *testing.T) {
	clock := newClock()
	r, hub, pub := hubRig(t, rigOpts{now: clock.now})
	p := readParams()
	p.Requester = ipc.Requester{Agent: "dev-1", Token: "tok-1"}
	old := pendingOf(t, r.call(MethodRun, p)).Ticket
	at := clock.now()
	hub.DecideWith(old, ticket.Approved, "", nil, func(d *ticket.Decision) { d.At = at })
	clock.add(-5 * time.Minute)
	r.broker.mu.Lock()
	r.broker.queues["%2F/order-events"]["arguments"] = map[string]any{"x-max-age": "7D"}
	r.broker.mu.Unlock()
	run := p
	run.Ticket = old
	var pd pendingOut
	deadline := time.Now().Add(10 * time.Second)
	for {
		if pd = pendingOf(t, r.call(MethodRun, run)); pd.Ticket != old {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the decision never arrived")
		}
		time.Sleep(30 * time.Millisecond)
	}
	if r.reader.reads != 0 || r.io.promptCount() != 0 || hub.Consumed(old) {
		t.Fatalf("expired ticket ran: reads %d", r.reader.reads)
	}
	tk, err := ticket.VerifyTicket(pub, hub.Ticket(pd.Ticket))
	if err != nil {
		t.Fatal(err)
	}
	rn := tk.Renews
	if rn == nil || rn.Of != old || rn.Reason != ticket.RenewApprovalExpired || rn.ApprovedAt == nil || !rn.Checked ||
		len(rn.Changes) != 1 || rn.Changes[0].Input != "queue" || rn.Changes[0].Change != ticket.ChangeChanged {
		t.Fatalf("renews %+v", rn)
	}
	// The same expired ticket again: the same renewal.
	if again := pendingOf(t, r.call(MethodRun, run)); again.Ticket != pd.Ticket {
		t.Fatalf("renewed twice: %s, %s", again.Ticket, pd.Ticket)
	}
	hub.DecideWith(pd.Ticket, ticket.Approved, "", nil, func(d *ticket.Decision) { d.At = clock.now() })
	run.Ticket = pd.Ticket
	out := mustOK(t, r.runDecided(run))
	if r.reader.reads != 1 || strings.Contains(out, "alice@example.com") || !hub.Consumed(pd.Ticket) {
		t.Fatalf("reads %d consumed %v", r.reader.reads, hub.Consumed(pd.Ticket))
	}
	if resp := r.call(MethodRun, run); resp.Error == nil || r.reader.reads != 1 {
		t.Fatalf("ran twice: %+v", resp)
	}
	if a := r.auditLog(); !strings.Contains(a, `"event":"renew"`) || !strings.Contains(a, "renewed by ticket "+pd.Ticket) {
		t.Errorf("audit:\n%s", a)
	}
}

// Hub mode off (rollback D.5): a console started again without the hub
// withdraws the plans hub mode left pending and refuses their tickets
// explicitly; nothing is read, nothing is asked.
func TestStandaloneAfterHubWithdraws(t *testing.T) {
	r, _, _ := hubRig(t, rigOpts{})
	p := readParams()
	p.Requester = ipc.Requester{Agent: "dev-1", Token: "tok-1"}
	pd := pendingOf(t, r.call(MethodRun, p))
	r.closeHub()
	st := newRig(t, rigOpts{broker: r.broker, root: r.root, state: r.state})
	run := p
	run.Ticket = pd.Ticket
	resp := st.call(MethodRun, run)
	if resp.Error == nil || resp.Error.Code != ipc.CodeWithdrawn || !strings.Contains(resp.Error.Message, "withdrawn") {
		t.Fatalf("standalone run of a hub ticket: %+v %s", resp.Error, resp.Result)
	}
	if st.reader.reads != 0 || r.reader.reads != 0 || st.io.promptCount() != 0 {
		t.Fatalf("reads %d/%d prompts %d", st.reader.reads, r.reader.reads, st.io.promptCount())
	}
	ps, err := plans.Open(filepath.Join(r.state, product.P.Name, "hub", hubInstance, "plans"))
	if err != nil {
		t.Fatal(err)
	}
	defer ps.Close()
	if untold := ps.Untold(); len(untold) != 1 || untold[0].State != plans.Withdrawn {
		t.Fatalf("plans left: %+v", untold)
	}
}
