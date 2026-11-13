package config

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"maps"
	"path"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/BurntSushi/toml"

	kconfig "github.com/DavidGodefroid/lockkit/config"
	"github.com/DavidGodefroid/lockkit/managed"
	"github.com/DavidGodefroid/lockkit/pii"

	"github.com/DavidGodefroid/lockmq/internal/msgmask"
)

// ManagedTool is lockmq's section of the company's managed file
// ([lockmq.bounds], [lockmq.defaults], [lockmq.profiles.<name>]).
const ManagedTool = "lockmq"

// Managed is lockmq's part of the company's managed file (lockkit
// managed): its defaults, its profiles and its bounds. A nil *Managed is
// no managed file.
type Managed struct {
	File *managed.File
	// Defaults is [lockmq.defaults] as the file holds it (nil: none): the
	// keys a profile leaves unset take it.
	Defaults map[string]any
	// Profiles are [lockmq.profiles.<name>], built with the defaults.
	Profiles map[string]Profile
	// Bounds are [lockmq.bounds] and the [family] rules lockmq enforces
	// (nil: none).
	Bounds *Bounds
}

// LoadManaged reads the machine's managed file: nil and no error when
// there is none. A file lockkit refuses (badly owned, invalid, badly
// signed) or a lockmq section that is invalid or beyond its own bounds is
// an error: the caller refuses to start.
func LoadManaged() (*Managed, error) {
	f, err := managed.Load(context.Background())
	if err != nil || f == nil {
		return nil, err
	}
	return ParseManaged(f)
}

// ParseManaged reads lockmq's part of f. A managed default or a managed
// profile beyond the bounds is an error.
func ParseManaged(f *managed.File) (*Managed, error) {
	if f == nil {
		return nil, nil
	}
	m := &Managed{File: f, Profiles: map[string]Profile{}}
	where := func(kind string, err error) error {
		return fmt.Errorf("managed: %s: [%s.%s]: %w", f.Path, ManagedTool, kind, err)
	}
	b, err := parseBounds(f)
	if err != nil {
		return nil, where(managed.KindBounds, err)
	}
	m.Bounds = b
	if ok, err := f.Decode(&rawProfile{}, ManagedTool, managed.KindDefaults); err != nil {
		return nil, err
	} else if ok {
		var t rawTable
		if _, err := f.Decode(&t, ManagedTool, managed.KindDefaults); err != nil {
			return nil, err
		}
		m.Defaults = t.v
		if err := m.checkDefaults(); err != nil {
			return nil, where(managed.KindDefaults, err)
		}
	}
	for _, name := range f.Profiles(ManagedTool) {
		key := managed.KindProfiles + "." + name
		if _, err := f.Decode(&rawProfile{}, ManagedTool, managed.KindProfiles, name); err != nil {
			return nil, err
		}
		var t rawTable
		if _, err := f.Decode(&t, ManagedTool, managed.KindProfiles, name); err != nil {
			return nil, err
		}
		p, err := profileFromTable(name, t.v, m.Defaults)
		if err != nil {
			return nil, where(key, err)
		}
		m.Bounds.require(&p)
		if lines, err := m.Bounds.Violations(m.Bounds.policyOf(p)); err != nil {
			return nil, where(key, err)
		} else if len(lines) > 0 {
			return nil, where(key, fmt.Errorf("the managed profile is beyond the managed bounds: %s", lines[0]))
		}
		m.Profiles[name] = p
	}
	return m, nil
}

// rawTable is a table as the file holds it (the keys it sets). Its
// UnmarshalTOML takes the table whole: decoded into a map[string]any, the
// keys of a nested table would read as unknown to File.Decode's strict
// check.
type rawTable struct{ v map[string]any }

// UnmarshalTOML implements toml.Unmarshaler.
func (r *rawTable) UnmarshalTOML(v any) error {
	m, ok := v.(map[string]any)
	if !ok {
		return errors.New("want a table")
	}
	r.v = m
	return nil
}

