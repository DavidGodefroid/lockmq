package mqconsole

import (
	"fmt"

	"github.com/DavidGodefroid/lockkit/console"
	"github.com/DavidGodefroid/lockkit/hubclient"
)

var _ console.PolicyApplier = (*Backend)(nil)

// ApplyPolicy implements console.PolicyApplier: the ticket limits of the
// policy in force (limits.tickets_per_window, limits.ticket_window,
// limits.max_pending) go to the hub client. The console calls it with the
// start policy and with each policy it puts in force, so a raise reaches
// here only once the human reviewed it; a lowering applies at once
// (lockkit#150).
func (b *Backend) ApplyPolicy(p console.Policy) error {
	pol, ok := p.(Policy)
	if !ok {
		return fmt.Errorf("not a lockmq policy: %T", p)
	}
	l := pol.Policy.Profile.TicketLimits()
	b.limMu.Lock()
	defer b.limMu.Unlock()
	b.limits = &l
	if b.hubClient != nil {
		b.hubClient.SetLimits(l)
	}
	return nil
}

// setHubClient keeps the console's hub client for ApplyPolicy and gives
// it the limits of a policy applied already.
func (b *Backend) setHubClient(c *hubclient.Client) {
	b.limMu.Lock()
	defer b.limMu.Unlock()
	b.hubClient = c
	if c != nil && b.limits != nil {
		c.SetLimits(*b.limits)
	}
}
