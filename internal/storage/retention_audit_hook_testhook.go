//go:build test

package storage

import (
	"fmt"
	"os"
)

// auditAppend under the `test` build tag is a swap-able var so the
// audit-failure test can inject a synthetic write error mid-loop. See
// retention_audit_hook.go for the production wiring and rationale for the
// build-tag split (coding-standards rule 12).
var auditAppend = func(path string, line []byte) error {
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("open audit %q: %w", path, err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		_ = f.Close()
		return fmt.Errorf("chmod audit %q: %w", path, err)
	}
	if _, err := f.Write(line); err != nil {
		_ = f.Close()
		return fmt.Errorf("write audit %q: %w", path, err)
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return fmt.Errorf("fsync audit %q: %w", path, err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("close audit %q: %w", path, err)
	}
	return nil
}

// SetAuditAppendForTest replaces the auditAppend function used by Prune
// for the duration of a test. It returns a restore closure the test must
// defer to put the production function back. Only compiled with
// `-tags=test`. Symmetric with SetSyncFuncForTest / SetBackupClockForTest.
func SetAuditAppendForTest(fn func(path string, line []byte) error) func() {
	prev := auditAppend
	auditAppend = fn
	return func() { auditAppend = prev }
}
