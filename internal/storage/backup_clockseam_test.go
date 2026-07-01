//go:build test

package storage

import (
	"errors"
	"testing"
	"time"
)

// TestBackup_SameNanosecondCollision runs under `-tags=test` and pins the
// clock to a fixed instant so two Backup calls compute the identical
// timestamp suffix. The second call's AtomicWrite hits MustNotExist and
// surfaces ErrTargetExists — proof that Backup does not silently retry
// with a nudged timestamp when the clock repeats. The non-test build has
// no equivalent seam, so a production binary cannot construct this
// scenario except via a real backwards-clock event.
func TestBackup_SameNanosecondCollision(t *testing.T) {
	r, home := backupHome(t)
	src := writeSource(t, home, ".claude/settings.json", []byte(`{"a":1}`))

	fixed := time.Date(2026, 7, 1, 12, 34, 56, 111111111, time.UTC)
	restore := SetBackupClockForTest(func() time.Time { return fixed })
	defer restore()

	rec1, err := Backup(r, "claudecode", "settings.json", src)
	if err != nil {
		t.Fatalf("first Backup = %v", err)
	}
	if !rec1.Timestamp.Equal(fixed) {
		t.Fatalf("first rec Timestamp = %v; want %v", rec1.Timestamp, fixed)
	}

	// Second call with the same pinned clock hits the same destination.
	rec2, err := Backup(r, "claudecode", "settings.json", src)
	if !errors.Is(err, ErrTargetExists) {
		t.Fatalf("second Backup = %v; want ErrTargetExists (rec=%+v)", err, rec2)
	}
	if rec2 != (BackupRecord{}) {
		t.Fatalf("second Backup receipt = %+v; want zero value on error", rec2)
	}
}
