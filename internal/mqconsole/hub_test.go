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
)

const hubInstance = "queues"

// hubRig is a console in hub mode beside a fake lockhub.
func hubRig(t *testing.T, o rigOpts) (*rig, *hubtest.Hub, ed25519.PublicKey) {
	t.Helper()
	var hub *hubtest.Hub
	var pub ed25519.PublicKey
	o.hub, hub, pub = hubOptions(t)
	return newRig(t, o), hub, pub
}

// hubOptions is a test hub and the console's options for it.
func hubOptions(t *testing.T) (*console.HubOptions, *hubtest.Hub, ed25519.PublicKey) {
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
	return &console.HubOptions{URL: hub.URL(), KeyFile: hubKey, Instance: hubInstance, Home: home}, hub, pub
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

// unchecked is what a renewal of this lock says of its inputs: the
// params unchanged, the queue unchecked (read only at the run).
func checkUnchecked(t *testing.T, ch []ticket.Change) {
	t.Helper()
	if len(ch) != 1 || ch[0].Input != "queue" || ch[0].Change != ticket.ChangeUnchecked {
		t.Fatalf("changes %+v", ch)
	}
}

// The recompute of a renewal gives what the run's recheck gives, as far
// as it can know without the broker: the policy fingerprint and plan hash
// bound at plan time, the params' digest; the queue is unchecked, never
// claimed unchanged, and nothing is read from the broker for it.
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
			// The run's recheck hashes the plan as stored.
			var body hubBody
			if err := json.Unmarshal(sp.Body, &body); err != nil {
				t.Fatal(err)
			}
			pp, err := r.be.prepare(context.Background(), r.srv, body.Params)
			if err != nil {
				t.Fatal(err)
			}
			if rh, err := planHash(body.Params, pp.props); err != nil || rh != sp.Binding.PlanHash || policyFP(pp.pol) != sp.Binding.PolicyFP {
				t.Fatalf("recheck %s %v", rh, err)
			}
			r.broker.mu.Lock()
			r.broker.queues[c.queue]["arguments"] = map[string]any{"x-delivery-limit": 50.0, "x-max-age": "1D"}
			served := len(r.broker.requests)
			r.broker.mu.Unlock()
			fp, hash, inputs, err := r.be.recompute(sp)
			if err != nil || fp != sp.Binding.PolicyFP || hash != sp.Binding.PlanHash || inputs["params"] != sp.Binding.Inputs["params"] {
				t.Fatalf("recompute %s %s %v %v; stored %+v", fp, hash, inputs, err, sp.Binding)
			}
			checkUnchecked(t, ticket.DiffInputs(sp.Binding.Inputs, inputs))
			// The session closed (logout, idle timeout): the same, and the
			// backend forgets it.
			r.srv.Close()
			if r.be.session() != nil {
				t.Fatal("a closed session is still the backend's")
			}
			_, h2, in2, err := r.be.recompute(sp)
			if err != nil || h2 != hash {
				t.Fatalf("closed session: %s %v", h2, err)
			}
			checkUnchecked(t, ticket.DiffInputs(sp.Binding.Inputs, in2))
			r.broker.mu.Lock()
			defer r.broker.mu.Unlock()
			if len(r.broker.requests) != served {
				t.Fatalf("the recompute read the broker: %v", r.broker.requests[served:])
			}
		})
	}
}

// The recompute runs on the hub's watch goroutine while the console
// serves, reloads its policy and closes its session (run with -race).
func TestHubRecomputeConcurrent(t *testing.T) {
	r, _, _ := hubRig(t, rigOpts{tier: "peek"})
	p := peekParams("payments")
	p.Requester = ipc.Requester{Agent: "dev-1", Token: "tok-1"}
	sp := storedPlan(t, r, pendingOf(t, r.call(MethodRun, p)).Ticket, "tok-1")
	var wg sync.WaitGroup
	stop := make(chan struct{})
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			if _, _, _, err := r.be.recompute(sp); err != nil {
				t.Error(err)
				return
			}
		}
	}()
	for range 20 {
		r.call(MethodPlan, peekParams("payments"))
		r.srv.CheckPolicy()
	}
	r.srv.Close()
	close(stop)
	wg.Wait()
}

