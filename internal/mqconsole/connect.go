package mqconsole

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"strings"

	kconfig "github.com/DavidGodefroid/lockkit/config"
	"github.com/DavidGodefroid/lockkit/console"
	"github.com/DavidGodefroid/lockkit/sanitize"
	"github.com/DavidGodefroid/lockkit/tunnel"

	"github.com/DavidGodefroid/lockmq/internal/broker/mgmt"
	"github.com/DavidGodefroid/lockmq/internal/broker/stream"
	"github.com/DavidGodefroid/lockmq/internal/config"
	"github.com/DavidGodefroid/lockmq/internal/ops"
	"github.com/DavidGodefroid/lockmq/internal/product"
)

// session is one login to the broker: the management client, the stream
// readers of the configured protocols, and what the broker said about the
// account at login.
type session struct {
	mgmt *mgmt.Client
	// proto reads streams over the stream protocol (nil without
	// stream_url); amqp over AMQP 0-9-1 (nil without amqp_url).
	proto, amqp stream.Reader
	tunnel      *tunnel.Tunnel
	// version is the broker's rabbitmq_version, tags the account's tags.
	version string
	user    string
	tags    []string
}

// Close implements console.Session.
func (s *session) Close() error {
	if s.mgmt != nil {
		s.mgmt.Close()
	}
	if s.tunnel != nil {
		return s.tunnel.Close()
	}
	return nil
}

// streamInfo is the stream protocol's metadata side, nil when the stream
// protocol is not configured or cannot be reached (ssh).
func (s *session) streamInfo() ops.StreamInfo {
	if s.proto == nil || s.tunnel != nil {
		return nil
	}
	return s.proto
}

// reader picks the stream reader: the stream protocol, else AMQP. via
// names it for the approval screen.
func (s *session) reader() (r stream.Reader, via string) {
	if s.proto != nil && s.tunnel == nil {
		return s.proto, "stream protocol"
	}
	if s.amqp != nil {
		return s.amqp, "AMQP 0-9-1"
	}
	return nil, ""
}

// Connect implements console.Backend: one login for the management API
// and both stream protocols, through the profile's ssh tunnel when it has
// one. The profile is the approved one, never an unconfirmed edit.
func (b *Backend) Connect(ctx context.Context, c console.Credentials) (console.Session, error) {
	pol, err := b.inForce()
	if err != nil {
		return nil, err
	}
	pr := pol.Policy.Profile
	s := &session{user: c.User}
	var dial func(ctx context.Context, network, addr string) (net.Conn, error)
	if pr.SSH != nil {
		t, err := b.openTunnel(ctx, pr)
		if err != nil {
			return nil, err
		}
		s.tunnel, dial = t, t.Dial
	}
	fail := func(err error) (console.Session, error) {
		_ = s.Close()
		return nil, err
	}
	base, err := url.Parse(pr.URL)
	if err != nil {
		return fail(errors.New("connect: invalid url"))
	}
	host, _, isTLS := endpoint("url", pr.URL)
	var tlsCfg *tls.Config
	if isTLS {
		if tlsCfg, err = tlsConfig(pr.Base, host); err != nil {
			return fail(err)
		}
	}
	s.mgmt = mgmt.New(base, c.User, c.Secret, tlsCfg, dial, pr.Limits.RequestTimeout)

	var who struct {
		Name string `json:"name"`
		Tags any    `json:"tags"`
	}
	if err := s.mgmt.Get(ctx, mgmtPath("whoami"), nil, &who); err != nil {
		return fail(fmt.Errorf("connect: management API: %w", err))
	}
	s.tags = tagList(who.Tags)
	var ov struct {
		Version string `json:"rabbitmq_version"`
	}
	if err := s.mgmt.Get(ctx, mgmtPath("overview"), nil, &ov); err != nil {
		return fail(fmt.Errorf("connect: management API: %w", err))
	}
	s.version = ov.Version

	streamDial := stream.DialFunc(nil)
	if dial != nil {
		streamDial = dial
	}
	if pr.StreamURL != "" {
		h, p, t := endpoint("stream_url", pr.StreamURL)
		cfg, err := tlsIf(t, pr.Base, h)
		if err != nil {
			return fail(err)
		}
		s.proto = b.newStreamProto(h, p, cfg, c.User, c.Secret, streamDial)
	}
	if pr.AMQPURL != "" {
		h, p, t := endpoint("amqp_url", pr.AMQPURL)
		cfg, err := tlsIf(t, pr.Base, h)
		if err != nil {
			return fail(err)
		}
		s.amqp = b.newAMQP(h, p, cfg, c.User, c.Secret, streamDial)
	}
	b.mu.Lock()
	b.cur = s
	b.mu.Unlock()
	return s, nil
}

func mgmtPath(kind string, segs ...string) mgmt.Path { return mgmt.Path{Kind: kind, Segs: segs} }

