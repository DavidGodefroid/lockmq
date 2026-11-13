//go:build integration && lockfamily_managed_test

package integration

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/DavidGodefroid/lockkit/audit"
	"github.com/DavidGodefroid/lockkit/console"
	"github.com/DavidGodefroid/lockkit/fingerprint"
	"github.com/DavidGodefroid/lockkit/hubclient"
	"github.com/DavidGodefroid/lockkit/hubclient/hubtest"
	"github.com/DavidGodefroid/lockkit/ipc"
	"github.com/DavidGodefroid/lockkit/managed"
	"github.com/DavidGodefroid/lockkit/sysconf"

	"github.com/DavidGodefroid/lockmq/internal/mqconsole"
	"github.com/DavidGodefroid/lockmq/internal/product"
)

// useManaged makes body the machine's managed file for the test.
func useManaged(t *testing.T, body string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), managed.FileName)
	if err := os.WriteFile(path, []byte("version = 1\norganisation = \"Acme\"\n"+body), 0o644); err != nil {
		t.Fatal(err)
	}
	managed.SetTestPath(path)
	t.Cleanup(func() { managed.SetTestPath("") })
}

// runIO is the console's terminal under console.Run: it counts every
// question, answers yes, and types the read-only password.
type runIO struct {
	mu      sync.Mutex
	out     []string
	prompts int
	secrets int
}

func (s *runIO) Println(l string) { s.mu.Lock(); defer s.mu.Unlock(); s.out = append(s.out, l) }
func (s *runIO) Ask(context.Context, string, time.Duration) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.prompts++
	return "y", true
}
func (s *runIO) AskSecret(context.Context, string) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.secrets++
	return []byte(roPass), nil
}
func (s *runIO) counts() (int, int) { s.mu.Lock(); defer s.mu.Unlock(); return s.prompts, s.secrets }
func (s *runIO) output() string     { s.mu.Lock(); defer s.mu.Unlock(); return strings.Join(s.out, "\n") }

// isolation is a same-user environment for console.Run.
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

// runConsole is a console.Run on profile it of root, as `lockmq console`
// runs it. ready is closed once it listens; done gets its error.
type runConsole struct {
	io     *runIO
	state  string
	ready  chan struct{}
	done   chan error
	cancel context.CancelFunc
}

func startConsole(t *testing.T, root string) *runConsole {
	t.Helper()
	t.Setenv("LOCKMQ_RUNTIME_DIR", t.TempDir())
	home := t.TempDir()
	const instance = "it-managed"
	key, err := hubclient.InstanceKey(home, instance)
	if err != nil {
		t.Fatal(err)
	}
	hub := hubtest.New(t, instance, key.Public().(ed25519.PublicKey))
	hubKey := filepath.Join(t.TempDir(), "hub.pub")
	if err := os.WriteFile(hubKey, []byte(base64.StdEncoding.EncodeToString(hub.Pub)+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	c := &runConsole{io: &runIO{}, state: t.TempDir(), ready: make(chan struct{}), done: make(chan error, 1)}
	be := mqconsole.New(mqconsole.Options{Profile: "it", Root: root, StateDir: c.state, IO: c.io})
	env := isolation()
	ctx, cancel := context.WithCancel(context.Background())
	c.cancel = cancel
	var once sync.Once
	go func() {
		c.done <- console.Run(ctx, product.P, be, console.Options{Profile: "it", ProjectRoot: root, IO: c.io, StateDir: c.state,
			Isolation: &env, Harden: func() error { return nil }, Sanitizer: mqconsole.Sanitizer(),
			Hub: console.HubOptions{URL: hub.URL(), KeyFile: hubKey, Instance: instance, Home: home}, OnHub: be.OnHub,
			Ready: func(string) { once.Do(func() { close(c.ready) }) }})
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-c.done:
		case <-time.After(10 * time.Second):
			t.Error("the console did not stop")
		}
	})
	return c
}

func (c *runConsole) auditLog() string {
	raw, _ := os.ReadFile(filepath.Join(c.state, product.P.Name, "audit.log"))
	return string(raw)
}

func writeConfig(t *testing.T, root, cfg string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(root, ".lockmq"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".lockmq", "config.toml"), []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
}

// approvedFiles are the approved policies kept under state.
func approvedFiles(t *testing.T, state string) []string {
	t.Helper()
	var out []string
	_ = filepath.WalkDir(state, func(p string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() && filepath.Base(filepath.Dir(p)) == "approved" && strings.HasSuffix(p, ".json") {
			out = append(out, p)
		}
		return nil
	})
	return out
}

