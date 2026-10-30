package main

import (
	"bytes"
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/DavidGodefroid/lockkit/console"
	"github.com/DavidGodefroid/lockkit/ipc"
	"github.com/DavidGodefroid/lockkit/sysconf"

	"github.com/DavidGodefroid/lockmq/internal/product"
)

func TestParseAllowedUIDs(t *testing.T) {
	for in, want := range map[string][]int{
		"":               nil,
		"1000":           {1000},
		"1000,1001":      {1000, 1001},
		"1001,1001":      {1001},
		"4294967294":     {4294967294},
		"10012,1000,999": {10012, 1000, 999},
	} {
		got, err := parseAllowedUIDs(in)
		if err != nil || !slices.Equal(got, want) {
			t.Errorf("parseAllowedUIDs(%q) = %v, %v; want %v", in, got, err, want)
		}
	}
	for _, bad := range []string{"0", "007", "1000,0", "1,,2", ",1", "1,", "-1", "+1", " 1", "1 ", "a", "4294967295", "99999999999999999999"} {
		if got, err := parseAllowedUIDs(bad); err == nil {
			t.Errorf("parseAllowedUIDs(%q) = %v, want an error", bad, got)
		}
	}
}

// fakeIO records the console's lines.
type fakeIO struct{ bytes.Buffer }

func (f *fakeIO) Println(s string)                                          { f.WriteString(s + "\n") }
func (f *fakeIO) Ask(context.Context, string, time.Duration) (string, bool) { return "", false }
func (f *fakeIO) AskSecret(context.Context, string) ([]byte, error) {
	return nil, errors.New("no input")
}

// isolationEnv is a console container's: the console account lockmq
// (10011) in its own session, the image's accounts, the clients group
// holding 10012 only.
func isolationEnv(allowed []int) console.IsolationEnv {
	accounts := map[int]bool{0: true, 10011: true, 10012: true}
	return console.IsolationEnv{
		LoadSys: func() (*sysconf.Config, error) {
			return &sysconf.Config{ServiceUser: "lockmq", ClientGroup: "lockmq-clients", SocketDir: "/run/lockmq", AllowedUIDs: allowed, X11: sysconf.X11Refuse}, nil
		},
		Display:       func() sysconf.Display { return sysconf.Display{Kind: sysconf.DisplayTTY} },
		UID:           func() int { return 10011 },
		UserName:      func() (string, error) { return "lockmq", nil },
		LoginUID:      func() (int, bool) { return -1, false },
		TerminalOwner: func() (int, bool) { return 10011, true },
		ClientAllowed: func(sys *sysconf.Config, uid int) bool {
			return slices.Contains(sys.AllowedUIDs, uid) || uid == 10012
		},
		ClientGID:      func(*sysconf.Config) (int, error) { return 10010, nil },
		AccountExists:  func(uid int) bool { return accounts[uid] },
		Relay:          func() string { return "" },
		MachinectlUIDs: func() ([]int, bool) { return nil, true },
		DesktopEnv:     func() bool { return false },
		DisplaySet:     func() bool { return false },
		ParentChain:    func() ([]sysconf.Proc, error) { return nil, nil },
	}
}

// --allowed-uids adds to the image's allowed_uids, as the build argument
// does, and to nothing else: the setup read is not changed in place.
func TestAllowUIDsExtendsTheSetup(t *testing.T) {
	base := []int{4242}
	env := allowUIDs(isolationEnv(base), []int{1000, 4242})
	sys, err := env.LoadSys()
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(sys.AllowedUIDs, []int{4242, 1000}) {
		t.Fatalf("AllowedUIDs = %v, want [4242 1000]", sys.AllowedUIDs)
	}
	if !slices.Equal(base, []int{4242}) {
		t.Fatalf("the image's list changed: %v", base)
	}
	if same := allowUIDs(isolationEnv(nil), nil); same.LoadSys == nil {
		t.Fatal("no uids: LoadSys lost")
	}
}

// The isolation checks see the uids: the peer is served, and the
// console's own uid is refused (the separation would be void).
func TestAllowUIDsIsolation(t *testing.T) {
	p := product.P
	io := &fakeIO{}
	iso, err := console.CheckIsolation(p, io, allowUIDs(isolationEnv(nil), []int{1000}))
	if err != nil {
		t.Fatalf("--allowed-uids 1000: %v\n%s", err, io.String())
	}
	served := iso.PeerCheck()
	for uid, want := range map[int]bool{1000: true, 10012: true, 10011: true, 1001: false, 0: false} {
		if served(ipc.Cred{UID: uid}) != want {
			t.Errorf("peer uid %d served = %v, want %v", uid, !want, want)
		}
	}
	iso, err = console.CheckIsolation(p, io, isolationEnv(nil))
	if err != nil || iso.PeerCheck()(ipc.Cred{UID: 1000}) {
		t.Fatalf("without --allowed-uids, uid 1000 is served (%v)", err)
	}
	_, err = console.CheckIsolation(p, io, allowUIDs(isolationEnv(nil), []int{10011}))
	if err == nil || !strings.Contains(err.Error(), "may also connect as a client") {
		t.Fatalf("--allowed-uids 10011 (the console): %v, want a refusal", err)
	}
	// A session or terminal of an allowed uid is the agent's: refused.
	env := allowUIDs(isolationEnv(nil), []int{1000})
	env.TerminalOwner = func() (int, bool) { return 1000, true }
	if _, err := console.CheckIsolation(p, io, env); err == nil || !strings.Contains(err.Error(), "terminal belongs to uid 1000") {
		t.Fatalf("terminal of an allowed uid: %v, want a refusal", err)
	}
	env = isolationEnv(nil)
	env.LoadSys = func() (*sysconf.Config, error) { return nil, nil }
	if _, err := allowUIDs(env, []int{1000}).LoadSys(); err == nil || !strings.Contains(err.Error(), "separated setup") {
		t.Fatalf("no separated setup: %v, want an error", err)
	}
}
