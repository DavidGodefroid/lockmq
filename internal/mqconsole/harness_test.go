package mqconsole

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/DavidGodefroid/lockkit/audit"
	"github.com/DavidGodefroid/lockkit/console"
	"github.com/DavidGodefroid/lockkit/ipc"

	"github.com/DavidGodefroid/lockmq/internal/broker/stream"
	"github.com/DavidGodefroid/lockmq/internal/msgmask"
	"github.com/DavidGodefroid/lockmq/internal/product"
)

// fakeBroker is a management API with a few fabricated queues.
type fakeBroker struct {
	mu      sync.Mutex
	srv     *httptest.Server
	tags    any
	version string
	// queues by escaped "vhost/name".
	queues map[string]map[string]any
	// peeks counts POST .../get calls; requests every path served.
	peeks    int
	requests []string
	// counts is the x-delivery-count of the head message, per queue.
	counts map[string]int
}

const secretBody = `{"customer": {"email": "alice@example.com", "name": "Alice Example"}, "note": "Order 42 internal remark", "total": 12.5}`

func newBroker(t *testing.T) *fakeBroker {
	b := &fakeBroker{tags: "monitoring", version: "4.1.8", counts: map[string]int{}, queues: map[string]map[string]any{
		"%2F/order-events":  {"name": "order-events", "vhost": "/", "type": "stream", "messages": 3.0, "arguments": map[string]any{}},
		"%2F/orders.failed": {"name": "orders.failed", "vhost": "/", "type": "classic", "messages_ready": 2.0, "messages_unacknowledged": 0.0, "consumers": 0.0},
		"%2F/payments":      {"name": "payments", "vhost": "/", "type": "quorum", "messages_ready": 4.0, "arguments": map[string]any{"x-delivery-limit": 20.0}},
		"%2F/tight":         {"name": "tight", "vhost": "/", "type": "quorum", "messages_ready": 1.0, "arguments": map[string]any{"x-delivery-limit": 3.0}},
		"%2F/orders.dlq":    {"name": "orders.dlq", "vhost": "/", "type": "classic", "messages_ready": 1.0, "messages_unacknowledged": 0.0, "consumers": 0.0},
	}}
	b.srv = httptest.NewServer(http.HandlerFunc(b.serve))
	t.Cleanup(b.srv.Close)
	return b
}

func (b *fakeBroker) serve(w http.ResponseWriter, r *http.Request) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if u, p, ok := r.BasicAuth(); !ok || u != "ro" || p != "pw" {
		w.WriteHeader(401)
		return
	}
	path := r.URL.EscapedPath()
	b.requests = append(b.requests, r.Method+" "+path)
	write := func(v any) { _ = json.NewEncoder(w).Encode(v) }
	switch {
	case path == "/api/whoami":
		write(map[string]any{"name": "ro", "tags": b.tags})
	case path == "/api/overview":
		write(map[string]any{"rabbitmq_version": b.version, "cluster_name": "rabbit@example"})
	case path == "/api/consumers/%2F" || path == "/api/operator-policies/%2F" || path == "/api/feature-flags":
		write([]any{})
	case path == "/api/nodes":
		write([]any{map[string]any{"name": "rabbit@example", "mem_alarm": false, "disk_free_alarm": false}})
	case strings.HasPrefix(path, "/api/queues/%2F/") && strings.HasSuffix(path, "/bindings"):
		write([]any{})
	case strings.HasPrefix(path, "/api/queues/%2F/") && strings.HasSuffix(path, "/get"):
		if r.Method != http.MethodPost {
			w.WriteHeader(405)
			return
		}
		b.peeks++
		key := strings.TrimSuffix(strings.TrimPrefix(path, "/api/queues/"), "/get")
		if key == "%2F/orders.dlq" {
			// A large dead-lettered message without content type, cut at
			// truncate bytes as the management API does.
			var req struct {
				Truncate int `json:"truncate"`
			}
			_ = json.NewDecoder(r.Body).Decode(&req)
			full := dlqBody()
			cut := full
			if req.Truncate > 0 && len(cut) > req.Truncate {
				cut = cut[:req.Truncate]
			}
			write([]map[string]any{{"payload": cut, "payload_bytes": len(full), "payload_encoding": "string",
				"redelivered": false, "exchange": "", "routing_key": "orders.dlq", "message_count": 0, "properties": map[string]any{}}})
			return
		}
		headers := map[string]any{"x-customer-ref": "C-778899"}
		if c, ok := b.counts[key]; ok {
			headers["x-delivery-count"] = float64(c)
		}
		b.counts[key]++
		write([]map[string]any{{"payload": secretBody, "payload_encoding": "string", "redelivered": b.counts[key] > 1,
			"exchange": "orders", "routing_key": "order.alice@example.com", "message_count": 1,
			"properties": map[string]any{"content_type": "application/json", "headers": headers}}})
	case strings.HasPrefix(path, "/api/queues/%2F/"):
		q, ok := b.queues[strings.TrimPrefix(path, "/api/queues/")]
		if !ok {
			w.WriteHeader(404)
			return
		}
		write(q)
	case path == "/api/queues/%2F":
		var items []any
		for _, q := range b.queues {
			items = append(items, q)
		}
		write(map[string]any{"items": items, "total_count": len(items), "filtered_count": len(items), "page": 1, "page_size": 100})
	default:
		w.WriteHeader(404)
	}
}

// dlqBody is an ~80 KiB JSON array of order records with names and
// streets: personal data no detector recognises.
func dlqBody() string {
	rec := `{"name":"Jane Doe","street":"12 Elm Road","note":"` + strings.Repeat("x", 200) + `"}`
	return "[" + strings.TrimSuffix(strings.Repeat(rec+",", 300), ",") + "]"
}

