package main

import (
	"bytes"
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/DavidGodefroid/lockkit/agentwire"
	"github.com/DavidGodefroid/lockkit/sysconf"
	"github.com/DavidGodefroid/lockkit/verbs"

	"github.com/DavidGodefroid/lockmq/internal/config"
	"github.com/DavidGodefroid/lockmq/internal/mcptools"
)

// containerEnv is an env for profile add and doctor as they run in the
// image, through the launcher: LOCKMQ_HOST_UID is this uid.
func containerEnv(t *testing.T, root, stdin string, out, errb *bytes.Buffer) env {
	t.Helper()
	return env{stdin: strings.NewReader(stdin), stdout: out, stderr: errb, cwd: root,
		userConfig: filepath.Join(t.TempDir(), "none.toml"),
		container:  func() bool { return true },
		getenv: func(name string) string {
			if name == hostUIDEnv {
				return strconv.Itoa(os.Getuid())
			}
			return ""
		},
		leaveRoot: func() error { return nil },
		agentEnv: func() (agentwire.Env, error) {
			return agentwire.Env{Home: t.TempDir(), Getenv: func(string) string { return "" },
				LookPath: func(string) (string, error) { return "", os.ErrNotExist },
				Run:      func(string, ...string) ([]byte, error) { return nil, os.ErrPermission }}, nil
		},
		sys: func() (*sysconf.Config, error) {
			return &sysconf.Config{ServiceUser: "lockmq", ClientGroup: "lockmq-clients", SocketDir: "/run/lockmq"}, nil
		}}
}

// In a container profile add names the machine host.docker.internal and
// refuses the keychain and loopback hosts, which are the container itself.
func TestProfileAddInContainer(t *testing.T) {
	answers := func(url, creds string) string {
		return strings.Join([]string{"dev", url, "", "", "", "lockmq-ro", creds, "", ""}, "\n") + "\n"
	}
	ttyEnv := func(root, stdin string, out, errb *bytes.Buffer) env {
		e := containerEnv(t, root, stdin, out, errb)
		e.tty = true
		return e
	}
	add := []string{"profile", "add"}
	root := project(t, "")
	var out, errb bytes.Buffer
	if code := run(ttyEnv(root, answers("", ""), &out, &errb), add); code != exitOK {
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
		if code := run(ttyEnv(project(t, ""), answers(c.url, c.creds), &out, &errb), add); code == exitOK || !strings.Contains(errb.String(), c.want) {
			t.Errorf("%+v: exit %d, %s", c, code, errb.String())
		}
	}
	// The non-interactive form refuses a loopback host too.
	out.Reset()
	errb.Reset()
	args := append(add, "--non-interactive", "--url", "http://localhost:15672")
	if code := run(containerEnv(t, project(t, ""), "", &out, &errb), args); code != exitUsage || !strings.Contains(errb.String(), "the console's own container") {
		t.Errorf("non-interactive loopback: exit %d, %s", code, errb.String())
	}
}

// doctorFixture runs doctor in the image, as the launcher does (as root
// with --fix), recording the root scripts and when doctor leaves root.
type doctorFixture struct {
	t      *testing.T
	root   string
	svc    svcAccount
	events []string
	// run makes the root scripts run (as this uid), not only recorded.
	run bool
	// leaveErr fails the switch to the agent account.
	leaveErr error
}

func (f *doctorFixture) doctor(stdin string, tty bool, args ...string) (int, string, string) {
	f.t.Helper()
	var out, errb bytes.Buffer
	e := containerEnv(f.t, f.root, stdin, &out, &errb)
	e.tty = tty
	e.svc = func(name string) (svcAccount, bool) { return f.svc, name == "lockmq" }
	e.euid = func() int { return 0 }
	e.root = func(s string) error {
		f.events = append(f.events, "root:"+s)
		if f.run {
			if out, err := exec.Command("sh", "-c", s).CombinedOutput(); err != nil {
				return errors.New(string(out) + err.Error())
			}
		}
		return nil
	}
	e.leaveRoot = func() error { f.events = append(f.events, "leave"); return f.leaveErr }
	e.dial = func(string) (mcptools.Conn, error) { return nil, os.ErrNotExist }
	code := run(e, append([]string{"doctor"}, args...))
	return code, out.String(), errb.String()
}

