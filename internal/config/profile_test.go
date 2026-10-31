package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func parseOne(t *testing.T, body string) (Profile, error) {
	t.Helper()
	ps, err := ParseProfiles([]byte(body), "test.toml")
	if err != nil {
		return Profile{}, err
	}
	p, ok := ps["main"]
	if !ok {
		t.Fatalf("profile main not parsed: %v", ps)
	}
	return p, nil
}

const minimal = `
[profiles.main]
url = "https://rabbitmq.example.com:15671"
`

func TestDefaults(t *testing.T) {
	p, err := parseOne(t, minimal)
	if err != nil {
		t.Fatal(err)
	}
	if p.Tier != TierRead {
		t.Errorf("tier = %v, want read", p.Tier)
	}
	want := Limits{
		RequestTimeout: 10 * time.Second, ReadMax: 20, PeekMax: 5, DeliveryHeadroom: 3,
		MaxBodyBytes: 16384, MaxOutputBytes: 65536, ListPageSize: 100,
	}
	if p.Limits != want {
		t.Errorf("limits = %+v, want %+v", p.Limits, want)
	}
	if p.IdleTimeout != 20*time.Minute || p.MaxSession != 4*time.Hour {
		t.Errorf("session = %s / %s, want 20m / 4h", p.IdleTimeout, p.MaxSession)
	}
	if p.TLS != "verify-full" {
		t.Errorf("tls = %q, want verify-full", p.TLS)
	}
	if p.Credentials != "ask" {
		t.Errorf("credentials = %q, want ask", p.Credentials)
	}
	if strings.Join(p.Detectors, ",") != "card,email,iban,phone" {
		t.Errorf("detectors = %v", p.Detectors)
	}
	if p.BodyMode != "detect" {
		t.Errorf("body_mode outside production = %q, want detect", p.BodyMode)
	}

	prod, err := parseOne(t, minimal+"production = true\n")
	if err != nil {
		t.Fatal(err)
	}
	if prod.BodyMode != "redact-unknown" {
		t.Errorf("body_mode on production = %q, want redact-unknown", prod.BodyMode)
	}

	explicit, err := parseOne(t, minimal+"detectors = []\n[profiles.main.limits]\ndelivery_headroom = 1\n")
	if err != nil {
		t.Fatal(err)
	}
	if len(explicit.Detectors) != 0 || explicit.Limits.DeliveryHeadroom != 1 {
		t.Errorf("explicit empty detectors / headroom 1 not kept: %v %d", explicit.Detectors, explicit.Limits.DeliveryHeadroom)
	}
}

func TestFullProfile(t *testing.T) {
	p, err := parseOne(t, `
[profiles.main]
url = "https://rabbitmq.example.com:15671"
amqp_url = "amqps://rabbitmq.example.com:5671"
stream_url = "rabbitmq-stream+tls://rabbitmq.example.com:5551"
vhosts = ["orders", "/"]
user = "lockmq-ro"
credentials = "vault"
vault_path = "rabbitmq/creds/lockmq-ro"
tier = "peek"
production = true
detectors = ["email", "phone"]
body_mode = "detect"

[profiles.main.limits]
request_timeout = "5s"
read_max = 50
peek_max = 3
delivery_headroom = 4
idle_timeout = "10m"
max_session = "2h"
`)
	if err != nil {
		t.Fatal(err)
	}
	if p.Tier != TierPeek || p.Limits.ReadMax != 50 || p.Limits.PeekMax != 3 || p.Limits.DeliveryHeadroom != 4 {
		t.Errorf("profile = %+v", p)
	}
	if strings.Join(p.VHosts, ",") != "/,orders" {
		t.Errorf("vhosts not sorted: %v", p.VHosts)
	}
	if p.IdleTimeout != 10*time.Minute || p.MaxSession != 2*time.Hour || p.Limits.RequestTimeout != 5*time.Second {
		t.Errorf("durations = %s %s %s", p.IdleTimeout, p.MaxSession, p.Limits.RequestTimeout)
	}
}

