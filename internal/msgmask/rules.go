// Package msgmask masks the personal data of RabbitMQ messages: body,
// headers (application properties) and properties, from the rules of
// .lockmq/pii.toml, the profile's body mode, field-name heuristics and
// value detectors (lockkit's pii).
package msgmask

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"slices"
	"strings"

	kconfig "github.com/DavidGodefroid/lockkit/config"
	"github.com/DavidGodefroid/lockkit/pii"

	"github.com/DavidGodefroid/lockmq/internal/product"
)

// Rule masks (or, in an allow list, clears) one body path, header name
// glob or property of the messages of some queues or exchanges.
type Rule struct {
	// Queues and Exchanges are "vhost/name" globs ("*/*", "orders/order-*");
	// the vhost "/" is written as a leading "/" ("//orders").
	Queues, Exchanges []string
	// Exactly one of Body (a JSON path), Header (a name glob) and Property.
	Body, Header, Property string
	// Mode is the mask mode of a mask rule (redact, partial, email); empty
	// in an allow rule.
	Mode string

	path jpath
}

// Rules are the mask and allow rules of a project.
type Rules struct {
	Mask, Allow []Rule
}

// Properties a rule may name: the free-text AMQP properties.
var ruleProperties = []string{"app_id", "cluster_id", "correlation_id", "message_id", "reply_to", "type", "user_id"}

type rawRule struct {
	Queue    []string `toml:"queue"`
	Exchange []string `toml:"exchange"`
	Body     string   `toml:"body"`
	Header   string   `toml:"header"`
	Property string   `toml:"property"`
	Mode     string   `toml:"mode"`
}

type rawRules struct {
	Mask  []rawRule `toml:"mask"`
	Allow []rawRule `toml:"allow"`
}

// LoadRules reads .lockmq/pii.toml (design §7). A missing file is no rules.
func LoadRules(path string) (Rules, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return Rules{}, nil
	}
	if err != nil {
		return Rules{}, fmt.Errorf("pii rules: %w", err)
	}
	var raw rawRules
	if _, err := kconfig.Decode(product.P, data, path, &raw); err != nil {
		return Rules{}, err
	}
	var out Rules
	for i, r := range raw.Mask {
		rule, err := build(r, true)
		if err != nil {
			return Rules{}, fmt.Errorf("pii rules: %s: mask rule %d: %w", path, i+1, err)
		}
		out.Mask = append(out.Mask, rule)
	}
	for i, r := range raw.Allow {
		rule, err := build(r, false)
		if err != nil {
			return Rules{}, fmt.Errorf("pii rules: %s: allow rule %d: %w", path, i+1, err)
		}
		out.Allow = append(out.Allow, rule)
	}
	return out, nil
}

func build(r rawRule, mask bool) (Rule, error) {
	rule := Rule{Queues: kconfig.SortedSet(r.Queue), Exchanges: kconfig.SortedSet(r.Exchange),
		Body: r.Body, Header: r.Header, Property: r.Property, Mode: r.Mode}
	if len(r.Queue) == 0 && len(r.Exchange) == 0 {
		return Rule{}, errors.New("set queue or exchange (a list of vhost/name globs)")
	}
	for _, g := range append(slices.Clone(rule.Queues), rule.Exchanges...) {
		if err := checkGlob(g, true); err != nil {
			return Rule{}, err
		}
	}
	n := 0
	for _, s := range []string{r.Body, r.Header, r.Property} {
		if s != "" {
			n++
		}
	}
	if n != 1 {
		return Rule{}, errors.New("set exactly one of body, header and property")
	}
	switch {
	case r.Body != "":
		p, err := parsePath(r.Body)
		if err != nil {
			return Rule{}, fmt.Errorf("body: %w", err)
		}
		rule.path = p
		rule.Body = p.String()
	case r.Header != "":
		if err := checkGlob(r.Header, false); err != nil {
			return Rule{}, fmt.Errorf("header: %w", err)
		}
	case r.Property != "":
		if !slices.Contains(ruleProperties, r.Property) {
			return Rule{}, fmt.Errorf("property: want one of %s", strings.Join(ruleProperties, ", "))
		}
	}
	if mask {
		if rule.Mode == "" {
			rule.Mode = pii.ModeRedact
		}
		if !pii.ValidMode(rule.Mode) {
			return Rule{}, errors.New("mode: want redact, partial or email")
		}
	} else if rule.Mode != "" {
		return Rule{}, errors.New("an allow rule has no mode")
	}
	return rule, nil
}

