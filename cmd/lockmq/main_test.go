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
		container:  func() bool { return false },
		svc:        func(string) (svcAccount, bool) { return svcAccount{}, false },
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
	_, _, _ = runWith(t, root, conn, nil, "", "read", "order-events", "--from", "first", "--unmask")
	_, _, _ = runWith(t, root, conn, nil, "", "pii", "add", "queue=*/* header=x-ref")
	_, _, _ = runWith(t, root, conn, nil, "", "request", "tier=peek")
	want := []string{
		`op.run {"op":"bindings","target":"exchange:orders"}`,
		`op.plan {"op":"peek","target":"orders.failed","count":2,"intent":"why"}`,
		`op.run {"op":"read","target":"order-events","from":"first","unmask":true}`,
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

// The host install and update are refused: lockmq runs in Docker, and the
// launcher on the host updates itself.
func TestHostModeCommandsRefused(t *testing.T) {
	for _, args := range [][]string{{"install"}, {"install", "--yes"}, {"update"}, {"update", "v1.0.0"}} {
		var out, errb bytes.Buffer
		e := env{stdin: strings.NewReader(""), stdout: &out, stderr: &errb, cwd: t.TempDir()}
		if code := run(e, args); code != exitFail {
			t.Errorf("%v: exit %d, want %d", args, code, exitFail)
		}
		if !strings.Contains(errb.String(), "runs in Docker") || !strings.Contains(errb.String(), "scripts/install.sh") ||
			!strings.Contains(errb.String(), "`lockmq update` on the host") || !strings.Contains(errb.String(), "docs/docker.md") || out.Len() != 0 {
			t.Errorf("%v: stdout %q stderr %q", args, out.String(), errb.String())
		}
	}
}

// shellQuote makes any path one shell word.
func TestShellQuote(t *testing.T) {
	for in, want := range map[string]string{"/p": "'/p'", "a b": "'a b'", "it's": `'it'\''s'`, "": "''"} {
		if got := shellQuote(in); got != want {
			t.Errorf("shellQuote(%q) = %s, want %s", in, got, want)
		}
	}
}

// The help names no host install; update is the launcher's.
func TestHelpHasNoHostInstall(t *testing.T) {
	for _, gone := range []string{"\n  install ", "/etc/lockmq (sudo)", "(sudo)"} {
		if strings.Contains(usageText, gone) {
			t.Errorf("help still names %q:\n%s", gone, usageText)
		}
	}
	if !strings.Contains(usageText, "\n  update ") {
		t.Errorf("help lacks the launcher's update:\n%s", usageText)
	}
}

// The tests run outside lea: no requester comes from the environment.
func TestMain(m *testing.M) {
	os.Unsetenv("LEA_AGENT")
	os.Unsetenv("LEA_AGENT_TOKEN")
	os.Exit(m.Run())
}

func TestUsageNamesHubPlainHTTP(t *testing.T) {
	if !strings.Contains(usageText, "[--hub-plain-http]") {
		t.Errorf("help does not name --hub-plain-http:\n%s", usageText)
	}
}

// A secret pasted as --profile is not echoed: neither an invalid name
// (lockkit v0.8.1's CheckProfileName does not quote it) nor a well-formed
// one that names no configured profile.
func TestProfileFlagDoesNotEcho(t *testing.T) {
	root := project(t, oneProfile)
	for _, v := range []string{"glsa_CanarySecretToken123", "https://user:glsa_Canary@h/path?token=glsa_Canary"} {
		code, out, stderr := runWith(t, root, &fakeConn{}, nil, "", "overview", "--profile", v)
		if code != exitUsage {
			t.Errorf("%s: exit %d, want %d", v, code, exitUsage)
		}
		if strings.Contains(out+stderr, "glsa_Canary") || strings.Contains(out+stderr, "user:") || !strings.Contains(stderr, "--profile") {
			t.Errorf("%s: stderr %q", v, stderr)
		}
	}
}

// A secret pasted as a flag (name or value) of a client command is not
// echoed by the usage error.
func TestClientFlagErrorsDoNotEcho(t *testing.T) {
	root := project(t, oneProfile)
	for _, args := range [][]string{
		{"queues", "--page", "https://user:glsa_Canary@h"},
		{"queues", "--glsa_CanarySecretToken123"},
		{"queues", "--glsa_CanarySecretToken123=x"},
	} {
		code, out, stderr := runWith(t, root, &fakeConn{}, nil, "", args...)
		if code != exitUsage {
			t.Errorf("%v: exit %d, want %d", args, code, exitUsage)
		}
		if strings.Contains(out+stderr, "glsa_Canary") || !strings.Contains(stderr, "wrong arguments") {
			t.Errorf("%v: stderr %q", args, stderr)
		}
	}
}
