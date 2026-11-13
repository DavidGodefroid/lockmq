//go:build lockfamily_managed_test

package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/DavidGodefroid/lockkit/managed"

	"github.com/DavidGodefroid/lockmq/internal/msgmask"
)

// useManaged makes body the machine's managed file for the test.
func useManaged(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), managed.FileName)
	if err := os.WriteFile(path, []byte("version = 1\norganisation = \"Acme\"\n"+body), 0o644); err != nil {
		t.Fatal(err)
	}
	managed.SetTestPath(path)
	t.Cleanup(func() { managed.SetTestPath("") })
	return path
}

// project writes a project config and returns its root.
func project(t *testing.T, cfg string) string {
	t.Helper()
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".lockmq"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".lockmq", "config.toml"), []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
	return root
}

// violations loads the profile dev of cfg under the managed file and
// returns the lines that refuse it.
func violations(t *testing.T, cfg string) []string {
	t.Helper()
	m, err := LoadManaged()
	if err != nil {
		t.Fatal(err)
	}
	ps, _, err := LoadWith(project(t, cfg), "", m)
	if err != nil {
		t.Fatal(err)
	}
	p, ok := ps["dev"]
	if !ok {
		t.Fatalf("no profile dev: %v", ps)
	}
	lines, err := m.BoundsOf().Violations(m.BoundsOf().policyOf(p))
	if err != nil {
		t.Fatal(err)
	}
	return lines
}

const devURL = "[profiles.dev]\nurl = \"http://127.0.0.1:15672\"\n"

// One test per bound of [lockmq.bounds]: a profile beyond it is a
// violation citing the managed key; a profile at it or stricter is none.
func TestManagedBounds(t *testing.T) {
	for _, c := range []struct {
		name, bounds, loose, ok, want string
	}{
		{"production", "production = true", "", "production = true\n",
			"blocked by company policy: lockmq.bounds.production = true (profile dev: production = false)"},
		{"tier", `tier = "read"`, "tier = \"peek\"\n", "tier = \"read\"\n",
			"blocked by company policy: lockmq.bounds.tier = read (profile dev: tier = peek)"},
		{"tls", `tls = "verify-full"`, "tls = \"prefer\"\n", "tls = \"verify-full\"\n",
			"blocked by company policy: lockmq.bounds.tls = verify-full (profile dev: tls = prefer)"},
		{"body_mode", `body_mode = "redact-unknown"`, "body_mode = \"detect\"\n", "body_mode = \"redact-unknown\"\n",
			"blocked by company policy: lockmq.bounds.body_mode = redact-unknown (profile dev: body_mode = detect)"},
		{"vhosts outside", `vhosts = ["orders", "billing"]`, "vhosts = [\"orders\", \"hr\"]\n", "vhosts = [\"orders\"]\n",
			"blocked by company policy: lockmq.bounds.vhosts does not allow hr (profile dev: vhosts + hr)"},
		{"vhosts every", `vhosts = ["orders"]`, "", "vhosts = [\"orders\"]\n",
			"blocked by company policy: lockmq.bounds.vhosts = orders (profile dev: vhosts = (every vhost))"},
		{"credentials_ttl", `credentials_ttl = "1h"`, "credentials_ttl = \"2h\"\n", "credentials_ttl = \"30m\"\n",
			"blocked by company policy: lockmq.bounds.credentials_ttl = 1h0m0s (profile dev: credentials_ttl = 2h0m0s)"},
		{"credentials_ttl unset", `credentials_ttl = "1h"`, "", "credentials_ttl = \"1h\"\n",
			"blocked by company policy: lockmq.bounds.credentials_ttl = 1h0m0s (profile dev: credentials_ttl = 0s)"},
		{"auto", "auto = { read = \"ask\" }", "auto = { read = \"auto\" }\n", "auto = { read = \"ask\", peek = \"deny\" }\n",
			"blocked by company policy: lockmq.bounds.auto.read = ask (profile dev: auto.read = auto)"},
		{"limits.read_max", "limits = { read_max = 10 }", "limits = { read_max = 50 }\n", "limits = { read_max = 10 }\n",
			"blocked by company policy: lockmq.bounds.limits.read_max = 10 (profile dev: limits.read_max = 50)"},
		{"limits.peek_max", "limits = { peek_max = 2 }", "limits = { peek_max = 3 }\n", "limits = { peek_max = 1 }\n",
			"blocked by company policy: lockmq.bounds.limits.peek_max = 2 (profile dev: limits.peek_max = 3)"},
		{"limits.max_body_bytes", "limits = { max_body_bytes = 4096 }", "", "limits = { max_body_bytes = 4096 }\n",
			"blocked by company policy: lockmq.bounds.limits.max_body_bytes = 4096 (profile dev: limits.max_body_bytes = 16384)"},
		{"limits.max_output_bytes", "limits = { max_output_bytes = 8192 }", "limits = { max_output_bytes = 9000 }\n", "limits = { max_output_bytes = 8000 }\n",
			"blocked by company policy: lockmq.bounds.limits.max_output_bytes = 8192 (profile dev: limits.max_output_bytes = 9000)"},
		{"limits.list_page_size", "limits = { list_page_size = 50 }", "", "limits = { list_page_size = 50 }\n",
			"blocked by company policy: lockmq.bounds.limits.list_page_size = 50 (profile dev: limits.list_page_size = 100)"},
		{"limits.request_timeout", `limits = { request_timeout = "5s" }`, "", "limits = { request_timeout = \"5s\" }\n",
			"blocked by company policy: lockmq.bounds.limits.request_timeout = 5s (profile dev: limits.request_timeout = 10s)"},
		{"limits.idle_timeout", `limits = { idle_timeout = "10m" }`, "", "limits = { idle_timeout = \"10m\" }\n",
			"blocked by company policy: lockmq.bounds.limits.idle_timeout = 10m0s (profile dev: limits.idle_timeout = 20m0s)"},
		{"limits.max_session", `limits = { max_session = "1h" }`, "", "limits = { max_session = \"1h\", idle_timeout = \"30m\" }\n",
			"blocked by company policy: lockmq.bounds.limits.max_session = 1h0m0s (profile dev: limits.max_session = 4h0m0s)"},
	} {
		t.Run(c.name, func(t *testing.T) {
			useManaged(t, "[lockmq.bounds]\n"+c.bounds+"\n")
			got := violations(t, devURL+c.loose)
			if len(got) != 1 || got[0] != c.want {
				t.Errorf("violations %q, want [%q]", got, c.want)
			}
			if got := violations(t, devURL+c.ok); len(got) > 0 {
				t.Errorf("a profile within the bounds: violations %q", got)
			}
		})
	}
}

