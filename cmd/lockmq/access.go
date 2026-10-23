package main

import (
	"fmt"
	"io/fs"
	"os"
	"os/user"
	"path/filepath"
	"strconv"

	"github.com/DavidGodefroid/lockmq/internal/product"
	"github.com/DavidGodefroid/lockmq/internal/separate"
)

// projectCheck is one check of the project's .lockmq directory, and the
// root script that repairs it ("" when there is none).
type projectCheck struct {
	ok           bool
	line, repair string
}

// svcAccount is the console account, as file permissions see it.
type svcAccount struct {
	name string
	uid  int
}

// lookupService finds the console account; false when this machine (or
// image) has none, as on a host without lockmq install.
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

// projectChecks are what the console account needs of the project at root:
// to own its .lockmq directory, where it writes pii.toml (a directory the
// agent's account owns would let it replace that file, and with it the
// masking rules), and to read the files in it.
func projectChecks(root string, svc svcAccount) []projectCheck {
	var out []projectCheck
	// The console account must enter the project and list it (in Docker
	// only the mounted directory itself keeps its host mode). No repair:
	// opening a directory to others is the human's choice.
	if fi, err := os.Stat(root); err == nil {
		if uid, ok := separate.FileOwner(fi); ok {
			perm := fi.Mode().Perm()
			if (uid == svc.uid && perm&0o500 != 0o500) || (uid != svc.uid && perm&0o005 != 0o005) {
				out = append(out, projectCheck{line: fmt.Sprintf("project access: %s is %04o: the console account (%s, uid %d) cannot enter it and finds no profile; run: chmod o+rx %s (or setfacl -m u:%d:rx %s)",
					root, perm, svc.name, svc.uid, separate.ShellQuote(root), svc.uid, separate.ShellQuote(root))})
			}
		}
	}
	dir := filepath.Join(root, product.P.ProjectDir())
	fi, err := os.Lstat(dir)
	if err != nil {
		return out
	}
	if fi.Mode()&fs.ModeSymlink != 0 {
		return append(out, projectCheck{line: dir + " is a symbolic link: the console account must own a directory of the project itself; replace it with a directory"})
	}
	if !fi.IsDir() {
		return append(out, projectCheck{line: dir + " is not a directory"})
	}
	uid, ok := separate.FileOwner(fi)
	if !ok {
		return out
	}
	perm := fi.Mode().Perm()
	if uid != svc.uid || perm&0o022 != 0 || perm&0o005 != 0o005 {
		// The uid, not the name: in Docker the console account exists only
		// in the image.
		out = append(out, projectCheck{
			line: fmt.Sprintf("project %s: %04o, owner uid %d; the console account (%s, uid %d) must own it (it writes pii.toml there) and every account read it; run: sudo chown %d %s && sudo chmod 0755 %s",
				product.P.ProjectDir(), perm, uid, svc.name, svc.uid, svc.uid, separate.ShellQuote(dir), separate.ShellQuote(dir)),
			repair: giveDirScript(dir, svc.uid),
		})
	} else {
		out = append(out, projectCheck{ok: true, line: fmt.Sprintf("project %s: %04o, owned by %s", product.P.ProjectDir(), perm, svc.name)})
	}
	for _, name := range []string{"config.toml", "pii.toml"} {
		fi, err := os.Lstat(filepath.Join(dir, name))
		if err != nil || !fi.Mode().IsRegular() {
			continue
		}
		owner, ok := separate.FileOwner(fi)
		if !ok || owner == svc.uid || fi.Mode().Perm()&0o004 != 0 {
			continue
		}
		if links, ok := separate.FileLinks(fi); !ok || links != 1 || owner == 0 {
			// A second name, or root's file: it may be another file linked
			// here, which a chmod as root would expose.
			out = append(out, projectCheck{line: fmt.Sprintf("project %s/%s: not readable by %s, and owned by uid %d with %d links: not repaired; replace it with a file of your own",
				product.P.ProjectDir(), name, svc.name, owner, links)})
			continue
		}
		out = append(out, projectCheck{
			line: fmt.Sprintf("project %s/%s: %04o, not readable by %s; run: chmod 0644 %s",
				product.P.ProjectDir(), name, fi.Mode().Perm(), svc.name, separate.ShellQuote(filepath.Join(dir, name))),
			repair: chmodFileScript(dir, name, svc.uid, owner),
		})
	}
	return out
}

// giveDirScript is the root script that gives dir to uid, mode 0755.
func giveDirScript(dir string, uid int) string {
	return inDirScript(dir, fmt.Sprintf("chown %d . && chmod 0755 .", uid))
}

// chmodFileScript is the root script that makes dir/name readable, 0644.
// chmod follows a symbolic link, and test ! -L then chmod is a race in a
// directory the agent's account can write: the script first checks that
// the directory it entered belongs to uid (the console account; the
// repair of the directory runs before it) and that neither its group nor
// others may write it, nor an ACL entry, so no other account can swap the
// file between the test and the chmod. The file must then be a regular
// file of owner (never root) with a single link: a hard link planted
// before the repair could name any file of the filesystem.
func chmodFileScript(dir, name string, uid, owner int) string {
	n := separate.ShellQuote(name)
	return inDirScript(dir, fmt.Sprintf(`set -- $(ls -ldn .) && test "$3" = %d && case "$1" in ?????w*|????????w*|*+) false;; esac && `+
		`test ! -L %s && set -- $(ls -ln -- %s) && case "$1" in -*) ;; *) false;; esac && test "$2" = 1 && test "$3" = %d && test "$3" != 0 && chmod 0644 %s`,
		uid, n, n, owner, n))
}

// inDirScript runs cmds as root inside dir, in a subshell. The agent's
// account may own dir and the directories above it, and swap any of them
// for a symbolic link between the check and the script: a chown or chmod
// by path would then change whatever the link names. The script enters dir
// once and checks that it is the directory at that path, symbolic links
// resolved, before it changes ".", which no rename can redirect.
func inDirScript(dir, cmds string) string {
	real := dir
	if parent, err := filepath.EvalSymlinks(filepath.Dir(dir)); err == nil {
		real = filepath.Join(parent, filepath.Base(dir))
	}
	return fmt.Sprintf("(cd -P -- %s && test \"$(pwd -P)\" = %s && %s)", separate.ShellQuote(dir), separate.ShellQuote(real), cmds)
}
