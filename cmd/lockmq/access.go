package main

import (
	"errors"
	"fmt"
	"io/fs"
	"math"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/DavidGodefroid/lockmq/internal/product"
)

// projectCheck is one check of the project's .lockmq directory, and the
// root script that repairs it ("" when there is none). warn marks what
// doctor could not check, which lox up still checks.
type projectCheck struct {
	ok, warn     bool
	line, repair string
}

// svcAccount is the console account, as file permissions see it.
type svcAccount struct {
	name string
	uid  int
}

// lookupService finds the console account; false when this machine (or
// image) has none, as outside the image.
func lookupService(name string) (svcAccount, bool) {
	u, err := user.Lookup(name)
	if err != nil {
		return svcAccount{}, false
	}
	uid, err := strconv.Atoi(u.Uid)
	if err != nil {
		return svcAccount{}, false
	}
	return svcAccount{name: name, uid: uid}, true
}

// hostUIDEnv names your uid on the host, which the launcher passes to
// doctor: in the image doctor runs as the agent account (or, for --fix, as
// root), never as you.
const hostUIDEnv = "LOCKMQ_HOST_UID"

// you is the uid that runs `lox up`, which must own .lockmq: the
// launcher's LOCKMQ_HOST_UID, else, outside a container, this process's.
func (e env) you() (int, error) {
	s := e.getenvOr(hostUIDEnv)
	if s == "" {
		if e.inContainer() || e.geteuid() == 0 {
			return -1, errors.New(hostUIDEnv + " is not set: in a container doctor cannot tell your uid; run it through the launcher (docs/docker.md)")
		}
		return os.Getuid(), nil
	}
	n, err := strconv.ParseUint(s, 10, 32)
	if err != nil || n == math.MaxUint32 || strconv.FormatUint(n, 10) != s {
		return -1, errors.New(hostUIDEnv + ": not a uid")
	}
	return int(n), nil
}

// getenvOr is the environment variable name: e.getenv when set (tests),
// else the process's.
func (e env) getenvOr(name string) string {
	if e.getenv != nil {
		return e.getenv(name)
	}
	return os.Getenv(name)
}

// projectChecks are what `lox up` needs of the project's .lockmq at root
// (lockhub's docs/lox-up.md, "Profiles: a reviewed copy"): it reads the
// directory as you (uid you), following no link, and copies only your own
// regular files and directories that neither group nor others can write.
// The console reads that copy, never the project.
//
// svc is the console account: an older `lockmq doctor --fix` gave it
// .lockmq (and a copy of pii.toml), which lox up then refuses; the repair
// gives them back to you, as root. Nothing else is handed over: a link, or
// a file of another account (root's included), is yours to replace.
func projectChecks(root string, you int, svc svcAccount) []projectCheck {
	dir := filepath.Join(root, product.P.ProjectDir())
	fi, err := os.Lstat(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return []projectCheck{{line: "project " + product.P.ProjectDir() + ": " + err.Error()}}
	}
	w := projectWalk{you: you, svc: svc}
	w.walk(dir, product.P.ProjectDir(), fi, false)
	if len(w.out) == 0 {
		return []projectCheck{{ok: true, line: fmt.Sprintf("project %s: yours (uid %d), no one else can write it: lox up can copy it", product.P.ProjectDir(), you)}}
	}
	return w.out
}

// projectWalk collects the checks of .lockmq, each directory's after
// those of its entries: the root repairs of a directory's entries run
// while it is still the console account's, before it is given back.
type projectWalk struct {
	you int
	svc svcAccount
	out []projectCheck
}

