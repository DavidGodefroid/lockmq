package mqconsole

import (
	"net"
	"strconv"
	"strings"

	kconfig "github.com/DavidGodefroid/lockkit/config"
	"github.com/DavidGodefroid/lockkit/console"

	"github.com/DavidGodefroid/lockmq/internal/config"
)

// Policy is the console's view of a profile's policy: config.Policy plus
// the profile lockkit's console needs. Its JSON is config.Policy's, so the
// approved store holds the same document.
type Policy struct {
	config.Policy
}

var _ console.Policy = Policy{}

// Diff implements config.Policy on the wrapped policies.
func (p Policy) Diff(approved kconfig.Policy) []kconfig.Change {
	if a, ok := approved.(Policy); ok {
		approved = a.Policy
	}
	return p.Policy.Diff(approved)
}

// Management API default ports.
const (
	mgmtPort    = 15672
	mgmtTLSPort = 15671
)

// Profile implements console.Policy. Where names every broker endpoint of
// the profile, so that a change of any of them ends the session.
func (p Policy) Profile() console.Profile {
	pr := p.Policy.Profile
	u, _ := config.ParseURL("url", pr.URL)
	def := mgmtPort
	if u.TLS {
		def = mgmtTLSPort
	}
	port := u.Port
	if port == 0 {
		port = def
	}
	return console.Profile{
		Name: pr.Name, Production: pr.Production, Where: where(pr),
		Host: u.Host, Port: port, DefaultPort: def,
		User: pr.User, Credentials: pr.Credentials, VaultPath: pr.VaultPath, CredentialsTTL: pr.CredentialsTTL,
		IdleTimeout: pr.IdleTimeout, MaxSession: pr.MaxSession,
	}
}

// endpoint is host:port of a profile URL, with the scheme's default port.
func endpoint(key, raw string) (host string, port int, tls bool) {
	u, err := config.ParseURL(key, raw)
	if err != nil {
		return "", 0, false
	}
	port = u.Port
	if port == 0 {
		port = map[string][2]int{"url": {mgmtPort, mgmtTLSPort}, "amqp_url": {5672, 5671}, "stream_url": {5552, 5551}}[key][boolIndex(u.TLS)]
	}
	return u.Host, port, u.TLS
}

func boolIndex(b bool) int {
	if b {
		return 1
	}
	return 0
}

func where(pr config.Profile) string {
	parts := []string{hostPort("url", pr.URL)}
	if pr.AMQPURL != "" {
		parts = append(parts, "amqp "+hostPort("amqp_url", pr.AMQPURL))
	}
	if pr.StreamURL != "" {
		parts = append(parts, "stream "+hostPort("stream_url", pr.StreamURL))
	}
	if pr.SSH != nil {
		parts = append(parts, "via ssh "+pr.SSH.Host)
	}
	return strings.Join(parts, " · ")
}

func hostPort(key, raw string) string {
	h, p, _ := endpoint(key, raw)
	return net.JoinHostPort(h, strconv.Itoa(p))
}
