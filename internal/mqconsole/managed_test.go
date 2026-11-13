//go:build lockfamily_managed_test

package mqconsole

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/DavidGodefroid/lockkit/console"
	"github.com/DavidGodefroid/lockkit/managed"

	"github.com/DavidGodefroid/lockmq/internal/msgmask"
	"github.com/DavidGodefroid/lockmq/internal/product"
)

func useManaged(t *testing.T, body string) *managed.File {
	t.Helper()
	path := filepath.Join(t.TempDir(), managed.FileName)
	if err := os.WriteFile(path, []byte("version = 1\norganisation = \"Acme\"\n"+body), 0o644); err != nil {
		t.Fatal(err)
	}
	managed.SetTestPath(path)
	t.Cleanup(func() { managed.SetTestPath("") })
	f, err := managed.Load(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func writeFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

const twoProfiles = `[profiles.uat]
url = "http://127.0.0.1:15672"
body_mode = "detect"
detectors = []
[profiles.prod]
url = "http://127.0.0.1:15672"
production = true
tier = "peek"
`

// The backend's ManagedBounds is what the console checks: a policy beyond
// a bound, or outside an allowlist, is refused citing the managed key;
// one within them passes.
func TestManagedBoundsBackend(t *testing.T) {
	f := useManaged(t, "[lockmq.bounds]\ntier = \"read\"\ncredentials = [\"ask\"]\n")
	root := t.TempDir()
	writeFile(t, filepath.Join(root, ".lockmq", "config.toml"), twoProfiles+"[profiles.kc]\nurl = \"http://127.0.0.1:15672\"\ncredentials = \"keychain\"\n")
	for _, c := range []struct{ profile, want string }{
		{"uat", ""},
		{"prod", "blocked by company policy: lockmq.bounds.tier = read (profile prod: tier = peek)"},
		{"kc", "blocked by company policy: lockmq.bounds.credentials = ask (profile kc: credentials = keychain)"},
	} {
		be := New(Options{Profile: c.profile, Root: root})
		pol, err := be.LoadPolicy()
		if err != nil {
			t.Fatal(err)
		}
		err = console.CheckManaged(product.P, be, pol, f)
		if c.want == "" {
			if err != nil {
				t.Errorf("%s: %v", c.profile, err)
			}
			continue
		}
		if !errors.Is(err, console.ErrManaged) || !strings.Contains(strings.Join(console.ManagedLines(err), "\n")+err.Error(), c.want) {
			t.Errorf("%s: %v (lines %q), want %q", c.profile, err, console.ManagedLines(err), c.want)
		}
	}
}

// A managed pii_mask masks its field under every profile, whatever the
// body mode, detectors and user rules; a user allow rule on it is refused
// at load.
func TestManagedPIIMaskEveryProfile(t *testing.T) {
	useManaged(t, "[[lockmq.bounds.pii_mask]]\nqueue = [\"orders/*\"]\nbody = \"$.customer.email\"\n")
	root := t.TempDir()
	writeFile(t, filepath.Join(root, ".lockmq", "config.toml"), twoProfiles)
	writeFile(t, filepath.Join(root, ".lockmq", "pii.toml"), "[[mask]]\nqueue = [\"orders/*\"]\nbody = \"$.customer.email\"\nmode = \"partial\"\n")
	msg := msgmask.Message{Body: []byte(`{"customer":{"email":"alice@example.com"},"id":"o-1"}`)}
	for _, name := range []string{"uat", "prod"} {
		be := New(Options{Profile: name, Root: root})
		p, err := be.LoadPolicy()
		if err != nil {
			t.Fatal(err)
		}
		out := be.mask(p.(Policy), "orders", "order-events", []msgmask.Message{msg}, nil)
		b, _ := json.Marshal(out)
		if strings.Contains(string(b), "alice") || strings.Contains(string(b), "example.com") {
			t.Errorf("%s: the managed mask did not apply: %s", name, b)
		}
	}

	writeFile(t, filepath.Join(root, ".lockmq", "pii.toml"), "[[allow]]\nqueue = [\"orders/*\"]\nbody = \"$.customer\"\n")
	if _, err := New(Options{Profile: "uat", Root: root}).LoadPolicy(); err == nil || !strings.Contains(err.Error(), "lockmq.bounds.pii_mask") {
		t.Fatalf("a user allow rule on a managed mask: %v", err)
	}
}

// A managed profile is served with no config file at all.
func TestManagedProfileOnly(t *testing.T) {
	useManaged(t, "[lockmq.profiles.shared]\nurl = \"http://127.0.0.1:15672\"\nvhosts = [\"orders\"]\n")
	be := New(Options{Profile: "shared", UserConfig: filepath.Join(t.TempDir(), "missing.toml")})
	p, err := be.LoadPolicy()
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(p.(Policy).Policy.Profile.VHosts, ","); got != "orders" {
		t.Fatalf("vhosts %q", got)
	}
}
