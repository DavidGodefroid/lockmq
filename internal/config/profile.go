// Package config loads lockmq profiles from the project and user config
// files, applies the defaults, refuses anything that looks like a secret,
// and computes the security-relevant policy of a profile. The file lookup,
// the secret-safe decoding, the shared profile base and the diff helpers
// are lockkit's.
package config

import (
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode"

	kconfig "github.com/DavidGodefroid/lockkit/config"
	"github.com/DavidGodefroid/lockkit/pii"

	"github.com/DavidGodefroid/lockmq/internal/product"
)

// Tier is what a profile lets the agent read. The order is TierRead <
// TierPeek: peek also reads classic and quorum queue messages, which is
// not strictly read-only.
type Tier int

// Tiers, in increasing order of what they allow.
const (
	TierRead Tier = iota
	TierPeek
)

var tierNames = [...]string{"read", "peek"}

// ParseTier parses "read" or "peek".
func ParseTier(s string) (Tier, error) {
	for i, n := range tierNames {
		if s == n {
			return Tier(i), nil
		}
	}
	return TierRead, fmt.Errorf("unknown tier %q (want read or peek)", s)
}

func (t Tier) String() string {
	if t >= 0 && int(t) < len(tierNames) {
		return tierNames[t]
	}
	return fmt.Sprintf("tier(%d)", int(t))
}

// MarshalText encodes the tier by name.
func (t Tier) MarshalText() ([]byte, error) {
	if t < 0 || int(t) >= len(tierNames) {
		return nil, fmt.Errorf("invalid tier %d", int(t))
	}
	return []byte(t.String()), nil
}

// UnmarshalText decodes a tier name.
func (t *Tier) UnmarshalText(b []byte) error {
	v, err := ParseTier(string(b))
	if err != nil {
		return err
	}
	*t = v
	return nil
}

// Limits bound the cost and size of one request.
type Limits struct {
	RequestTimeout time.Duration `json:"request_timeout"`
	// ReadMax and PeekMax are the most messages one stream read or one
	// queue peek returns.
	ReadMax int `json:"read_max"`
	PeekMax int `json:"peek_max"`
	// DeliveryHeadroom refuses a quorum queue peek this close to the
	// queue's delivery limit.
	DeliveryHeadroom int `json:"delivery_headroom"`
	// MaxBodyBytes caps one message body after masking.
	MaxBodyBytes   int `json:"max_body_bytes"`
	MaxOutputBytes int `json:"max_output_bytes"`
	ListPageSize   int `json:"list_page_size"`
}

// Profile is one named broker with its policy. It never holds a secret.
type Profile struct {
	Name string `json:"name"`
	kconfig.Base
	// URL is the management HTTP API; AMQPURL and StreamURL are optional.
	// None holds a user or a password.
	URL       string `json:"url"`
	AMQPURL   string `json:"amqp_url,omitempty"`
	StreamURL string `json:"stream_url,omitempty"`
	// VHosts are the vhosts served, sorted; empty serves every vhost the
	// account can see.
	VHosts    []string `json:"vhosts"`
	Tier      Tier     `json:"tier"`
	Detectors []string `json:"detectors"`
	BodyMode  string   `json:"body_mode"`
	Limits    Limits   `json:"limits"`
}

type rawLimits struct {
	RequestTimeout   string `toml:"request_timeout"`
	ReadMax          int    `toml:"read_max"`
	PeekMax          int    `toml:"peek_max"`
	DeliveryHeadroom int    `toml:"delivery_headroom"`
	MaxBodyBytes     int    `toml:"max_body_bytes"`
	MaxOutputBytes   int    `toml:"max_output_bytes"`
	ListPageSize     int    `toml:"list_page_size"`
	IdleTimeout      string `toml:"idle_timeout"`
	MaxSession       string `toml:"max_session"`
}

type rawProfile struct {
	URL            string          `toml:"url"`
	AMQPURL        string          `toml:"amqp_url"`
	StreamURL      string          `toml:"stream_url"`
	VHosts         []string        `toml:"vhosts"`
	User           string          `toml:"user"`
	Credentials    string          `toml:"credentials"`
	VaultPath      string          `toml:"vault_path"`
	CredentialsTTL string          `toml:"credentials_ttl"`
	TLS            string          `toml:"tls"`
	TLSCA          string          `toml:"tls_ca"`
	TLSCert        string          `toml:"tls_cert"`
	TLSKey         string          `toml:"tls_key"`
	SSH            *kconfig.RawSSH `toml:"ssh"`
	Tier           string          `toml:"tier"`
	Production     bool            `toml:"production"`
	Detectors      []string        `toml:"detectors"`
	BodyMode       string          `toml:"body_mode"`
	Limits         rawLimits       `toml:"limits"`
}