// walk checks path, named name in the lines, which lstat described as fi;
// inSvc tells that its directory is the console account's.
func (w *projectWalk) walk(path, name string, fi fs.FileInfo, inSvc bool) {
	kind := "file"
	if fi.IsDir() {
		kind = "directory"
	}
	switch {
	case fi.Mode()&fs.ModeSymlink != 0:
		w.fail(fmt.Sprintf("project %s: a symbolic link: lox up copies no link; replace it with a file or a directory of your own", name), "")
		return
	case !fi.IsDir() && !fi.Mode().IsRegular():
		w.fail(fmt.Sprintf("project %s: not a regular file or a directory: lox up copies none; remove it", name), "")
		return
	}
	owner, ok := fileOwner(fi)
	if !ok {
		return
	}
	perm := fi.Mode().Perm()
	svcOwned := w.svc.name != "" && owner == w.svc.uid && owner != w.you && owner != 0
	readable, mode := perm&0o400 != 0, "go-w,u+r"
	if fi.IsDir() {
		readable, mode = perm&0o500 == 0o500, "go-w,u+rx"
		// A directory of another account is reported, not listed.
		if (owner == w.you && readable) || svcOwned {
			w.entries(path, name, svcOwned)
		}
	}
	switch {
	case owner == w.you:
		var why []string
		if perm&0o022 != 0 {
			why = append(why, "writable by group or others")
		}
		if !readable {
			why = append(why, "not readable by you")
		}
		if len(why) == 0 {
			return
		}
		w.fail(fmt.Sprintf("project %s: %04o, %s: lox up refuses it; run: chmod %s %s",
			name, perm, strings.Join(why, " and "), mode, shellQuote(path)),
			modeScript(path, fi.IsDir(), w.you))
	case svcOwned:
		line := fmt.Sprintf("project %s: owned by the console account (%s, uid %d), as an older lockmq doctor --fix left it: lox up refuses a %s you (uid %d) do not own; lockmq doctor --fix gives it back to you",
			name, w.svc.name, owner, kind, w.you)
		switch links, ok := fileLinks(fi); {
		case fi.IsDir():
			w.fail(line, giveBackDirScript(path, w.svc.uid, w.you))
		case !inSvc:
			// In a directory of yours: replace it with your own copy, which
			// needs no root (a chown as root there is a race: the file can
			// be swapped for another between the test and the change).
			w.fail(fmt.Sprintf("project %s: owned by the console account (%s, uid %d): lox up refuses a file you (uid %d) do not own; replace it with a copy of your own: cp %[5]s %[5]s.new && mv %[5]s.new %[5]s",
				name, w.svc.name, owner, w.you, shellQuote(path)), "")
		case !ok || links != 1:
			// A second name: it may be another file linked here.
			w.fail(fmt.Sprintf("project %s: owned by the console account (uid %d) with %d links: not repaired; replace it with a file of your own", name, owner, links), "")
		default:
			w.fail(line, giveBackFileScript(filepath.Dir(path), filepath.Base(path), w.svc.uid, w.you))
		}
	default:
		w.fail(fmt.Sprintf("project %s: owned by uid %d, not by you (uid %d): lox up refuses it; replace it with a %s of your own", name, owner, w.you, kind), "")
	}
}

// entries walks the entries of the directory path; svcOwned tells that it
// is the console account's.
func (w *projectWalk) entries(path, name string, svcOwned bool) {
	ents, err := os.ReadDir(path)
	if err != nil {
		w.out = append(w.out, projectCheck{warn: true, line: fmt.Sprintf("project %s: not checked inside (%v): the account doctor runs as cannot list it; lox up checks it", name, cause(err))})
		return
	}
	for _, ent := range ents {
		p := filepath.Join(path, ent.Name())
		fi, err := os.Lstat(p)
		if err != nil {
			w.fail(fmt.Sprintf("project %s/%s: %v", name, ent.Name(), cause(err)), "")
			continue
		}
		w.walk(p, name+"/"+ent.Name(), fi, svcOwned)
	}
}

// cause is the error of a path operation without the path (the line names
// it already).
func cause(err error) error {
	var pe *fs.PathError
	if errors.As(err, &pe) {
		return pe.Err
	}
	return err
}

