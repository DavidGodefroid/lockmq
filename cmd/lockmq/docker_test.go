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
	if err := os.Chmod(root, 0o700); err != nil {
		t.Fatal(err)
	}
	if code, out, _ := doctor("", false); code != exitFail || !strings.Contains(out, "✗ project access: "+root+" is 0700") {
		t.Errorf("no project access failure: exit %d\n%s", code, out)
	}
	if err := os.Chmod(root, 0o755); err != nil {
		t.Fatal(err)
	}
	code, out, scripts := doctor("", false)
	if strings.Contains(out, "project access") || code != exitFail || len(scripts) != 0 || !strings.Contains(out, "✗ project .lockmq: 0700") || !strings.Contains(out, "console account (lockmq, uid "+strconv.Itoa(svc.uid)+")") ||
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
	if len(scripts) != 1 || !strings.Contains(scripts[0], giveDirScript(dot, svc.uid, os.Getuid())) || !strings.Contains(scripts[0], fileScript(dot, "config.toml", svc.uid, os.Getuid(), "")) {
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
	if err := os.Chmod(root, 0o755); err != nil {
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
	if err := sh(giveDirScript(dot, os.Getuid(), os.Getuid())); err != nil {
		t.Fatal(err)
	}
	if got := mode(dot); got != 0o755 {
		t.Errorf("mode %04o, want 0755", got)
	}
	// Never a directory of another owner than the one checked, nor root's.
	if err := os.Chmod(dot, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, owner := range []int{0, os.Getuid() + 7} {
		if err := sh(giveDirScript(dot, os.Getuid(), owner)); err == nil {
			t.Errorf("directory handed over with owner %d checked", owner)
		}
	}
	if got := mode(dot); got != 0o700 {
		t.Errorf("mode changed to %04o", got)
	}
	if err := sh(giveDirScript(dot, os.Getuid(), os.Getuid())); err != nil {
		t.Fatal(err)
	}
	cfg := dot + "/config.toml"
	if err := os.WriteFile(cfg, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	// Not when the directory belongs to another uid, or another account
	// may write it: the file could be swapped between test and chmod.
	if err := sh(fileScript(dot, "config.toml", os.Getuid()+7, os.Getuid(), "")); err == nil {
		t.Error("chmod in a directory of another uid")
	}
	if err := os.Chmod(dot, 0o775); err != nil {
		t.Fatal(err)
	}
	if err := sh(fileScript(dot, "config.toml", os.Getuid(), os.Getuid(), "")); err == nil {
		t.Error("chmod in a group-writable directory")
	}
	if got := mode(cfg); got != 0o600 {
		t.Errorf("config.toml changed to %04o", got)
	}
	if err := os.Chmod(dot, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := sh(fileScript(dot, "config.toml", os.Getuid(), os.Getuid(), "")); err != nil {
		t.Fatal(err)
	}
	if got := mode(cfg); got != 0o644 {
		t.Errorf("config.toml %04o, want 0644", got)
	}

	// A hard link to another file, or a file of another owner.
	other2 := t.TempDir()
	victim := other2 + "/victim"
	if err := os.WriteFile(victim, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(cfg); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(victim, cfg); err != nil {
		t.Log("no hard link here:", err)
	} else {
		if err := sh(fileScript(dot, "config.toml", os.Getuid(), os.Getuid(), "")); err == nil {
			t.Error("chmod through a hard link")
		}
		if got := mode(victim); got != 0o600 {
			t.Errorf("hard link target changed to %04o", got)
		}
		found := false
		for _, c := range projectChecks(root, svcAccount{name: "lockmq", uid: os.Getuid() + 7}) {
			if strings.Contains(c.line, "config.toml") {
				found = true
				if c.ok || c.repair != "" || !strings.Contains(c.line, "2 links") {
					t.Errorf("hard-linked config.toml offered a repair: %+v", c)
				}
			}
		}
		if !found {
			t.Error("no check of the hard-linked config.toml")
		}
		if err := os.Remove(cfg); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(cfg, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := sh(fileScript(dot, "config.toml", os.Getuid(), os.Getuid()+7, "")); err == nil {
		t.Error("chmod of a file of another owner")
	}
	if err := os.Remove(cfg); err != nil {
		t.Fatal(err)
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
	if err := sh(giveDirScript(dot, os.Getuid(), os.Getuid())); err == nil {
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
	if err := sh(fileScript(dot, "pii.toml", os.Getuid(), os.Getuid(), "")); err == nil {
		t.Error("chmod ran through a symbolic link")
	}
	if got := mode(secret); got != 0o600 {
		t.Errorf("link target changed to %04o", got)
	}
}

// A pii.toml the console account does not own is reported: its owner can
// rewrite the masking rules. The repair gives it to the console account.
func TestProjectChecksPIIOwner(t *testing.T) {
	root := t.TempDir()
	dot := filepath.Join(root, ".lockmq")
	if err := os.Mkdir(dot, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dot, "pii.toml"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	me := os.Getuid()
	svc := svcAccount{name: "lockmq", uid: me + 7}
	var pii *projectCheck
	for _, c := range projectChecks(root, svc) {
		if strings.Contains(c.line, "pii.toml") {
			pii = &c
		}
	}
	if pii == nil || pii.ok || !strings.Contains(pii.line, "can rewrite the masking rules") || pii.repair != fileScript(dot, "pii.toml", svc.uid, me, "chown "+strconv.Itoa(svc.uid)) {
		t.Fatalf("pii.toml check: %+v", pii)
	}
	// The script runs: here the console account is this uid.
	if err := exec.Command("sh", "-c", "set -eu\n"+fileScript(dot, "pii.toml", me, me, "chown "+strconv.Itoa(me))).Run(); err != nil {
		t.Fatal(err)
	}
	for _, c := range projectChecks(root, svcAccount{name: "lockmq", uid: me}) {
		if !c.ok {
			t.Errorf("after the repair: %+v", c)
		}
	}
}
