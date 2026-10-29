package mqconsole

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/DavidGodefroid/lockkit/audit"
	kconfig "github.com/DavidGodefroid/lockkit/config"
	"github.com/DavidGodefroid/lockkit/console"
	"github.com/DavidGodefroid/lockkit/hubclient"
	"github.com/DavidGodefroid/lockkit/ipc"
	"github.com/DavidGodefroid/lockkit/pii"
	"github.com/DavidGodefroid/lockkit/plans"
	"github.com/DavidGodefroid/lockkit/render"
	"github.com/DavidGodefroid/lockkit/ticket"

	"github.com/DavidGodefroid/lockmq/internal/product"
)

// hubPlanTTL is how long a hub plan waits for its decision and its run.
const hubPlanTTL = 24 * time.Hour

// hubBody is a hub plan as the plan store keeps it: the request, run
// again from scratch once decided. The plan hash covers it.
type hubBody struct {
	Params Params `json:"params"`
}

// planHash is the plan hash of a request: what the human approves.
func planHash(p Params) (string, error) {
	p.Intent, p.Ticket, p.Requester = "", "", ipc.Requester{}
	return ticket.HashOf(hubBody{Params: p})
}

// policyFP is the fingerprint of the policy in force, in the ticket's form.
func policyFP(pol Policy) string { return "sha256:" + kconfig.Fingerprint(pol.Policy) }

// screenDetectors mask personal data on the hub's screen.
var screenDetectors = func() []pii.Detector {
	ds, err := pii.Detectors([]string{"email", "phone", "iban", "card"})
	if err != nil {
		panic(err)
	}
	return ds
}()

// hubPlan turns a read or peek into a plan decided on the hub: stored,
// bound to the peer, the agent's token and the policy, its masked screen
// pushed in a signed ticket. The agent hears pending.
func (b *Backend) hubPlan(ctx context.Context, req ipc.Request, env console.Env, h *console.Hub, p Params, pp *prep) ipc.Response {
	hash, err := planHash(p)
	if err != nil {
		return console.ErrResp(req.ID, ipc.CodeInternal, env.Sanitizer().Error(err))
	}
	clean := p
	clean.Ticket, clean.Requester = "", ipc.Requester{}
	raw, err := json.Marshal(hubBody{Params: clean})
	if err != nil {
		return console.ErrResp(req.ID, ipc.CodeInternal, env.Sanitizer().Error(err))
	}
	caller := plans.Caller{UID: peerUID(ctx), Token: p.Requester.Token}
	stored, err := h.Store.Create(plans.Binding{UID: caller.UID, Agent: p.Requester.Agent, TokenHash: plans.HashToken(caller.Token),
		PolicyFP: policyFP(pp.pol), PlanHash: hash}, raw, hubPlanTTL)
	if err != nil {
		return console.ErrResp(req.ID, ipc.CodeInternal, env.Sanitizer().Error(err))
	}
	sc := *pp.screen
	sc.Record = func(base audit.Base) audit.Record {
		return Record{Base: base, Op: p.Op, VHost: pp.vhost, Target: pp.target, Unmasked: pp.unmask}
	}
	lines := make([]render.Line, 0, len(sc.Body))
	for _, l := range sc.Body {
		lines = append(lines, render.Structure(l))
	}
	sc.Hub = &console.HubTicket{PlanID: stored.ID, Caller: caller, Risk: ticket.RiskReadPII,
		Requester: ticket.Requester{Agent: p.Requester.Agent},
		Masked: render.MaskScreen(render.ScreenSource{Header: sc.Header, Note: sc.Note, Body: lines, Warnings: sc.Warnings, Red: sc.Red},
			render.Masking{Detectors: screenDetectors})}
	a := env.Approve(ctx, req.ID, sc)
	if a.Pending != nil {
		return a.Refusal
	}
	_ = h.Store.End(stored.ID, caller, plans.Withdrawn, "not submitted")
	if a.Approved {
		return console.ErrResp(req.ID, ipc.CodeInternal, "approved outside the hub in hub mode")
	}
	return a.Refusal
}