func (w *projectWalk) fail(line, repair string) {
	w.out = append(w.out, projectCheck{line: line, repair: repair})
}

// modeScript is the root script that makes path, yours (uid you), a
// directory or a file that neither group nor others can write and you can
// read. A directory is changed as "." once entered; a file by its name in
// its directory, after a test: a file swapped for a link in between would
// have its target changed, but these modes only take write away from group
// and others and give its owner read, which opens it to no other account.
func modeScript(path string, dir bool, you int) string {
	if dir {
		return inDirScript(path, fmt.Sprintf(`set -- $(ls -ldn .) && test "$3" = %d && chmod go-w,u+rx .`, you))
	}
	n := inDirName(filepath.Base(path))
	return inDirScript(filepath.Dir(path), fmt.Sprintf(`test ! -L %[1]s && set -- $(ls -ln %[1]s) && case "$1" in -*) ;; *) false;; esac && test "$3" = %[2]d && chmod go-w,u+r %[1]s`, n, you))
}

// inDirName is the quoted operand of the entry name in the current
// directory: "./name", which no command takes for an option (BSD chmod
// takes no "--").
func inDirName(name string) string {
	return shellQuote("./" + name)
}

// giveBackDirScript is the root script that gives the directory dir, owned
// by the console account (uid svc, never root), back to you, writable by
// you only.
func giveBackDirScript(dir string, svc, you int) string {
	return inDirScript(dir, fmt.Sprintf(`set -- $(ls -ldn .) && test "$3" = %d && test "$3" != 0 && chown %d . && chmod go-w,u+rx .`, svc, you))
}

// giveBackFileScript is the root script that gives dir/name, a file of the
// console account (uid svc) in a directory of the console account, back to
// you. chown follows no link here, and the script first checks that the
// directory it entered belongs to svc and that neither its group nor
// others may write it, nor an ACL entry, so no other account can swap the
// file between the test and the change. The file must then be a regular
// file of svc (never root) with a single link: a hard link planted before
// the repair could name any file of the filesystem.
func giveBackFileScript(dir, name string, svc, you int) string {
	n := inDirName(name)
	return inDirScript(dir, fmt.Sprintf(`set -- $(ls -ldn .) && test "$3" = %[1]d && case "$1" in ?????w*|????????w*|*+) false;; esac && `+
		`test ! -L %[2]s && set -- $(ls -ln %[2]s) && case "$1" in -*) ;; *) false;; esac && test "$2" = 1 && test "$3" = %[1]d && test "$3" != 0 && chown %[3]d %[2]s && chmod go-w,u+r %[2]s`,
		svc, n, you))
}

// inDirScript runs cmds as root inside dir, in a subshell. You (and your
// agents) may own dir and the directories above it, and swap any of them
// for a symbolic link between the check and the script: a chown or chmod
// by path would then change whatever the link names. The script enters dir
// once and checks that it is the directory at that path, symbolic links
// resolved, before it changes ".", which no rename can redirect.
func inDirScript(dir, cmds string) string {
	real := dir
	if parent, err := filepath.EvalSymlinks(filepath.Dir(dir)); err == nil {
		real = filepath.Join(parent, filepath.Base(dir))
	}
	return fmt.Sprintf("(cd -P -- %s && test \"$(pwd -P)\" = %s && %s)", shellQuote(dir), shellQuote(real), cmds)
}

// shellQuote quotes s for a POSIX shell.
func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

// asRoot runs a shell script as root: e.root when set (tests), else sudo
// sh -c on the human's terminal, or sh -c when this already is root (a
// container run with --user 0, which has no sudo). The script holds no
// secret.
func (e env) asRoot(script string) error {
	if e.root != nil {
		return e.root(script)
	}
	cmd := exec.Command("sudo", "sh", "-c", script)
	if e.geteuid() == 0 {
		cmd = exec.Command("sh", "-c", script)
	}
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, e.stdout, e.stderr
	return cmd.Run()
}