// An expired ticket is asked again (decision H1): its run answers pending
// with a new ticket that renews it and names the queue unchecked; once
// approved, the new ticket runs once.
//
// Read tickets (read-pii) have no TTL in lockkit, pending or approved (a
// pending plan takes its ticket's expiry, none): the expiry this lock meets
// is an approval its clock cannot use, here one dated ahead of it (the
// console's clock moved back), which Begin ends Expired.
func TestHubExpiredTicketRenewed(t *testing.T) {
	clock := newClock()
	r, hub, pub := hubRig(t, rigOpts{now: clock.now})
	r.peer = &ipc.Cred{UID: 1000, PID: 1} // lockkit renews only plans of a known peer
	p := readParams()
	p.Requester = ipc.Requester{Agent: "dev-1", Token: "tok-1"}
	old := pendingOf(t, r.call(MethodRun, p)).Ticket
	at := clock.now()
	hub.DecideWith(old, ticket.Approved, "", nil, func(d *ticket.Decision) { d.At = at })
	clock.add(-5 * time.Minute)
	run := p
	run.Ticket = old
	run.Target = "orders.failed" // the agent's params beside a ticket are not the plan's
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
	if rn == nil || rn.Of != old || rn.Reason != ticket.RenewApprovalExpired || rn.ApprovedAt == nil || !rn.Checked {
		t.Fatalf("renews %+v", rn)
	}
	checkUnchecked(t, rn.Changes)
	// The same expired ticket again: the same renewal, audited once.
	if again := pendingOf(t, r.call(MethodRun, run)); again.Ticket != pd.Ticket {
		t.Fatalf("renewed twice: %s, %s", again.Ticket, pd.Ticket)
	}
	a := r.auditLog()
	if strings.Count(a, `"event":"renew"`) != 1 || !strings.Contains(a, "renewed by ticket "+pd.Ticket) ||
		!strings.Contains(a, `"target":"order-events"`) || strings.Contains(a, "orders.failed") {
		t.Errorf("audit:\n%s", a)
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
}

// A renewal the human asks on the hub (its renew request, on the watch
// goroutine) makes the new ticket with no agent; the agent's run of the
// old ticket then points at it; once approved it runs once.
func TestHubRenewRequestFromHub(t *testing.T) {
	r, hub, pub := hubRig(t, rigOpts{tier: "peek"})
	r.peer = &ipc.Cred{UID: 1000, PID: 1}
	p := peekParams("payments")
	p.Requester = ipc.Requester{Agent: "dev-1", Token: "tok-1"}
	old := pendingOf(t, r.call(MethodRun, p)).Ticket
	// The approval aged (as Begin records it): the plan is Expired.
	caller := plans.Caller{UID: 1000, Token: "tok-1"}
	sp, err := r.srv.Hub().Store.ByTicket(old, caller)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.srv.Hub().Store.ExpireApproval(sp.ID, caller, time.Now().UTC().Truncate(time.Millisecond), "approval aged"); err != nil {
		t.Fatal(err)
	}
	rid := hub.Request(old, ticket.RequestRenew, nil, nil)
	var a struct{ Ticket, Error string }
	deadline := time.Now().Add(10 * time.Second)
	for hub.Answer(rid) == nil {
		if time.Now().After(deadline) {
			t.Fatal("the renew request was never answered")
		}
		time.Sleep(30 * time.Millisecond)
	}
	if err := json.Unmarshal(hub.Answer(rid), &a); err != nil || a.Ticket == "" || a.Ticket == old {
		t.Fatalf("answer %s\n%s", hub.Answer(rid), r.io.output())
	}
	tk, err := ticket.VerifyTicket(pub, hub.Ticket(a.Ticket))
	if err != nil || tk.Renews == nil || tk.Renews.Of != old || !tk.Renews.Checked {
		t.Fatalf("renewal %+v %v", tk, err)
	}
	checkUnchecked(t, tk.Renews.Changes)
	run := p
	run.Ticket = old
	if pd := pendingOf(t, r.call(MethodRun, run)); pd.Ticket != a.Ticket {
		t.Fatalf("agent run of the old ticket: %s, want %s", pd.Ticket, a.Ticket)
	}
	if au := r.auditLog(); strings.Count(au, `"event":"renew"`) != 1 || !strings.Contains(au, `"approval":"hub-claimed"`) {
		t.Errorf("audit:\n%s", au)
	}
	hub.Decide(a.Ticket, ticket.Approved, "")
	run.Ticket = a.Ticket
	mustOK(t, r.runDecided(run))
	if r.broker.peekCount() != 1 {
		t.Fatalf("peeks %d", r.broker.peekCount())
	}
}
