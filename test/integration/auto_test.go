//go:build integration

package integration

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/DavidGodefroid/lockkit/audit"
	"github.com/DavidGodefroid/lockkit/console"
	"github.com/DavidGodefroid/lockkit/hubclient"
	"github.com/DavidGodefroid/lockkit/hubclient/hubtest"
	"github.com/DavidGodefroid/lockkit/ipc"
	"github.com/DavidGodefroid/lockkit/ticket"

	"github.com/DavidGodefroid/lockmq/internal/config"
	"github.com/DavidGodefroid/lockmq/internal/mqconsole"
	"github.com/DavidGodefroid/lockmq/internal/product"
)

// decider plays `lox run` in auto mode on a test hub: it reads the
// profile's rules as `lockmq profile rules` gives them and approves by
// rule what the rules and the signed ticket let it (a ticket not
// NeverAuto; a connect ticket also needs connect "auto"); everything else
// waits for the human (human). fileRules plays a decider that trusts
// rules read from config files the agent edited: it approves a connect
// ticket by its rules alone, NeverAuto or not.
type decider struct {
	t     *testing.T
	hub   *hubtest.Hub
	rules map[string]config.AutoValue

	mu        sync.Mutex
	seen      map[string]bool
	waiting   []*ticket.Ticket
	fileRules map[string]config.AutoValue
}

func (d *decider) loop(done <-chan struct{}) {
	for {
		select {
		case <-done:
			return
		case <-time.After(10 * time.Millisecond):
		}
		for _, id := range d.hub.Tickets() {
			d.mu.Lock()
			if d.seen[id] {
				d.mu.Unlock()
				continue
			}
			d.seen[id] = true
			d.mu.Unlock()
			var tk ticket.Ticket
			if json.Unmarshal(d.hub.Ticket(id), &tk) != nil {
				continue
			}
			auto := !tk.Flags.NeverAuto
			d.mu.Lock()
			files := d.fileRules
			d.mu.Unlock()
			switch {
			case tk.Kind == ticket.KindConnect && files != nil:
				auto = files[config.ActConnect] == config.AutoAuto
			case tk.Kind == ticket.KindConnect:
				auto = auto && d.rules[config.ActConnect] == config.AutoAuto
			}
			if auto {
				d.hub.DecideWith(id, ticket.Approved, "", nil, func(dc *ticket.Decision) {
					dc.Rule = &ticket.Rule{By: ticket.RuleAuto, ID: "lox.auto." + string(tk.Kind)}
				})
				continue
			}
			d.mu.Lock()
			d.waiting = append(d.waiting, &tk)
			d.mu.Unlock()
		}
	}
}

// question waits for the next ticket the human must answer.
func (d *decider) question() *ticket.Ticket {
	d.t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		d.mu.Lock()
		if len(d.waiting) > 0 {
			tk := d.waiting[0]
			d.waiting = d.waiting[1:]
			d.mu.Unlock()
			return tk
		}
		d.mu.Unlock()
		time.Sleep(10 * time.Millisecond)
	}
	d.t.Fatal("no question for the human")
	return nil
}

// questions is how many tickets wait for the human.
func (d *decider) questions() int { d.mu.Lock(); defer d.mu.Unlock(); return len(d.waiting) }

// hubRig is a console in hub mode on one profile of cfg, its hub decided
// by a decider.
type hubRig struct {
	t     *testing.T
	srv   *console.Server
	dec   *decider
	state string
}

