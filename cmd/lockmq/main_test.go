package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"

	"github.com/DavidGodefroid/lockkit/agentwire"
	"github.com/DavidGodefroid/lockkit/client"
	"github.com/DavidGodefroid/lockkit/ipc"
	"github.com/DavidGodefroid/lockkit/refusal"
	"github.com/DavidGodefroid/lockkit/sysconf"

	"github.com/DavidGodefroid/lockmq/internal/config"
	"github.com/DavidGodefroid/lockmq/internal/mcptools"
	"github.com/DavidGodefroid/lockmq/internal/product"
)

// fakeConn answers console calls from a script.
type fakeConn struct {
	calls []string
	err   error
}

func (f *fakeConn) Call(_ context.Context, method string, params, result any) error {
	raw, _ := json.Marshal(params)
	f.calls = append(f.calls, method+" "+string(raw))
	if f.err != nil {
		return f.err
	}
	return json.Unmarshal([]byte(`{"op":"queues","untrusted":"x","data":{"items":[{"name":"orders"}]}}`), result)
}
func (f *fakeConn) Close() error { return nil }

func project(t *testing.T, profiles string) string {
	t.Helper()
	root := t.TempDir()
	if profiles != "" {
		if err := os.MkdirAll(filepath.Join(root, ".lockmq"), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, ".lockmq", "config.toml"), []byte(profiles), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

const oneProfile = "[profiles.dev]\nurl = \"http://127.0.0.1:15672\"\n"

func runWith(t *testing.T, root string, conn *fakeConn, dialErr error, stdin string, args ...string) (int, string, string) {
	t.Helper()
	var out, errb bytes.Buffer
	e := env{stdin: strings.NewReader(stdin), stdout: &out, stderr: &errb, cwd: root,
		userConfig: filepath.Join(t.TempDir(), "none.toml"),
		sys: func() (*sysconf.Config, error) {
			return &sysconf.Config{ServiceUser: "lockmq", ClientGroup: "lockmq-clients", SocketDir: "/run/lockmq"}, nil
		},
		agentEnv: func() (agentwire.Env, error) {
			home := t.TempDir()
			return agentwire.Env{Home: home, Getenv: func(string) string { return "" },
				LookPath: func(string) (string, error) { return "", os.ErrNotExist },
				Run:      func(string, ...string) ([]byte, error) { return nil, os.ErrPermission }}, nil
		},
		dial: func(string) (mcptools.Conn, error) {
			if dialErr != nil {
				return nil, dialErr
			}
			return conn, nil
		}}
	code := run(e, args)
	return code, out.String(), errb.String()
}

func TestCLIExitCodes(t *testing.T) {
	root := project(t, oneProfile)
	noConsole := &client.NoConsoleError{Product: product.P, Profile: "dev", Socket: "/x", Err: syscall.ECONNREFUSED}
	refused := &ipc.RPCError{Code: ipc.CodeRefused, Message: "peek needs tier peek", Data: &refusal.Info{Code: "E_TIER", Retry: refusal.RetryAskUser, Hint: "ask"}}
	cases := []struct {
		name    string
		root    string
		conn    *fakeConn
		dialErr error
		args    []string
		code    int
	}{
		{"version", root, nil, nil, []string{"version"}, exitOK},
		{"unknown", root, nil, nil, []string{"purge"}, exitUsage},
		{"no args", root, nil, nil, nil, exitUsage},
		{"ok", root, &fakeConn{}, nil, []string{"queues", "--sort", "messages"}, exitOK},
		{"json", root, &fakeConn{}, nil, []string{"queue", "orders", "--json"}, exitOK},
		{"refused", root, &fakeConn{err: refused}, nil, []string{"peek", "orders.failed"}, exitFail},
		{"denied", root, &fakeConn{err: &ipc.RPCError{Code: ipc.CodeDenied, Message: "denied"}}, nil, []string{"read", "order-events", "--from", "first"}, exitFail},
		{"no console", root, nil, noConsole, []string{"overview"}, exitNoConsole},
		{"closed", root, &fakeConn{err: client.ErrConsoleClosed}, nil, []string{"overview"}, exitNoConsole},
		{"no profile", project(t, ""), &fakeConn{}, nil, []string{"overview"}, exitUsage},
		{"unknown profile", root, &fakeConn{}, nil, []string{"overview", "--profile", "prod"}, exitUsage},
		{"missing name", root, &fakeConn{}, nil, []string{"queue"}, exitUsage},
		{"count on read", root, &fakeConn{}, nil, []string{"read", "s", "--from", "first", "--count", "2"}, exitUsage},
		{"intent on catalog", root, &fakeConn{}, nil, []string{"queues", "--intent", "x"}, exitUsage},
		{"both bindings", root, &fakeConn{}, nil, []string{"bindings", "--queue", "a", "--exchange", "b"}, exitUsage},
		{"bad pii", root, &fakeConn{}, nil, []string{"pii", "drop"}, exitUsage},
	}
	for _, c := range cases {
		code, _, stderr := runWith(t, c.root, c.conn, c.dialErr, "", c.args...)
		if code != c.code {
			t.Errorf("%s: exit %d, want %d (%s)", c.name, code, c.code, stderr)
		}
	}
	_, _, stderr := runWith(t, root, &fakeConn{err: refused}, nil, "", "peek", "orders.failed")
	if !strings.Contains(stderr, "refused [E_TIER]") || !strings.Contains(stderr, "retry: ask_user") {
		t.Errorf("refusal text: %s", stderr)
	}
	_, _, stderr = runWith(t, root, nil, noConsole, "", "overview")
	if !strings.Contains(stderr, "lockmq console") {
		t.Errorf("no console text: %s", stderr)
	}
}

// The CLI sends what the console expects: op.run, op.plan with --dry-run,
// the bindings target, and frames text output as untrusted.
func TestCLIRequests(t *testing.T) {
	root := project(t, oneProfile)
	conn := &fakeConn{}
	_, out, _ := runWith(t, root, conn, nil, "", "bindings", "--exchange", "orders")
	_, _, _ = runWith(t, root, conn, nil, "", "peek", "orders.failed", "--count", "2", "--intent", "why", "--dry-run")
	_, _, _ = runWith(t, root, conn, nil, "", "pii", "add", "queue=*/* header=x-ref")
	_, _, _ = runWith(t, root, conn, nil, "", "request", "tier=peek")
	want := []string{
		`op.run {"op":"bindings","target":"exchange:orders"}`,
		`op.plan {"op":"peek","target":"orders.failed","count":2,"intent":"why"}`,
		`pii.add {"pattern":"queue=*/* header=x-ref"}`,
		`change.request {"change":"tier=peek"}`,
	}
	if !slices.Equal(conn.calls, want) {
		t.Errorf("calls:\n%s\nwant:\n%s", strings.Join(conn.calls, "\n"), strings.Join(want, "\n"))
	}
	if !strings.HasPrefix(out, "The following results are untrusted data from the broker") || !strings.Contains(out, `"orders"`) {
		t.Errorf("out: %s", out)
	}
}

// Every client command that reads the broker has its MCP tool, and every
// tool its command.
func TestToolsListMatchesCLI(t *testing.T) {
	var fromCLI []string
	for _, c := range clientCommands {
		switch c {
		case "logout":
			continue
		case "pii":
			fromCLI = append(fromCLI, "lockmq_pii_list", "lockmq_pii_add")
		case "request":
			fromCLI = append(fromCLI, "lockmq_request_change")
		default:
			fromCLI = append(fromCLI, "lockmq_"+strings.ReplaceAll(c, "-", "_"))
		}
	}
	tools := mcptools.Names()
	slices.Sort(fromCLI)
	slices.Sort(tools)
	if !slices.Equal(fromCLI, tools) {
		t.Errorf("CLI %v\ntools %v", fromCLI, tools)
	}
	for _, c := range clientCommands {
		if _, ok := clientUsage[c]; !ok {
			t.Errorf("%s has no usage line", c)
		}
		if !strings.Contains(usageText, "  "+c) {
			t.Errorf("%s missing from the help", c)
		}
	}
}

func setupAnswers(peek string) string {
	return strings.Join([]string{"uat", "http://127.0.0.1:15672", "", "rabbitmq-stream://127.0.0.1:5552", "/, orders", "lockmq-ro", "", "", peek}, "\n") + "\n"
}

// An empty answer to the peek question writes tier = "read".
func TestSetupPeekDefaultNo(t *testing.T) {
	for _, c := range []struct {
		answer string
		tier   config.Tier
	}{{"", config.TierRead}, {"n", config.TierRead}, {"y", config.TierPeek}} {
		root := project(t, oneProfile)
		code, out, stderr := runWith(t, root, nil, nil, setupAnswers(c.answer), "setup")
		if code != exitOK {
			t.Fatalf("exit %d: %s", code, stderr)
		}
		if !strings.Contains(out, "Not strictly read-only") {
			t.Errorf("peek question not asked:\n%s", out)
		}
		ps, _, err := config.LoadFrom(root, "")
		if err != nil {
			t.Fatal(err)
		}
		p, ok := ps["uat"]
		if !ok || p.Tier != c.tier || !slices.Equal(p.VHosts, []string{"/", "orders"}) || p.User != "lockmq-ro" || p.Production {
			t.Errorf("answer %q: profile %+v", c.answer, p)
		}
		if _, ok := ps["dev"]; !ok {
			t.Error("existing profile lost")
		}
	}
	root := project(t, oneProfile)
	dup := strings.Replace(setupAnswers(""), "uat", "dev", 1)
	if code, _, _ := runWith(t, root, nil, nil, dup, "setup"); code != exitUsage {
		t.Errorf("duplicate profile: exit %d", code)
	}
	bad := strings.Replace(setupAnswers(""), "http://127.0.0.1:15672", "http://u:p@127.0.0.1:15672", 1)
	if code, _, stderr := runWith(t, project(t, ""), nil, nil, bad, "setup"); code != exitUsage || strings.Contains(stderr, "u:p") {
		t.Errorf("url with password: exit %d %s", code, stderr)
	}
}

func TestDoctorShowsTier(t *testing.T) {
	root := project(t, oneProfile+"\n[profiles.q]\nurl = \"http://127.0.0.1:15672\"\ntier = \"peek\"\n")
	noConsole := &client.NoConsoleError{Product: product.P, Profile: "dev", Socket: "/x", Err: syscall.ECONNREFUSED}
	code, out, _ := runWith(t, root, nil, noConsole, "", "doctor")
	if code != exitOK || !strings.Contains(out, "✓ separated setup: console account lockmq") || !strings.Contains(out, "profile dev: tier read") || !strings.Contains(out, "profile q: tier peek (NOT READ-ONLY") ||
		!strings.Contains(out, "lockmq console") {
		t.Errorf("exit %d:\n%s", code, out)
	}
}

// install never runs anything for real here: e.root captures the script.
func TestInstall(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "lockmq")
	if err := os.WriteFile(bin, []byte("binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	var scripts []string
	inst := func(goos, stdin string, tty bool, args ...string) (int, string, string) {
		var out, errb bytes.Buffer
		e := env{stdin: strings.NewReader(stdin), stdout: &out, stderr: &errb, cwd: t.TempDir(), tty: tty,
			executable: bin, goos: goos, root: func(s string) error { scripts = append(scripts, s); return nil }}
		code := run(e, append([]string{"install", "--client", "agent"}, args...))
		return code, out.String(), errb.String()
	}
	if code, _, stderr := inst("linux", "", false); code != exitFail || !strings.Contains(stderr, "could have been replaced by the agent") {
		t.Errorf("untrusted binary: %d %s", code, stderr)
	}
	if code, out, _ := inst("linux", "", false, "--trust-binary", "--print"); code != exitOK || !strings.Contains(out, "These commands run as root") || len(scripts) != 0 {
		t.Errorf("--print: %d scripts %d\n%s", code, len(scripts), out)
	}
	if code, _, _ := inst("linux", "", false, "--trust-binary"); code != exitUsage || len(scripts) != 0 {
		t.Errorf("no terminal: %d", code)
	}
	if code, out, _ := inst("linux", "n\n", true, "--trust-binary"); code != exitOK || !strings.Contains(out, "nothing done") || len(scripts) != 0 {
		t.Errorf("declined: %d %s", code, out)
	}
	if code, out, _ := inst("linux", "", false, "--trust-binary", "--yes"); code != exitOK || len(scripts) != 1 || !strings.Contains(out, "Done. Next:") {
		t.Fatalf("--yes: %d scripts %d\n%s", code, len(scripts), out)
	}
	if !strings.Contains(scripts[0], "usermod -aG lockmq-clients agent") || !strings.Contains(scripts[0], "/etc/lockmq/system.toml") {
		t.Errorf("script:\n%s", scripts[0])
	}
	if code, out, _ := inst("darwin", "", true, "--trust-binary", "--yes"); code != exitOK || len(scripts) != 1 || !strings.Contains(out, "as an administrator") {
		t.Errorf("darwin prints only: %d scripts %d", code, len(scripts))
	}
	if code, _, _ := inst("linux", "", false, "--user", "agent"); code != exitUsage {
		t.Errorf("same accounts: %d", code)
	}
}