// checkDefaults refuses a managed default beyond the bounds: the defaults
// make a profile of their own, and a violation of a key they set is an
// error.
func (m *Managed) checkDefaults() error {
	t := maps.Clone(m.Defaults)
	if _, ok := t["url"]; !ok {
		t["url"] = "http://127.0.0.1:15672"
	}
	p, err := profileFromTable("managed-defaults", t, nil)
	if err != nil {
		return err
	}
	m.Bounds.require(&p)
	mb, err := m.Bounds.Check(m.Bounds.policyOf(p))
	var vs []managed.Violation
	var be *BoundsError
	switch {
	case errors.As(err, &be):
		vs = be.Violations
	case err != nil:
		return err
	}
	vs = append(vs, managed.Check(m.Bounds.policyOf(p), mb)...)
	set := flatKeys(m.Defaults, "")
	for _, v := range vs {
		for _, k := range set {
			if v.Field == k || strings.HasPrefix(v.Field, k+".") {
				return fmt.Errorf("the managed default is beyond the managed bounds: %s", strings.Replace(v.String(), "(profile managed-defaults: ", "(defaults: ", 1))
			}
		}
	}
	return nil
}

// flatKeys lists the keys of t, a table's keys as "limits.read_max".
func flatKeys(t map[string]any, prefix string) []string {
	var out []string
	for _, k := range slices.Sorted(maps.Keys(t)) {
		out = append(out, prefix+k)
		if sub, ok := t[k].(map[string]any); ok && k == "limits" {
			out = append(out, flatKeys(sub, prefix+k+".")...)
		}
	}
	return out
}

// DescribeDefaults is [lockmq.defaults] as lines for `lockmq managed
// show` ("lockmq.defaults.limits.read_max = 10").
func (m *Managed) DescribeDefaults() []string {
	if m == nil {
		return nil
	}
	var out []string
	var walk func(t map[string]any, prefix string)
	walk = func(t map[string]any, prefix string) {
		for _, k := range slices.Sorted(maps.Keys(t)) {
			if sub, ok := t[k].(map[string]any); ok {
				walk(sub, prefix+k+".")
				continue
			}
			var buf bytes.Buffer
			if err := toml.NewEncoder(&buf).Encode(map[string]any{"v": t[k]}); err != nil {
				continue
			}
			v := strings.TrimSpace(strings.TrimPrefix(buf.String(), "v = "))
			v = strings.Join(strings.Fields(v), " ")
			out = append(out, ManagedTool+"."+managed.KindDefaults+"."+prefix+k+" = "+v)
		}
	}
	walk(m.Defaults, "")
	return out
}

// defaults is the managed defaults (nil for a nil m).
func (m *Managed) defaults() map[string]any {
	if m == nil {
		return nil
	}
	return m.Defaults
}

// profiles is the managed profiles (nil for a nil m).
func (m *Managed) profiles() map[string]Profile {
	if m == nil {
		return nil
	}
	return m.Profiles
}

// BoundsOf is m's bounds, nil for a nil m.
func (m *Managed) BoundsOf() *Bounds {
	if m == nil {
		return nil
	}
	return m.Bounds
}

// withDefaults is the profile table t with the keys it does not set taken
// from the defaults d; the limits table is merged key by key, the ssh and
// auto keys are taken whole.
func withDefaults(t, d map[string]any) map[string]any {
	out := maps.Clone(t)
	if out == nil {
		out = map[string]any{}
	}
	for k, v := range d {
		cur, ok := out[k]
		if !ok {
			out[k] = v
			continue
		}
		cl, ok1 := cur.(map[string]any)
		dl, ok2 := v.(map[string]any)
		if k == "limits" && ok1 && ok2 {
			out[k] = withDefaults(cl, dl)
		}
	}
	return out
}

// profileFromTable builds the profile name from its table t and the
// defaults d.
func profileFromTable(name string, t, d map[string]any) (Profile, error) {
	t = withDefaults(t, d)
	var r rawProfile
	if err := decodeTable(t, &r); err != nil {
		return Profile{}, fmt.Errorf("profile %q: %w", name, err)
	}
	_, detectorsSet := t["detectors"]
	lim, _ := t["limits"].(map[string]any)
	_, headroomSet := lim["delivery_headroom"]
	return buildProfile(name, r, detectorsSet, headroomSet)
}