// With [lockmq.bounds] tier = "read", a peek profile is refused at start
// without a question, audited refused-managed; a read profile starts and
// its approval carries the managed fingerprint; a vhost outside the bounds
// added while it runs is refused at the next poll, never pending.
func TestManagedBoundsConsole(t *testing.T) {
	each(t, func(t *testing.T, b *broker) {
		useManaged(t, "[lockmq.bounds]\ntier = \"read\"\nvhosts = [\"/\"]\n")

		root := t.TempDir()
		writeConfig(t, root, profileTOML(b, "peek"))
		c := startConsole(t, root)
		select {
		case err := <-c.done:
			if !errors.Is(err, console.ErrManaged) || !strings.Contains(strings.Join(console.ManagedLines(err), "\n")+err.Error(), "lockmq.bounds.tier") {
				t.Fatalf("start of a peek profile: %v\n%s", err, c.io.output())
			}
			c.done <- err
		case <-c.ready:
			t.Fatal("a peek profile beyond tier = read started")
		case <-time.After(30 * time.Second):
			t.Fatal("no answer from the console")
		}
		if prompts, secrets := c.io.counts(); prompts+secrets != 0 {
			t.Errorf("refused after %d questions and %d secret prompts, want none", prompts, secrets)
		}
		if !strings.Contains(c.auditLog(), "refused-managed") {
			t.Errorf("no refused-managed in the audit:\n%s", c.auditLog())
		}

		root = t.TempDir()
		writeConfig(t, root, profileTOML(b, "read"))
		c = startConsole(t, root)
		select {
		case <-c.ready:
		case err := <-c.done:
			t.Fatalf("a read profile: %v\n%s", err, c.io.output())
		case <-time.After(60 * time.Second):
			t.Fatalf("the console did not start:\n%s", c.io.output())
		}
		files := approvedFiles(t, c.state)
		if len(files) != 1 {
			t.Fatalf("approved policies %v", files)
		}
		raw, _ := os.ReadFile(files[0])
		var doc map[string]any
		if err := json.Unmarshal(raw, &doc); err != nil || !strings.Contains(string(raw), "managed_sha256") {
			t.Fatalf("approved policy without managed_sha256: %s %v", raw, err)
		}
		if !strings.Contains(c.io.output(), "Managed by Acme (sha256:") {
			t.Errorf("no managed line at start:\n%s", c.io.output())
		}

		before := strings.Count(c.auditLog(), "refused-managed")
		prompts, _ := c.io.counts()
		writeConfig(t, root, strings.Replace(profileTOML(b, "read"), `vhosts = ["/"]`, `vhosts = ["/", "other"]`, 1))
		deadline := time.Now().Add(15 * time.Second)
		for strings.Count(c.auditLog(), "refused-managed") == before && time.Now().Before(deadline) {
			time.Sleep(100 * time.Millisecond)
		}
		if strings.Count(c.auditLog(), "refused-managed") == before {
			t.Fatalf("the vhost beyond the bounds was not refused at the poll:\n%s", c.auditLog())
		}
		if now, _ := c.io.counts(); now != prompts {
			t.Errorf("the refused loosening asked %d questions", now-prompts)
		}
		if strings.Contains(c.auditLog(), `"pending"`) {
			t.Errorf("the loosening went pending:\n%s", c.auditLog())
		}
	})
}

// A managed profile is served, and a list through it works; a user or
// project profile of its name fails the load.
func TestManagedProfileServed(t *testing.T) {
	each(t, func(t *testing.T, b *broker) {
		useManaged(t, "[lockmq.profiles.it]\n"+strings.TrimPrefix(profileTOML(b, "read"), "[profiles.it]\n"))
		root := t.TempDir()
		writeConfig(t, root, "")
		state := t.TempDir()
		io := &scriptIO{}
		be := mqconsole.New(mqconsole.Options{Profile: "it", Root: root, StateDir: state, IO: io})
		pol, err := be.LoadPolicy()
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		sess, err := be.Connect(ctx, console.Credentials{User: roUser, Secret: []byte(roPass)})
		if err != nil {
			t.Fatal(err)
		}
		log, err := audit.Open(product.P, state)
		if err != nil {
			t.Fatal(err)
		}
		srv, err := console.NewServer(console.ServerConfig{Product: product.P, Backend: be, Policy: pol, Session: sess,
			Audit: log, IO: io, Sanitizer: mqconsole.Sanitizer()})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(srv.Close)
		raw, _ := json.Marshal(mqconsole.Params{Op: "queues"})
		resp := srv.Handle(context.Background(), ipc.Request{JSONRPC: "2.0", ID: 1, Method: mqconsole.MethodRun, Params: raw})
		if !strings.Contains(fmt.Sprint(okData(t, resp)), classicName) {
			t.Errorf("queues through the managed profile: %s", resp.Result)
		}

		writeConfig(t, root, profileTOML(b, "read"))
		if _, err := mqconsole.New(mqconsole.Options{Profile: "it", Root: root, StateDir: state, IO: io}).LoadPolicy(); err == nil ||
			!strings.Contains(err.Error(), `profile "it" is provided by the company's managed configuration`) {
			t.Fatalf("a project profile of the managed name: %v", err)
		}
	})
}
