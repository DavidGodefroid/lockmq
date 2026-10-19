//go:build !unix

package separate

import "os"

// fileOwner is unknown off Unix.
func fileOwner(os.FileInfo) (uid int, ok bool) { return 0, false }
