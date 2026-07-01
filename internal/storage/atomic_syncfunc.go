//go:build !test

package storage

import "os"

// syncFile is the production wiring for the "fsync the temp file" step of
// AtomicWrite. Under the default (non-test) build, it is a plain function
// that delegates directly to (*os.File).Sync — no package-level mutable
// state, no indirection cost beyond the call boundary.
//
// A companion file under `//go:build test` redeclares syncFile as a var and
// exports SetSyncFuncForTest so unit tests can force a synthetic fsync
// failure. That seam is compiled only when tests are built with `-tags=test`
// so production binaries have no swap point (coding-standards rule 12 on
// package-level mutable state at runtime).
func syncFile(f *os.File) error { return f.Sync() }
