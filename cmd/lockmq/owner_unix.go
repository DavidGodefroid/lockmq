//go:build unix

package main

import (
	"fmt"
	"os"
	"os/user"
	"strconv"
	"syscall"
)

// fileOwner returns the owner of a file.
func fileOwner(fi os.FileInfo) (uid int, ok bool) {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, false
	}
	return int(st.Uid), true
}

// fileLinks returns the hard link count of a file.
func fileLinks(fi os.FileInfo) (n int, ok bool) {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, false
	}
	return int(st.Nlink), true
}

// becomeAccount makes this process the account name for good: its groups,
// its gid, then its uid (real, effective and saved: no way back to root;
// Go applies them to every thread), and HOME its home.
func becomeAccount(name string) error {
	u, err := user.Lookup(name)
	if err != nil {
		return err
	}
	uid, err := strconv.Atoi(u.Uid)
	if err != nil {
		return err
	}
	gid, err := strconv.Atoi(u.Gid)
	if err != nil {
		return err
	}
	if uid == 0 {
		return fmt.Errorf("account %s is root", name)
	}
	ids, err := u.GroupIds()
	if err != nil {
		return err
	}
	groups := []int{gid}
	for _, s := range ids {
		if g, err := strconv.Atoi(s); err == nil && g != gid {
			groups = append(groups, g)
		}
	}
	if err := syscall.Setgroups(groups); err != nil {
		return fmt.Errorf("setgroups: %w", err)
	}
	if err := syscall.Setgid(gid); err != nil {
		return fmt.Errorf("setgid: %w", err)
	}
	if err := syscall.Setuid(uid); err != nil {
		return fmt.Errorf("setuid: %w", err)
	}
	if os.Getuid() != uid || os.Geteuid() != uid {
		return fmt.Errorf("still uid %d", os.Geteuid())
	}
	return os.Setenv("HOME", u.HomeDir)
}