// decodeTable decodes the table m into into, strictly.
func decodeTable(m map[string]any, into any) error {
	var buf bytes.Buffer
	if err := toml.NewEncoder(&buf).Encode(m); err != nil {
		return err
	}
	md, err := toml.NewDecoder(&buf).Decode(into)
	if err != nil {
		return errors.New(strings.TrimPrefix(err.Error(), "toml: "))
	}
	if und := md.Undecoded(); len(und) > 0 {
		return fmt.Errorf("unknown key %q", und[0].String())
	}
	return nil
}

// Bounds are [lockmq.bounds] and the [family] rules lockmq enforces: the
// loosest policy the company allows. A nil *Bounds bounds nothing.
type Bounds struct {
	production     bool
	tier           *Tier
	tls, bodyMode  string
	vhosts         []string
	vhostsSet      bool
	credentials    []string
	hosts          []string
	credentialsTTL time.Duration
	auto           map[string]AutoValue
	ints           map[string]int
	durations      map[string]time.Duration
	detectors      []string
	masks          []msgmask.Rule
	vaultRequired  bool
}

type rawBoundLimits struct {
	ReadMax        *int   `toml:"read_max"`
	PeekMax        *int   `toml:"peek_max"`
	MaxBodyBytes   *int   `toml:"max_body_bytes"`
	MaxOutputBytes *int   `toml:"max_output_bytes"`
	ListPageSize   *int   `toml:"list_page_size"`
	RequestTimeout string `toml:"request_timeout"`
	IdleTimeout    string `toml:"idle_timeout"`
	MaxSession     string `toml:"max_session"`
}

type rawBounds struct {
	Production     *bool              `toml:"production"`
	Tier           string             `toml:"tier"`
	TLS            string             `toml:"tls"`
	BodyMode       string             `toml:"body_mode"`
	VHosts         []string           `toml:"vhosts"`
	Credentials    []string           `toml:"credentials"`
	Hosts          []string           `toml:"hosts"`
	CredentialsTTL string             `toml:"credentials_ttl"`
	Auto           map[string]string  `toml:"auto"`
	Limits         rawBoundLimits     `toml:"limits"`
	Detectors      []string           `toml:"detectors"`
	PIIMask        []msgmask.RuleSpec `toml:"pii_mask"`
}

// boundKey is the managed key of a bound ("lockmq.bounds.tier").
func boundKey(k string) string { return ManagedTool + "." + managed.KindBounds + "." + k }

