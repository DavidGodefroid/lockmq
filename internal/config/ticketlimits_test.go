package config

import (
	"strings"
	"testing"
	"time"

	kconfig "github.com/DavidGodefroid/lockkit/config"
	"github.com/DavidGodefroid/lockkit/hubclient"
)

// limits.tickets_per_window, limits.ticket_window and limits.max_pending
// reach the hub client's limits; unset, its defaults apply (lockkit#150).
func TestTicketLimitsParse(t *testing.T) {
	p, err := parseOne(t, minimal)
	if err != nil {
		t.Fatal(err)
	}
	if got := p.TicketLimits(); got != hubclient.DefaultLimits() {
		t.Errorf("unset: %+v, want the defaults %+v", got, hubclient.DefaultLimits())
	}
	p, err = parseOne(t, minimal+"[profiles.main.limits]\ntickets_per_window = 60\nticket_window = \"10m\"\nmax_pending = 40\n")
	if err != nil {
		t.Fatal(err)
	}
	if p.TicketsPerWindow != 60 || p.TicketWindow != 10*time.Minute || p.MaxPending != 40 {
		t.Fatalf("parsed %+v", p.Base)
	}
	want := hubclient.Limits{MaxPending: 40, RateN: 60, RateWindow: 10 * time.Minute, RateTotal: hubclient.RateTotal}
	if got := p.TicketLimits(); got != want {
		t.Errorf("TicketLimits %+v, want %+v", got, want)
	}
}

func TestTicketLimitsInvalid(t *testing.T) {
	for _, c := range []struct{ name, limits, want string }{
		{"negative rate", "tickets_per_window = -1\n", "limits.tickets_per_window"},
		{"negative pending", "max_pending = -3\n", "limits.max_pending"},
		{"window not a duration", "ticket_window = \"soon\"\n", "limits.ticket_window"},
		{"negative window", "ticket_window = \"-5m\"\n", "limits.ticket_window"},
	} {
		t.Run(c.name, func(t *testing.T) {
			_, err := parseOne(t, minimal+"[profiles.main.limits]\n"+c.limits)
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("err %v, want one naming %s", err, c.want)
			}
		})
	}
}

// Raising a ticket limit or shortening the window lets an agent ask the
// human more often: a loosening, reviewed. Lowering is free; an approved
// policy from before the limits existed (0) compares as the default.
func TestTicketLimitsDiff(t *testing.T) {
	a := basePolicy(t)
	field := func(changes []changeT, f string) *changeT {
		for i := range changes {
			if changes[i].Field == f {
				return &changes[i]
			}
		}
		return nil
	}
	for _, c := range []struct {
		name, field string
		set         func(*kconfig.Base)
		loosens     bool
	}{
		{"more tickets", "limits.tickets_per_window", func(b *kconfig.Base) { b.TicketsPerWindow = 60 }, true},
		{"fewer tickets", "limits.tickets_per_window", func(b *kconfig.Base) { b.TicketsPerWindow = 10 }, false},
		{"default tickets", "limits.tickets_per_window", func(b *kconfig.Base) { b.TicketsPerWindow = hubclient.RateN }, false},
		{"shorter window", "limits.ticket_window", func(b *kconfig.Base) { b.TicketWindow = time.Minute }, true},
		{"longer window", "limits.ticket_window", func(b *kconfig.Base) { b.TicketWindow = time.Hour }, false},
		{"more pending", "limits.max_pending", func(b *kconfig.Base) { b.MaxPending = 50 }, true},
		{"fewer pending", "limits.max_pending", func(b *kconfig.Base) { b.MaxPending = 5 }, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			b := a
			c.set(&b.Profile.Base)
			if kconfig.Fingerprint(b) == kconfig.Fingerprint(a) {
				t.Errorf("%s: not in the fingerprint", c.field)
			}
			ch := b.Diff(a)
			got := field(ch, c.field)
			if got == nil || got.Loosens != c.loosens {
				t.Fatalf("%+v, want %s loosens=%v", ch, c.field, c.loosens)
			}
			if back := field(a.Diff(b), c.field); back == nil || back.Loosens && c.loosens {
				t.Errorf("reverse %+v", back)
			}
		})
	}
}

// The status screen names the ticket limits in force.
func TestTicketLimitsDescribe(t *testing.T) {
	p := basePolicy(t)
	p.Profile.TicketsPerWindow = 12
	got := strings.Join(p.Describe(), "\n")
	if !strings.Contains(got, "tickets: 12 per 5m0s · 30 pending") {
		t.Errorf("describe %q", got)
	}
}
