package mqconsole

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/DavidGodefroid/lockkit/console"
	"github.com/DavidGodefroid/lockkit/hubclient/hubtest"
	"github.com/DavidGodefroid/lockkit/ipc"
	"github.com/DavidGodefroid/lockkit/ticket"
)

// autoTOML is the auto table of profile name with one rule.
func autoTOML(name, action, value string) string {
	return "[profiles." + name + ".auto]\n" + action + " = \"" + value + "\"\n"
}

// byAutoRule is a decision by one of `lox run`'s auto rules.
func byAutoRule(id string) func(*ticket.Decision) {
	return func(d *ticket.Decision) { d.Rule = &ticket.Rule{By: ticket.RuleAuto, ID: id} }
}

// actionTickets are the tickets the lock pushed, other than connect ones.
func actionTickets(t *testing.T, hub *hubtest.Hub) []*ticket.Ticket {
	t.Helper()
	var out []*ticket.Ticket
	for _, id := range hub.Tickets() {
		var tk ticket.Ticket
		if err := json.Unmarshal(hub.Ticket(id), &tk); err != nil {
			t.Fatal(err)
		}
		if tk.Kind != ticket.KindConnect {
			out = append(out, &tk)
		}
	}
	return out
}

// paramsOf is a request of auto action act.
func paramsOf(act string) Params {
	switch act {
	case "read":
		return readParams()
	case "peek":
		return peekParams("payments")
	}
	return Params{Op: "queues"}
}

// Every action under every value: deny refuses before any ticket; auto
// lets an auto rule of the decider approve (a metadata read runs at once);
// ask sends a ticket only a human may approve. The audit names the
// profile's rule and who decided.
func TestAutoRulesPerActionAndValue(t *testing.T) {
	for _, act := range []string{"list", "read", "peek"} {
		for _, val := range []string{"auto", "ask", "deny"} {
			t.Run(act+"="+val, func(t *testing.T) {
				r, hub, pub := hubRig(t, rigOpts{tier: "peek", auto: autoTOML("uat", act, val)})
				p := paramsOf(act)
				p.Requester = ipc.Requester{Agent: "dev-1", Token: "tok-1"}
				rule := "auto." + act + "=" + val
				resp := r.call(MethodRun, p)
				if val == "deny" {
					mustRefused(t, resp, "E_AUTO_DENIED")
					if n := len(actionTickets(t, hub)); n != 0 || r.reader.reads != 0 || r.broker.peekCount() != 0 {
						t.Fatalf("tickets %d reads %d peeks %d", n, r.reader.reads, r.broker.peekCount())
					}
					if a := r.auditLog(); !strings.Contains(a, `"auto_rule":"`+rule+`"`) || !strings.Contains(a, `"code":"E_AUTO_DENIED"`) {
						t.Errorf("audit:\n%s", a)
					}
					var pl Plan
					if err := json.Unmarshal([]byte(mustOK(t, r.call(MethodPlan, p))), &pl); err != nil || pl.Verdict != "refused" {
						t.Errorf("dry run: %+v %v", pl, err)
					}
					return
				}
				if act == "list" && val == "auto" {
					mustOK(t, resp)
					if n := len(actionTickets(t, hub)); n != 0 {
						t.Fatalf("a ticket for an auto metadata read: %d", n)
					}
					return
				}
				pd := pendingOf(t, resp)
				tk, err := ticket.VerifyTicket(pub, hub.Ticket(pd.Ticket))
				if err != nil {
					t.Fatal(err)
				}
				if tk.V != ticket.VersionKinds || tk.Kind != ticket.KindAction || tk.Subject == nil || tk.Subject.Verb != act ||
					tk.Subject.Profile != "uat" || tk.Flags.NeverAuto != (val == "ask") {
					t.Fatalf("ticket %+v subject %+v", tk, tk.Subject)
				}
				run := p
				run.Ticket = pd.Ticket
				hub.DecideWith(pd.Ticket, ticket.Approved, "", nil, byAutoRule("lox.auto."+act))
				out := r.runDecided(run)
				if val == "ask" {
					if out.Error == nil || r.reader.reads != 0 || r.broker.peekCount() != 0 {
						t.Fatalf("an auto rule's approval ran an asked request: %+v", out)
					}
					// The human's approval runs it.
					pd = pendingOf(t, r.call(MethodRun, p))
					hub.Decide(pd.Ticket, ticket.Approved, "")
					run.Ticket = pd.Ticket
					mustOK(t, r.runDecided(run))
					if a := r.auditLog(); !strings.Contains(a, `"auto_rule":"`+rule+`"`) || !strings.Contains(a, `"decided_by":"human"`) {
						t.Errorf("audit:\n%s", a)
					}
					return
				}
				mustOK(t, out)
				if a := r.auditLog(); !strings.Contains(a, `"auto_rule":"`+rule+`"`) || !strings.Contains(a, `"decided_by":"auto:lox.auto.`+act+`"`) {
					t.Errorf("audit:\n%s", a)
				}
			})
		}
	}
}

