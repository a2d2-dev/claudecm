//go:build !unix

package storage

import "os"

// verifyHomeOwnership is a no-op on non-Unix platforms; the root-owned
// / non-root-euid check has no meaningful analog outside POSIX.
func verifyHomeOwnership(_ string, _ os.FileInfo) error {
	return nil
}