// parseBounds reads [lockmq.bounds] and the [family] vault rule: nil when
// f has neither. Its errors name the key, never quote a value.
func parseBounds(f *managed.File) (*Bounds, error) {
	var r rawBounds
	ok, err := f.Decode(&r, ManagedTool, managed.KindBounds)
	if err != nil {
		return nil, err
	}
	vault := f.Family.Vault != nil && f.Family.Vault.Required
	if !ok && !vault {
		return nil, nil
	}
	b := &Bounds{vaultRequired: vault, ints: map[string]int{}, durations: map[string]time.Duration{}}
	if r.Production != nil {
		if !*r.Production {
			return nil, errors.New("production: only true bounds anything (every profile is production)")
		}
		b.production = true
	}
	if r.Tier != "" {
		t, err := ParseTier(r.Tier)
		if err != nil {
			return nil, fmt.Errorf("tier: %w", err)
		}
		b.tier = &t
	}
	if r.TLS != "" {
		if kconfig.TLSRank(r.TLS) < 0 {
			return nil, errors.New("tls: unknown tls mode (want disable, prefer, require, verify-ca or verify-full)")
		}
		b.tls = r.TLS
	}
	switch r.BodyMode {
	case "", BodyRedactUnknown:
		b.bodyMode = r.BodyMode
	default:
		return nil, fmt.Errorf("body_mode: only %q bounds anything", BodyRedactUnknown)
	}
	if r.VHosts != nil {
		if len(r.VHosts) == 0 {
			return nil, errors.New("vhosts: list at least one vhost")
		}
		b.vhosts, b.vhostsSet = kconfig.SortedSet(r.VHosts), true
	}
	for _, c := range r.Credentials {
		switch c {
		case kconfig.CredentialsAsk, kconfig.CredentialsKeychain, kconfig.CredentialsVault:
		default:
			return nil, errors.New("credentials: unknown credentials mode (want ask, keychain or vault)")
		}
	}
	if r.Credentials != nil && len(r.Credentials) == 0 {
		return nil, errors.New("credentials: list at least one credentials mode")
	}
	b.credentials = r.Credentials
	for i, h := range r.Hosts {
		if _, err := path.Match(h, ""); err != nil || h == "" {
			return nil, fmt.Errorf("hosts entry %d: want a host pattern such as \"*.mq.example.com\"", i+1)
		}
	}
	if r.Hosts != nil && len(r.Hosts) == 0 {
		return nil, errors.New("hosts: list at least one host pattern")
	}
	b.hosts = r.Hosts
	if r.CredentialsTTL != "" {
		d, err := time.ParseDuration(r.CredentialsTTL)
		if err != nil || d < time.Minute {
			return nil, errors.New("credentials_ttl: want a duration of at least 1m")
		}
		b.credentialsTTL = d
	}
	for _, act := range slices.Sorted(maps.Keys(r.Auto)) {
		if !slices.Contains(AutoActions, act) {
			return nil, fmt.Errorf("auto: unknown action %q (want %s)", act, strings.Join(AutoActions, ", "))
		}
		v, err := ParseAutoValue(r.Auto[act])
		if err != nil {
			return nil, fmt.Errorf("auto.%s: %w", act, err)
		}
		if b.auto == nil {
			b.auto = map[string]AutoValue{}
		}
		b.auto[act] = v
	}
	for _, l := range []struct {
		key string
		v   *int
	}{{"read_max", r.Limits.ReadMax}, {"peek_max", r.Limits.PeekMax}, {"max_body_bytes", r.Limits.MaxBodyBytes},
		{"max_output_bytes", r.Limits.MaxOutputBytes}, {"list_page_size", r.Limits.ListPageSize}} {
		if l.v == nil {
			continue
		}
		if *l.v < 1 {
			return nil, fmt.Errorf("limits.%s: want at least 1", l.key)
		}
		b.ints[l.key] = *l.v
	}
	for _, l := range []struct{ key, v string }{{"request_timeout", r.Limits.RequestTimeout},
		{"idle_timeout", r.Limits.IdleTimeout}, {"max_session", r.Limits.MaxSession}} {
		if l.v == "" {
			continue
		}
		d, err := time.ParseDuration(l.v)
		if err != nil || d <= 0 {
			return nil, fmt.Errorf("limits.%s: want a positive duration such as \"10s\" or \"20m\"", l.key)
		}
		b.durations[l.key] = d
	}
	if _, err := pii.Detectors(r.Detectors); err != nil {
		return nil, fmt.Errorf("detectors: %w", err)
	}
	b.detectors = kconfig.SortedSet(r.Detectors)
	for i, s := range r.PIIMask {
		rule, err := msgmask.BuildMask(s)
		if err != nil {
			return nil, fmt.Errorf("pii_mask %d: %w", i+1, err)
		}
		b.masks = append(b.masks, rule)
	}
	return b, nil
}

// Describe is the bounds as lines for `lockmq managed show`.
func (b *Bounds) Describe() []string {
	if b == nil {
		return nil
	}
	var out []string
	add := func(k, v string) { out = append(out, boundKey(k)+" = "+v) }
	if b.production {
		add("production", "true")
	}
	if b.tier != nil {
		add("tier", b.tier.String())
	}
	if b.tls != "" {
		add("tls", b.tls)
	}
	if b.bodyMode != "" {
		add("body_mode", b.bodyMode)
	}
	if b.vhostsSet {
		add("vhosts", strings.Join(b.vhosts, ", "))
	}
	if b.credentials != nil {
		add("credentials", strings.Join(b.credentials, ", "))
	}
	if b.hosts != nil {
		add("hosts", strings.Join(b.hosts, ", "))
	}
	if b.credentialsTTL > 0 {
		add("credentials_ttl", b.credentialsTTL.String())
	}
	for _, act := range AutoActions {
		if v, ok := b.auto[act]; ok {
			add("auto."+act, string(v))
		}
	}
	for _, k := range slices.Sorted(maps.Keys(b.ints)) {
		add("limits."+k, strconv.Itoa(b.ints[k]))
	}
	for _, k := range slices.Sorted(maps.Keys(b.durations)) {
		add("limits."+k, b.durations[k].String())
	}
	if len(b.detectors) > 0 {
		add("detectors", strings.Join(b.detectors, ", "))
	}
	for _, r := range b.masks {
		add("pii_mask", r.String()+" ("+r.Mode+")")
	}
	if b.vaultRequired {
		out = append(out, "family.vault.required = true")
	}
	return out
}

