//go:build integration

package integration

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/DavidGodefroid/lockkit/audit"
	"github.com/DavidGodefroid/lockkit/console"
	"github.com/DavidGodefroid/lockkit/ipc"

	"github.com/DavidGodefroid/lockmq/internal/mqconsole"
	"github.com/DavidGodefroid/lockmq/internal/product"
)

// scriptIO answers the console's prompts from a script.
type scriptIO struct {
	mu      sync.Mutex
	out     []string
	prompts int
	answers []string
}

func (s *scriptIO) Println(l string) { s.mu.Lock(); defer s.mu.Unlock(); s.out = append(s.out, l) }
func (s *scriptIO) Ask(_ context.Context, _ string, _ time.Duration) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.prompts++
	if len(s.answers) == 0 {
		return "", false
	}
	a := s.answers[0]
	s.answers = s.answers[1:]
	return a, true
}
func (s *scriptIO) AskSecret(context.Context, string) ([]byte, error) {
	return nil, errors.New("no secret prompt in tests")
}
func (s *scriptIO) answer(a ...string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.answers = append(s.answers, a...)
}
func (s *scriptIO) promptCount() int { s.mu.Lock(); defer s.mu.Unlock(); return s.prompts }

// rig is a lockmq console on one broker, driven in process: the real
// backend, the real broker connections, lockkit's server and approvals.
type rig struct {
	t    *testing.T
	b    *broker
	io   *scriptIO
	srv  *console.Server
	root string
}

func profileTOML(b *broker, tier string) string {
	return fmt.Sprintf("[profiles.it]\nurl = %q\namqp_url = %q\nstream_url = %q\nuser = %q\nvhosts = [\"/\"]\ntier = %q\n",
		b.MgmtURL(), fmt.Sprintf("amqp://127.0.0.1:%d", b.AMQP), fmt.Sprintf("rabbitmq-stream://localhost:%d", b.Stream), roUser, tier)
}

func (r *rig) writeConfig(tier string) {
	r.t.Helper()
	if err := os.WriteFile(filepath.Join(r.root, ".lockmq", "config.toml"), []byte(profileTOML(r.b, tier)), 0o600); err != nil {
		r.t.Fatal(err)
	}
}

func newRig(t *testing.T, b *broker, tier string) *rig {
	t.Helper()
	r := &rig{t: t, b: b, io: &scriptIO{}, root: t.TempDir()}
	if err := os.MkdirAll(filepath.Join(r.root, ".lockmq"), 0o700); err != nil {
		t.Fatal(err)
	}
	r.writeConfig(tier)
	state := t.TempDir()
	be := mqconsole.New(mqconsole.Options{Profile: "it", Root: r.root, StateDir: state, IO: r.io})
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
	if f := be.Checks(ctx, sess); len(f) != 0 {
		t.Fatalf("checks of the monitoring user: %+v", f)
	}
	log, err := audit.Open(product.P, state)
	if err != nil {
		t.Fatal(err)
	}
	r.srv, err = console.NewServer(console.ServerConfig{Product: product.P, Backend: be, Policy: pol, Session: sess,
		Audit: log, IO: r.io, Sanitizer: mqconsole.Sanitizer()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(r.srv.Close)
	return r
}

func (r *rig) run(op, target string, count int, from string) ipc.Response {
	r.t.Helper()
	p := mqconsole.Params{Op: op, Intent: "integration test"}
	p.Target, p.Count, p.From = target, count, from
	raw, _ := json.Marshal(p)
	return r.srv.Handle(context.Background(), ipc.Request{JSONRPC: "2.0", ID: 1, Method: mqconsole.MethodRun, Params: raw})
}

func okData(t *testing.T, resp ipc.Response) map[string]any {
	t.Helper()
	if resp.Error != nil {
		t.Fatalf("error %d: %s", resp.Error.Code, resp.Error.Message)
	}
	var res struct {
		Untrusted string `json:"untrusted"`
		Data      any    `json:"data"`
	}
	if err := json.Unmarshal(resp.Result, &res); err != nil {
		t.Fatal(err)
	}
	if res.Untrusted == "" {
		t.Error("result not framed as untrusted")
	}
	m, _ := res.Data.(map[string]any)
	if m == nil {
		m = map[string]any{"value": res.Data}
	}
	return m
}

func refusedCode(resp ipc.Response) string {
	if resp.Error == nil || resp.Error.Code != ipc.CodeRefused || resp.Error.Data == nil {
		return ""
	}
	return string(resp.Error.Data.Code)
}

// Every catalogue operation answers through the console with only its
// allowlisted fields, and a stream read through it comes back masked.
func TestCatalogEndToEnd(t *testing.T) {
	each(t, func(t *testing.T, b *broker) {
		r := newRig(t, b, "read")
		cases := []struct{ op, target, consumer string }{
			{"overview", "", ""}, {"queues", "", ""}, {"queue", dlqName, ""}, {"exchanges", "", ""},
			{"bindings", "", ""}, {"bindings", "queue:" + classicName, ""}, {"bindings", "exchange:amq.direct", ""},
			{"policies", "", ""}, {"consumers", "", ""}, {"connections", "", ""}, {"channels", "", ""},
			{"stream", streamName, ""}, {"stream-offset", streamName, namedRef},
		}
		for _, c := range cases {
			p := mqconsole.Params{Op: c.op}
			p.Target, p.Consumer = c.target, c.consumer
			raw, _ := json.Marshal(p)
			resp := r.srv.Handle(context.Background(), ipc.Request{JSONRPC: "2.0", ID: 1, Method: mqconsole.MethodRun, Params: raw})
			if resp.Error != nil {
				t.Errorf("%s %s: %d %s", c.op, c.target, resp.Error.Code, resp.Error.Message)
				continue
			}
			data := okData(t, resp)
			text := string(resp.Result)
			for _, never := range []string{roPass, adminPass, "peer_host", "backing_queue_status", "garbage_collection"} {
				if strings.Contains(text, never) {
					t.Errorf("%s %s: answer holds %q", c.op, c.target, never)
				}
			}
			switch c.op {
			case "queue":
				q, _ := data["queue"].(map[string]any)
				if q["name"] != dlqName || q["type"] != "classic" {
					t.Errorf("queue: %v", q)
				}
			case "stream-offset":
				if fmt.Sprint(data["stored_offset"]) != fmt.Sprint(namedOffset) {
					t.Errorf("stream-offset: %v", data)
				}
			}
		}
		if r.io.promptCount() != 0 {
			t.Errorf("catalogue prompted %d times", r.io.promptCount())
		}

		r.io.answer("y")
		resp := r.run("read", streamName, 3, "first")
		data := okData(t, resp)
		msgs, _ := data["messages"].([]any)
		if len(msgs) != 3 || strings.Contains(string(resp.Result), "@example.com") {
			t.Errorf("read: %d messages, clear email: %v", len(msgs), strings.Contains(string(resp.Result), "@example.com"))
		}
	})
}
