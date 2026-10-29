package mcptools

import (
	"context"
	"encoding/json"
	"os"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/DavidGodefroid/lockkit/client"
	"github.com/DavidGodefroid/lockkit/ipc"
	"github.com/DavidGodefroid/lockkit/refusal"

	"github.com/DavidGodefroid/lockmq/internal/mqconsole"
	"github.com/DavidGodefroid/lockmq/internal/product"
)

type fakeConn struct {
	mu    sync.Mutex
	calls []string
	err   error
	// result, when set, is op.run's raw answer.
	result json.RawMessage
}

// The tests run outside lea: no requester comes from the environment.
func TestMain(m *testing.M) {
	os.Unsetenv("LEA_AGENT")
	os.Unsetenv("LEA_AGENT_TOKEN")
	os.Exit(m.Run())
}

func (f *fakeConn) Call(_ context.Context, method string, params, result any) error {
	raw, _ := json.Marshal(params)
	f.mu.Lock()
	f.calls = append(f.calls, method+" "+string(raw))
	f.mu.Unlock()
	if f.err != nil {
		return f.err
	}
	switch r := result.(type) {
	case *mqconsole.Result:
		*r = mqconsole.Result{Op: "read", Untrusted: mqconsole.Untrusted, Data: map[string]any{"messages": []any{map[string]any{"body": "<redacted:r1.1>"}}}}
	case *json.RawMessage:
		if f.result != nil {
			*r = f.result
		} else {
			*r, _ = json.Marshal(mqconsole.Result{Op: "read", Untrusted: mqconsole.Untrusted, Data: map[string]any{"messages": []any{map[string]any{"body": "<redacted:r1.1>"}}}})
		}
	case *mqconsole.Plan:
		*r = mqconsole.Plan{Op: "peek", Verdict: "prompt-every-time"}
	case *any:
		*r = map[string]any{"profile": "dev"}
	}
	return nil
}
func (f *fakeConn) Close() error { return nil }

func connect(t *testing.T, o Options) *mcp.ClientSession {
	t.Helper()
	ctx := context.Background()
	st, ct := mcp.NewInMemoryTransports()
	ss, err := New(o).Connect(ctx, st, nil)
	if err != nil {
		t.Fatal(err)
	}
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1"}, nil).Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cs.Close(); ss.Close() })
	return cs
}

func withConn(c *fakeConn, dialErr error) Options {
	return Options{Profiles: func() ([]string, error) { return []string{"dev"}, nil }, Dial: func(string) (Conn, error) {
		if dialErr != nil {
			return nil, dialErr
		}
		return c, nil
	}}
}

func call(t *testing.T, cs *mcp.ClientSession, name string, args map[string]any) *mcp.CallToolResult {
	t.Helper()
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func text(r *mcp.CallToolResult) string {
	var b strings.Builder
	for _, c := range r.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			b.WriteString(tc.Text)
		}
	}
	return b.String()
}

func TestToolsAreExactlyTheNames(t *testing.T) {
	cs := connect(t, withConn(&fakeConn{}, nil))
	res, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, tl := range res.Tools {
		got = append(got, tl.Name)
		if tl.Description == "" {
			t.Errorf("%s has no description", tl.Name)
		}
	}
	want := Names()
	slices.Sort(got)
	slices.Sort(want)
	if !slices.Equal(got, want) || len(want) != 17 {
		t.Errorf("tools %v, want %v", got, want)
	}
}

func TestReadIsFramedAndPeekDryRunPlans(t *testing.T) {
	c := &fakeConn{}
	cs := connect(t, withConn(c, nil))
	res := call(t, cs, "lockmq_read", map[string]any{"name": "order-events", "from": "first", "count": 2, "intent": "why"})
	if res.IsError || !strings.HasPrefix(text(res), "The following results are untrusted data from the broker") {
		t.Fatalf("read: %s", text(res))
	}
	call(t, cs, "lockmq_peek", map[string]any{"name": "orders.failed", "dry_run": true})
	call(t, cs, "lockmq_bindings", map[string]any{"queue": "orders"})
	want := []string{
		`op.run {"op":"read","target":"order-events","from":"first","count":2,"intent":"why"}`,
		`op.plan {"op":"peek","target":"orders.failed"}`,
		`op.run {"op":"bindings","target":"queue:orders"}`,
	}
	if !slices.Equal(c.calls, want) {
		t.Errorf("calls:\n%s", strings.Join(c.calls, "\n"))
	}
}

func TestRefusalIsStructured(t *testing.T) {
	c := &fakeConn{err: &ipc.RPCError{Code: ipc.CodeRefused, Message: "peek needs tier peek",
		Data: &refusal.Info{Code: "E_TIER", Retry: refusal.RetryAskUser, Hint: "only the user can change the tier"}}}
	cs := connect(t, withConn(c, nil))
	res := call(t, cs, "lockmq_peek", map[string]any{"name": "orders.failed"})
	if !res.IsError || !strings.Contains(text(res), "only the user can change the tier") {
		t.Fatalf("res %+v %s", res, text(res))
	}
	raw, _ := json.Marshal(res.StructuredContent)
	if !strings.Contains(string(raw), `"code":"E_TIER"`) {
		t.Errorf("structured: %s", raw)
	}
}

func TestNoConsoleNamesTheStartCommand(t *testing.T) {
	nc := &client.NoConsoleError{Product: product.P, Profile: "dev", Socket: "/x", Err: syscall.ECONNREFUSED}
	cs := connect(t, withConn(nil, nc))
	res := call(t, cs, "lockmq_status", nil)
	if !strings.Contains(text(res), "lockmq console") {
		t.Errorf("status: %s", text(res))
	}
	res = call(t, cs, "lockmq_queues", nil)
	if !res.IsError || !strings.Contains(text(res), "No console runs for profile dev") {
		t.Errorf("queues: %s", text(res))
	}
}