// require adds the required detectors to p.
func (b *Bounds) require(p *Profile) {
	if b == nil || len(b.detectors) == 0 {
		return
	}
	p.Detectors = kconfig.SortedSet(append(slices.Clone(p.Detectors), b.detectors...))
}

// Require adds the detectors the bounds require to every profile of ps.
func (b *Bounds) Require(ps map[string]Profile) {
	for n, p := range ps {
		b.require(&p)
		ps[n] = p
	}
}

// policyOf is p's policy with the managed mask rules only.
func (b *Bounds) policyOf(p Profile) Policy {
	rules, _ := b.ApplyPII(msgmask.Rules{})
	mask, allow, modes := rules.Texts()
	return NewPolicy(p, mask, allow, modes)
}

// ApplyPII adds the managed mask rules to rules: a user mask rule of the
// same target is replaced by the managed one. A user allow rule that could
// clear what a managed rule masks is refused.
func (b *Bounds) ApplyPII(rules msgmask.Rules) (msgmask.Rules, error) {
	if b == nil || len(b.masks) == 0 {
		return rules, nil
	}
	for i, a := range rules.Allow {
		for _, m := range b.masks {
			if msgmask.Overlaps(a, m) {
				return msgmask.Rules{}, fmt.Errorf("pii rules: allow rule %d would clear what the company's managed configuration masks (%s %s): remove it", i+1, boundKey("pii_mask"), m.String())
			}
		}
	}
	out := msgmask.Rules{Allow: rules.Allow}
	for _, r := range rules.Mask {
		if !slices.ContainsFunc(b.masks, func(m msgmask.Rule) bool { return m.String() == r.String() }) {
			out.Mask = append(out.Mask, r)
		}
	}
	out.Mask = append(out.Mask, b.masks...)
	return out, nil
}

// BoundsError is a policy outside an allowlist bound: credentials, hosts,
// family.vault.required. The console refuses it as a violation.
type BoundsError struct {
	Violations []managed.Violation
}

func (e *BoundsError) Error() string {
	lines := make([]string, len(e.Violations))
	for i, v := range e.Violations {
		lines[i] = v.String()
	}
	return strings.Join(lines, "; ")
}

