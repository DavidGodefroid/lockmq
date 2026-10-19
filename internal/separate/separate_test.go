//go:build unix

package separate

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/DavidGodefroid/lockkit/kit"
)

var p = kit.Product{Name: "lockmq", Account: "lockmq", ClientsGroup: "lockmq-clients", KeychainService: "lockmq", EnvPrefix: "LOCKMQ_"}

func TestCheck(t *testing.T) {
	for _, bad := range []Options{
		{Client: "agent", Service: "agent", Group: "g"},
		{Client: "Agent!", Service: "lockmq", Group: "g"},
		{Client: "agent", Service: "lockmq", Group: "a b"},
		{Client: "agent; rm", Service: "lockmq", Group: "g"},
	} {
		if Check(bad) == nil {
			t.Errorf("accepted %+v", bad)
		}
	}
	o := Options{Client: "agent"}
	Fill(p, &o)
	if Check(o) != nil || o.Service != "lockmq" || o.Group != "lockmq-clients" {
		t.Errorf("defaults %+v", o)
	}
}

// The scripts name the product's account, group, system file, socket
// directory and binary, and nothing of another product.
func TestScripts(t *testing.T) {
	o := Options{Client: "agent", Service: "lockmq", Group: "lockmq-clients"}
	linux := Script(p, "linux", "/opt/lockmq", o, "")
	for _, want := range []string{"useradd --create-home --shell /bin/bash --comment \"lockmq console\" lockmq",
		"usermod -aG lockmq-clients agent", "cat > /etc/lockmq/system.toml", `service_user = "lockmq"`,
		`socket_dir   = "/run/lockmq"`, "d /run/lockmq 2710 lockmq lockmq-clients -", "/etc/tmpfiles.d/lockmq.conf",
		"install -m 0755 -o root -g root '/opt/lockmq' /usr/local/bin/lockmq"} {
		if !strings.Contains(linux, want) {
			t.Errorf("linux script lacks %q:\n%s", want, linux)
		}
	}
	darwin := Script(p, "darwin", "/opt/lockmq", o, "")
	if !strings.Contains(darwin, "dseditgroup -o edit -a agent -t user lockmq-clients") || !strings.Contains(darwin, `socket_dir   = "/usr/local/var/run/lockmq"`) {
		t.Errorf("darwin script:\n%s", darwin)
	}
	for _, s := range []string{linux, darwin} {
		if strings.Contains(s, "locksql") {
			t.Errorf("another product named:\n%s", s)
		}
	}
	// A release already at /usr/local/bin is not copied onto itself.
	if s := Script(p, "linux", InstalledBinary(p), o, ""); strings.Contains(s, "install -m 0755") {
		t.Errorf("copy onto itself:\n%s", s)
	}
	if s := Script(p, "linux", InstalledBinary(p), o, strings.Repeat("a", 64)); !strings.Contains(s, `install -m 0755 -o root -g root "$tmp" /usr/local/bin/lockmq`) {
		t.Errorf("pinned copy:\n%s", s)
	}
}

// The script is valid sh.
func TestScriptParses(t *testing.T) {
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("no sh")
	}
	o := Options{Client: "agent", Service: "lockmq", Group: "lockmq-clients"}
	for _, goos := range []string{"linux", "darwin"} {
		if out, err := exec.Command(sh, "-n", "-c", Script(p, goos, "/opt/it's lockmq", o, strings.Repeat("b", 64))).CombinedOutput(); err != nil {
			t.Errorf("%s: %v\n%s", goos, err, out)
		}
	}
}

// With a pin, the root script copies the binary, checks the copy and stops
// before any change when it differs.
func TestPinnedSourceChecksTheCopy(t *testing.T) {
	if _, err := exec.LookPath("sha256sum"); err != nil {
		t.Skip("sha256sum is not installed")
	}
	bin := filepath.Join(t.TempDir(), "lock mq")
	if err := os.WriteFile(bin, []byte("binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	sum, err := FileSum(bin)
	if err != nil {
		t.Fatal(err)
	}
	prefix, src := PinnedSource(p, bin, sum, "sha256sum -c -")
	run := func() string {
		out, _ := exec.Command("sh", "-c", "set -eu\n"+prefix+"cat "+src+"; echo; echo DONE").CombinedOutput()
		return string(out)
	}
	if out := run(); !strings.Contains(out, "binary\nDONE") {
		t.Fatalf("unchanged binary:\n%s", out)
	}
	if err := os.WriteFile(bin, []byte("replaced"), 0o755); err != nil {
		t.Fatal(err)
	}
	if out := run(); strings.Contains(out, "DONE") || !strings.Contains(out, "lockmq install: the binary changed") {
		t.Fatalf("replaced binary:\n%s", out)
	}
}

func TestSourceRefusal(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("as root the file is root-owned")
	}
	path := filepath.Join(t.TempDir(), "lockmq")
	if err := os.WriteFile(path, []byte("binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	msg, err := SourceRefusal(p, path)
	if err != nil || !strings.Contains(msg, "could have been replaced by the agent") || !strings.Contains(msg, "--trust-binary") {
		t.Errorf("%q %v", msg, err)
	}
	if _, err := SourceRefusal(p, filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Error("missing binary accepted")
	}
}