func newHubRig(t *testing.T, b *broker, cfg, profile string) *hubRig {
	t.Helper()
	root, state, home := t.TempDir(), t.TempDir(), t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".lockmq"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".lockmq", "config.toml"), []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	ps, _, err := config.LoadFrom(root, "")
	if err != nil {
		t.Fatal(err)
	}
	instance := "it-" + profile
	key, err := hubclient.InstanceKey(home, instance)
	if err != nil {
		t.Fatal(err)
	}
	hub := hubtest.New(t, instance, key.Public().(ed25519.PublicKey))
	hubKey := filepath.Join(t.TempDir(), "hub.pub")
	if err := os.WriteFile(hubKey, []byte(base64.StdEncoding.EncodeToString(hub.Pub)+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	dec := &decider{t: t, hub: hub, rules: ps[profile].Auto.Effective(), seen: map[string]bool{}}
	io := &scriptIO{}
	be := mqconsole.New(mqconsole.Options{Profile: profile, Root: root, StateDir: state, IO: io})
	pol, err := be.LoadPolicy()
	if err != nil {
		t.Fatal(err)
	}
	cctx, ccancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer ccancel()
	sess, err := be.Connect(cctx, console.Credentials{User: roUser, Secret: []byte(roPass)})
	if err != nil {
		t.Fatal(err)
	}
	log, err := audit.Open(product.P, state)
	if err != nil {
		t.Fatal(err)
	}
	h, err := console.OpenHub(product.P, console.HubOptions{URL: hub.URL(), KeyFile: hubKey, Instance: instance, Home: home}, state, nil)
	if err != nil {
		t.Fatal(err)
	}
	be.OnHub(h)
	ctx, cancel := context.WithCancel(context.Background())
	watched, decided := make(chan struct{}), make(chan struct{})
	go func() { defer close(watched); h.Watch(ctx, io) }()
	go func() { defer close(decided); dec.loop(ctx.Done()) }()
	srv, err := console.NewServer(console.ServerConfig{Product: product.P, Backend: be, Policy: pol, Session: sess, Audit: log, IO: io,
		Sanitizer: mqconsole.Sanitizer(), Hub: h, Approver: h.Approver(console.NewTypedApprover(io))})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		srv.Close()
		cancel()
		<-watched
		<-decided
		_ = h.Close()
	})
	return &hubRig{t: t, srv: srv, dec: dec, state: state}
}

func (r *hubRig) call(p mqconsole.Params) ipc.Response {
	r.t.Helper()
	p.Requester = ipc.Requester{Agent: "it-agent", Token: "it-token"}
	raw, _ := json.Marshal(p)
	return r.srv.Handle(context.Background(), ipc.Request{JSONRPC: "2.0", ID: 1, Method: mqconsole.MethodRun, Params: raw})
}

// pendingTicket is the ticket of a pending answer, "" when not pending.
func pendingTicket(resp ipc.Response) string {
	var p struct{ Status, Ticket string }
	if resp.Error != nil || json.Unmarshal(resp.Result, &p) != nil || p.Status != "pending" {
		return ""
	}
	return p.Ticket
}

