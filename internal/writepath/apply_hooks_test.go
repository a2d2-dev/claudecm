//go:build test

// apply_hooks_test.go exercises the FR-5 step-9 concurrent-edit drift
// detection (Story E2-S4) by injecting a mutation between the step-3
// read (which captured PreFingerprint) and the step-9 drift-check
// Stat. The mutation runs on the same goroutine as Apply — it fires
// synchronously from postReadHookForTest — so no goroutine coordination
// is needed. The hook itself is a build-tag seam declared in
// apply_hooks_testhook.go under `//go:build test`; production binaries
// contain the no-op function from apply_hooks.go and cannot execute
// these tests. Coding-standards rule 12 (no runtime mutable package
// state in production) is preserved because SetPostReadHookForTest
// is unreachable from a non-test build.

package writepath

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/a2d2-dev/claudecm/internal/storage"
)

// runWithHook installs fn as the between-read-and-drift-check hook for
// the duration of a test and defers the restore closure. Keeps the
// test bodies terse and forces every user to go through the setter so
// a future refactor can add e.g. a call counter without touching each
// test site.
func runWithHook(t *testing.T, fn func()) {
	t.Helper()
	restore := SetPostReadHookForTest(fn)
	t.Cleanup(restore)
}

// TestApply_DriftDetection_SizeChanged pins the size-drift branch:
// external write grows the file after our step-3 read. Apply must
// return ErrConcurrentEdit, must NOT overwrite the drifted bytes on
// disk, and must leave the step-7 backup intact with the pre-drift
// content for audit. AC row: FR-5 step 9, first bullet.
func TestApply_DriftDetection_SizeChanged(t *testing.T) {
	r, home := newTestHome(t)
	target := filepath.Join(home, "tool", "config.json")
	ensureParent(t, target)
	original := []byte(`{"a":1}`) // len=7
	if err := os.WriteFile(target, original, 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}
	concurrent := []byte(`{"a":1,"b":2}`) // len=13, larger

	runWithHook(t, func() {
		// External writer replaces the file with a longer document
		// AFTER our step-3 read + PreFingerprint capture and AFTER
		// step-7 backup captured the original bytes. The subsequent
		// drift-check Stat must catch the size change.
		if err := os.WriteFile(target, concurrent, 0o600); err != nil {
			t.Fatalf("hook: %v", err)
		}
	})

	plan := WritePlan{
		Tool:       "tool",
		Target:     target,
		NewContent: []byte(`{"a":2}`),
		Parser:     jsonParser,
		OwnedKeys:  []string{"a"},
	}
	report, err := Apply(context.Background(), r, plan)
	if !errors.Is(err, ErrConcurrentEdit) {
		t.Fatalf("err = %v; want wraps ErrConcurrentEdit", err)
	}
	// The concurrent writer's bytes must remain on disk — we did NOT
	// stomp them with our stale-derived NewContent.
	got, _ := os.ReadFile(target)
	if !reflect.DeepEqual(got, concurrent) {
		t.Fatalf("on-disk bytes = %q; want concurrent %q (AtomicWrite must not have fired)", got, concurrent)
	}
	// The backup captured the pre-drift original bytes and stays on
	// disk (retention will reap it eventually).
	if report.Backup.BackupPath == "" {
		t.Fatalf("Backup.BackupPath empty; want populated for audit")
	}
	backupBytes, berr := os.ReadFile(report.Backup.BackupPath)
	if berr != nil {
		t.Fatalf("read backup: %v", berr)
	}
	if !reflect.DeepEqual(backupBytes, original) {
		t.Fatalf("backup bytes = %q; want %q (backup must reflect pre-drift state)", backupBytes, original)
	}
	// PostFingerprint stays zero — we did not write.
	if (report.PostFingerprint != storage.Fingerprint{}) {
		t.Fatalf("PostFingerprint = %+v; want zero on drift abort", report.PostFingerprint)
	}
	if report.RolledBack {
		t.Fatalf("RolledBack = true; want false on drift abort (nothing to roll back)")
	}
}

// TestApply_DriftDetection_ShaChanged pins the SHA-drift branch:
// external write replaces the content with same-length bytes. Size
// stays equal so the size check passes; SHA256 must catch it. Note
// that os.WriteFile also updates mtime, so this test really exercises
// "sha256 OR mtime differs" — both would independently trigger the
// abort. The AC is just that ErrConcurrentEdit fires.
func TestApply_DriftDetection_ShaChanged(t *testing.T) {
	r, home := newTestHome(t)
	target := filepath.Join(home, "tool", "config.json")
	ensureParent(t, target)
	original := []byte(`{"a":1}`) // len=7
	if err := os.WriteFile(target, original, 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}
	concurrent := []byte(`{"a":9}`) // len=7, same size, different sha

	runWithHook(t, func() {
		if err := os.WriteFile(target, concurrent, 0o600); err != nil {
			t.Fatalf("hook: %v", err)
		}
	})

	plan := WritePlan{
		Tool:       "tool",
		Target:     target,
		NewContent: []byte(`{"a":2}`),
		Parser:     jsonParser,
		OwnedKeys:  []string{"a"},
	}
	report, err := Apply(context.Background(), r, plan)
	if !errors.Is(err, ErrConcurrentEdit) {
		t.Fatalf("err = %v; want wraps ErrConcurrentEdit", err)
	}
	got, _ := os.ReadFile(target)
	if !reflect.DeepEqual(got, concurrent) {
		t.Fatalf("on-disk bytes = %q; want concurrent %q", got, concurrent)
	}
	if report.Backup.BackupPath == "" {
		t.Fatalf("Backup.BackupPath empty; want populated")
	}
}