// A profile stricter than or equal to every bound has no violation.
func TestManagedBoundsStricter(t *testing.T) {
	useManaged(t, `[lockmq.bounds]
production = true
tier = "peek"
tls = "require"
body_mode = "redact-unknown"
vhosts = ["orders", "billing"]
credentials = ["ask", "vault"]
hosts = ["127.0.0.1"]
credentials_ttl = "1h"
auto = { read = "ask" }
limits = { read_max = 20, peek_max = 5, max_body_bytes = 16384, max_output_bytes = 65536, list_page_size = 100, request_timeout = "10s", idle_timeout = "20m", max_session = "4h" }
`)
	if got := violations(t, devURL+"production = true\ntier = \"read\"\nvhosts = [\"orders\"]\ncredentials_ttl = \"20m\"\nauto = { read = \"deny\" }\n"); len(got) > 0 {
		t.Errorf("violations %q", got)
	}
}

// The allowlists Diff cannot express are checked by Check: a value outside
// them is refused citing the managed key, one inside is accepted.
func TestManagedAllowlists(t *testing.T) {
	for _, c := range []struct {
		name, bounds, out, in, want string
	}{
		{"credentials", `credentials = ["vault"]`, "", "credentials = \"vault\"\nvault_path = \"rabbitmq/creds/ro\"\n",
			"blocked by company policy: lockmq.bounds.credentials = vault (profile dev: credentials = ask)"},
		{"credentials keychain", `credentials = ["ask", "vault"]`, "credentials = \"keychain\"\n", "credentials = \"ask\"\n",
			"blocked by company policy: lockmq.bounds.credentials = ask, vault (profile dev: credentials = keychain)"},
		{"hosts url", `hosts = ["*.mq.example.com", "127.0.0.1"]`, "", "",
			"blocked by company policy: lockmq.bounds.hosts = *.mq.example.com, 127.0.0.1 (profile dev: amqp_url = evil.example.org)"},
		{"hosts ssh", `hosts = ["127.0.0.1"]`, "ssh = { host = \"bastion.example.org\", user = \"ops\", auth = \"agent\" }\n", "",
			"blocked by company policy: lockmq.bounds.hosts = 127.0.0.1 (profile dev: ssh.host = bastion.example.org)"},
	} {
		t.Run(c.name, func(t *testing.T) {
			useManaged(t, "[lockmq.bounds]\n"+c.bounds+"\n")
			out := devURL + c.out
			if c.name == "hosts url" {
				out += "amqp_url = \"amqp://evil.example.org\"\n"
			}
			got := violations(t, out)
			if len(got) != 1 || got[0] != c.want {
				t.Errorf("violations %q, want [%q]", got, c.want)
			}
			if got := violations(t, devURL+c.in); len(got) > 0 {
				t.Errorf("a value inside the list: violations %q", got)
			}
		})
	}
}