// tagList reads the tags of whoami: a list (4.x) or a comma-separated
// string (3.x).
func tagList(v any) []string {
	var out []string
	switch t := v.(type) {
	case string:
		for _, s := range strings.Split(t, ",") {
			if s = strings.TrimSpace(s); s != "" {
				out = append(out, s)
			}
		}
	case []any:
		for _, x := range t {
			if s, ok := x.(string); ok && s != "" {
				out = append(out, s)
			}
		}
	}
	return out
}

func tlsIf(on bool, base kconfig.Base, host string) (*tls.Config, error) {
	if !on {
		return nil, nil
	}
	return tlsConfig(base, host)
}

// tlsConfig is the TLS of a TLS URL scheme under the profile's tls mode:
// verify-full checks the chain and the name, verify-ca (and require with
// tls_ca) the chain only, require and prefer encrypt without verifying.
func tlsConfig(b kconfig.Base, host string) (*tls.Config, error) {
	cfg := &tls.Config{MinVersion: tls.VersionTLS12, ServerName: host}
	var roots *x509.CertPool
	if b.TLSCA != "" {
		pem, err := os.ReadFile(b.TLSCA)
		if err != nil {
			return nil, errors.New("connect: tls_ca: cannot read the file")
		}
		roots = x509.NewCertPool()
		if !roots.AppendCertsFromPEM(pem) {
			return nil, errors.New("connect: tls_ca: no PEM certificate in the file")
		}
		cfg.RootCAs = roots
	}
	if b.TLSCert != "" {
		pair, err := tls.LoadX509KeyPair(b.TLSCert, b.TLSKey)
		if err != nil {
			return nil, errors.New("connect: tls_cert/tls_key: cannot load the key pair")
		}
		cfg.Certificates = []tls.Certificate{pair}
	}
	switch {
	case b.TLS == kconfig.TLSVerifyFull:
	case kconfig.TLSVerifiesChain(b.TLS, b.TLSCA):
		// The chain, not the name.
		cfg.InsecureSkipVerify = true
		cfg.VerifyPeerCertificate = func(raw [][]byte, _ [][]*x509.Certificate) error {
			return verifyChain(raw, roots)
		}
	default:
		cfg.InsecureSkipVerify = true
	}
	return cfg, nil
}

func verifyChain(raw [][]byte, roots *x509.CertPool) error {
	if len(raw) == 0 {
		return errors.New("tls: no certificate")
	}
	certs := make([]*x509.Certificate, len(raw))
	for i, r := range raw {
		c, err := x509.ParseCertificate(r)
		if err != nil {
			return errors.New("tls: invalid certificate")
		}
		certs[i] = c
	}
	inter := x509.NewCertPool()
	for _, c := range certs[1:] {
		inter.AddCert(c)
	}
	_, err := certs[0].Verify(x509.VerifyOptions{Roots: roots, Intermediates: inter})
	return err
}

// openTunnel opens the profile's ssh tunnel: an unknown bastion key is
// trusted only on the human's answer ("yes", or on production the last 8
// characters of its fingerprint); a key passphrase or an SSH password is
// asked on the console's terminal.
func (b *Backend) openTunnel(ctx context.Context, pr config.Profile) (*tunnel.Tunnel, error) {
	if b.o.IO == nil {
		return nil, errors.New("connect: ssh: no terminal to answer the bastion's prompts")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, fmt.Errorf("connect: ssh: %w", err)
	}
	khPath := tunnel.KnownHostsPath(home)
	confirm := func(host, keyType, fp string) bool {
		b.o.IO.Println(fmt.Sprintf("The authenticity of %s can't be established.", sanitize.Terminal(host, false)))
		b.o.IO.Println(fmt.Sprintf("%s key fingerprint is %s", sanitize.Terminal(keyType, false), sanitize.Terminal(fp, false)))
		prompt, expected := "Trust this key and add it to "+sanitize.Terminal(khPath, false)+"? [yes/N] ", "yes"
		if pr.Production {
			if len(fp) < 8 {
				return false
			}
			prompt, expected = "Type the last 8 characters of the fingerprint to trust it: ", fp[len(fp)-8:]
		}
		r := console.NewTypedApprover(b.o.IO).Approve(ctx, console.Decision{Prompt: prompt, Expected: expected, Production: true})
		return r.Outcome == console.Approved
	}
	hk, err := tunnel.KnownHosts(khPath, confirm)
	if err != nil {
		return nil, fmt.Errorf("connect: ssh: %w", err)
	}
	algs, err := tunnel.HostKeyAlgorithms(khPath, *pr.SSH)
	if err != nil {
		return nil, fmt.Errorf("connect: ssh: %w", err)
	}
	t, err := tunnel.Open(ctx, tunnel.Options{
		Profile: *pr.SSH, Home: home, HostKey: hk, HostKeyAlgorithms: algs,
		Secret:    func(prompt string) ([]byte, error) { return b.o.IO.AskSecret(ctx, prompt) },
		AgentSock: os.Getenv("SSH_AUTH_SOCK"), ClientName: product.P.Name,
	})
	if err != nil {
		return nil, fmt.Errorf("connect: ssh: %w", err)
	}
	return t, nil
}