// hubRun runs the request of a decided ticket once: the decision verified
// and matched, the request prepared again under the policy in force, its
// hash unchanged.
func (b *Backend) hubRun(ctx context.Context, req ipc.Request, env console.Env, h *console.Hub, in Params) ipc.Response {
	caller := plans.Caller{UID: peerUID(ctx), Token: in.Requester.Token}
	var body hubBody
	var pp *prep
	var prepErr error
	recheck := func(sp *plans.Plan) (string, string, error) {
		if err := json.Unmarshal(sp.Body, &body); err != nil {
			return "", "", err
		}
		pp, prepErr = b.prepare(ctx, env, body.Params)
		if prepErr != nil {
			return "", "", fmt.Errorf("%w: %v", plans.ErrStale, prepErr)
		}
		hash, err := planHash(body.Params)
		return policyFP(pp.pol), hash, err
	}
	run, d, err := h.Client.Begin(h.Store, in.Ticket, caller, recheck)
	if err != nil {
		if d != nil && errors.Is(err, hubclient.ErrDenied) {
			b.auditDecision(ctx, env, in.Ticket, body.Params, d, audit.EventDenied)
		}
		if prepErr != nil {
			return b.errResp(ctx, env, req.ID, body.Params, pp, prepErr)
		}
		return hubRefusal(req.ID, in.Ticket, h.Instance, err)
	}
	b.auditDecision(ctx, env, in.Ticket, body.Params, d, audit.EventApproved)
	p := body.Params
	p.Requester = in.Requester
	resp := b.execute(ctx, env, req.ID, p, pp)
	reason := ""
	if resp.Error != nil {
		reason = resp.Error.Message
	}
	if err := h.Client.Finish(ctx, h.Store, run.ID, caller, resp.Error == nil, reason); err != nil {
		env.Println("ticket " + in.Ticket + ": the hub was not told it was consumed: " + env.Safe(err.Error()))
	}
	return resp
}

// auditDecision records the hub's decision on a ticket.
func (b *Backend) auditDecision(ctx context.Context, env console.Env, id string, p Params, d *ticket.Decision, event string) {
	base := audit.Base{Event: event, Decision: string(d.Decision), Approval: "hub", Reason: d.Reason, Ticket: id,
		Approver: &d.Approver, Facade: d.Facade, HubSig: d.Sig, Profile: env.Policy().Profile().Name, Intent: p.Intent}
	console.StampPeer(ctx, &base)
	env.Audit(Record{Base: base, Op: p.Op, VHost: p.VHost, Target: p.Target, Unmasked: p.Unmask})
}

// hubRefusal answers a run whose ticket cannot run.
func hubRefusal(id int64, tk, instance string, err error) ipc.Response {
	switch {
	case errors.Is(err, hubclient.ErrPending):
		return console.PendingResp(id, product.P.Name, console.Pending{Ticket: tk, Instance: instance})
	case errors.Is(err, hubclient.ErrDenied):
		return console.ErrResp(id, ipc.CodeDenied, "denied by the human on the approval hub; do not retry unless asked")
	case errors.Is(err, ticket.ErrExpired):
		return console.ErrResp(id, ipc.CodeTimeout, "the approval expired; do not retry unless asked")
	case errors.Is(err, plans.ErrStale):
		return console.ErrResp(id, ipc.CodeNoSuchPlan, "the request changed since it was approved (stale): ask again")
	case errors.Is(err, plans.ErrWrongRequester):
		return console.ErrResp(id, ipc.CodeWrongRequester, "the ticket was asked by another agent")
	}
	return console.ErrResp(id, ipc.CodeNoSuchPlan, "no such pending ticket, or it already ran")
}

// peerUID is the uid of the client behind ctx, -1 when unknown.
func peerUID(ctx context.Context) int {
	if c, ok := console.PeerFrom(ctx); ok {
		return c.UID
	}
	return -1
}
