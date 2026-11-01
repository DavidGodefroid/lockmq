//go:build !unix

package main

import (
	"errors"
	"os"
)

// fileOwner is unknown off Unix.
func fileOwner(os.FileInfo) (uid int, ok bool) { return 0, false }

// fileLinks is unknown off Unix.
func fileLinks(os.FileInfo) (n int, ok bool) { return 0, false }

// becomeAccount is Unix only.
func becomeAccount(string) error { return errors.New("no accounts to switch to off Unix") }