// TestApply_DriftDetection_ModTimeChangedOnly pins the strict-policy
// claim from the apply.go header: even a ModTime-only drift (touch(1)
// with no content change) aborts. This is intentionally strict —
// users hitting a spurious ModTime bump can rerun. Filesystems whose
// ModTime granularity truncates below what Chtimes wrote are
// tolerated by comparing time.Time via .Equal(), which normalizes
// wall-clock representation.
func TestApply_DriftDetection_ModTimeChangedOnly(t *testing.T) {
	r, home := newTestHome(t)
	target := filepath.Join(home, "tool", "config.json")
	ensureParent(t, target)
	original := []byte(`{"a":1}`)
	if err := os.WriteFile(target, original, 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}

	runWithHook(t, func() {
		// Push mtime forward by a whole second so any FS mtime
		// granularity coarser than nanosecond still sees a difference.
		// Atime bumped along for symmetry; storage.Stat only compares
		// mtime today but that's not this test's contract.
		bumped := time.Now().Add(1 * time.Second)
		if err := os.Chtimes(target, bumped, bumped); err != nil {
			t.Fatalf("hook chtimes: %v", err)
		}
	})

	plan := WritePlan{
		Tool:       "tool",
		Target:     target,
		NewContent: []byte(`{"a":2}`),
		Parser:     jsonParser,
		OwnedKeys:  []string{"a"},
	}
	report, err := Apply(context.Background(), r, plan)
	if !errors.Is(err, ErrConcurrentEdit) {
		t.Fatalf("err = %v; want wraps ErrConcurrentEdit (ModTime-only drift is still drift)", err)
	}
	// Content unchanged on disk (touch only bumped mtime).
	got, _ := os.ReadFile(target)
	if !reflect.DeepEqual(got, original) {
		t.Fatalf("on-disk bytes = %q; want %q (touch must not change content)", got, original)
	}
	if report.Backup.BackupPath == "" {
		t.Fatalf("Backup.BackupPath empty; want populated")
	}
}

// TestApply_DriftDetection_ExistenceAppearedUnderLock pins the
// existence-drift branch (NFR-C3 first-write race): PreFingerprint
// said the target did not exist, but a concurrent creator materialized
// it under our lock. Apply must refuse via ErrConcurrentEdit rather
// than atomically writing over the new file.
func TestApply_DriftDetection_ExistenceAppearedUnderLock(t *testing.T) {
	r, home := newTestHome(t)
	target := filepath.Join(home, "tool", "config.json")
	ensureParent(t, target)
	// No seed — file must not exist at step 3.

	concurrent := []byte(`{"stolen":true}`)
	runWithHook(t, func() {
		if err := os.WriteFile(target, concurrent, 0o600); err != nil {
			t.Fatalf("hook: %v", err)
		}
	})

	plan := WritePlan{
		Tool:       "tool",
		Target:     target,
		NewContent: []byte(`{"model":"opus"}`),
		Parser:     jsonParser,
		OwnedKeys:  []string{"model"},
	}
	report, err := Apply(context.Background(), r, plan)
	if !errors.Is(err, ErrConcurrentEdit) {
		t.Fatalf("err = %v; want wraps ErrConcurrentEdit", err)
	}
	// The concurrent creator's bytes must remain on disk.
	got, _ := os.ReadFile(target)
	if !reflect.DeepEqual(got, concurrent) {
		t.Fatalf("on-disk bytes = %q; want concurrent %q (must not overwrite)", got, concurrent)
	}
	// First-write drift: no backup was taken (Backup returned
	// ErrNothingToBackup in step 7 before the hook fired).
	if (report.Backup != storage.BackupRecord{}) {
		t.Fatalf("Backup = %+v; want zero on first-write drift (nothing to back up)", report.Backup)
	}
	if (report.PostFingerprint != storage.Fingerprint{}) {
		t.Fatalf("PostFingerprint = %+v; want zero on drift abort", report.PostFingerprint)
	}
}

// TestApply_DriftDetection_FirstWriteNoCheck pins the "no drift check
// when neither side exists" carve-out: PreFingerprint said !exists AND
// the drift-check Stat also says !exists → step 9 short-circuits and
// step 7 AtomicWrite creates the file normally. This is the legitimate
// first-write path and must NOT be spuriously refused.
func TestApply_DriftDetection_FirstWriteNoCheck(t *testing.T) {
	r, home := newTestHome(t)
	target := filepath.Join(home, "tool", "config.json")
	ensureParent(t, target)
	// No seed. Hook is intentionally a no-op — nothing mutates between
	// read and drift-check Stat, so both sides remain !exists.
	runWithHook(t, func() { /* no-op */ })

	newBytes := []byte(`{"model":"opus"}`)
	plan := WritePlan{
		Tool:       "tool",
		Target:     target,
		NewContent: newBytes,
		Parser:     jsonParser,
		OwnedKeys:  []string{"model"},
	}
	report, err := Apply(context.Background(), r, plan)
	if err != nil {
		t.Fatalf("Apply err = %v; want nil on legitimate first write", err)
	}
	if errors.Is(err, ErrConcurrentEdit) {
		t.Fatalf("err wraps ErrConcurrentEdit; first-write must not drift-abort when both sides !exists")
	}
	got, _ := os.ReadFile(target)
	if !reflect.DeepEqual(got, newBytes) {
		t.Fatalf("bytes = %q; want %q", got, newBytes)
	}
	if report.PostFingerprint.SHA256 == "" {
		t.Fatalf("PostFingerprint.SHA256 empty; want hash of new bytes on successful first write")
	}
}