type rawFile struct {
	Profiles map[string]rawProfile `toml:"profiles"`
}

// Load reads the project config (found by walking up from cwd) and the user
// config. On a profile name present in both, the project profile wins as a
// whole. It also returns the project root ("" when none).
func Load(cwd string) (map[string]Profile, string, error) {
	userPath, _ := kconfig.UserConfigPath(product.P)
	return LoadFrom(cwd, userPath)
}

// LoadFrom is Load with an explicit user config path ("" for none). A
// missing user config file is not an error.
func LoadFrom(cwd, userConfigPath string) (map[string]Profile, string, error) {
	out := map[string]Profile{}
	if userConfigPath != "" {
		ps, err := loadFile(userConfigPath)
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			return nil, "", err
		}
		for n, p := range ps {
			out[n] = p
		}
	}
	root, ok := kconfig.FindProjectRoot(product.P, cwd)
	if !ok {
		return out, "", nil
	}
	ps, err := loadFile(kconfig.ProjectConfigPath(product.P, root))
	if err != nil {
		return nil, "", err
	}
	for n, p := range ps {
		out[n] = p
	}
	return out, root, nil
}

// LoadFile reads the profiles of one config file.
func LoadFile(path string) (map[string]Profile, error) { return loadFile(path) }

func loadFile(path string) (map[string]Profile, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("config: %w", err)
	}
	return ParseProfiles(data, filepath.Clean(path))
}

// ParseProfiles decodes and validates config data; name labels errors.
// Errors never quote a value that could hold a secret.
func ParseProfiles(data []byte, name string) (map[string]Profile, error) {
	var raw rawFile
	md, err := kconfig.Decode(product.P, data, name, &raw)
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(raw.Profiles))
	for pn := range raw.Profiles {
		names = append(names, pn)
	}
	sort.Strings(names)
	out := make(map[string]Profile, len(names))
	for _, pn := range names {
		p, err := buildProfile(pn, raw.Profiles[pn],
			md.IsDefined("profiles", pn, "detectors"),
			md.IsDefined("profiles", pn, "limits", "delivery_headroom"))
		if err != nil {
			return nil, fmt.Errorf("config: %s: %w", name, err)
		}
		out[pn] = p
	}
	return out, nil
}

// urlKind is what one of the three URL keys accepts: its schemes, and
// whether the scheme is TLS.
type urlKind struct {
	key     string
	schemes map[string]bool // scheme -> TLS
	path    bool            // a path prefix is allowed
}

var (
	mgmtURL   = urlKind{"url", map[string]bool{"http": false, "https": true}, true}
	amqpURL   = urlKind{"amqp_url", map[string]bool{"amqp": false, "amqps": true}, false}
	streamURL = urlKind{"stream_url", map[string]bool{"rabbitmq-stream": false, "rabbitmq-stream+tls": true}, false}
)

// ParsedURL is a validated broker URL.
type ParsedURL struct {
	Host string
	Port int // 0: the scheme's default
	TLS  bool
	Path string
}

// ParseURL validates s for the key ("url", "amqp_url" or "stream_url"). Its
// errors never quote s.
func ParseURL(key, s string) (ParsedURL, error) {
	k := map[string]urlKind{"url": mgmtURL, "amqp_url": amqpURL, "stream_url": streamURL}[key]
	if k.key == "" {
		return ParsedURL{}, fmt.Errorf("unknown url key %q", key)
	}
	return k.parse(s)
}

