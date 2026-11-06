package mqconsole

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/DavidGodefroid/lockkit/audit"
	kconfig "github.com/DavidGodefroid/lockkit/config"
	"github.com/DavidGodefroid/lockkit/console"
	"github.com/DavidGodefroid/lockkit/ipc"
	"github.com/DavidGodefroid/lockkit/lazyconn"
	"github.com/DavidGodefroid/lockkit/pii"
	"github.com/DavidGodefroid/lockkit/refusal"
	"github.com/DavidGodefroid/lockkit/render"
	"github.com/DavidGodefroid/lockkit/ticket"

	"github.com/DavidGodefroid/lockmq/internal/config"
	"github.com/DavidGodefroid/lockmq/internal/ops"
	"github.com/DavidGodefroid/lockmq/internal/product"
)

// The profile's auto rules (config.AutoRules) are enforced here, on the
// lock, whatever the decider does:
//
//   - deny: the request is refused before any ticket (E_AUTO_DENIED), in
//     auto and manual mode alike; a connection is not opened;
//   - ask: the ticket carries Flags.NeverAuto (an auto rule's approval is
//     refused, ticket.ErrAuto). A connect ticket under ask is NeverAuto
//     until a human approved one under the approved policy in force (the
//     first connection of each console run and each policy fingerprint);
//     only the reconnections after it may be approved by an auto rule;
//   - auto: the ticket goes to the hub as a version-2 ticket of kind
//     action, its verb the action, for `lox run`'s auto mode to approve;
//     a metadata read (list) under auto runs at once, as it always did.
//
// The rules are those of the approved policy, never the config files the
// agent can edit: a file-only loosening changes nothing here until the
// human approves it. `lox run` reads them with `lockmq profile rules
// --json` (the approved policy's, the file rules that differ flagged as
// pending review), but the ticket's signed Flags.NeverAuto is what binds:
// the lock refuses an auto rule's approval of it whatever the decider
// read. The decision names its rule (ticket.Rule), audited with the
// lock's own.

// actionOf is the auto action of a catalogue operation.
func actionOf(op string) string {
	switch op {
	case "read":
		return config.ActRead
	case "peek":
		return config.ActPeek
	}
	return config.ActList
}

// autoRefusal refuses an action its profile's rule denies.
func autoRefusal(pr config.Profile, action string) *refusal.Error {
	return refusal.Newf(ops.CodeAutoDenied, "%s is denied on profile %s by its auto rules (%s); only the user can change them",
		action, pr.Name, pr.Auto.RuleID(action))
}

// ruleIDOf is the profile rule behind op under the policy in force ("" when
// there is none).
func ruleIDOf(env console.Env, op string) string {
	if pol, ok := env.Policy().(Policy); ok {
		return pol.Policy.Profile.Auto.RuleID(actionOf(op))
	}
	return ""
}

// decidedBy names who decided d: "auto:<rule>" for one of the decider's
// auto rules, "human" for a human, "" for a version-1 decision.
func decidedBy(d *ticket.Decision) string {
	switch {
	case d == nil || d.Rule == nil:
		return ""
	case d.Rule.By == ticket.RuleAuto:
		return "auto:" + d.Rule.ID
	}
	return d.Rule.By
}

// listScreen is the approval screen of a metadata read its profile asks
// for (auto.list = "ask").
func listScreen(pol Policy, p Params) *console.Screen {
	pr := pol.Policy.Profile
	what := "LIST " + p.Op
	if p.VHost != "" {
		what += "  vhost " + p.VHost
	}
	if p.Target != "" {
		what += "  " + p.Target
	}
	return &console.Screen{
		Header:    fmt.Sprintf("%s · tier %s", hostPort("url", pr.URL), pr.Tier),
		Note:      p.Intent,
		Body:      []string{what, "", "metadata only · no message content", "asked: " + pr.Auto.RuleID(config.ActList)},
		NeverAuto: true,
	}
}

