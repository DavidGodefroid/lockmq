package main

import (
	"bytes"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/DavidGodefroid/lockkit/sysconf"

	"github.com/DavidGodefroid/lockmq/internal/config"
	"github.com/DavidGodefroid/lockmq/internal/mcptools"
)

// containerEnv is an env for setup and doctor as they run in the image.
func containerEnv(t *testing.T, root, stdin string, out, errb *bytes.Buffer) env {
	t.Helper()
	return env{stdin: strings.NewReader(stdin), stdout: out, stderr: errb, cwd: root,
		userConfig: filepath.Join(t.TempDir(), "none.toml"),
		container:  func() bool { return true },
		sys: func() (*sysconf.Config, error) {
			return &sysconf.Config{ServiceUser: "lockmq", ClientGroup: "lockmq-clients", SocketDir: "/run/lockmq"}, nil
		}}
}

// In a container the wizard names the machine host.docker.internal and
// refuses the keychain and loopback hosts, which are the container itself.
func TestSetupInContainer(t *testing.T) {
	answers := func(url, creds string) string {
		return strings.Join([]string{"dev", url, "", "", "", "lockmq-ro", creds, "", ""}, "\n") + "\n"
	}
	root := project(t, "")
	var out, errb bytes.Buffer
	if code := run(containerEnv(t, root, answers("", ""), &out, &errb), []string{"setup"}); code != exitOK {
		t.Fatalf("exit %d: %s", code, errb.String())
	}
	for _, want := range []string{"[http://host.docker.internal:15672]", "Credentials: ask or vault [ask]", "localhost is the container itself", "docs/docker.md"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("no %q in:\n%s", want, out.String())
		}
	}
	if strings.Contains(out.String(), "keychain") {
		t.Errorf("keychain offered in a container:\n%s", out.String())
	}
	ps, _, err := config.LoadFrom(root, "")
	if err != nil {
		t.Fatal(err)
	}
	if p := ps["dev"]; p.URL != "http://host.docker.internal:15672" || p.Credentials != "ask" {
		t.Errorf("profile %+v", p)
	}
	for _, c := range []struct{ url, creds, want string }{
		{"", "keychain", "a container has no keychain"},
		{"http://127.0.0.1:15672", "", "the console's own container"},
		{"http://localhost:15672", "", "the console's own container"},
	} {
		out.Reset()
		errb.Reset()
		if code := run(containerEnv(t, project(t, ""), answers(c.url, c.creds), &out, &errb), []string{"setup"}); code == exitOK || !strings.Contains(errb.String(), c.want) {
			t.Errorf("%+v: exit %d, %s", c, code, errb.String())
		}
	}
}

// doctor --fix gives .lockmq to the console account by uid, through the
// root script, run without sudo when doctor already is root.
func TestDoctorFix(t *testing.T) {
	root := project(t, oneProfile)
	dot := filepath.Join(root, ".lockmq")
	svc := svcAccount{name: "lockmq", uid: os.Getuid() + 7}
	doctor := func(stdin string, tty bool, args ...string) (int, string, []string) {
		var out, errb bytes.Buffer
		var scripts []string
		e := containerEnv(t, root, stdin, &out, &errb)
		e.tty = tty
		e.svc = func(name string) (svcAccount, bool) { return svc, name == "lockmq" }
		e.euid = func() int { return 0 }
		e.root = func(s string) error { scripts = append(scripts, s); return nil }
		e.dial = func(string) (mcptools.Conn, error) { return nil, os.ErrNotExist }
		code := run(e, append([]string{"doctor"}, args...))
		return code, out.String() + errb.String(), scripts
	}
	code, out, scripts := doctor("", false)
	if code != exitFail || len(scripts) != 0 || !strings.Contains(out, "✗ project .lockmq: 0700") || !strings.Contains(out, "console account (lockmq, uid "+strconv.Itoa(svc.uid)+")") ||
		!strings.Contains(out, "lockmq doctor --fix") || !strings.Contains(out, "config.toml: 0600, not readable") {
		t.Errorf("check: exit %d scripts %d\n%s", code, len(scripts), out)
	}
	if code, out, scripts := doctor("", false, "--fix"); code != exitUsage || len(scripts) != 0 {
		t.Errorf("--fix without a terminal: exit %d scripts %d\n%s", code, len(scripts), out)
	}
	if code, out, scripts := doctor("y\n", true, "--fix"); len(scripts) != 1 || !strings.Contains(out, "Run them now? [y/N]") {
		t.Errorf("--fix as root: exit %d scripts %d\n%s", code, len(scripts), out)
	}
	if code, _, _ := doctor("", false, "--yes"); code != exitUsage {
		t.Errorf("--yes alone: exit %d", code)
	}
	_, _, scripts = doctor("", false, "--fix", "--yes")
	if len(scripts) != 1 || !strings.Contains(scripts[0], giveDirScript(dot, svc.uid)) || !strings.Contains(scripts[0], chmodFileScript(dot, "config.toml", svc.uid)) {
		t.Fatalf("scripts %q", scripts)
	}
	// The repaired state passes.
	svc.uid = os.Getuid()
	if err := os.Chmod(dot, 0o755); err != nil {
		t.Fatal(err)
	}
	if code, out, _ := doctor("", false); !strings.Contains(out, "✓ project .lockmq: 0755, owned by lockmq") || strings.Contains(out, "✗ project") {
		t.Errorf("repaired: exit %d\n%s", code, out)
	}
}

