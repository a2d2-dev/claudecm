//go:build !test

package storage

import "time"

// backupClock is the production wiring for the "capture 'now' before the
// backup write" step of Backup. Under the default (non-test) build, it is a
// plain function that returns time.Now() — no package-level mutable state,
// no indirection cost beyond the call boundary.
//
// A companion file under `//go:build test` redeclares backupClock as a var
// and exports SetBackupClockForTest so unit tests can force a fixed clock
// to simulate same-nanosecond collisions between two Backup calls. That
// seam is compiled only when tests are built with `-tags=test` so
// production binaries have no swap point (coding-standards rule 12 on
// package-level mutable state at runtime). The pattern mirrors the fsync
// seam in atomic_syncfunc.go.
func backupClock() time.Time { return time.Now() }
