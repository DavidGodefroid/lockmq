//go:build unix

package separate

import (
	"os"
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
