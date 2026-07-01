//go:build test

package storage

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestAtomicWrite_FsyncErrorPath_FirstWrite runs under `-tags=test` and
// injects a synthetic fsync failure via SetSyncFuncForTest. On the very
// first write against a missing target it asserts:
//   - AtomicWrite returns a wrapped error carrying the injected sentinel;
//   - the target does NOT exist afterwards (no partial-content publish);
//   - no ".claudecm-tmp-*" siblings are left behind.
func TestAtomicWrite_FsyncErrorPath_FirstWrite(t *testing.T) {
	r, home := atomicHome(t)
	parent := filepath.Join(home, ConfigDirName, ProfilesDirName)
	target := filepath.Join(parent, "first.yaml")

	sentinel := errors.New("synthetic-fsync-failure")
	restore := SetSyncFuncForTest(func(*os.File) error { return sentinel })
	defer restore()

	_, err := AtomicWrite(r, target, []byte("would-never-publish"), AtomicWriteOptions{})
	if err == nil {
		t.Fatal("AtomicWrite with injected fsync error = nil; want error")
	}
	if !errors.Is(err, sentinel) {
		t.Fatalf("err = %v; want errors.Is sentinel", err)
	}
	if !strings.Contains(err.Error(), "fsync temp") {
		t.Fatalf("err = %v; want wrapping to name the fsync stage", err)
	}
	if _, statErr := os.Lstat(target); !os.IsNotExist(statErr) {
		t.Fatalf("target created despite fsync failure (stat=%v)", statErr)
	}
	assertNoTempFiles(t, parent)
}

// TestAtomicWrite_FsyncErrorPath_Overwrite asserts that when an fsync
// failure occurs mid-overwrite, the previously published target is left
// byte-for-byte untouched and no orphan temp files remain.
func TestAtomicWrite_FsyncErrorPath_Overwrite(t *testing.T) {
	r, home := atomicHome(t)
	parent := filepath.Join(home, ConfigDirName, ProfilesDirName)
	target := filepath.Join(parent, "keep.yaml")

	original := []byte("original-payload-do-not-lose")
	if _, err := AtomicWrite(r, target, original, AtomicWriteOptions{}); err != nil {
		t.Fatalf("seed write: %v", err)
	}

	sentinel := errors.New("synthetic-fsync-failure")
	restore := SetSyncFuncForTest(func(*os.File) error { return sentinel })
	defer restore()

	_, err := AtomicWrite(r, target, []byte("attempted-clobber"), AtomicWriteOptions{})
	if err == nil {
		t.Fatal("overwrite with injected fsync error = nil; want error")
	}
	if !errors.Is(err, sentinel) {
		t.Fatalf("err = %v; want errors.Is sentinel", err)
	}

	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("readfile after failed overwrite: %v", err)
	}
	if string(got) != string(original) {
		t.Fatalf("original clobbered: got %q; want %q", got, original)
	}
	assertNoTempFiles(t, parent)
}

// TestAtomicWrite_FsyncErrorPath_MustNotExist confirms the failure path is
// the same under MustNotExist=true: fsync fails BEFORE the link-based
// publish, so no target directory entry ever gets created and no temp
// files leak.
func TestAtomicWrite_FsyncErrorPath_MustNotExist(t *testing.T) {
	r, home := atomicHome(t)
	parent := filepath.Join(home, ConfigDirName, ProfilesDirName)
	target := filepath.Join(parent, "must-not-exist.yaml")

	sentinel := errors.New("synthetic-fsync-failure")
	restore := SetSyncFuncForTest(func(*os.File) error { return sentinel })
	defer restore()

	_, err := AtomicWrite(r, target, []byte("nope"), AtomicWriteOptions{MustNotExist: true})
	if err == nil {
		t.Fatal("MustNotExist + injected fsync error = nil; want error")
	}
	if !errors.Is(err, sentinel) {
		t.Fatalf("err = %v; want errors.Is sentinel", err)
	}
	if _, statErr := os.Lstat(target); !os.IsNotExist(statErr) {
		t.Fatalf("target created despite fsync failure (stat=%v)", statErr)
	}
	assertNoTempFiles(t, parent)
}