// An unmasked read is a human's decision whatever the rule.
func TestAutoUnmaskStaysNeverAuto(t *testing.T) {
	r, hub, pub := hubRig(t, rigOpts{allowUnmask: true, auto: "auto = \"full-auto\"\n"})
	p := readParams()
	p.Unmask = true
	p.Requester = ipc.Requester{Agent: "dev-1", Token: "tok-1"}
	pd := pendingOf(t, r.call(MethodRun, p))
	tk, err := ticket.VerifyTicket(pub, hub.Ticket(pd.Ticket))
	if err != nil || !tk.Flags.NeverAuto {
		t.Fatalf("ticket %+v %v", tk, err)
	}
}

// waitConnect waits for the lock's next connect ticket after n of them.
func waitConnect(t *testing.T, hub *hubtest.Hub, pub []byte, n int) *ticket.Ticket {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		seen := 0
		for _, id := range hub.Tickets() {
			tk, err := ticket.VerifyTicket(pub, hub.Ticket(id))
			if err != nil {
				t.Fatal(err)
			}
			if tk.Kind == ticket.KindConnect {
				if seen == n {
					return tk
				}
				seen++
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("no connect ticket")
	return nil
}

// In hub mode the broker session serves no request before its connect
// ticket is approved: the first request asks it and waits; the requests
// after it share the approval. The ticket names the profile, the
// endpoint and the credential source, never a secret; `lox run` may
// approve it by a rule (connect is asked once per session, not
// NeverAuto). The audit records the profile's rule and the decider's.
func TestConnectTicketOncePerSession(t *testing.T) {
	r, hub, pub := hubRig(t, rigOpts{manualConnect: true, production: true})
	got := make(chan ipc.Response, 1)
	go func() { got <- r.call(MethodRun, Params{Op: "queues", Requester: ipc.Requester{Agent: "dev-1"}}) }()
	tk := waitConnect(t, hub, pub, 0)
	select {
	case resp := <-got:
		t.Fatalf("served before the connect decision: %+v", resp)
	case <-time.After(50 * time.Millisecond):
	}
	s := tk.Subject
	if tk.V != ticket.VersionKinds || s.Profile != "prod" || !strings.HasPrefix(s.Endpoint, "127.0.0.1:") || s.Credential != ticket.CredentialAsk ||
		s.SecretTo != "" || tk.Flags.NeverAuto || !tk.Flags.Production || tk.Requester.Agent != "dev-1" ||
		!strings.Contains(strings.Join(tk.Screen.Body, "\n"), "auto.connect=ask") {
		t.Fatalf("connect ticket %+v subject %+v", tk, s)
	}
	hub.DecideWith(tk.ID, ticket.Approved, "", nil, byAutoRule("lox.session.prod"))
	mustOK(t, <-got)
	mustOK(t, r.call(MethodRun, Params{Op: "queues"}))
	if n := len(hub.Tickets()); n != 1 {
		t.Fatalf("tickets %d: one connect ticket per session", n)
	}
	if !hub.Consumed(tk.ID) {
		t.Error("the connect ticket was not consumed")
	}
	a := r.auditLog()
	if !strings.Contains(a, `"op":"connect"`) || !strings.Contains(a, `"auto_rule":"auto.connect=ask"`) || !strings.Contains(a, `"decided_by":"auto:lox.session.prod"`) {
		t.Errorf("audit:\n%s", a)
	}
}

// A connect ticket denied on the hub serves nothing.
func TestConnectTicketDenied(t *testing.T) {
	r, hub, pub := hubRig(t, rigOpts{manualConnect: true})
	got := make(chan ipc.Response, 1)
	go func() { got <- r.call(MethodRun, Params{Op: "queues"}) }()
	tk := waitConnect(t, hub, pub, 0)
	hub.Decide(tk.ID, ticket.Denied, "not now")
	if resp := <-got; resp.Error == nil || resp.Error.Code != ipc.CodeDenied {
		t.Fatalf("denied connect: %+v %s", resp.Error, resp.Result)
	}
	if a := r.auditLog(); !strings.Contains(a, `"event":"denied"`) || !strings.Contains(a, `"op":"connect"`) {
		t.Errorf("audit:\n%s", a)
	}
}

// auto.connect = "deny" never logs in.
func TestConnectDeniedByRule(t *testing.T) {
	broker := newBroker(t)
	root := t.TempDir()
	writeProject(t, root, broker.srv.URL, rigOpts{auto: autoTOML("uat", "connect", "deny")})
	be := New(Options{Profile: "uat", Root: root, StateDir: t.TempDir(), IO: &fakeIO{}})
	_, err := be.Connect(context.Background(), console.Credentials{User: "ro", Secret: []byte("pw")})
	if err == nil || !strings.Contains(err.Error(), "auto.connect=deny") {
		t.Fatalf("connect: %v", err)
	}
}