// A .lockmq that is a symbolic link gets no repair: a chown by root would
// change whatever it names.
func TestProjectChecksSymlinkedDotDir(t *testing.T) {
	root := t.TempDir()
	if err := os.Symlink(t.TempDir(), filepath.Join(root, ".lockmq")); err != nil {
		t.Fatal(err)
	}
	cs := projectChecks(root, svcAccount{name: "lockmq", uid: os.Getuid() + 7})
	if len(cs) != 1 || cs[0].ok || cs[0].repair != "" || !strings.Contains(cs[0].line, "symbolic link") {
		t.Errorf("symlinked .lockmq: %+v", cs)
	}
}

// The root scripts change the directory they entered, never what a
// symbolic link swapped in at its path names.
func TestInDirScript(t *testing.T) {
	sh := func(script string) error {
		return exec.Command("sh", "-c", "set -eu\n"+script).Run()
	}
	mode := func(p string) fs.FileMode {
		fi, err := os.Lstat(p)
		if err != nil {
			t.Fatal(err)
		}
		return fi.Mode().Perm()
	}
	root := t.TempDir()
	dot := root + "/.lockmq"
	if err := os.Mkdir(dot, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := sh(giveDirScript(dot, os.Getuid())); err != nil {
		t.Fatal(err)
	}
	if got := mode(dot); got != 0o755 {
		t.Errorf("mode %04o, want 0755", got)
	}
	cfg := dot + "/config.toml"
	if err := os.WriteFile(cfg, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	// Not when the directory belongs to another uid, or another account
	// may write it: the file could be swapped between test and chmod.
	if err := sh(chmodFileScript(dot, "config.toml", os.Getuid()+7)); err == nil {
		t.Error("chmod in a directory of another uid")
	}
	if err := os.Chmod(dot, 0o775); err != nil {
		t.Fatal(err)
	}
	if err := sh(chmodFileScript(dot, "config.toml", os.Getuid())); err == nil {
		t.Error("chmod in a group-writable directory")
	}
	if got := mode(cfg); got != 0o600 {
		t.Errorf("config.toml changed to %04o", got)
	}
	if err := os.Chmod(dot, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := sh(chmodFileScript(dot, "config.toml", os.Getuid())); err != nil {
		t.Fatal(err)
	}
	if got := mode(cfg); got != 0o644 {
		t.Errorf("config.toml %04o, want 0644", got)
	}

	// .lockmq swapped for a link to another directory.
	other := t.TempDir()
	if err := os.Chmod(other, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(dot); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(other, dot); err != nil {
		t.Fatal(err)
	}
	if err := sh(giveDirScript(dot, os.Getuid())); err == nil {
		t.Error("script ran through a symbolic link")
	}
	if got := mode(other); got != 0o700 {
		t.Errorf("link target changed to %04o", got)
	}

	// A file swapped for a link.
	if err := os.Remove(dot); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(dot, 0o755); err != nil {
		t.Fatal(err)
	}
	secret := other + "/secret"
	if err := os.WriteFile(secret, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(secret, dot+"/pii.toml"); err != nil {
		t.Fatal(err)
	}
	if err := sh(chmodFileScript(dot, "pii.toml", os.Getuid())); err == nil {
		t.Error("chmod ran through a symbolic link")
	}
	if got := mode(secret); got != 0o600 {
		t.Errorf("link target changed to %04o", got)
	}
}