func TestRefusals(t *testing.T) {
	cases := []struct {
		name, body, want string
	}{
		{"amqp userinfo", minimal + `amqp_url = "amqps://u:p@h"` + "\n", "amqp_url"},
		{"amqp user only", minimal + `amqp_url = "amqps://u@h"` + "\n", "amqp_url"},
		{"stream userinfo", minimal + `stream_url = "rabbitmq-stream://u:p@h:5552"` + "\n", "stream_url"},
		{"url userinfo", "[profiles.main]\nurl = \"https://u:p@h\"\n", "url"},
		{"read_max", minimal + "[profiles.main.limits]\nread_max = 201\n", "read_max"},
		{"peek_max", minimal + "[profiles.main.limits]\npeek_max = 21\n", "peek_max"},
		{"tier write", minimal + `tier = "write"` + "\n", "tier"},
		{"unknown key", minimal + `colour = "blue"` + "\n", "colour"},
		{"secret key", minimal + `password = "x"` + "\n", "secret"},
		{"no url", "[profiles.main]\nvhosts = [\"/\"]\n", "url"},
		{"url scheme", "[profiles.main]\nurl = \"ftp://h\"\n", "url"},
		{"amqp scheme", minimal + `amqp_url = "https://h"` + "\n", "amqp_url"},
		{"body_mode", minimal + `body_mode = "clear"` + "\n", "body_mode"},
		{"detector", minimal + `detectors = ["shoe_size"]` + "\n", "detector"},
		{"negative", minimal + "[profiles.main.limits]\nlist_page_size = -1\n", "list_page_size"},
		{"headroom 0", minimal + "[profiles.main.limits]\ndelivery_headroom = 0\n", "delivery_headroom"},
		{"headroom negative", minimal + "[profiles.main.limits]\ndelivery_headroom = -1\n", "delivery_headroom"},
		{"vhost dot-dot", minimal + "vhosts = [\"..\"]\n", "vhosts"},
		{"vhost dot", minimal + "vhosts = [\".\"]\n", "vhosts"},
		{"vhost dot-dot inside", minimal + "vhosts = [\"a/../b\"]\n", "vhosts"},
		{"vault without path", minimal + `credentials = "vault"` + "\n", "vault_path"},
		{"prod name", "[profiles.y]\nurl = \"https://h\"\nproduction = true\n", "production"},
		{"control char", minimal + "vhosts = [\"a\\u001bb\"]\n", "vhosts"},
		{"idle above max", minimal + "[profiles.main.limits]\nidle_timeout = \"3h\"\nmax_session = \"1h\"\n", "idle_timeout"},
		{"bad duration", minimal + "[profiles.main.limits]\nrequest_timeout = \"soon\"\n", "request_timeout"},
		{"prod plain http", "[profiles.main]\nurl = \"http://rabbitmq.example.com:15672\"\nproduction = true\n", "tls"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := ParseProfiles([]byte(c.body), "test.toml")
			if err == nil {
				t.Fatalf("accepted:\n%s", c.body)
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Errorf("error %q does not name %q", err, c.want)
			}
			for _, secret := range []string{"u:p", "//u@"} {
				if strings.Contains(err.Error(), secret) {
					t.Errorf("error quotes the userinfo: %q", err)
				}
			}
		})
	}
}

func TestLoadProjectWins(t *testing.T) {
	dir := t.TempDir()
	user := filepath.Join(dir, "user.toml")
	writeFile(t, user, "[profiles.main]\nurl = \"https://user.example.com\"\n[profiles.other]\nurl = \"https://other.example.com\"\n")
	proj := filepath.Join(dir, "proj")
	writeFile(t, filepath.Join(proj, ".lockmq", "config.toml"), "[profiles.main]\nurl = \"https://project.example.com\"\n")
	sub := filepath.Join(proj, "sub")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	ps, root, err := LoadFrom(sub, user)
	if err != nil {
		t.Fatal(err)
	}
	if root != proj {
		t.Errorf("root = %q, want %q", root, proj)
	}
	if ps["main"].URL != "https://project.example.com" || ps["other"].URL != "https://other.example.com" {
		t.Errorf("profiles = %+v", ps)
	}
}

func writeFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// Canary secrets pasted into the wrong key: an error names the key and
// never quotes the value (a pasted token, a URL with userinfo).
const (
	canary    = "glsa_CanarySecretToken123"
	canaryURL = "https://user:glsa_Canary@h"
)

func TestErrorsDoNotEchoValues(t *testing.T) {
	type tc struct{ name, body, key string }
	var cases []tc
	for _, v := range []string{canary, canaryURL} {
		q := `"` + v + `"`
		cases = append(cases,
			tc{"credentials " + v, minimal + "credentials = " + q + "\n", "credentials"},
			tc{"tier " + v, minimal + "tier = " + q + "\n", "tier"},
			tc{"body_mode " + v, minimal + "body_mode = " + q + "\n", "body_mode"},
			tc{"tls " + v, minimal + "tls = " + q + "\n", "tls"},
			tc{"url " + v, "[profiles.main]\nurl = " + q + "\n", "url"},
			tc{"amqp_url " + v, minimal + "amqp_url = " + q + "\n", "amqp_url"},
			tc{"stream_url " + v, minimal + "stream_url = " + q + "\n", "stream_url"},
			tc{"vault_path " + v, minimal + "credentials = \"vault\"\nvault_path = \"-" + v + "\"\n", "vault_path"},
			tc{"vault_path unused " + v, minimal + "vault_path = " + q + "\n", "vault_path"},
			tc{"credentials_ttl " + v, minimal + "credentials_ttl = " + q + "\n", "credentials_ttl"},
			tc{"request_timeout " + v, minimal + "[profiles.main.limits]\nrequest_timeout = " + q + "\n", "request_timeout"},
			tc{"idle_timeout " + v, minimal + "[profiles.main.limits]\nidle_timeout = " + q + "\n", "idle_timeout"},
			tc{"max_session " + v, minimal + "[profiles.main.limits]\nmax_session = " + q + "\n", "max_session"},
			tc{"vhosts " + v, minimal + "vhosts = [\"" + v + "\\u001b\"]\n", "vhosts"},
			tc{"user " + v, minimal + "user = \"" + v + "\\u001b\"\n", "user"},
			tc{"tls_ca " + v, minimal + "tls_ca = \"" + v + "\\u001b\"\n", "tls_ca"},
			tc{"tls_cert " + v, minimal + "tls_cert = \"" + v + "\\u001b\"\n", "tls_cert"},
			tc{"tls_key " + v, minimal + "tls_key = \"" + v + "\\u001b\"\n", "tls_key"},
		)
	}
	cases = append(cases,
		tc{"user dsn", minimal + "user = \"postgres://u:" + canary + "@h/db\"\n", "user"},
		tc{"vault_path dsn", minimal + "credentials = \"vault\"\nvault_path = \"postgres://u:" + canary + "@h/db\"\n", "vault_path"},
	)
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := ParseProfiles([]byte(c.body), "test.toml")
			if err == nil {
				t.Fatalf("accepted:\n%s", c.body)
			}
			msg := err.Error()
			for _, s := range []string{canary, "glsa_Canary", "user:"} {
				if strings.Contains(msg, s) {
					t.Errorf("error echoes the value (%q): %q", s, msg)
				}
			}
			if !strings.Contains(msg, c.key) {
				t.Errorf("error %q does not name %q", msg, c.key)
			}
		})
	}
}

func TestParseTierDoesNotEcho(t *testing.T) {
	_, err := ParseTier(canary)
	if err == nil || strings.Contains(err.Error(), canary) || !strings.Contains(err.Error(), "tier") {
		t.Errorf("ParseTier(canary) = %v", err)
	}
	var tier Tier
	if err := tier.UnmarshalText([]byte(canary)); err == nil || strings.Contains(err.Error(), canary) {
		t.Errorf("UnmarshalText(canary) = %v", err)
	}
}