// doctor --fix makes .lockmq what lox run copies: yours, written by you
// only. The repairs run as root, then doctor becomes the agent account
// before any check.
func TestDoctorFix(t *testing.T) {
	root := project(t, oneProfile)
	dot := filepath.Join(root, ".lockmq")
	cfg := filepath.Join(dot, "config.toml")
	f := &doctorFixture{t: t, root: root, svc: svcAccount{name: "lockmq", uid: os.Getuid() + 7}}
	if err := os.Chmod(dot, 0o775); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(cfg, 0o622); err != nil {
		t.Fatal(err)
	}
	code, out, _ := f.doctor("", false)
	if code != exitFail || len(f.events) != 1 || f.events[0] != "leave" ||
		!strings.Contains(out, "✗ project .lockmq: 0775, writable by group or others: lox run refuses it") ||
		!strings.Contains(out, "✗ project .lockmq/config.toml: 0622, writable by group or others") ||
		!strings.Contains(out, "repair: lockmq doctor --fix") {
		t.Errorf("check: exit %d events %q\n%s", code, f.events, out)
	}
	f.events = nil
	if code, out, _ := f.doctor("", false, "--fix"); code != exitUsage || len(f.events) != 0 {
		t.Errorf("--fix without a terminal: exit %d events %q\n%s", code, f.events, out)
	}
	if code, _, _ := f.doctor("", false, "--yes"); code != exitUsage {
		t.Errorf("--yes alone: exit %d", code)
	}
	f.events = nil
	if code, out, _ := f.doctor("n\n", true, "--fix"); len(f.events) != 1 || f.events[0] != "leave" || !strings.Contains(out, "Run them now? [y/N]") || !strings.Contains(out, "root repairs not made") || code != exitFail {
		t.Errorf("--fix answered no: exit %d events %q\n%s", code, f.events, out)
	}
	f.events = nil
	f.run = true
	code, out, _ = f.doctor("", false, "--fix", "--yes")
	if len(f.events) != 2 || !strings.HasPrefix(f.events[0], "root:") || f.events[1] != "leave" {
		t.Fatalf("the repairs as root, then the agent account: %q", f.events)
	}
	for _, want := range []string{modeScript(cfg, false, os.Getuid()), modeScript(dot, true, os.Getuid())} {
		if !strings.Contains(f.events[0], want) {
			t.Errorf("no %q in the script:\n%s", want, f.events[0])
		}
	}
	// The repaired state passes: exit 0, the console's warning aside.
	if code != exitOK || !strings.Contains(out, "✓ project .lockmq: yours (uid "+strconv.Itoa(os.Getuid())+")") || strings.Contains(out, "✗") {
		t.Errorf("repaired: exit %d\n%s", code, out)
	}
	for p, want := range map[string]fs.FileMode{dot: 0o755, cfg: 0o600} {
		if fi, err := os.Stat(p); err != nil || fi.Mode().Perm() != want {
			t.Errorf("%s: %v %v, want %04o", p, fi, err, want)
		}
	}
	// Nothing left to repair: no script.
	f.events = nil
	if code, _, _ := f.doctor("", false, "--fix", "--yes"); code != exitOK || len(f.events) != 1 || f.events[0] != "leave" {
		t.Errorf("second --fix: exit %d events %q", code, f.events)
	}
}

