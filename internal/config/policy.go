package config

import (
	"fmt"
	"slices"
	"strconv"
	"strings"

	kconfig "github.com/DavidGodefroid/lockkit/config"
)

// DefaultMaskMode is the mode of a mask rule that sets none.
const DefaultMaskMode = "redact"

// Policy is the security-relevant view of one profile plus its PII rules:
// what the console fingerprints, compares with the last approved one and
// asks the human about before applying any loosening. Mask and Allow hold
// rules in their canonical text form (package msgmask).
type Policy struct {
	Profile Profile  `json:"profile"`
	Mask    []string `json:"pii_mask"`
	Allow   []string `json:"pii_allow"`
	// Modes maps a mask rule to its mode when it is not redact.
	Modes map[string]string `json:"pii_modes,omitempty"`
}

var _ kconfig.Policy = Policy{}

// NewPolicy builds the canonical policy of p with its PII rules.
func NewPolicy(p Profile, mask, allow []string, modes map[string]string) Policy {
	return Canonical(Policy{Profile: p, Mask: mask, Allow: allow, Modes: modes})
}

// Canonical sorts and de-duplicates the lists of p and drops the modes of
// rules it does not hold and the default mode: its JSON is the fingerprinted
// form.
func Canonical(p Policy) Policy {
	p.Profile.Detectors = kconfig.SortedSet(p.Profile.Detectors)
	p.Profile.VHosts = kconfig.SortedSet(p.Profile.VHosts)
	p.Mask = kconfig.SortedSet(p.Mask)
	p.Allow = kconfig.SortedSet(p.Allow)
	var modes map[string]string
	for rule, m := range p.Modes {
		if m == "" || m == DefaultMaskMode {
			continue
		}
		if _, found := slices.BinarySearch(p.Mask, rule); !found {
			continue
		}
		if modes == nil {
			modes = map[string]string{}
		}
		modes[rule] = m
	}
	p.Modes = modes
	return p
}

func (p Policy) modeOf(rule string) string {
	if m, ok := p.Modes[rule]; ok {
		return m
	}
	return DefaultMaskMode
}

