package mqconsole

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/DavidGodefroid/lockkit/console"
	"github.com/DavidGodefroid/lockkit/hubclient"
	"github.com/DavidGodefroid/lockkit/hubclient/hubtest"
	"github.com/DavidGodefroid/lockkit/ipc"
	"github.com/DavidGodefroid/lockkit/ticket"
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