// subjectOf is the structured request of p's ticket: the profile, the
// action as the verb, the target masked like the screen. A target that
// would not be a valid subject target, or that masking changed into one,
// is left out (the masked screen still shows it).
func subjectOf(pol Policy, p Params, pp *prep) *ticket.Subject {
	s := &ticket.Subject{Profile: pol.Policy.Profile.Name, Verb: actionOf(p.Op)}
	target := pp.target
	if target == "" && pp.op.Name != "" && !pp.op.Prompt {
		target = pp.op.Name
	}
	if m, ok := pii.Detect(target, screenDetectors).(string); ok && m == target {
		s.Target = target
	}
	if s.Validate(ticket.KindAction) != nil {
		s.Target = ""
	}
	return s
}

// riskOf is the risk class of op's ticket: message content is read-pii,
// metadata read.
func riskOf(op ops.Op) ticket.Risk {
	if op.Prompt {
		return ticket.RiskReadPII
	}
	return ticket.RiskRead
}

// credentialOf is the profile's credential source as a connect ticket
// names it.
func credentialOf(pr config.Profile) string {
	switch pr.Credentials {
	case kconfig.CredentialsKeychain:
		return ticket.CredentialKeychain
	case kconfig.CredentialsVault:
		return ticket.CredentialVault
	}
	return ticket.CredentialAsk
}

// errConnectDenied: the connection was denied on the approval hub.
var errConnectDenied = errors.New("the broker connection was denied on the approval hub; do not retry unless asked")

// noWaitKey marks a context in which a connect ticket must not be asked
// (the recheck of a hub run): an ungranted session is refused instead.
type noWaitKey struct{}

// ensureConnect lets a request use the broker session s in hub mode only
// once a connect ticket for s was approved under the policy in force: the
// first request of each broker session (the console's login, a
// reconnection) and the first after the approved policy changed asks one
// and waits for its decision; the requests after it share the approval.
// Under auto.connect = "ask" the ticket is NeverAuto, so only a human may
// approve it, until a human approved one under this policy fingerprint in
// this console run: the reconnections after that may be approved by an
// auto rule ("asked once per session", held by the lock). Under "auto" an
// auto rule may approve any of them. An auto.connect = "deny" rule
// refuses every request. Outside hub mode it does nothing.
func (b *Backend) ensureConnect(ctx context.Context, env console.Env, s *session, who ipc.Requester) error {
	h := console.HubOf(env)
	if h == nil {
		return nil
	}
	pol, ok := env.Policy().(Policy)
	if !ok {
		return refusal.New(refusal.Policy, "no lockmq policy in force")
	}
	pr := pol.Policy.Profile
	if pr.Auto.Rule(config.ActConnect) == config.AutoDeny {
		return autoRefusal(pr, config.ActConnect)
	}
	fp := policyFP(pol)
	b.connMu.Lock()
	defer b.connMu.Unlock()
	if b.granted == s && b.grantedFP == fp {
		return nil
	}
	if ctx.Value(noWaitKey{}) != nil {
		return errConnLost{errors.New("the broker session or its approved policy changed since the request was approved: ask again")}
	}
	human := pr.Auto.Rule(config.ActConnect) == config.AutoAsk && b.humanFP != fp
	d, err := b.askConnect(ctx, env, h, pol, who, human)
	if err != nil {
		return err
	}
	b.granted, b.grantedFP = s, fp
	if d.Rule != nil && d.Rule.By == ticket.RuleHuman {
		b.humanFP = fp
	}
	return nil
}

// errHumanConnect: an auto rule approved a connect ticket that only a
// human may approve (the first of auto.connect = "ask").
var errHumanConnect = errors.New("the profile's auto rules ask a human for this broker connection (auto.connect=ask), but an auto rule approved it: not connected; ask again and wait for the human's decision")