// Diff lists the changes from approved to p and marks each loosening
// (design §4): read → peek, a lower delivery_headroom, any larger limit, a
// longer request_timeout, a new vhost (or every vhost), any change of url,
// amqp_url or stream_url, body_mode redact-unknown → detect, a removed
// detector or mask rule, an added allow rule, a mask mode away from redact,
// and the shared rules of the profile base (kconfig.DiffBase).
func (p Policy) Diff(approved kconfig.Policy) []kconfig.Change {
	a, ok := approved.(Policy)
	if !ok {
		return []kconfig.Change{{Field: "policy", Old: fmt.Sprintf("%T", approved), New: "lockmq", Loosens: true}}
	}
	a, c := Canonical(a), Canonical(p)
	ap, cp := a.Profile, c.Profile
	out := kconfig.DiffBase(ap.Base, cp.Base)
	same := func(field, o, n string) {
		if o != n {
			out = append(out, kconfig.Change{Field: field, Old: o, New: n, Loosens: true})
		}
	}
	// Another URL is another broker; removing an optional protocol only
	// tightens.
	same("url", ap.URL, cp.URL)
	for _, f := range []struct{ field, o, n string }{{"amqp_url", ap.AMQPURL, cp.AMQPURL}, {"stream_url", ap.StreamURL, cp.StreamURL}} {
		if f.o != f.n {
			out = append(out, kconfig.Change{Field: f.field, Old: f.o, New: f.n, Loosens: f.n != ""})
		}
	}
	switch {
	case len(ap.VHosts) > 0 && len(cp.VHosts) == 0:
		out = append(out, kconfig.Change{Field: "vhosts", Old: strings.Join(ap.VHosts, ", "), New: "(every vhost)", Loosens: true})
	case len(ap.VHosts) == 0 && len(cp.VHosts) > 0:
		out = append(out, kconfig.Change{Field: "vhosts", Old: "(every vhost)", New: strings.Join(cp.VHosts, ", "), Loosens: false})
	default:
		out = append(out, kconfig.SetDiff("vhosts", ap.VHosts, cp.VHosts, false)...)
	}
	if ap.Tier != cp.Tier {
		out = append(out, kconfig.Change{Field: "tier", Old: ap.Tier.String(), New: cp.Tier.String(), Loosens: cp.Tier > ap.Tier})
	}
	if ap.BodyMode != cp.BodyMode {
		out = append(out, kconfig.Change{Field: "body_mode", Old: ap.BodyMode, New: cp.BodyMode, Loosens: cp.BodyMode != BodyRedactUnknown})
	}
	out = append(out, kconfig.SetDiff("detectors", ap.Detectors, cp.Detectors, true)...)

	al, cl := ap.Limits, cp.Limits
	for _, f := range []struct {
		field string
		o, n  int64
	}{
		{"read_max", int64(al.ReadMax), int64(cl.ReadMax)},
		{"peek_max", int64(al.PeekMax), int64(cl.PeekMax)},
		{"max_body_bytes", int64(al.MaxBodyBytes), int64(cl.MaxBodyBytes)},
		{"max_output_bytes", int64(al.MaxOutputBytes), int64(cl.MaxOutputBytes)},
		{"list_page_size", int64(al.ListPageSize), int64(cl.ListPageSize)},
	} {
		if f.o != f.n {
			out = append(out, kconfig.Change{Field: "limits." + f.field, Old: strconv.FormatInt(f.o, 10), New: strconv.FormatInt(f.n, 10), Loosens: f.n > f.o})
		}
	}
	if al.RequestTimeout != cl.RequestTimeout {
		out = append(out, kconfig.Change{Field: "limits.request_timeout", Old: al.RequestTimeout.String(), New: cl.RequestTimeout.String(), Loosens: cl.RequestTimeout > al.RequestTimeout})
	}
	if al.DeliveryHeadroom != cl.DeliveryHeadroom {
		out = append(out, kconfig.Change{Field: "limits.delivery_headroom", Old: strconv.Itoa(al.DeliveryHeadroom), New: strconv.Itoa(cl.DeliveryHeadroom), Loosens: cl.DeliveryHeadroom < al.DeliveryHeadroom})
	}

	out = append(out, kconfig.SetDiff("pii.mask", a.Mask, c.Mask, true)...)
	out = append(out, kconfig.SetDiff("pii.allow", a.Allow, c.Allow, false)...)
	for _, rule := range c.Mask {
		if _, found := slices.BinarySearch(a.Mask, rule); !found {
			continue
		}
		if om, nm := a.modeOf(rule), c.modeOf(rule); om != nm {
			out = append(out, kconfig.Change{Field: "pii.mode " + rule, Old: om, New: nm, Loosens: nm != DefaultMaskMode})
		}
	}
	return out
}

// Describe is the policy as lines for the console's status screen.
func (p Policy) Describe() []string {
	c := Canonical(p)
	pr := c.Profile
	vh := "every vhost"
	if len(pr.VHosts) > 0 {
		vh = strings.Join(pr.VHosts, ", ")
	}
	lines := []string{
		"management API: " + pr.URL,
	}
	if pr.AMQPURL != "" {
		lines = append(lines, "AMQP: "+pr.AMQPURL)
	}
	if pr.StreamURL != "" {
		lines = append(lines, "stream protocol: "+pr.StreamURL)
	}
	tier := "tier: " + pr.Tier.String()
	if pr.Tier == TierPeek {
		tier += " (NOT READ-ONLY: queue peeks requeue messages, redelivered=true, delivery count may grow)"
	}
	lines = append(lines,
		"vhosts: "+vh,
		tier,
		fmt.Sprintf("production: %t · tls: %s · credentials: %s", pr.Production, pr.TLS, pr.Credentials),
		fmt.Sprintf("limits: read_max %d · peek_max %d · delivery_headroom %d · max_body_bytes %d · max_output_bytes %d · list_page_size %d · request_timeout %s",
			pr.Limits.ReadMax, pr.Limits.PeekMax, pr.Limits.DeliveryHeadroom, pr.Limits.MaxBodyBytes, pr.Limits.MaxOutputBytes, pr.Limits.ListPageSize, pr.Limits.RequestTimeout),
		fmt.Sprintf("session: idle %s · max %s", pr.IdleTimeout, pr.MaxSession),
		"body_mode: "+pr.BodyMode+" · detectors: "+strings.Join(pr.Detectors, ", "),
	)
	if pr.SSH != nil {
		lines = append(lines, "ssh: "+kconfig.SSHString(pr.SSH))
	}
	for _, r := range c.Mask {
		lines = append(lines, "mask "+r+" ("+c.modeOf(r)+")")
	}
	for _, r := range c.Allow {
		lines = append(lines, "allow "+r)
	}
	return lines
}
