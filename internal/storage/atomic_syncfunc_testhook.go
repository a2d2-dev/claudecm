//go:build test

package storage

import "os"

// syncFile under the `test` build tag is a swap-able var so tests can inject
// a synthetic fsync failure via SetSyncFuncForTest. See atomic_syncfunc.go
// for the production wiring and rationale for the build-tag split.
var syncFile = func(f *os.File) error { return f.Sync() }

// SetSyncFuncForTest replaces the syncFile function used by AtomicWrite for
// the duration of a test. It returns a restore closure the test must defer
// to put the production function back. Only compiled with `-tags=test`.
func SetSyncFuncForTest(fn func(*os.File) error) func() {
	prev := syncFile
	syncFile = fn
	return func() { syncFile = prev }
}