func (k urlKind) parse(s string) (ParsedURL, error) {
	want := make([]string, 0, len(k.schemes))
	for sc := range k.schemes {
		want = append(want, sc+"://")
	}
	sort.Strings(want)
	if kconfig.EmbedsPassword(s) || strings.Contains(s[strings.Index(s, "://")+1:], "@") {
		return ParsedURL{}, fmt.Errorf("%s must not hold a user or password (set user; %s asks for the password)", k.key, product.P.Name)
	}
	u, err := url.Parse(s)
	if err != nil {
		return ParsedURL{}, fmt.Errorf("%s is not a valid URL", k.key)
	}
	tls, ok := k.schemes[strings.ToLower(u.Scheme)]
	if !ok {
		return ParsedURL{}, fmt.Errorf("%s: want %s", k.key, strings.Join(want, " or "))
	}
	if u.User != nil {
		return ParsedURL{}, fmt.Errorf("%s must not hold a user or password", k.key)
	}
	if u.Hostname() == "" {
		return ParsedURL{}, fmt.Errorf("%s has no host", k.key)
	}
	if u.RawQuery != "" || u.Fragment != "" || u.Opaque != "" {
		return ParsedURL{}, fmt.Errorf("%s must not have a query or a fragment", k.key)
	}
	if !k.path && u.Path != "" && u.Path != "/" {
		return ParsedURL{}, fmt.Errorf("%s must not have a path (vhosts are set with vhosts)", k.key)
	}
	p := ParsedURL{Host: u.Hostname(), TLS: tls, Path: strings.TrimSuffix(u.Path, "/")}
	if ps := u.Port(); ps != "" {
		var port int
		if _, err := fmt.Sscanf(ps, "%d", &port); err != nil || port < 1 || port > 65535 {
			return ParsedURL{}, fmt.Errorf("%s: port out of range", k.key)
		}
		p.Port = port
	}
	return p, nil
}

