package mqconsole

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/DavidGodefroid/lockkit/console"
	"github.com/DavidGodefroid/lockkit/fingerprint"
	"github.com/DavidGodefroid/lockkit/ipc"
	"github.com/DavidGodefroid/lockkit/sysconf"

	"github.com/DavidGodefroid/lockmq/internal/product"
)

func readParams() Params {
	p := Params{Op: "read", Intent: "see the order events"}
	p.Target, p.From, p.Count = "order-events", "first", 2
	return p
}

func peekParams(queue string) Params {
	p := Params{Op: "peek", Intent: "see why orders fail"}
	p.Target, p.Count = queue, 1
	return p
}

// Catalogue operations run without a prompt and are audited.
func TestCatalogNotPrompted(t *testing.T) {
	r := newRig(t, rigOpts{})
	out := mustOK(t, r.call(MethodRun, Params{Op: "queues"}))
	if !strings.Contains(out, "order-events") || r.io.promptCount() != 0 {
		t.Fatalf("out %s prompts %d", out, r.io.promptCount())
	}
	if !strings.Contains(r.auditLog(), `"event":"catalog"`) {
		t.Errorf("audit: %s", r.auditLog())
	}
	mustRefused(t, r.call(MethodRun, Params{Op: "purge"}), "E_BAD_ARGS")
}

// A stream read is prompted in manual mode, auto-approved in auto mode
// outside production, and its content comes out masked.
func TestReadPromptedAndAutoOutsideProd(t *testing.T) {
	r := newRig(t, rigOpts{answers: []string{"y"}})
	out := mustOK(t, r.call(MethodRun, readParams()))
	if r.io.promptCount() != 1 || r.reader.reads != 1 {
		t.Fatalf("prompts %d reads %d", r.io.promptCount(), r.reader.reads)
	}
	if strings.Contains(out, "alice@example.com") || strings.Contains(out, "Alice Example") || !strings.Contains(out, "proposals") {
		t.Errorf("clear personal data: %s", out)
	}
	if !strings.Contains(r.io.output(), "READ stream order-events") || !strings.Contains(r.io.output(), "no offset stored") {
		t.Errorf("screen:\n%s", r.io.output())
	}

	auto := newRig(t, rigOpts{auto: true})
	mustOK(t, auto.call(MethodRun, readParams()))
	if auto.io.promptCount() != 0 || auto.reader.reads != 1 {
		t.Fatalf("auto: prompts %d reads %d", auto.io.promptCount(), auto.reader.reads)
	}

	denied := newRig(t, rigOpts{answers: []string{"n not now"}})
	resp := denied.call(MethodRun, readParams())
	if resp.Error == nil || resp.Error.Code != ipc.CodeDenied || denied.reader.reads != 0 {
		t.Fatalf("denied: %+v reads %d", resp.Error, denied.reader.reads)
	}
}

// A peek is prompted every time, auto mode or not, framed in red, and
// marks the delivery count it saw.
func TestPeekNeverAuto(t *testing.T) {
	r := newRig(t, rigOpts{tier: "peek", auto: true, answers: []string{"y", "y"}})
	out := mustOK(t, r.call(MethodRun, peekParams("payments")))
	if r.io.promptCount() != 1 || r.broker.peekCount() != 1 {
		t.Fatalf("prompts %d peeks %d", r.io.promptCount(), r.broker.peekCount())
	}
	if !strings.Contains(r.io.output(), "NOT READ-ONLY") || !strings.Contains(r.io.output(), "first peek of this queue") {
		t.Errorf("screen:\n%s", r.io.output())
	}
	if strings.Contains(out, "alice@example.com") || !strings.Contains(out, `"redelivered":false`) {
		t.Errorf("out: %s", out)
	}
	mustOK(t, r.call(MethodRun, peekParams("payments")))
	if !strings.Contains(r.io.output(), "max seen so far 0/20") {
		t.Errorf("second screen:\n%s", r.io.output())
	}
}