// A fresh setup, as init and profile add leave it, passes: warnings only.
func TestDoctorFreshSetupPasses(t *testing.T) {
	root := project(t, oneProfile)
	for p, m := range map[string]fs.FileMode{filepath.Join(root, ".lockmq"): 0o755, filepath.Join(root, ".lockmq", "config.toml"): 0o644} {
		if err := os.Chmod(p, m); err != nil {
			t.Fatal(err)
		}
	}
	f := &doctorFixture{t: t, root: root, svc: svcAccount{name: "lockmq", uid: os.Getuid() + 7}}
	code, out, errb := f.doctor("", false, "--fix", "--yes", "--json")
	var d verbs.Doctor
	oneJSON(t, out, &d)
	if code != exitOK || !d.OK || len(d.Fixed) != 0 || len(f.events) != 1 {
		t.Fatalf("exit %d events %q %+v\n%s", code, f.events, d, errb)
	}
	for _, c := range d.Checks {
		if c.Check == "project" && c.Status != verbs.StatusOK {
			t.Errorf("project: %+v", c)
		}
	}
}

// A doctor run as root that cannot become the agent account checks
// nothing: its checks would run as root.
func TestDoctorRefusesToCheckAsRoot(t *testing.T) {
	f := &doctorFixture{t: t, root: project(t, oneProfile), leaveErr: errors.New("no account agent")}
	code, out, _ := f.doctor("", false, "--json")
	var d verbs.Doctor
	oneJSON(t, out, &d)
	if code != exitFail || d.OK || len(d.Checks) != 1 || d.Checks[0].Check != "account" || !strings.Contains(d.Checks[0].Detail, "no account agent") {
		t.Errorf("exit %d %+v", code, d)
	}
}

// Without LOCKMQ_HOST_UID doctor in a container cannot tell whose .lockmq
// it is: it neither checks nor repairs it, and says why.
func TestDoctorWithoutHostUID(t *testing.T) {
	root := project(t, oneProfile)
	if err := os.Chmod(filepath.Join(root, ".lockmq"), 0o777); err != nil {
		t.Fatal(err)
	}
	for _, v := range []string{"", "abc", "-1", "007", "4294967295"} {
		var out, errb bytes.Buffer
		var scripts []string
		e := containerEnv(t, root, "", &out, &errb)
		e.getenv = func(string) string { return v }
		e.root = func(s string) error { scripts = append(scripts, s); return nil }
		e.dial = func(string) (mcptools.Conn, error) { return nil, os.ErrNotExist }
		code := run(e, []string{"doctor", "--fix", "--yes"})
		if code != exitOK || len(scripts) != 0 || !strings.Contains(out.String(), "- project .lockmq: not checked: LOCKMQ_HOST_UID") {
			t.Errorf("%q: exit %d scripts %d\n%s%s", v, code, len(scripts), out.String(), errb.String())
		}
	}
}

