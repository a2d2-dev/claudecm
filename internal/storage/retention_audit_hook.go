//go:build !test

package storage

import (
	"fmt"
	"os"
	"path/filepath"
)

// auditAppend is the production wiring for the "append one line to the
// audit log" step of Prune. Under the default (non-test) build, it is a
// plain function that opens the audit-log path with O_APPEND|O_CREATE|
// O_WRONLY at mode 0600, reasserts the mode via Chmod, writes the line,
// fsyncs, and closes. No package-level mutable state, no swap point.
//
// A companion file under `//go:build test` redeclares auditAppend as a
// var and exports SetAuditAppendForTest so the audit-failure test can
// force the append to error after N successful calls (the "audit-log
// write failure aborts further removals" case). See retention.go header
// for rationale on why the seam is build-tag gated (coding-standards
// rule 12: production binaries have no runtime swap point).
//
// Notes:
//   - O_APPEND is intentional. AtomicWrite is designed for whole-file
//     replaces via temp+rename; for an append-only audit log, an O_APPEND
//     write is the correct primitive — each entry is a complete line and
//     is durable after the fsync.
//   - Chmod is re-asserted every call so a wider umask (or a file that
//     was hand-touched at 0644) is tightened back to 0600.
//   - Sync is called on the file before Close. On ext4/xfs the file
//     bytes are durable at that point. However, the FIRST append also
//     creates a new inode entry in the parent directory (the
//     ~/.claudecm/audit.log file itself), and on ext4/xfs that
//     directory entry is not durable until the parent inode is fsynced.
//     EnsureDir does not fsync the parent — it only MkdirAll's. So we
//     fsync the parent dir here every call. It is a single syscall and
//     the cost is dominated by the file fsync above; making it
//     unconditional keeps the code path uniform and closes the
//     first-write durability gap without a branch.
func auditAppend(path string, line []byte) error {
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("open audit %q: %w", path, err)
	}
	// Re-assert the mode. If the file already existed with a wider mode
	// (e.g. an operator hand-touched it at 0644) we tighten it. Rule 10.
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
	// fsync the parent directory. On a fresh audit.log this is what makes
	// the new directory entry (name → inode) durable across a crash. On
	// subsequent appends the entry already exists and this is a no-op
	// on-disk but still a syscall — we accept that cost for a uniform
	// code path. Mirrors AtomicWrite's post-rename fsyncDir step.
	if err := fsyncDir(filepath.Dir(path)); err != nil {
		return fmt.Errorf("fsync audit parent %q: %w", filepath.Dir(path), err)
	}
	return nil
}