// Refusals come before any screen and any broker write.
func TestPeekRefusedBeforeScreen(t *testing.T) {
	r := newRig(t, rigOpts{tier: "peek"})
	mustRefused(t, r.call(MethodRun, peekParams("tight")), "E_DELIVERY_LIMIT")
	mustRefused(t, r.call(MethodRun, peekParams("order-events")), "E_WRONG_QUEUE_TYPE")
	p := peekParams("payments")
	p.Count = 6
	mustRefused(t, r.call(MethodRun, p), "E_BAD_ARGS")
	if r.io.promptCount() != 0 || r.broker.peekCount() != 0 {
		t.Fatalf("prompts %d peeks %d", r.io.promptCount(), r.broker.peekCount())
	}

	ro := newRig(t, rigOpts{})
	mustRefused(t, ro.call(MethodRun, peekParams("payments")), "E_TIER")
	noFrom := readParams()
	noFrom.From = ""
	mustRefused(t, ro.call(MethodRun, noFrom), "E_BAD_ARGS")
	q := readParams()
	q.Target = "payments"
	mustRefused(t, ro.call(MethodRun, q), "E_WRONG_QUEUE_TYPE")
	if ro.io.promptCount() != 0 || ro.broker.peekCount() != 0 || ro.reader.reads != 0 {
		t.Fatalf("read-only: prompts %d peeks %d reads %d", ro.io.promptCount(), ro.broker.peekCount(), ro.reader.reads)
	}
	if !strings.Contains(ro.auditLog(), `"code":"E_TIER"`) {
		t.Errorf("refusal not audited: %s", ro.auditLog())
	}
}

// op.plan is a dry run: a verdict, the screen, no prompt, no broker write.
func TestPlanIsDryRun(t *testing.T) {
	r := newRig(t, rigOpts{tier: "peek"})
	var pl Plan
	if err := json.Unmarshal([]byte(mustOK(t, r.call(MethodPlan, peekParams("payments")))), &pl); err != nil {
		t.Fatal(err)
	}
	if pl.Verdict != "prompt-every-time" || len(pl.Warnings) == 0 || r.io.promptCount() != 0 || r.broker.peekCount() != 0 {
		t.Fatalf("plan %+v prompts %d peeks %d", pl, r.io.promptCount(), r.broker.peekCount())
	}
	if err := json.Unmarshal([]byte(mustOK(t, r.call(MethodPlan, peekParams("tight")))), &pl); err != nil {
		t.Fatal(err)
	}
	if pl.Verdict != "refused" || pl.Refusal == nil || pl.Refusal.Code != "E_DELIVERY_LIMIT" {
		t.Fatalf("plan %+v", pl)
	}
	if err := json.Unmarshal([]byte(mustOK(t, r.call(MethodPlan, readParams()))), &pl); err != nil {
		t.Fatal(err)
	}
	if pl.Verdict != "prompt" || r.reader.reads != 0 {
		t.Fatalf("read plan %+v reads %d", pl, r.reader.reads)
	}
}

// On production the profile name is typed: "y" is not enough.
func TestProdTypedName(t *testing.T) {
	r := newRig(t, rigOpts{production: true, auto: true, answers: []string{"y", "prod"}})
	resp := r.call(MethodRun, readParams())
	if resp.Error == nil || resp.Error.Code != ipc.CodeDenied || r.reader.reads != 0 {
		t.Fatalf("y on production: %+v", resp.Error)
	}
	mustOK(t, r.call(MethodRun, readParams()))
	if r.reader.reads != 1 || r.io.promptCount() != 2 {
		t.Fatalf("reads %d prompts %d", r.reader.reads, r.io.promptCount())
	}
}

// Results are framed as untrusted data.
func TestResultsFramedUntrusted(t *testing.T) {
	r := newRig(t, rigOpts{auto: true})
	for _, p := range []Params{{Op: "overview"}, readParams()} {
		var res Result
		if err := json.Unmarshal([]byte(mustOK(t, r.call(MethodRun, p))), &res); err != nil {
			t.Fatal(err)
		}
		if res.Untrusted != Untrusted || res.Op != p.Op {
			t.Errorf("%s: %+v", p.Op, res)
		}
	}
}

// The audit log holds what was asked, never what was read.
func TestAuditHasNoContent(t *testing.T) {
	r := newRig(t, rigOpts{tier: "peek", auto: true, answers: []string{"y"}})
	mustOK(t, r.call(MethodRun, readParams()))
	mustOK(t, r.call(MethodRun, peekParams("orders.failed")))
	log := r.auditLog()
	for _, leak := range []string{"alice", "Alice", "C-778899", "Order 42", "12.5", "pw"} {
		if strings.Contains(log, leak) {
			t.Errorf("audit holds %q:\n%s", leak, log)
		}
	}
	for _, want := range []string{`"event":"auto"`, `"event":"approved"`, `"event":"run"`, `"op":"peek"`, `"target":"orders.failed"`, `"count":1`} {
		if !strings.Contains(log, want) {
			t.Errorf("audit misses %s:\n%s", want, log)
		}
	}
}