// What an older doctor --fix left, .lockmq and a copy of pii.toml owned
// by the console account, is given back to you: the files first, while
// their directory is still the console account's.
func TestProjectChecksGiveBackTheConsoleAccount(t *testing.T) {
	root := t.TempDir()
	dot := filepath.Join(root, ".lockmq")
	if err := os.Mkdir(dot, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{"config.toml", "pii.toml"} {
		if err := os.WriteFile(filepath.Join(dot, n), []byte("x = 1\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	me := os.Getuid()
	// Here this uid plays the console account, and you are another.
	cs := projectChecks(root, me+7, svcAccount{name: "lockmq", uid: me})
	want := []string{
		giveBackFileScript(dot, "config.toml", me, me+7),
		giveBackFileScript(dot, "pii.toml", me, me+7),
		giveBackDirScript(dot, me, me+7),
	}
	if len(cs) != len(want) {
		t.Fatalf("checks %+v", cs)
	}
	for i, c := range cs {
		if c.ok || c.repair != want[i] || !strings.Contains(c.line, "owned by the console account (lockmq, uid "+strconv.Itoa(me)+"), as an older lockmq doctor --fix left it") {
			t.Errorf("check %d: %+v", i, c)
		}
	}
	// The scripts run (you are this uid too here: chown to oneself).
	for _, s := range []string{giveBackFileScript(dot, "pii.toml", me, me), giveBackDirScript(dot, me, me)} {
		if out, err := exec.Command("sh", "-c", "set -eu\n"+s).CombinedOutput(); err != nil {
			t.Fatalf("%v: %s", err, out)
		}
	}
	for _, c := range projectChecks(root, me, svcAccount{name: "lockmq", uid: me + 7}) {
		if !c.ok {
			t.Errorf("after the repair: %+v", c)
		}
	}
	// Without the console account's name (outside the image), a file of
	// another uid is only reported.
	for _, c := range projectChecks(root, me+7, svcAccount{}) {
		if c.ok || c.repair != "" || !strings.Contains(c.line, "not by you (uid "+strconv.Itoa(me+7)+")") {
			t.Errorf("another owner: %+v", c)
		}
	}
}

// Links get no repair, and lox run copies none: neither .lockmq as a link
// nor a link in it, nor a hard-linked file of the console account.
func TestProjectChecksLinks(t *testing.T) {
	me := os.Getuid()
	root := t.TempDir()
	if err := os.Symlink(t.TempDir(), filepath.Join(root, ".lockmq")); err != nil {
		t.Fatal(err)
	}
	cs := projectChecks(root, me, svcAccount{name: "lockmq", uid: me + 7})
	if len(cs) != 1 || cs[0].ok || cs[0].repair != "" || !strings.Contains(cs[0].line, "a symbolic link") {
		t.Errorf("symlinked .lockmq: %+v", cs)
	}
	root = t.TempDir()
	dot := filepath.Join(root, ".lockmq")
	if err := os.Mkdir(dot, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/etc/passwd", filepath.Join(dot, "config.toml")); err != nil {
		t.Fatal(err)
	}
	cs = projectChecks(root, me, svcAccount{name: "lockmq", uid: me + 7})
	if len(cs) != 1 || cs[0].repair != "" || !strings.Contains(cs[0].line, "project .lockmq/config.toml: a symbolic link") {
		t.Errorf("symlinked config.toml: %+v", cs)
	}
	if err := os.Remove(filepath.Join(dot, "config.toml")); err != nil {
		t.Fatal(err)
	}
	victim := filepath.Join(t.TempDir(), "victim")
	if err := os.WriteFile(victim, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(victim, filepath.Join(dot, "pii.toml")); err != nil {
		t.Skip("no hard link here:", err)
	}
	// This uid plays the console account.
	for _, c := range projectChecks(root, me+7, svcAccount{name: "lockmq", uid: me}) {
		if strings.Contains(c.line, "pii.toml") && (c.repair != "" || !strings.Contains(c.line, "2 links")) {
			t.Errorf("hard-linked pii.toml: %+v", c)
		}
	}
}

// A subdirectory doctor cannot list is a warning, not a failure: lox run,
// as you, still checks it.
func TestProjectChecksUnlistable(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root lists any directory")
	}
	me := os.Getuid()
	root := t.TempDir()
	sub := filepath.Join(root, ".lockmq", "keys")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	// This uid plays the console account, which owns keys, 0300.
	if err := os.Chmod(sub, 0o300); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(sub, 0o755) })
	var warned bool
	for _, c := range projectChecks(root, me+7, svcAccount{name: "lockmq", uid: me}) {
		if c.warn && strings.Contains(c.line, "project .lockmq/keys: not checked inside") {
			warned = true
		}
	}
	if !warned {
		t.Error("no warning for the unlistable directory")
	}
}

// A project behind a directory the agent account cannot enter is reported:
// the agent commands would find no profile there.
func TestDoctorClosedProject(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root enters any directory")
	}
	closed := filepath.Join(t.TempDir(), "closed")
	proj := filepath.Join(closed, "proj")
	if err := os.MkdirAll(filepath.Join(proj, ".lockmq"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(closed, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(closed, 0o755) })
	if got := closedProject(proj); got != closed {
		t.Errorf("closedProject %q, want %q", got, closed)
	}
	if got := closedProject(t.TempDir()); got != "" {
		t.Errorf("open directory reported closed: %q", got)
	}
	code, out, _ := runWith(t, proj, nil, os.ErrNotExist, "", "doctor")
	if code == exitOK || !strings.Contains(out, "✗ project access: "+closed+" (or a directory above it) is closed") {
		t.Errorf("exit %d\n%s", code, out)
	}
}

// The root scripts change the directory they entered, never what a
// symbolic link swapped in at its path names, nor a file of another owner.
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
	me := os.Getuid()
	root := t.TempDir()
	dot := root + "/.lockmq"
	if err := os.Mkdir(dot, 0o777); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dot, 0o777); err != nil {
		t.Fatal(err)
	}
	if err := sh(modeScript(dot, true, me)); err != nil {
		t.Fatal(err)
	}
	if got := mode(dot); got != 0o755 {
		t.Errorf("mode %04o, want 0755", got)
	}
	// Never a directory of another owner than the one checked, nor root's.
	if err := os.Chmod(dot, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, owner := range []int{0, me + 7} {
		if err := sh(giveBackDirScript(dot, owner, me)); err == nil {
			t.Errorf("directory given back with owner %d checked", owner)
		}
		if err := sh(modeScript(dot, true, owner)); err == nil && owner != me {
			t.Errorf("mode changed with owner %d checked", owner)
		}
	}
	if err := sh(giveBackDirScript(dot, me, me)); err != nil {
		t.Fatal(err)
	}
	if got := mode(dot); got != 0o700 {
		t.Errorf("given back %04o, want 0700 kept", got)
	}
	cfg := dot + "/pii.toml"
	if err := os.WriteFile(cfg, nil, 0o666); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(cfg, 0o666); err != nil {
		t.Fatal(err)
	}
	// Not in a directory of another uid, or one another account may write:
	// the file could be swapped between test and chown.
	if err := sh(giveBackFileScript(dot, "pii.toml", me+7, me)); err == nil {
		t.Error("chown in a directory of another uid")
	}
	if err := os.Chmod(dot, 0o775); err != nil {
		t.Fatal(err)
	}
	if err := sh(giveBackFileScript(dot, "pii.toml", me, me)); err == nil {
		t.Error("chown in a group-writable directory")
	}
	if got := mode(cfg); got != 0o666 {
		t.Errorf("pii.toml changed to %04o", got)
	}
	if err := os.Chmod(dot, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := sh(modeScript(cfg, false, me+7)); err == nil {
		t.Error("chmod of a file of another owner")
	}
	if err := sh(giveBackFileScript(dot, "pii.toml", me, me)); err != nil {
		t.Fatal(err)
	}
	if got := mode(cfg); got != 0o644 {
		t.Errorf("pii.toml %04o, want 0644", got)
	}

	// A hard link to another file.
	victim := t.TempDir() + "/victim"
	if err := os.WriteFile(victim, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(cfg); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(victim, cfg); err != nil {
		t.Log("no hard link here:", err)
	} else if err := sh(giveBackFileScript(dot, "pii.toml", me, me)); err == nil {
		t.Error("chown through a hard link")
	}
	_ = os.Remove(cfg)

	// .lockmq swapped for a link to another directory.
	other := t.TempDir()
	if err := os.Chmod(other, 0o777); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(dot); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(other, dot); err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{giveBackDirScript(dot, me, me), modeScript(dot, true, me)} {
		if err := sh(s); err == nil {
			t.Error("script ran through a symbolic link")
		}
	}
	if got := mode(other); got != 0o777 {
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
	if err := os.WriteFile(secret, nil, 0o666); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(secret, 0o666); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(secret, dot+"/pii.toml"); err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{giveBackFileScript(dot, "pii.toml", me, me), modeScript(dot+"/pii.toml", false, me)} {
		if err := sh(s); err == nil {
			t.Error("script ran through a symbolic link")
		}
	}
	if got := mode(secret); got != 0o666 {
		t.Errorf("link target changed to %04o", got)
	}
}

// The account a doctor run as root becomes is the image's agent account.
func TestAgentAccountIsTheImages(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("..", "..", "Dockerfile"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), "\nUSER "+agentAccount+"\n") || !strings.Contains(string(b), "-G lockmq-clients -s /bin/sh -g \"lockmq agent\" "+agentAccount) {
		t.Errorf("the Dockerfile's agent account is not %q", agentAccount)
	}
}
