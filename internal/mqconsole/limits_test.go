package mqconsole

import (
	"testing"
	"time"

	"github.com/DavidGodefroid/lockkit/console"
	"github.com/DavidGodefroid/lockkit/hubclient"
	"github.com/DavidGodefroid/lockkit/ipc"

	"github.com/DavidGodefroid/lockmq/internal/product"
)

// The profile's limits.tickets_per_window, limits.ticket_window and
// limits.max_pending reach the hub client, at start and when a policy is
// put in force; a ticket over the limit is refused, nothing left pending
// (lockkit#150).
func TestHubTicketLimits(t *testing.T) {
	r, hub, _ := hubRig(t, rigOpts{limits: "tickets_per_window = 2\nticket_window = \"10m\"\nmax_pending = 5\n"})
	want := hubclient.Limits{MaxPending: 5, RateN: 2, RateWindow: 10 * time.Minute, RateTotal: hubclient.RateTotal}
	if got := r.srv.Hub().Client.Limits(); got != want {
		t.Fatalf("hub client limits %+v, want %+v", got, want)
	}
	// Each read its own ticket; the connect ticket a human approves is the
	// first of the window.
	read := func(n int) ipc.Response {
		p := readParams()
		p.Count = n
		return r.call(MethodRun, p)
	}
	pendingOf(t, read(1))
	before := len(hub.Tickets())
	resp := read(2)
	if resp.Error == nil || resp.Error.Code != ipc.CodeInternal {
		t.Fatalf("over the limit: %+v %s", resp.Error, resp.Result)
	}
	if n := len(hub.Tickets()); n != before {
		t.Fatalf("a ticket over the limit reached the hub: %d tickets, want %d", n, before)
	}

	// A policy put in force with other limits replaces them; 0 is the
	// default.
	pol, err := r.be.LoadPolicy()
	if err != nil {
		t.Fatal(err)
	}
	np := pol.(Policy)
	np.Policy.Profile.TicketsPerWindow, np.Policy.Profile.TicketWindow, np.Policy.Profile.MaxPending = 60, 0, 0
	if err := r.be.ApplyPolicy(np); err != nil {
		t.Fatal(err)
	}
	want = hubclient.DefaultLimits()
	want.RateN = 60
	if got := r.srv.Hub().Client.Limits(); got != want {
		t.Fatalf("after the policy change %+v, want %+v", got, want)
	}
	pendingOf(t, read(3))
}

// Limits applied before the hub is open reach its client when it opens.
func TestApplyPolicyBeforeHub(t *testing.T) {
	r := newRig(t, rigOpts{limits: "max_pending = 7\n"})
	pol, err := r.be.LoadPolicy()
	if err != nil {
		t.Fatal(err)
	}
	if err := r.be.ApplyPolicy(pol); err != nil {
		t.Fatal(err)
	}
	opts, _, _ := hubOptions(t)
	h, err := console.OpenHub(product.P, *opts, t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = h.Close() })
	r.be.OnHub(h)
	if got := h.Client.Limits().MaxPending; got != 7 {
		t.Fatalf("max_pending %d, want 7", got)
	}
}