// checkGlob refuses a glob that the canonical text form could not hold.
func checkGlob(g string, vhostName bool) error {
	if g == "" || len(g) > 512 || strings.ContainsAny(g, " \t\r\n,=") {
		return fmt.Errorf("glob %q: no spaces, commas or '='", g)
	}
	if err := kconfig.CheckValue(product.P, "glob", g); err != nil {
		return err
	}
	if vhostName && !strings.Contains(g, "/") {
		return fmt.Errorf("glob %q: want vhost/name (\"*/*\", \"//name\" for the vhost /)", g)
	}
	return nil
}

// String is the rule's canonical text: the form stored in the policy and
// sent by clients ("queue=*/* body=$.customer.email"). The mode is kept
// apart (Texts).
func (r Rule) String() string {
	var parts []string
	if len(r.Queues) > 0 {
		parts = append(parts, "queue="+strings.Join(r.Queues, ","))
	}
	if len(r.Exchanges) > 0 {
		parts = append(parts, "exchange="+strings.Join(r.Exchanges, ","))
	}
	switch {
	case r.Body != "":
		parts = append(parts, "body="+r.Body)
	case r.Header != "":
		parts = append(parts, "header="+r.Header)
	case r.Property != "":
		parts = append(parts, "property="+r.Property)
	}
	return strings.Join(parts, " ")
}

// ParseRule parses a mask rule's canonical text (mode redact).
func ParseRule(text string) (Rule, error) {
	var r rawRule
	seen := map[string]bool{}
	for _, f := range strings.Fields(text) {
		k, v, ok := strings.Cut(f, "=")
		if !ok || v == "" || seen[k] {
			return Rule{}, fmt.Errorf("rule: want key=value fields (queue, exchange, body, header, property)")
		}
		seen[k] = true
		switch k {
		case "queue":
			r.Queue = strings.Split(v, ",")
		case "exchange":
			r.Exchange = strings.Split(v, ",")
		case "body":
			r.Body = v
		case "header":
			r.Header = v
		case "property":
			r.Property = v
		default:
			return Rule{}, fmt.Errorf("rule: unknown field %q", k)
		}
	}
	return build(r, true)
}

// Texts returns the canonical texts of the mask and allow rules and the
// modes of the mask rules that are not redact: the policy's form.
func (rs Rules) Texts() (mask, allow []string, modes map[string]string) {
	for _, r := range rs.Mask {
		t := r.String()
		mask = append(mask, t)
		if r.Mode != pii.ModeRedact {
			if modes == nil {
				modes = map[string]string{}
			}
			modes[t] = r.Mode
		}
	}
	for _, r := range rs.Allow {
		allow = append(allow, r.String())
	}
	return kconfig.SortedSet(mask), kconfig.SortedSet(allow), modes
}

// matches reports whether one of globs names vhost/name.
func matches(globs []string, vhost, name string) bool {
	for _, g := range globs {
		gv, gn, _ := strings.Cut(g, "/")
		if gv == "" {
			// "//orders": the vhost "/".
			gv = "/"
			gn = strings.TrimPrefix(g, "//")
		}
		if glob(gv, vhost) && glob(gn, name) {
			return true
		}
	}
	return false
}

// glob matches s against g, where * is any run of characters and ? one.
func glob(g, s string) bool {
	gr, sr := []rune(g), []rune(s)
	gi, si := 0, 0
	star, mark := -1, 0
	for si < len(sr) {
		switch {
		case gi < len(gr) && (gr[gi] == '?' || gr[gi] == sr[si]):
			gi++
			si++
		case gi < len(gr) && gr[gi] == '*':
			star, mark = gi, si
			gi++
		case star >= 0:
			gi = star + 1
			mark++
			si = mark
		default:
			return false
		}
	}
	for gi < len(gr) && gr[gi] == '*' {
		gi++
	}
	return gi == len(gr)
}
