//go:build unix

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// mountsManaged reports whether a docker run's arguments name the managed
// directory, on the host (dir) or in the container (/etc/lockfamily).
func mountsManaged(run []string, dir string) bool {
	for _, a := range run {
		if strings.Contains(a, "/etc/lockfamily") || strings.Contains(a, dir) {
			return true
		}
	}
	return false
}

// The machine's managed directory, when there is one, is mounted read-only
// on /etc/lockfamily in every container (as lox run mounts it in its
// locks); without one, nothing is.
func TestLauncherManagedMount(t *testing.T) {
	f := newLauncherFixture(t)
	commands := [][]string{
		{"managed", "show"}, {"managed", "check", "--profile", "prod"}, {"doctor"}, {"doctor", "--fix", "--yes"},
		{"profile", "add"}, {"init", "claude"}, {"mcp"}, {"status"}, {"peek", "orders"}, {"console"}, {"version"}, {"help"},
	}
	// Without the directory, docker run's arguments are those of a launcher
	// without managed support.
	without := map[string][]string{}
	for _, args := range commands {
		out, calls, err := f.run(t, f.project, nil, args...)
		if err != nil {
			t.Fatalf("%v: %v\n%s", args, err, out)
		}
		run := lastRun(t, calls)
		if mountsManaged(run, f.managed) {
			t.Errorf("%v: no managed directory, but mounted: %q", args, run)
		}
		without[strings.Join(args, " ")] = run
	}
	if err := os.MkdirAll(f.managed, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, args := range commands {
		out, calls, err := f.run(t, f.project, nil, args...)
		if err != nil {
			t.Fatalf("%v: %v\n%s", args, err, out)
		}
		run := lastRun(t, calls)
		mount := []string{"-v", f.managed + ":/etc/lockfamily:ro"}
		if !hasSeq(run, mount...) {
			t.Errorf("%v: managed directory not mounted read-only on /etc/lockfamily: %q", args, run)
			continue
		}
		// Before the image: an argument of docker, not of lockmq.
		if img := imageArgs(run, "lockmq"); img == nil || mountsManaged(img, f.managed) {
			t.Errorf("%v: mount after the image: %q", args, run)
		}
		// The mount is all that changes.
		var rest []string
		for i := 0; i < len(run); i++ {
			if i+1 < len(run) && run[i] == mount[0] && run[i+1] == mount[1] {
				i++
				continue
			}
			rest = append(rest, run[i])
		}
		if got, want := strings.Join(rest, "\n"), strings.Join(without[strings.Join(args, " ")], "\n"); got != want {
			t.Errorf("%v: arguments beyond the mount changed:\n%q\nwant\n%q", args, rest, want)
		}
	}
}

// The managed directory follows uname -s: /Library/Application
// Support/lockfamily on macOS, /etc/lockfamily elsewhere.
func TestLauncherManagedPathByOS(t *testing.T) {
	f := newLauncherFixture(t)
	if err := os.MkdirAll(f.managed, 0o755); err != nil {
		t.Fatal(err)
	}
	other := filepath.Join(f.root, "elsewhere")
	uname := filepath.Join(f.bin, "uname")
	for _, tc := range []struct {
		os, darwin, other string
		mounted           bool
	}{
		{"Darwin", f.managed, other, true},
		{"Darwin", other, f.managed, false},
		{"Linux", other, f.managed, true},
		{"Linux", f.managed, other, false},
	} {
		if err := os.Remove(uname); err != nil && !os.IsNotExist(err) {
			t.Fatal(err)
		}
		writeExec(t, uname, "#!/bin/sh\necho "+tc.os+"\n")
		writeExec(t, f.script, f.managedAt(t, tc.darwin, tc.other))
		out, calls, err := f.run(t, f.project, nil, "managed", "show")
		if err != nil {
			t.Fatalf("%s: %v\n%s", tc.os, err, out)
		}
		run := lastRun(t, calls)
		if got := hasSeq(run, "-v", f.managed+":/etc/lockfamily:ro"); got != tc.mounted {
			t.Errorf("%s, darwin %s, other %s: mounted %v: %q", tc.os, tc.darwin, tc.other, got, run)
		}
		if !tc.mounted && mountsManaged(run, f.managed) {
			t.Errorf("%s: %q", tc.os, run)
		}
	}
	// The launcher as released names the two paths of lockkit's
	// managed.DefaultPath.
	for _, line := range []string{managedDarwin, managedOther} {
		if !strings.Contains(f.launcher, line) {
			t.Errorf("launcher lacks %q", line)
		}
	}
}

// A managed path that is not a directory, or that docker -v cannot mount,
// stops the launcher before docker runs.
func TestLauncherManagedRefused(t *testing.T) {
	f := newLauncherFixture(t)
	if err := os.MkdirAll(filepath.Dir(f.managed), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(f.managed, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	out, calls, err := f.run(t, f.project, nil, "managed", "show")
	if err == nil || !strings.Contains(out, f.managed+" is not a directory") || len(calls) != 0 {
		t.Errorf("a file: %v, calls %q\n%s", err, calls, out)
	}
	// A link to a directory is mounted: what the image checks (root owns
	// it) is the directory it leads to.
	if err := os.Remove(f.managed); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(t.TempDir(), f.managed); err != nil {
		t.Fatal(err)
	}
	out, calls, err = f.run(t, f.project, nil, "managed", "show")
	if err != nil || !hasSeq(lastRun(t, calls), "-v", f.managed+":/etc/lockfamily:ro") {
		t.Errorf("a link to a directory: %v, %q\n%s", err, calls, out)
	}
	// A dangling link: refused, not skipped.
	if err := os.Remove(f.managed); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(f.root, "nowhere"), f.managed); err != nil {
		t.Fatal(err)
	}
	out, calls, err = f.run(t, f.project, nil, "managed", "check")
	if err == nil || !strings.Contains(out, f.managed+" is not a directory") || len(calls) != 0 {
		t.Errorf("a dangling link: %v, calls %q\n%s", err, calls, out)
	}
	// A ":" in the path.
	colon := filepath.Join(f.root, "a:b")
	if err := os.MkdirAll(colon, 0o755); err != nil {
		t.Fatal(err)
	}
	writeExec(t, f.script, f.managedAt(t, colon, colon))
	out, calls, err = f.run(t, f.project, nil, "managed", "show")
	if err == nil || !strings.Contains(out, "which docker -v cannot mount") || len(calls) != 0 {
		t.Errorf("a \":\": %v, calls %q\n%s", err, calls, out)
	}
}