// runDecided asks p and runs its ticket until the decision arrived.
func (r *hubRig) runDecided(p mqconsole.Params, tk string) ipc.Response {
	r.t.Helper()
	p.Ticket = tk
	deadline := time.Now().Add(15 * time.Second)
	for {
		resp := r.call(p)
		if pendingTicket(resp) == "" || time.Now().After(deadline) {
			return resp
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func (r *hubRig) auditLog() string {
	raw, _ := os.ReadFile(filepath.Join(r.state, product.P.Name, "audit.log"))
	return string(raw)
}

// A local full-auto profile is left to the agent: the connection and the
// peek are approved by `lox run`'s rules, no question; a production
// profile locked down asks its connection once, asks every metadata read,
// and refuses peeks before any ticket, on a real broker.
func TestAutoModeFullAutoLocalAndLockedProd(t *testing.T) {
	each(t, func(t *testing.T, b *broker) {
		base := fmt.Sprintf("url = %q\namqp_url = %q\nuser = %q\nvhosts = [\"/\"]\ntier = \"peek\"\n",
			b.MgmtURL(), fmt.Sprintf("amqp://127.0.0.1:%d", b.AMQP), roUser)
		cfg := "[profiles.local]\n" + base + "auto = \"full-auto\"\n" +
			"[profiles.prod]\n" + base + "production = true\n[profiles.prod.auto]\nlist = \"ask\"\nread = \"ask\"\npeek = \"deny\"\n"

		local := newHubRig(t, b, cfg, "local")
		okData(t, local.call(mqconsole.Params{Op: "queues"}))
		peek := mqconsole.Params{Op: "peek"}
		peek.Target, peek.Count = dlqName, 1
		// `lox run`'s rule decides the peek within the hub's short wait:
		// it runs in the same call, never pending (#64).
		data := okData(t, local.call(peek))
		if msgs, _ := data["messages"].([]any); len(msgs) != 1 || strings.Contains(fmt.Sprint(data), "@example.com") {
			t.Fatalf("peek: %v", data)
		}
		if n := local.dec.questions(); n != 0 {
			t.Errorf("full-auto asked %d questions", n)
		}
		if a := local.auditLog(); !strings.Contains(a, `"auto_rule":"auto.connect=auto"`) || !strings.Contains(a, `"decided_by":"auto:lox.auto.connect"`) ||
			!strings.Contains(a, `"auto_rule":"auto.peek=auto"`) || !strings.Contains(a, `"decided_by":"auto:lox.auto.action"`) {
			t.Errorf("local audit:\n%s", a)
		}

		prod := newHubRig(t, b, cfg, "prod")
		before, err := b.queueDepth(dlqName)
		if err != nil {
			t.Fatal(err)
		}
		if code := refusedCode(prod.call(peek)); code != "E_AUTO_DENIED" {
			t.Fatalf("prod peek: %s", code)
		}
		// A metadata read the profile asks for is a plan only the human
		// may approve; its run asks the session's connection first.
		list := mqconsole.Params{Op: "queues"}
		tk := pendingTicket(prod.call(list))
		if tk == "" {
			t.Fatal("prod list not pending on the hub")
		}
		q := prod.dec.question()
		if q.ID != tk || !q.Flags.NeverAuto || q.Subject.Verb != "list" {
			t.Fatalf("list ticket %+v", q)
		}
		prod.dec.hub.Decide(tk, ticket.Approved, "")
		got := make(chan ipc.Response, 1)
		go func() { got <- prod.runDecided(list, tk) }()
		conn := prod.dec.question()
		if conn.Kind != ticket.KindConnect || !conn.Flags.Production || conn.Subject.Profile != "prod" {
			t.Fatalf("connect ticket %+v", conn)
		}
		prod.dec.hub.Decide(conn.ID, ticket.Approved, "")
		okData(t, <-got)
		// The session's connection was asked once.
		tk = pendingTicket(prod.call(list))
		if q := prod.dec.question(); tk == "" || q.ID != tk || q.Kind == ticket.KindConnect {
			t.Fatalf("second list: %q %+v", tk, q)
		}
		if after, err := b.queueDepth(dlqName); err != nil || after != before {
			t.Errorf("depth %d → %d (%v)", before, after, err)
		}
		if a := prod.auditLog(); !strings.Contains(a, `"auto_rule":"auto.peek=deny"`) || !strings.Contains(a, `"auto_rule":"auto.list=ask"`) ||
			!strings.Contains(a, `"decided_by":"human"`) {
			t.Errorf("prod audit:\n%s", a)
		}
	})
}

// A production profile approved with connect = "ask", whose config file
// the agent loosened to "auto" behind the console's back: a decider that
// trusts the file approves the connect ticket by an auto rule, and the
// lock refuses it (the ticket is signed never_auto), on a real broker.
// Nothing is served until a human approves the connection.
func TestAutoModeFileOnlyConnectLooseningRefused(t *testing.T) {
	each(t, func(t *testing.T, b *broker) {
		base := fmt.Sprintf("url = %q\namqp_url = %q\nuser = %q\nvhosts = [\"/\"]\ntier = \"peek\"\n",
			b.MgmtURL(), fmt.Sprintf("amqp://127.0.0.1:%d", b.AMQP), roUser)
		prod := newHubRig(t, b, "[profiles.prod]\n"+base+"production = true\n", "prod")
		prod.dec.mu.Lock()
		prod.dec.fileRules = map[string]config.AutoValue{config.ActConnect: config.AutoAuto}
		prod.dec.mu.Unlock()
		resp := prod.call(mqconsole.Params{Op: "queues"})
		if resp.Error == nil || resp.Error.Code != ipc.CodeDenied || !strings.Contains(resp.Error.Message, "auto.connect=ask") {
			t.Fatalf("a file-only loosening of connect was accepted: %+v %s", resp.Error, resp.Result)
		}
		if a := prod.auditLog(); !strings.Contains(a, `"decided_by":"auto:lox.auto.connect"`) || !strings.Contains(a, "never-auto") {
			t.Errorf("audit:\n%s", a)
		}
		// The human approves the next one: served.
		prod.dec.mu.Lock()
		prod.dec.fileRules = nil
		prod.dec.mu.Unlock()
		got := make(chan ipc.Response, 1)
		go func() { got <- prod.call(mqconsole.Params{Op: "queues"}) }()
		conn := prod.dec.question()
		if conn.Kind != ticket.KindConnect || !conn.Flags.NeverAuto {
			t.Fatalf("connect ticket %+v", conn)
		}
		prod.dec.hub.Decide(conn.ID, ticket.Approved, "")
		okData(t, <-got)
	})
}

// A profile that sets no auto rule keeps lockmq's guarantee on peeks:
// the peek ticket is signed never_auto, so `lox run` asks the human, and a
// decider that approves it by an auto rule anyway runs nothing (no
// message requeued, the ticket still waits); only the human's approval
// peeks, on a real broker.
func TestAutoModeDefaultPeekNeverAuto(t *testing.T) {
	each(t, func(t *testing.T, b *broker) {
		for _, name := range []string{"uat", "prod"} {
			t.Run(name, func(t *testing.T) { defaultPeekNeverAuto(t, b, name) })
		}
	})
}

func defaultPeekNeverAuto(t *testing.T, b *broker, name string) {
	base := fmt.Sprintf("url = %q\namqp_url = %q\nuser = %q\nvhosts = [\"/\"]\ntier = \"peek\"\n",
		b.MgmtURL(), fmt.Sprintf("amqp://127.0.0.1:%d", b.AMQP), roUser)
	if name == "prod" {
		base += "production = true\n"
	}
	r := newHubRig(t, b, "[profiles."+name+"]\n"+base, name)
	if r.dec.rules[config.ActPeek] != config.AutoAsk {
		t.Fatalf("default peek rule %q", r.dec.rules[config.ActPeek])
	}
	got := make(chan ipc.Response, 1)
	go func() { got <- r.call(mqconsole.Params{Op: "queues"}) }()
	conn := r.dec.question()
	if conn.Kind != ticket.KindConnect {
		t.Fatalf("connect ticket %+v", conn)
	}
	r.dec.hub.Decide(conn.ID, ticket.Approved, "")
	okData(t, <-got)

	peek := mqconsole.Params{Op: "peek"}
	peek.Target, peek.Count = dlqName, 1
	tk := pendingTicket(r.call(peek))
	q := r.dec.question()
	if tk == "" || q.ID != tk || q.Kind != ticket.KindAction || q.Subject.Verb != "peek" || !q.Flags.NeverAuto {
		t.Fatalf("default peek ticket %q %+v", tk, q)
	}
	r.dec.hub.DecideWith(tk, ticket.Approved, "", nil, func(dc *ticket.Decision) {
		dc.Rule = &ticket.Rule{By: ticket.RuleAuto, ID: "lox.auto.action"}
	})
	resp := r.runDecided(peek, tk)
	if resp.Error == nil || resp.Error.Code != ipc.CodeDenied || !strings.Contains(resp.Error.Message, "auto approval cannot run it") {
		t.Fatalf("an auto rule's approval of a default peek: %+v %s", resp.Error, resp.Result)
	}
	// The refused auto approval is audited: who decided, the
	// never-auto error, the stored request.
	refused := false
	for _, l := range strings.Split(r.auditLog(), "\n") {
		if strings.Contains(l, `"ticket":"`+tk+`"`) && strings.Contains(l, `"decided_by":"auto:lox.auto.action"`) &&
			strings.Contains(l, `"decision":"failed"`) && strings.Contains(l, "never-auto") && strings.Contains(l, `"op":"peek"`) {
			refused = true
		}
	}
	if !refused {
		t.Errorf("the refused auto approval of %s is not audited:\n%s", tk, r.auditLog())
	}
	// The same request still waits for a human; the human approves it.
	if again := pendingTicket(r.call(peek)); again != tk {
		t.Fatalf("after the refused auto approval: %q, want the pending %q", again, tk)
	}
	r.dec.hub.Decide(tk, ticket.Approved, "")
	data := okData(t, r.runDecided(peek, tk))
	if msgs, _ := data["messages"].([]any); len(msgs) != 1 {
		t.Fatalf("peek: %v", data)
	}
	if a := r.auditLog(); !strings.Contains(a, `"auto_rule":"auto.peek=ask"`) || !strings.Contains(a, `"decided_by":"human"`) {
		t.Errorf("audit:\n%s", a)
	}
}
