//go:build test

package storage

import "time"

// backupClock under the `test` build tag is a swap-able var so tests can
// inject a fixed clock and force same-nanosecond collisions between two
// Backup calls. See backup_clock.go for the production wiring and rationale
// for the build-tag split.
var backupClock = func() time.Time { return time.Now() }

// SetBackupClockForTest replaces the backupClock function used by Backup
// for the duration of a test. It returns a restore closure the test must
// defer to put the production function back. Only compiled with
// `-tags=test`.
func SetBackupClockForTest(fn func() time.Time) func() {
	prev := backupClock
	backupClock = fn
	return func() { backupClock = prev }
}