func (b *fakeBroker) peekCount() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.peeks
}

// fakeReader serves stream reads with fabricated messages.
type fakeReader struct{ reads int }

func (f *fakeReader) Read(_ context.Context, _, _ string, _ stream.From, max int) ([]msgmask.Message, error) {
	f.reads++
	var out []msgmask.Message
	for i := range min(max, 2) {
		out = append(out, msgmask.Message{Body: []byte(secretBody), ContentType: "application/json",
			Headers: map[string]any{"x-stream-offset": int64(100 + i), "x-customer-ref": "C-778899"}})
	}
	return out, nil
}
func (f *fakeReader) Stats(context.Context, string, string) (int64, int64, error) {
	return 100, 102, nil
}
func (f *fakeReader) QueryOffset(context.Context, string, string, string) (int64, error) {
	return 101, nil
}

// fakeIO answers prompts from a script and keeps what was printed.
type fakeIO struct {
	mu      sync.Mutex
	out     []string
	prompts []string
	answers []string
	secrets []string
}

func (f *fakeIO) Println(s string) { f.mu.Lock(); defer f.mu.Unlock(); f.out = append(f.out, s) }
func (f *fakeIO) Ask(_ context.Context, prompt string, _ time.Duration) (string, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.prompts = append(f.prompts, prompt)
	if len(f.answers) == 0 {
		return "", false
	}
	a := f.answers[0]
	f.answers = f.answers[1:]
	return a, true
}
func (f *fakeIO) AskSecret(_ context.Context, prompt string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.prompts = append(f.prompts, prompt)
	if len(f.secrets) == 0 {
		return nil, errors.New("no answer")
	}
	s := f.secrets[0]
	f.secrets = f.secrets[1:]
	return []byte(s), nil
}
func (f *fakeIO) promptCount() int { f.mu.Lock(); defer f.mu.Unlock(); return len(f.prompts) }
func (f *fakeIO) output() string   { f.mu.Lock(); defer f.mu.Unlock(); return strings.Join(f.out, "\n") }

// rig is a console serving one profile of a project against the fake
// broker.
type rig struct {
	t      *testing.T
	broker *fakeBroker
	reader *fakeReader
	io     *fakeIO
	be     *Backend
	srv    *console.Server
	root   string
	state  string
}

type rigOpts struct {
	production bool
	tier       string
	auto       bool
	pii        string
	answers    []string
	// allowUnmask and showResults are the console's switches at start.
	allowUnmask, showResults bool
}

func writeProject(t *testing.T, root, url string, o rigOpts) {
	t.Helper()
	name := "uat"
	if o.production {
		name = "prod"
	}
	tier := o.tier
	if tier == "" {
		tier = "read"
	}
	cfg := "[profiles." + name + "]\nurl = \"" + url + "\"\namqp_url = \"amqp://127.0.0.1:5672\"\nstream_url = \"rabbitmq-stream://127.0.0.1:5552\"\n" +
		"user = \"ro\"\ntier = \"" + tier + "\"\n"
	if o.production {
		cfg += "production = true\n"
	}
	dir := filepath.Join(root, ".lockmq")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "config.toml"), []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	if o.pii != "" {
		if err := os.WriteFile(filepath.Join(dir, "pii.toml"), []byte(o.pii), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

func newRig(t *testing.T, o rigOpts) *rig {
	t.Helper()
	r := &rig{t: t, broker: newBroker(t), reader: &fakeReader{}, io: &fakeIO{answers: o.answers}, root: t.TempDir(), state: t.TempDir()}
	writeProject(t, r.root, r.broker.srv.URL, o)
	name := "uat"
	if o.production {
		name = "prod"
	}
	r.be = New(Options{Profile: name, Root: r.root, StateDir: r.state, IO: r.io})
	fake := func(string, int, *tls.Config, string, []byte, stream.DialFunc) stream.Reader { return r.reader }
	r.be.newStreamProto, r.be.newAMQP = fake, fake
	pol, err := r.be.LoadPolicy()
	if err != nil {
		t.Fatal(err)
	}
	sess, err := r.be.Connect(context.Background(), console.Credentials{User: "ro", Secret: []byte("pw")})
	if err != nil {
		t.Fatal(err)
	}
	log, err := audit.Open(product.P, r.state)
	if err != nil {
		t.Fatal(err)
	}
	r.srv, err = console.NewServer(console.ServerConfig{Product: product.P, Backend: r.be, Policy: pol, Session: sess,
		Audit: log, IO: r.io, Auto: o.auto, AllowUnmask: o.allowUnmask, ShowResults: o.showResults, Sanitizer: Sanitizer()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(r.srv.Close)
	return r
}

func (r *rig) call(method string, params any) ipc.Response {
	r.t.Helper()
	raw, err := json.Marshal(params)
	if err != nil {
		r.t.Fatal(err)
	}
	return r.srv.Handle(context.Background(), ipc.Request{JSONRPC: "2.0", ID: 7, Method: method, Params: raw})
}

func (r *rig) auditLog() string {
	raw, _ := os.ReadFile(filepath.Join(r.state, product.P.Name, "audit.log"))
	return string(raw)
}

func mustOK(t *testing.T, resp ipc.Response) string {
	t.Helper()
	if resp.Error != nil {
		t.Fatalf("error %d: %s", resp.Error.Code, resp.Error.Message)
	}
	return string(resp.Result)
}

func mustRefused(t *testing.T, resp ipc.Response, code string) {
	t.Helper()
	if resp.Error == nil || resp.Error.Code != ipc.CodeRefused || resp.Error.Data == nil || string(resp.Error.Data.Code) != code {
		t.Fatalf("want refusal %s, got %+v (%+v)", code, resp.Error, resp.Result)
	}
}
