//go:build unix

package main

import (
	"bytes"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/DavidGodefroid/lockkit/client"
	"github.com/DavidGodefroid/lockkit/sysconf"

	"github.com/DavidGodefroid/lockmq/internal/mcptools"
	"github.com/DavidGodefroid/lockmq/internal/product"
)

// separatedSetup points the system setup at a separated-setup file whose
// socket directory is a short temporary one (unix socket paths are short),
// and clears the instance variables.
func separatedSetup(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "lmqin")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	conf := filepath.Join(t.TempDir(), "system.toml")
	if err := os.WriteFile(conf, []byte("service_user = \"root\"\nsocket_dir = \""+dir+"\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	oldPath, oldSkip := sysconf.Path, sysconf.SkipOwnerCheck
	sysconf.Path, sysconf.SkipOwnerCheck = conf, true
	t.Cleanup(func() { sysconf.Path, sysconf.SkipOwnerCheck = oldPath, oldSkip })
	t.Setenv("LOCKMQ_INSTANCE", "")
	t.Setenv("LEA_LOCKMQ_INSTANCES", "")
	return dir
}

// listenAt listens where a console of the instance would, and reports
// each connection on the returned channel.
func listenAt(t *testing.T, root, profile, instance string) <-chan struct{} {
	t.Helper()
	path, err := client.SocketPathInstance(product.P, root, profile, instance)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	l, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = l.Close() })
	got := make(chan struct{}, 4)
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			got <- struct{}{}
			_ = c.Close()
		}
	}()
	return got
}

func realEnv(root string, stdin io.Reader, out, errb *bytes.Buffer) env {
	return env{stdin: stdin, stdout: out, stderr: errb, cwd: root, userConfig: filepath.Join(os.TempDir(), "lockmq-none.toml")}
}

func waitConn(t *testing.T, got <-chan struct{}) {
	t.Helper()
	select {
	case <-got:
	case <-time.After(5 * time.Second):
		t.Fatal("no connection reached the instance's socket directory")
	}
}

func TestInstanceFlagDialsTheInstanceDirectory(t *testing.T) {
	separatedSetup(t)
	root := project(t, oneProfile)
	got := listenAt(t, root, "dev", "orders")
	var out, errb bytes.Buffer
	run(realEnv(root, strings.NewReader(""), &out, &errb), []string{"status", "--instance", "orders"})
	waitConn(t, got)
}

// --instance=orders (the = form) with two locks listed picks orders.
func TestInstanceFlagAmongSeveralListedDialsIt(t *testing.T) {
	separatedSetup(t)
	root := project(t, oneProfile)
	t.Setenv("LEA_LOCKMQ_INSTANCES", "inventory orders")
	got := listenAt(t, root, "dev", "orders")
	var out, errb bytes.Buffer
	run(realEnv(root, strings.NewReader(""), &out, &errb), []string{"queues", "--instance=orders"})
	waitConn(t, got)
}

func TestInstanceFromLOCKMQ_INSTANCEDialsIt(t *testing.T) {
	separatedSetup(t)
	root := project(t, oneProfile)
	t.Setenv("LOCKMQ_INSTANCE", "orders")
	got := listenAt(t, root, "dev", "orders")
	var out, errb bytes.Buffer
	run(realEnv(root, strings.NewReader(""), &out, &errb), []string{"status"})
	waitConn(t, got)
}

func TestTheOneListedInstanceIsTheDefault(t *testing.T) {
	separatedSetup(t)
	root := project(t, oneProfile)
	t.Setenv("LEA_LOCKMQ_INSTANCES", "orders")
	got := listenAt(t, root, "dev", "orders")
	var out, errb bytes.Buffer
	run(realEnv(root, strings.NewReader(""), &out, &errb), []string{"status"})
	waitConn(t, got)
}

func TestDoctorInstanceDialsTheInstanceDirectory(t *testing.T) {
	separatedSetup(t)
	root := project(t, oneProfile)
	got := listenAt(t, root, "dev", "orders")
	var out, errb bytes.Buffer
	run(realEnv(root, strings.NewReader(""), &out, &errb), []string{"doctor", "--instance", "orders"})
	waitConn(t, got)
}

