// Package mqconsole is lockmq's console backend for lockkit: its IPC
// methods (op.plan, op.run), the broker login behind them, the privilege
// audit of the account, its policy and PII rules. lockkit's console holds
// the terminal, the approval screens, the modes and the audit log.
package mqconsole

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"

	kconfig "github.com/DavidGodefroid/lockkit/config"
	"github.com/DavidGodefroid/lockkit/console"
	"github.com/DavidGodefroid/lockkit/kit"
	"github.com/DavidGodefroid/lockkit/pii"
	"github.com/DavidGodefroid/lockkit/sanitize"

	"github.com/DavidGodefroid/lockmq/internal/broker/stream"
	"github.com/DavidGodefroid/lockmq/internal/config"
	"github.com/DavidGodefroid/lockmq/internal/msgmask"
	"github.com/DavidGodefroid/lockmq/internal/ops"
	"github.com/DavidGodefroid/lockmq/internal/product"
)

// IPC methods of lockmq, beyond lockkit's own.
const (
	MethodRun  = "op.run"
	MethodPlan = "op.plan"
)

// Options locate the profile a console serves.
type Options struct {
	Profile string
	// Root is the project root ("" for a profile of the user config).
	Root string
	// UserConfig is the user config file ("" for none).
	UserConfig string
	// StateDir is the state base of the console (console.Options.StateDir;
	// "" is kit.StateBase()): Connect reads the approved policy there.
	StateDir string
	// IO answers the ssh tunnel's prompts (the console's terminal).
	IO console.IO
}

// Backend implements console.Backend and console.PIIBackend.
type Backend struct {
	o Options

	mu sync.Mutex
	// cur is the session Connect opened last. lockkit may hand Env.Session
	// back wrapped (a Vault lease), so handlers use cur, the session the
	// console vouches for once Env.Session returned without error.
	cur *session

	// seen keeps the delivery counts of peeks for the console session;
	// refs the values behind redacted references. Both survive a
	// reconnection, both are memory only.
	seen ops.Seen
	refs pii.RefStore

	// Test seams: the stream readers.
	newStreamProto, newAMQP func(host string, port int, cfg *tls.Config, user string, secret []byte, dial stream.DialFunc) stream.Reader
}

// New returns the backend of o.Profile.
func New(o Options) *Backend {
	return &Backend{o: o, newStreamProto: stream.NewStreamProto, newAMQP: stream.NewAMQP}
}

var (
	_ console.Backend    = (*Backend)(nil)
	_ console.PIIBackend = (*Backend)(nil)
)

// Sanitizer scrubs lockmq's error text: URI userinfo (amqp, amqps,
// rabbitmq-stream, http) and Authorization headers, after the secrets
// lockkit knows.
func Sanitizer() *sanitize.Sanitizer {
	return sanitize.New(sanitize.URLUserinfo, regexp.MustCompile(`(?i)authorization:\s*[^\r\n]*`))
}

// Methods implements console.Backend.
func (b *Backend) Methods() map[string]console.Handler {
	return map[string]console.Handler{MethodRun: b.run, MethodPlan: b.plan}
}

// piiPath is the rules file: .lockmq/pii.toml in the project, else
// pii.toml next to the user config.
func (b *Backend) piiPath() string {
	if b.o.Root != "" {
		return filepath.Join(b.o.Root, product.P.ProjectDir(), "pii.toml")
	}
	if b.o.UserConfig != "" {
		return filepath.Join(filepath.Dir(b.o.UserConfig), "pii.toml")
	}
	return ""
}