// pii.add appends a mask rule to the project's rules file and applies it.
func TestPIIAdd(t *testing.T) {
	r := newRig(t, rigOpts{auto: true, pii: "[[allow]]\nqueue = [\"//order-events\"]\nbody = \"$.total\"\n"})
	mustOK(t, r.call(ipc.MethodPIIAdd, ipc.PIIAddParams{Pattern: "queue=*/* header=x-trace"}))
	var list ipc.PIIListResult
	if err := json.Unmarshal([]byte(mustOK(t, r.call(ipc.MethodPIIList, nil))), &list); err != nil {
		t.Fatal(err)
	}
	if len(list.Mask) != 1 || list.Mask[0] != "queue=*/* header=x-trace" || len(list.Allow) != 1 {
		t.Fatalf("list %+v", list)
	}
	raw, _ := os.ReadFile(filepath.Join(r.root, ".lockmq", "pii.toml"))
	if !strings.Contains(string(raw), `header = "x-trace"`) {
		t.Errorf("file:\n%s", raw)
	}
	if resp := r.call(ipc.MethodPIIAdd, ipc.PIIAddParams{Pattern: "queue=*/* body=$..x"}); resp.Error == nil || resp.Error.Code != ipc.CodeInvalidParams {
		t.Errorf("bad pattern: %+v", resp.Error)
	}
	if out := mustOK(t, r.call(MethodRun, readParams())); !strings.Contains(out, `"total":12.5`) {
		t.Errorf("allowed path masked: %s", out)
	}
}

func TestChecksTags(t *testing.T) {
	r := newRig(t, rigOpts{})
	if f := r.be.Checks(context.Background(), nil); len(f) != 0 {
		t.Errorf("monitoring: %+v", f)
	}
	r.broker.tags = []any{"administrator", "monitoring"}
	if _, err := r.be.Connect(context.Background(), console.Credentials{User: "ro", Secret: []byte("pw")}); err != nil {
		t.Fatal(err)
	}
	if f := r.be.Checks(context.Background(), nil); len(f) != 1 || !strings.Contains(f[0].Text, "administrator") {
		t.Errorf("administrator: %+v", f)
	}
	if _, err := r.be.Connect(context.Background(), console.Credentials{User: "ro", Secret: []byte("wrong")}); err == nil || strings.Contains(err.Error(), "wrong") {
		t.Errorf("bad password: %v", err)
	}
}

// A broker account tagged administrator stops a production console before
// it listens.
func TestPrivilegeAudit(t *testing.T) {
	broker := newBroker(t)
	broker.tags = "administrator"
	root, state := t.TempDir(), t.TempDir()
	writeProject(t, root, broker.srv.URL, rigOpts{production: true})
	io := &fakeIO{answers: []string{"y", "prod"}, secrets: []string{"pw"}}
	be := New(Options{Profile: "prod", Root: root, StateDir: state, IO: io})
	d := t.TempDir()
	t.Setenv("LOCKMQ_RUNTIME_DIR", d)
	env := isolation()
	err := console.Run(context.Background(), product.P, be, console.Options{Profile: "prod", ProjectRoot: root, IO: io, StateDir: state,
		Isolation: &env, Harden: func() error { return nil }, Sanitizer: Sanitizer()})
	if err == nil || !strings.Contains(err.Error(), "administrator") {
		t.Fatalf("err = %v\n%s", err, io.output())
	}
}

// isolation is a same-user test environment for console.Run.
func isolation() console.IsolationEnv {
	return console.IsolationEnv{
		LoadSys:        func() (*sysconf.Config, error) { return nil, nil },
		Display:        func() sysconf.Display { return sysconf.Display{Kind: sysconf.DisplayWayland} },
		UID:            func() int { return 900 },
		UserName:       func() (string, error) { return "lockmq", nil },
		LoginUID:       func() (int, bool) { return 900, true },
		TerminalOwner:  func() (int, bool) { return 900, true },
		ClientAllowed:  func(*sysconf.Config, int) bool { return true },
		ClientGID:      func(*sysconf.Config) (int, error) { return 950, nil },
		AccountExists:  func(int) bool { return true },
		Relay:          func() string { return "" },
		MachinectlUIDs: func() ([]int, bool) { return nil, true },
		DesktopEnv:     func() bool { return false },
		DisplaySet:     func() bool { return false },
		ParentChain:    func() ([]sysconf.Proc, error) { return []sysconf.Proc{{PID: 1, UID: 0, Comm: "systemd"}}, nil },
		NewVerifier:    func(string) (fingerprint.Verifier, error) { return &fingerprint.Fake{}, nil },
		AllowSameUser:  true,
	}
}
