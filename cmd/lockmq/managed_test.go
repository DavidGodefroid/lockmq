//go:build lockfamily_managed_test

package main

import (
	"flag"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/DavidGodefroid/lockkit/client"
	"github.com/DavidGodefroid/lockkit/managed"

	"github.com/DavidGodefroid/lockmq/internal/product"
)

var update = flag.Bool("update", false, "rewrite the golden files of managed show, managed check and doctor")

// useManaged makes body the machine's managed file ("" for none) and
// returns its path.
func useManaged(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), managed.FileName)
	if body != "" {
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	managed.SetTestPath(path)
	t.Cleanup(func() { managed.SetTestPath("") })
	return path
}

// golden compares got, with the managed path made <managed>, with
// testdata/managed/<name>.golden.
func golden(t *testing.T, name, path, got string) {
	t.Helper()
	got = strings.ReplaceAll(got, path, "<managed>")
	file := filepath.Join("testdata", "managed", name+".golden")
	if *update {
		if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(file, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(file)
	if err != nil {
		t.Fatalf("%v (run go test -tags lockfamily_managed_test ./cmd/lockmq -update)", err)
	}
	if got != string(want) {
		t.Errorf("%s:\n--- got\n%s--- want\n%s", name, got, want)
	}
}

const managedFile = `version = 1
organisation = "Acme"

[lockmq.bounds]
tier = "read"
vhosts = ["orders", "billing"]
credentials = ["ask", "vault"]
limits = { read_max = 50, max_session = "8h" }
detectors = ["email"]

[[lockmq.bounds.pii_mask]]
queue = ["orders/*"]
body = "$.customer.email"

[lockmq.defaults]
tls = "require"
limits = { read_max = 10 }

[lockmq.profiles.shared]
url = "https://mq.example.com:15671"
vhosts = ["orders"]
`

// The output and exit codes of managed show and managed check are a
// contract: lox runs managed check before it starts the lock (lockhub#179).
func TestManagedShowGolden(t *testing.T) {
	root := project(t, oneProfile)
	for _, c := range []struct {
		name, body string
		code       int
	}{
		{"show-none", "", exitOK},
		{"show-file", managedFile, exitOK},
		{"show-refused", "version = 1\n[bogus]\nx = 1\n", exitUsage},
	} {
		t.Run(c.name, func(t *testing.T) {
			path := useManaged(t, c.body)
			code, out, stderr := runWith(t, root, nil, nil, "", "managed", "show")
			if code != c.code {
				t.Errorf("exit %d, want %d (%s)", code, c.code, stderr)
			}
			golden(t, c.name, path, "exit "+itoa(code)+"\n--- stdout\n"+out+"--- stderr\n"+stderr)
		})
	}
}

func TestManagedCheckGolden(t *testing.T) {
	for _, c := range []struct {
		name, cfg string
		args      []string
		code      int
	}{
		{"check-ok", oneProfile + "vhosts = [\"orders\"]\n", nil, exitOK},
		{"check-violation", oneProfile + "tier = \"peek\"\n[profiles.qa]\nurl = \"http://127.0.0.1:15672\"\nvhosts = [\"orders\"]\n", nil, exitUsage},
		{"check-profile", oneProfile + "tier = \"peek\"\n[profiles.qa]\nurl = \"http://127.0.0.1:15672\"\nvhosts = [\"orders\"]\n", []string{"--profile", "qa"}, exitOK},
		{"check-none", oneProfile + "tier = \"peek\"\n", nil, exitOK},
	} {
		t.Run(c.name, func(t *testing.T) {
			body := managedFile
			if c.name == "check-none" {
				body = ""
			}
			path := useManaged(t, body)
			code, out, stderr := runWith(t, project(t, c.cfg), nil, nil, "", append([]string{"managed", "check"}, c.args...)...)
			if code != c.code {
				t.Errorf("exit %d, want %d (%s%s)", code, c.code, out, stderr)
			}
			golden(t, c.name, path, "exit "+itoa(code)+"\n--- stdout\n"+out+"--- stderr\n"+stderr)
		})
	}
}

// doctor has a managed line: the file's description, none, or the error.
func TestDoctorManagedGolden(t *testing.T) {
	root := project(t, oneProfile)
	noConsole := &client.NoConsoleError{Product: product.P, Profile: "dev", Project: root, Socket: "/x", Err: syscall.ECONNREFUSED}
	for _, c := range []struct{ name, body string }{
		{"doctor-none", ""},
		{"doctor-file", managedFile},
		{"doctor-refused", "version = 2\n"},
	} {
		t.Run(c.name, func(t *testing.T) {
			path := useManaged(t, c.body)
			_, out, _ := runWith(t, root, nil, noConsole, "", "doctor")
			var line string
			for _, l := range strings.Split(out, "\n") {
				if strings.HasPrefix(l, "✓ managed:") || strings.HasPrefix(l, "✗ managed:") {
					line = l
				}
			}
			golden(t, c.name, path, line+"\n")
		})
	}
}

func itoa(n int) string { return string(rune('0' + n)) }