// LoadPolicy implements console.Backend: the profile from the config files
// and the PII rules, in canonical form.
func (b *Backend) LoadPolicy() (console.Policy, error) {
	var profiles map[string]config.Profile
	var err error
	if b.o.Root != "" {
		profiles, _, err = config.LoadFrom(b.o.Root, b.o.UserConfig)
	} else if b.o.UserConfig != "" {
		profiles, err = config.LoadFile(b.o.UserConfig)
	} else {
		err = errors.New("no config file")
	}
	if err != nil {
		return nil, err
	}
	pr, ok := profiles[b.o.Profile]
	if !ok {
		return nil, fmt.Errorf("no profile %q in the config files", b.o.Profile)
	}
	var rules msgmask.Rules
	if path := b.piiPath(); path != "" {
		if rules, err = msgmask.LoadRules(path); err != nil {
			return nil, err
		}
	}
	mask, allow, modes := rules.Texts()
	return Policy{config.NewPolicy(pr, mask, allow, modes)}, nil
}

// inForce is the approved policy (the console saves it before it
// connects), else the files' policy.
func (b *Backend) inForce() (Policy, error) {
	state := b.o.StateDir
	if state == "" {
		var err error
		if state, err = kit.StateBase(); err != nil {
			return Policy{}, err
		}
	}
	var ap Policy
	ok, err := kconfig.LoadApproved(product.P, state, kconfig.ApprovedKey(b.o.Root, b.o.Profile), &ap)
	if err != nil {
		return Policy{}, err
	}
	if ok {
		return ap, nil
	}
	cur, err := b.LoadPolicy()
	if err != nil {
		return Policy{}, err
	}
	return cur.(Policy), nil
}

func (b *Backend) session() *session {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.cur
}

// Checks implements console.Backend: a broker account tagged
// administrator or policymaker can change the broker; lockkit refuses it on
// production and asks for a typed "continue" elsewhere.
func (b *Backend) Checks(_ context.Context, _ console.Session) []console.Finding {
	s := b.session()
	if s == nil {
		return []console.Finding{{Refuse: true, Text: "no broker session to check"}}
	}
	var out []console.Finding
	for _, tag := range []string{"administrator", "policymaker"} {
		if slices.Contains(s.tags, tag) {
			out = append(out, console.Finding{Text: fmt.Sprintf(
				"the broker account has the %s tag: it can change the broker; use an account tagged monitoring, with configure and write set to \"^$\"", tag)})
		}
	}
	return out
}

// Status implements console.Backend.
func (b *Backend) Status(console.Session) any {
	s := b.session()
	if s == nil {
		return nil
	}
	_, via := s.reader()
	out := map[string]any{"broker_version": s.version, "user": s.user, "tags": slices.Clone(s.tags)}
	if via != "" {
		out["stream_reads_via"] = via
	}
	return out
}

// PIIRules implements console.PIIBackend. A mask rule whose mode is not
// redact is shown with it.
func (b *Backend) PIIRules(p console.Policy) (mask, allow []string) {
	pol, ok := p.(Policy)
	if !ok {
		return nil, nil
	}
	for _, r := range pol.Mask {
		if m := pol.Modes[r]; m != "" && m != config.DefaultMaskMode {
			r += " mode=" + m
		}
		mask = append(mask, r)
	}
	return mask, slices.Clone(pol.Allow)
}

// CheckMask implements console.PIIBackend: a mask rule's canonical text
// ("queue=orders/* body=$.customer.email", "queue=*/* header=x-customer-*").
func (b *Backend) CheckMask(pattern string) (string, error) {
	r, err := msgmask.ParseRule(strings.TrimSpace(pattern))
	if err != nil {
		return "", err
	}
	return r.String(), nil
}

// SaveMask implements console.PIIBackend: the rule is appended to the
// rules file on disk.
func (b *Backend) SaveMask(pattern string) error {
	path := b.piiPath()
	if path == "" {
		return errors.New("no rules file: start the console in a project")
	}
	r, err := msgmask.ParseRule(pattern)
	if err != nil {
		return err
	}
	return msgmask.AppendMask(path, r)
}

// WithMask implements console.PIIBackend.
func (b *Backend) WithMask(p console.Policy, pattern string) console.Policy {
	pol, ok := p.(Policy)
	if !ok {
		return p
	}
	next := pol.Policy
	next.Mask = append(slices.Clone(next.Mask), pattern)
	return Policy{config.Canonical(next)}
}