// Where no console is dialled (no profile), doctor --fix does not resolve
// the instance: an ambiguous or bad one is no failure.
func TestDoctorFixResolvesNoInstanceWithoutDialling(t *testing.T) {
	separatedSetup(t)
	root := project(t, "")
	for _, tc := range []struct {
		list string
		args []string
	}{
		{"inventory orders", []string{"doctor", "--fix", "--yes"}},
		{"", []string{"doctor", "--fix", "--yes", "--instance", "../x"}},
	} {
		t.Setenv("LEA_LOCKMQ_INSTANCES", tc.list)
		var out, errb bytes.Buffer
		run(realEnv(root, strings.NewReader(""), &out, &errb), tc.args)
		all := out.String() + errb.String()
		for _, bad := range []string{"several lockmq locks", "not a lock instance name"} {
			if strings.Contains(all, bad) {
				t.Errorf("%v: instance resolution failed doctor: %q", tc.args, all)
			}
		}
	}
}

func TestMCPPassesTheInstanceToItsDialer(t *testing.T) {
	separatedSetup(t)
	root := project(t, oneProfile)
	got := listenAt(t, root, "dev", "orders")
	pr, pw := io.Pipe()
	var out, errb bytes.Buffer
	done := make(chan int, 1)
	go func() { done <- run(realEnv(root, pr, &out, &errb), []string{"mcp", "--instance", "orders"}) }()
	for _, line := range []string{
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"t","version":"0"}}}`,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"` + mcptools.ToolStatus + `","arguments":{}}}`,
	} {
		if _, err := io.WriteString(pw, line+"\n"); err != nil {
			t.Fatal(err)
		}
	}
	waitConn(t, got)
	_ = pw.Close()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("mcp did not end")
	}
}

// The instance is refused, before any socket path or dial, as a usage
// error that names --instance and never the name.
func TestInstanceUsageErrors(t *testing.T) {
	root := project(t, oneProfile)
	cases := []struct {
		name  string
		list  string
		args  []string
		wants []string
	}{
		{"a bad name", "", []string{"status", "--instance", "../x"}, []string{"--instance", "not a lock instance name", "usage: lockmq status"}},
		{"a bad name for mcp", "", []string{"mcp", "--instance", "../x"}, []string{"--instance", "not a lock instance name", "usage: lockmq mcp"}},
		{"a bad name for doctor", "", []string{"doctor", "--instance", "../x"}, []string{"--instance", "not a lock instance name"}},
		{"several and none", "inventory orders", []string{"status"}, []string{"several lockmq locks: inventory orders", "--instance NAME"}},
		{"several and none for mcp", "inventory orders", []string{"mcp"}, []string{"several lockmq locks: inventory orders", "--instance NAME"}},
		{"several and none for doctor", "inventory orders", []string{"doctor"}, []string{"several lockmq locks: inventory orders", "--instance NAME"}},
		{"not among them", "inventory orders", []string{"status", "--instance", "shop"}, []string{"no lockmq lock shop: inventory orders"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("LOCKMQ_INSTANCE", "")
			t.Setenv("LEA_LOCKMQ_INSTANCES", tc.list)
			var out, errb bytes.Buffer
			dialed := false
			e := realEnv(root, strings.NewReader(""), &out, &errb)
			e.dial = func(string) (mcptools.Conn, error) { dialed = true; return nil, os.ErrNotExist }
			if code := run(e, tc.args); code != exitUsage {
				t.Fatalf("exit %d, want %d; stderr %q", code, exitUsage, errb.String())
			}
			if dialed {
				t.Error("the console was dialled")
			}
			for _, w := range tc.wants {
				if !strings.Contains(errb.String()+out.String(), w) {
					t.Errorf("output %q lacks %q", errb.String()+out.String(), w)
				}
			}
			if strings.Contains(errb.String()+out.String(), "../x") {
				t.Errorf("the refused name is echoed: %q", errb.String()+out.String())
			}
		})
	}
}

// Without instance the real dialer reaches the socket directory itself.
func TestNoInstanceDialsTheSocketDirectoryItself(t *testing.T) {
	separatedSetup(t)
	root := project(t, oneProfile)
	got := listenAt(t, root, "dev", "")
	var out, errb bytes.Buffer
	run(realEnv(root, strings.NewReader(""), &out, &errb), []string{"status"})
	waitConn(t, got)
}

func TestNoInstanceIsToday(t *testing.T) {
	t.Setenv("LOCKMQ_INSTANCE", "")
	t.Setenv("LEA_LOCKMQ_INSTANCES", "")
	root := project(t, oneProfile)
	if code, _, stderr := runWith(t, root, &fakeConn{}, nil, "", "queues"); code != exitOK {
		t.Fatalf("exit %d, stderr %q", code, stderr)
	}
}