// askConnect asks the connect ticket of the session's profile and waits
// for its decision, audited with the profile's rule and the decider's.
// human marks the ticket NeverAuto and takes nothing but a human's
// approval (Rule.By human) on it.
func (b *Backend) askConnect(ctx context.Context, env console.Env, h *console.Hub, pol Policy, who ipc.Requester, human bool) (*ticket.Decision, error) {
	pr := pol.Policy.Profile
	rule := pr.Auto.RuleID(config.ActConnect)
	base := audit.Base{Event: audit.EventPending, Decision: "pending", Approval: "hub", Profile: pr.Name}
	console.StampPeer(ctx, &base)
	rec := func(b audit.Base, d *ticket.Decision) Record {
		return Record{Base: b, Op: config.ActConnect, AutoRule: rule, DecidedBy: decidedBy(d)}
	}
	subj := ticket.Subject{Profile: pr.Name, Endpoint: hostPort("url", pr.URL), Credential: credentialOf(pr)}
	t, err := ticket.NewConnectTicket(ticket.Source{Lock: product.P.Name}, ticket.RiskReadPII, subj, policyFP(pol), b.now(), lazyconn.DefaultAskTimeout)
	if err != nil {
		return nil, err
	}
	t.Flags.Production = pr.Production
	t.Flags.NeverAuto = human
	t.Requester = ticket.Requester{Agent: who.Agent}
	src := render.ScreenSource{
		Header: pr.Name + " · connect",
		Body: []render.Line{
			render.Structure("endpoint: " + subj.Endpoint),
			render.Structure("credentials: " + subj.Credential + " (typed on the lock's console)"),
			render.Structure("rule: " + rule),
		},
		Warnings: []string{},
	}
	if human {
		src.Body = append(src.Body, render.Structure("asked of a human: the first connection under this approved policy (never_auto)"))
	}
	if pr.Production {
		src.Warnings = append(src.Warnings, "production")
	}
	base.Ticket = t.ID
	env.Audit(rec(base, nil))
	env.Println("connect: the broker session waits for its connect ticket " + t.ID + " (" + rule + ")")
	actx, cancel := context.WithTimeout(ctx, lazyconn.DefaultAskTimeout)
	defer cancel()
	d, err := h.Client.Ask(actx, t, render.MaskScreen(src, render.Masking{Detectors: screenDetectors}))
	if err != nil {
		base.Event, base.Decision, base.Error = audit.EventRun, "failed", env.Sanitizer().Error(err)
		env.Audit(rec(base, nil))
		if actx.Err() != nil && ctx.Err() == nil {
			return nil, errConnTimeout
		}
		return nil, fmt.Errorf("connect ticket: %w", err)
	}
	base.Approver, base.Facade, base.HubSig, base.Reason = &d.Approver, d.Facade, d.Sig, d.Reason
	if err := d.Match(t); err != nil {
		base.Event, base.Decision, base.Error = audit.EventRun, "failed", err.Error()
		env.Audit(rec(base, d))
		return nil, fmt.Errorf("connect ticket: %w", err)
	}
	if err := d.Usable(t, b.now(), lazyconn.DefaultMaxAge); err != nil {
		base.Event, base.Decision = audit.EventDenied, string(d.Decision)
		if !errors.Is(err, ticket.ErrDenied) {
			// An approval that cannot be acted on: aged, or an auto
			// rule's on a ticket only a human may decide.
			base.Event, base.Decision, base.Error = audit.EventRun, "failed", err.Error()
		}
		env.Audit(rec(base, d))
		if errors.Is(err, ticket.ErrDenied) {
			return nil, errConnectDenied
		}
		if errors.Is(err, ticket.ErrAuto) {
			return nil, errHumanConnect
		}
		return nil, fmt.Errorf("connect ticket: %w", err)
	}
	if human && (d.Rule == nil || d.Rule.By != ticket.RuleHuman) {
		// Usable refuses an auto rule's approval of a NeverAuto ticket;
		// this also refuses a decision that does not say who decided.
		base.Event, base.Decision, base.Error = audit.EventRun, "failed", ticket.ErrAuto.Error()
		env.Audit(rec(base, d))
		return nil, errHumanConnect
	}
	base.Event, base.Decision = audit.EventApproved, string(d.Decision)
	env.Audit(rec(base, d))
	// The hub's bookkeeping only: the session is usable either way.
	_ = h.Client.Consumed(ctx, t.ID)
	return d, nil
}

// errConnTimeout: no decision came on the connect ticket in time.
var errConnTimeout = errors.New("the connect ticket was not decided in time; do not retry unless asked")

func (b *Backend) now() time.Time {
	if b.clock != nil {
		return b.clock()
	}
	return time.Now()
}