// Check returns the managed bounds of pol: pol at its ceiling, which
// managed.Check compares pol with through Policy.Diff. A value outside an
// allowlist Diff cannot express (credentials, hosts,
// family.vault.required) is a *BoundsError naming the managed key. A nil
// b bounds nothing.
func (b *Bounds) Check(pol Policy) (managed.Bounds, error) {
	if b == nil {
		return managed.Bounds{}, nil
	}
	pol = Canonical(pol)
	pr := pol.Profile
	var vs []managed.Violation
	if b.credentials != nil && !slices.Contains(b.credentials, pr.Credentials) {
		vs = append(vs, managed.Violation{Key: boundKey("credentials"), Profile: pr.Name, Field: "credentials", Value: pr.Credentials, Bound: strings.Join(b.credentials, ", ")})
	}
	if b.vaultRequired && pr.Credentials != kconfig.CredentialsVault {
		vs = append(vs, managed.Violation{Key: "family.vault.required", Profile: pr.Name, Field: "credentials", Value: pr.Credentials, Bound: "true"})
	}
	if b.hosts != nil {
		for _, h := range profileHosts(pr) {
			if !b.hostAllowed(h.host) {
				vs = append(vs, managed.Violation{Key: boundKey("hosts"), Profile: pr.Name, Field: h.key, Value: h.host, Bound: strings.Join(b.hosts, ", ")})
			}
		}
	}
	c := pol
	cp := &c.Profile
	if b.production {
		cp.Production = true
	}
	if b.tier != nil {
		cp.Tier = *b.tier
	}
	if b.tls != "" {
		cp.TLS = b.tls
	}
	if b.bodyMode != "" {
		cp.BodyMode = b.bodyMode
	}
	if b.vhostsSet {
		cp.VHosts = slices.Clone(b.vhosts)
	}
	if b.credentialsTTL > 0 {
		cp.CredentialsTTL = b.credentialsTTL
	}
	for act, v := range b.auto {
		*cp.Auto.field(act) = v
	}
	for k, v := range b.ints {
		switch k {
		case "read_max":
			cp.Limits.ReadMax = v
		case "peek_max":
			cp.Limits.PeekMax = v
		case "max_body_bytes":
			cp.Limits.MaxBodyBytes = v
		case "max_output_bytes":
			cp.Limits.MaxOutputBytes = v
		case "list_page_size":
			cp.Limits.ListPageSize = v
		}
	}
	for k, v := range b.durations {
		switch k {
		case "request_timeout":
			cp.Limits.RequestTimeout = v
		case "idle_timeout":
			cp.IdleTimeout = v
		case "max_session":
			cp.MaxSession = v
		}
	}
	cp.Detectors = kconfig.SortedSet(append(slices.Clone(cp.Detectors), b.detectors...))
	keys := map[string]string{"pii.mask": "pii_mask", "pii.allow": "pii_mask"}
	if len(b.masks) > 0 {
		allow, err := msgmask.FromTexts(nil, pol.Allow, nil)
		if err != nil {
			return managed.Bounds{}, err
		}
		c.Allow = nil
		for _, a := range allow.Allow {
			if !slices.ContainsFunc(b.masks, func(m msgmask.Rule) bool { return msgmask.Overlaps(a, m) }) {
				c.Allow = append(c.Allow, a.String())
			}
		}
		c.Mask = slices.Clone(c.Mask)
		c.Modes = maps.Clone(c.Modes)
		if c.Modes == nil {
			c.Modes = map[string]string{}
		}
		for _, m := range b.masks {
			t := m.String()
			c.Mask = append(c.Mask, t)
			c.Modes[t] = m.Mode
			keys["pii.mode "+t] = "pii_mask"
		}
	}
	mb := managed.Bounds{Tool: ManagedTool, Profile: pr.Name, Ceiling: Canonical(c), Keys: keys}
	if len(vs) > 0 {
		return mb, &BoundsError{Violations: vs}
	}
	return mb, nil
}

// Violations are the lines that refuse pol: its allowlist violations and
// managed.Check's, each "blocked by company policy: …". An error is a
// bounds section that cannot be applied.
func (b *Bounds) Violations(pol Policy) ([]string, error) {
	mb, err := b.Check(pol)
	var lines []string
	var be *BoundsError
	switch {
	case errors.As(err, &be):
		for _, v := range be.Violations {
			lines = append(lines, v.String())
		}
	case err != nil:
		return nil, err
	}
	for _, v := range managed.Check(pol, mb) {
		lines = append(lines, v.String())
	}
	return lines, nil
}

type profileHost struct{ key, host string }

// profileHosts are the hosts p reaches: those of url, amqp_url, stream_url
// and ssh.host.
func profileHosts(p Profile) []profileHost {
	var out []profileHost
	for _, f := range []struct{ key, val string }{{"url", p.URL}, {"amqp_url", p.AMQPURL}, {"stream_url", p.StreamURL}} {
		if f.val == "" {
			continue
		}
		u, err := ParseURL(f.key, f.val)
		if err != nil {
			out = append(out, profileHost{f.key, "(invalid)"})
			continue
		}
		out = append(out, profileHost{f.key, u.Host})
	}
	if p.SSH != nil {
		out = append(out, profileHost{"ssh.host", p.SSH.Host})
	}
	return out
}

func (b *Bounds) hostAllowed(host string) bool {
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	for _, pat := range b.hosts {
		if ok, _ := path.Match(strings.ToLower(pat), host); ok {
			return true
		}
	}
	return false
}
