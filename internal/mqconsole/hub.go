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
	"github.com/DavidGodefroid/lockkit/refusal"
	"github.com/DavidGodefroid/lockkit/render"
	"github.com/DavidGodefroid/lockkit/ticket"

	"github.com/DavidGodefroid/lockmq/internal/ops"
	"github.com/DavidGodefroid/lockmq/internal/product"
)

// hubPlanTTL is how long a hub plan waits for its decision and its run.
const hubPlanTTL = 24 * time.Hour

// hubBody is a hub plan as the plan store keeps it: the request, run
// again from scratch once decided.
type hubBody struct {
	Params Params `json:"params"`
}

// planInputs are what the plan hash of a request covers: the operation,
// vhost, target and arguments, and the queue's properties as prepare read
// them (type, arguments, policies, broker major). The policy fingerprint is
// bound beside it. A queue changed after the approval hashes differently.
type planInputs struct {
	Params Params         `json:"params"`
	Queue  map[string]any `json:"queue"`
}

func planInputsOf(p Params, props map[string]any) planInputs {
	p.Intent, p.Ticket, p.Requester = "", "", ipc.Requester{}
	return planInputs{p, props}
}

// planHash is the plan hash of a request (planInputs).
func planHash(p Params, props map[string]any) (string, error) {
	return ticket.HashOf(planInputsOf(p, props))
}

// planDigests are the plan-hash inputs by name ("params", "queue"), as a
// renewal compares them (ticket.InputDigests over the planInputs hashed).
func planDigests(p Params, props map[string]any) (map[string]string, error) {
	return ticket.InputDigests(planInputsOf(p, props))
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
	hash, err := planHash(p, pp.props)
	if err != nil {
		return console.ErrResp(req.ID, ipc.CodeInternal, env.Sanitizer().Error(err))
	}
	inputs, err := planDigests(p, pp.props)
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
		PolicyFP: policyFP(pp.pol), PlanHash: hash, Inputs: inputs}, raw, hubPlanTTL)
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
		hash, err := planHash(body.Params, pp.props)
		return policyFP(pp.pol), hash, err
	}
	run, d, err := h.Client.Begin(h.Store, in.Ticket, caller, recheck)
	if err != nil {
		if errors.Is(err, ticket.ErrExpired) {
			// Expired (pending past its TTL, or an approval that aged
			// before this run): asked again on the hub, the agent hears
			// pending with the new ticket (decision H1).
			return b.renew(ctx, env, h, req.ID, in, caller)
		}
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

// renew answers the run of expired ticket in.Ticket with its renewal and
// audits it with the stored plan's request (not the params the agent sent
// beside the ticket). A ticket renewed already answers its renewal again,
// audited once.
func (b *Backend) renew(ctx context.Context, env console.Env, h *console.Hub, id int64, in Params, caller plans.Caller) ipc.Response {
	var stored Params
	prev := "" // the renewal made already, if any
	if old, err := h.Store.ExpiredByTicket(in.Ticket, caller); err == nil {
		var body hubBody
		if json.Unmarshal(old.Body, &body) == nil {
			stored = body.Params
		}
		if old.RenewedBy != "" {
			if n, err := h.Store.RenewalOf(old.ID); err == nil {
				prev = n.Ticket
			}
		}
	}
	resp := h.RenewResp(ctx, id, in.Ticket, caller)
	base := audit.Base{Event: audit.EventRenew, Decision: "renewed", Approval: "hub", Ticket: in.Ticket,
		Profile: env.Policy().Profile().Name, Intent: stored.Intent}
	var pd struct {
		Ticket string `json:"ticket"`
	}
	if resp.Error != nil {
		base.Decision, base.Error = "refused", resp.Error.Message
	} else if json.Unmarshal(resp.Result, &pd) == nil {
		if prev != "" && pd.Ticket == prev {
			return resp
		}
		base.Reason = "renewed by ticket " + pd.Ticket
	}
	console.StampPeer(ctx, &base)
	env.Audit(Record{Base: base, Op: stored.Op, VHost: stored.VHost, Target: stored.Target, Unmasked: stored.Unmask})
	return resp
}

// OnHub registers the recompute of the hub's renewals on h, before any
// request is served (console.Options.OnHub).
func (b *Backend) OnHub(h *console.Hub) { h.Client.SetRecompute(b.recompute) }

// recompute gives, for a stored hub plan, its policy fingerprint, plan
// hash and plan-hash inputs as they can be known now. It may run on the
// hub's watch goroutine (a renewal the human asks on the hub), so it
// touches neither the console's state nor the broker session: the policy
// is the approved one on disk (inForce: the console saves it before it
// applies it), the params come from the plan. The queue's properties need
// the broker: the "queue" input is ticket.UncheckedInput (the renewal
// shows it unchecked, never one click) and the plan hash stays the stored
// one; the run's recheck reads the queue on the session and refuses one
// that moved (E_STALE).
func (b *Backend) recompute(sp *plans.Plan) (string, string, map[string]string, error) {
	var body hubBody
	if err := json.Unmarshal(sp.Body, &body); err != nil {
		return "", "", nil, err
	}
	pol, err := b.inForce()
	if err != nil {
		return "", "", nil, err
	}
	inputs, err := planDigests(body.Params, nil)
	if err != nil {
		return "", "", nil, err
	}
	inputs["queue"] = ticket.UncheckedInput
	return policyFP(pol), sp.Binding.PlanHash, inputs, nil
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
		r := refusal.New(ops.CodeStale, "the queue changed since the request was approved: nothing was read")
		info := r.Info("", 0)
		resp := console.ErrResp(id, ipc.CodeRefused, r.Reason)
		resp.Error.Data = &info
		return resp
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