func buildProfile(name string, r rawProfile, detectorsSet, headroomSet bool) (Profile, error) {
	if err := kconfig.CheckProfileName(name, r.Production); err != nil {
		return Profile{}, err
	}
	errf := func(format string, a ...any) error {
		return fmt.Errorf("profile %q: "+format, append([]any{name}, a...)...)
	}
	for _, f := range []struct{ key, val string }{{"user", r.User}, {"vault_path", r.VaultPath},
		{"tls_ca", r.TLSCA}, {"tls_cert", r.TLSCert}, {"tls_key", r.TLSKey}} {
		if err := kconfig.CheckValue(product.P, f.key, f.val); err != nil {
			return Profile{}, errf("%v", err)
		}
	}
	for _, v := range r.VHosts {
		if v == "" {
			return Profile{}, errf("vhosts: an empty vhost name")
		}
		if err := kconfig.CheckValue(product.P, "vhosts", v); err != nil {
			return Profile{}, errf("%v", err)
		}
	}

	p := Profile{
		Name: name,
		Base: kconfig.Base{
			User: r.User, Credentials: r.Credentials, VaultPath: r.VaultPath,
			TLS: r.TLS, TLSCA: r.TLSCA, TLSCert: r.TLSCert, TLSKey: r.TLSKey, Production: r.Production,
		},
		URL: r.URL, AMQPURL: r.AMQPURL, StreamURL: r.StreamURL,
		VHosts: r.VHosts, Detectors: r.Detectors, BodyMode: r.BodyMode,
		Limits: Limits{
			ReadMax: r.Limits.ReadMax, PeekMax: r.Limits.PeekMax, DeliveryHeadroom: r.Limits.DeliveryHeadroom,
			MaxBodyBytes: r.Limits.MaxBodyBytes, MaxOutputBytes: r.Limits.MaxOutputBytes, ListPageSize: r.Limits.ListPageSize,
		},
	}

	if p.URL == "" {
		return Profile{}, errf("url (the management API) is required")
	}
	var urls []ParsedURL
	for _, f := range []struct {
		k   urlKind
		val string
	}{{mgmtURL, p.URL}, {amqpURL, p.AMQPURL}, {streamURL, p.StreamURL}} {
		if f.val == "" {
			continue
		}
		u, err := f.k.parse(f.val)
		if err != nil {
			return Profile{}, errf("%v", err)
		}
		urls = append(urls, u)
	}

	if r.Tier != "" {
		t, err := ParseTier(r.Tier)
		if err != nil {
			return Profile{}, errf("tier: %v", err)
		}
		p.Tier = t
	}
	switch p.Credentials {
	case "", kconfig.CredentialsAsk, kconfig.CredentialsKeychain:
		if p.VaultPath != "" {
			return Profile{}, errf("vault_path is only used with credentials = \"vault\"")
		}
	case kconfig.CredentialsVault:
		if p.VaultPath == "" {
			return Profile{}, errf("credentials = \"vault\" needs vault_path, such as \"rabbitmq/creds/<role>\"")
		}
		// The path is an argument of the vault CLI: it must not read as a flag.
		if strings.HasPrefix(p.VaultPath, "-") || strings.HasPrefix(p.VaultPath, "/") || strings.IndexFunc(p.VaultPath, unicode.IsSpace) >= 0 {
			return Profile{}, errf("vault_path: want a path such as \"rabbitmq/creds/<role>\", without spaces and not starting with '-' or '/'")
		}
	default:
		return Profile{}, errf("unknown credentials mode %q (want ask, keychain or vault)", p.Credentials)
	}
	if _, err := pii.Detectors(p.Detectors); err != nil {
		return Profile{}, errf("detectors: %v", err)
	}
	switch p.BodyMode {
	case "", BodyRedactUnknown, BodyDetect:
	default:
		return Profile{}, errf("unknown body_mode %q (want redact-unknown or detect)", p.BodyMode)
	}
	if p.TLS != "" && kconfig.TLSRank(p.TLS) < 0 {
		return Profile{}, errf("unknown tls mode %q (want disable, prefer, require, verify-ca or verify-full)", p.TLS)
	}
	if (p.TLSCert == "") != (p.TLSKey == "") {
		return Profile{}, errf("tls_cert and tls_key go together: set both, or neither")
	}

	for _, f := range []struct {
		key, val string
		dst      *time.Duration
	}{
		{"request_timeout", r.Limits.RequestTimeout, &p.Limits.RequestTimeout},
		{"idle_timeout", r.Limits.IdleTimeout, &p.IdleTimeout},
		{"max_session", r.Limits.MaxSession, &p.MaxSession},
	} {
		if f.val == "" {
			continue
		}
		d, err := time.ParseDuration(f.val)
		if err != nil || d <= 0 {
			return Profile{}, errf("limits.%s: want a positive duration such as \"10s\" or \"20m\"", f.key)
		}
		*f.dst = d
	}
	if r.CredentialsTTL != "" {
		d, err := time.ParseDuration(r.CredentialsTTL)
		if err != nil || d < time.Minute {
			return Profile{}, errf("credentials_ttl: want a duration of at least 1m, such as \"20m\" or \"1h\"")
		}
		p.CredentialsTTL = d
	}
	for _, f := range []struct {
		key      string
		val, max int
	}{
		{"read_max", p.Limits.ReadMax, ReadMaxCeiling},
		{"peek_max", p.Limits.PeekMax, PeekMaxCeiling},
		{"delivery_headroom", p.Limits.DeliveryHeadroom, 0},
		{"max_body_bytes", p.Limits.MaxBodyBytes, 0},
		{"max_output_bytes", p.Limits.MaxOutputBytes, 0},
		{"list_page_size", p.Limits.ListPageSize, 0},
	} {
		if f.val < 0 {
			return Profile{}, errf("limits.%s must not be negative", f.key)
		}
		if f.max > 0 && f.val > f.max {
			return Profile{}, errf("limits.%s (%d) is above its ceiling %d", f.key, f.val, f.max)
		}
	}

	if r.SSH != nil {
		s, err := kconfig.ParseSSH(product.P, *r.SSH, p.Credentials, errf)
		if err != nil {
			return Profile{}, err
		}
		p.SSH = s
	}

	applyDefaults(&p, detectorsSet, headroomSet)

	if p.TLSCA != "" && kconfig.TLSRank(p.TLS) < kconfig.TLSRank(kconfig.TLSRequire) {
		return Profile{}, errf("tls_ca needs tls = \"require\", \"verify-ca\" or \"verify-full\"")
	}
	for _, u := range urls {
		if u.TLS && p.TLS == kconfig.TLSDisable {
			return Profile{}, errf("tls = \"disable\" with a TLS URL scheme; use a plain scheme or another tls mode")
		}
		if !u.TLS && p.Production && p.SSH == nil && !kconfig.IsLoopback(u.Host) {
			return Profile{}, errf("a production profile on a remote host needs TLS URLs (https, amqps, rabbitmq-stream+tls) or an ssh tunnel; tls cannot apply to a plain scheme")
		}
	}
	switch {
	case p.MaxSession < MinIdleTimeout || p.MaxSession > MaxMaxSession:
		return Profile{}, errf("limits.max_session (%s) must be between %s and %s", p.MaxSession, MinIdleTimeout, MaxMaxSession)
	case p.IdleTimeout < MinIdleTimeout:
		return Profile{}, errf("limits.idle_timeout (%s) is below %s", p.IdleTimeout, MinIdleTimeout)
	case p.IdleTimeout > p.MaxSession:
		return Profile{}, errf("limits.idle_timeout (%s) is above limits.max_session (%s)", p.IdleTimeout, p.MaxSession)
	}
	return p, nil
}