// [family] vault.required refuses a profile whose credentials are not
// vault.
func TestManagedVaultRequired(t *testing.T) {
	useManaged(t, "[family]\nvault = { address = \"https://vault.example.com\", required = true }\n")
	want := "blocked by company policy: family.vault.required = true (profile dev: credentials = ask)"
	if got := violations(t, devURL); len(got) != 1 || got[0] != want {
		t.Errorf("violations %q, want [%q]", got, want)
	}
	if got := violations(t, devURL+"credentials = \"vault\"\nvault_path = \"rabbitmq/creds/ro\"\n"); len(got) > 0 {
		t.Errorf("a vault profile: violations %q", got)
	}
}

// A managed profile is served as written; a project or user profile of its
// name is refused naming it; a managed profile beyond the bounds is a load
// error.
func TestManagedProfiles(t *testing.T) {
	useManaged(t, `[lockmq.profiles.shared]
url = "https://mq.example.com:15671"
user = "ro"
production = true
vhosts = ["orders"]
`)
	ps, _, err := LoadFrom(project(t, devURL), "")
	if err != nil {
		t.Fatal(err)
	}
	sh, ok := ps["shared"]
	if !ok || sh.URL != "https://mq.example.com:15671" || sh.User != "ro" || !sh.Production || strings.Join(sh.VHosts, ",") != "orders" {
		t.Fatalf("managed profile %+v (found %v)", sh, ok)
	}
	if _, ok := ps["dev"]; !ok {
		t.Fatal("the project profile is gone")
	}

	_, _, err = LoadFrom(project(t, "[profiles.shared]\nurl = \"http://127.0.0.1:15672\"\n"), "")
	if err == nil || !strings.Contains(err.Error(), `profile "shared" is provided by the company's managed configuration`) || !strings.Contains(err.Error(), "project") {
		t.Fatalf("a project profile of a managed name: %v", err)
	}
	user := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(user, []byte("[profiles.shared]\nurl = \"http://127.0.0.1:15672\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadUser(user); err == nil || !strings.Contains(err.Error(), `profile "shared"`) || !strings.Contains(err.Error(), "user") {
		t.Fatalf("a user profile of a managed name: %v", err)
	}

	useManaged(t, `[lockmq.bounds]
tier = "read"
[lockmq.profiles.shared]
url = "https://mq.example.com:15671"
tier = "peek"
`)
	if _, err := LoadManaged(); err == nil || !strings.Contains(err.Error(), "[lockmq.profiles.shared]") || !strings.Contains(err.Error(), "lockmq.bounds.tier = read") {
		t.Fatalf("a managed profile beyond the bounds: %v", err)
	}
}

// A managed default applies where the profile omits the key; a value of
// the config files within the bounds wins; a default beyond the bounds is
// a load error.
func TestManagedDefaults(t *testing.T) {
	useManaged(t, `[lockmq.bounds]
limits = { read_max = 50 }
[lockmq.defaults]
tier = "peek"
detectors = ["email"]
limits = { read_max = 10, peek_max = 2 }
`)
	ps, _, err := LoadFrom(project(t, devURL+"[profiles.qa]\nurl = \"http://127.0.0.1:15672\"\ntier = \"read\"\nlimits = { read_max = 40 }\n"), "")
	if err != nil {
		t.Fatal(err)
	}
	dev, qa := ps["dev"], ps["qa"]
	if dev.Tier != TierPeek || dev.Limits.ReadMax != 10 || dev.Limits.PeekMax != 2 || strings.Join(dev.Detectors, ",") != "email" {
		t.Errorf("dev with the defaults: tier %s, limits %+v, detectors %v", dev.Tier, dev.Limits, dev.Detectors)
	}
	if qa.Tier != TierRead || qa.Limits.ReadMax != 40 || qa.Limits.PeekMax != 2 {
		t.Errorf("qa over the defaults: tier %s, limits %+v", qa.Tier, qa.Limits)
	}
	if dev.Limits.RequestTimeout != 10*time.Second {
		t.Errorf("a key neither sets keeps the built-in default: %s", dev.Limits.RequestTimeout)
	}

	useManaged(t, `[lockmq.bounds]
tier = "read"
[lockmq.defaults]
tier = "peek"
`)
	if _, err := LoadManaged(); err == nil || !strings.Contains(err.Error(), "[lockmq.defaults]") || !strings.Contains(err.Error(), "lockmq.bounds.tier = read (defaults: tier = peek)") {
		t.Fatalf("a default beyond the bounds: %v", err)
	}
	useManaged(t, "[lockmq.defaults]\nbogus = 1\n")
	if _, err := LoadManaged(); err == nil || !strings.Contains(err.Error(), "bogus") {
		t.Fatalf("an unknown default key: %v", err)
	}
}

// The detectors the bounds require are added to every profile; removing
// one is a violation.
func TestManagedDetectors(t *testing.T) {
	useManaged(t, "[lockmq.bounds]\ndetectors = [\"be_niss\"]\n")
	ps, _, err := LoadFrom(project(t, devURL+"detectors = []\n"), "")
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(ps["dev"].Detectors, ","); got != "be_niss" {
		t.Fatalf("detectors %q, want be_niss", got)
	}
	m, _ := LoadManaged()
	p := ps["dev"]
	p.Detectors = nil
	lines, err := m.Bounds.Violations(m.Bounds.policyOf(p))
	want := "blocked by company policy: lockmq.bounds.detectors requires be_niss (profile dev: detectors - be_niss)"
	if err != nil || len(lines) != 1 || lines[0] != want {
		t.Fatalf("a policy without a required detector: %q %v", lines, err)
	}
}

// A managed pii_mask is added to the rules in force; a user allow rule on
// what it masks is refused, a user mask of the same target takes the
// managed mode.
func TestManagedPIIMask(t *testing.T) {
	useManaged(t, `[[lockmq.bounds.pii_mask]]
queue = ["orders/*"]
body = "$.customer.email"
mode = "redact"
`)
	m, err := LoadManaged()
	if err != nil {
		t.Fatal(err)
	}
	user := msgmask.Rules{}
	weak, _ := msgmask.BuildMask(msgmask.RuleSpec{Queue: []string{"orders/*"}, Body: "$.customer.email", Mode: "partial"})
	user.Mask = append(user.Mask, weak)
	got, err := m.Bounds.ApplyPII(user)
	if err != nil {
		t.Fatal(err)
	}
	mask, _, modes := got.Texts()
	if len(mask) != 1 || mask[0] != "queue=orders/* body=$.customer.email" || modes != nil {
		t.Fatalf("rules %q modes %v", mask, modes)
	}
	for _, a := range []msgmask.RuleSpec{
		{Queue: []string{"orders/order-*"}, Body: "$.customer"},
		{Queue: []string{"*/*"}, Body: "$.customer.email"},
	} {
		r, err := msgmask.FromTexts(nil, []string{specText(a)}, nil)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := m.Bounds.ApplyPII(r); err == nil || !strings.Contains(err.Error(), "lockmq.bounds.pii_mask") {
			t.Errorf("allow %+v: %v", a, err)
		}
	}
	other, _ := msgmask.FromTexts(nil, []string{"queue=billing/* body=$.customer.email", "queue=orders/* body=$.order.id"}, nil)
	if _, err := m.Bounds.ApplyPII(other); err != nil {
		t.Errorf("an allow rule elsewhere: %v", err)
	}
}

func specText(s msgmask.RuleSpec) string {
	return "queue=" + strings.Join(s.Queue, ",") + " body=" + s.Body
}
