package storage

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// atomicHome builds a Resolver rooted at a fresh t.TempDir and pre-creates
// the .claudecm/profiles/ layout so tests can drop files into the same
// directory writepath will use in production.
func atomicHome(t *testing.T) (*Resolver, string) {
	t.Helper()
	home := t.TempDir()
	r := mustResolver(t, home)
	if err := r.EnsureConfigDir(); err != nil {
		t.Fatalf("EnsureConfigDir: %v", err)
	}
	return r, home
}

// listDir returns all directory entries by name. Used by tests that assert
// "no orphan temp files remained".
func listDir(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("readdir %s: %v", dir, err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

// assertNoTempFiles confirms no "*.claudecm-tmp-*" siblings are left behind
// in dir. This is the on-disk manifestation of the "temp is always cleaned"
// contract.
func assertNoTempFiles(t *testing.T, dir string) {
	t.Helper()
	for _, name := range listDir(t, dir) {
		if strings.Contains(name, ".claudecm-tmp-") {
			t.Fatalf("orphan temp file %q left in %s", name, dir)
		}
	}
}

func TestAtomicWrite_HappyFreshFile(t *testing.T) {
	r, home := atomicHome(t)
	target := filepath.Join(home, ConfigDirName, ProfilesDirName, "foo.yaml")
	data := []byte("hello atomic write\n")

	fp, err := AtomicWrite(r, target, data, AtomicWriteOptions{})
	if err != nil {
		t.Fatalf("AtomicWrite = %v", err)
	}
	// Fingerprint fields must be internally consistent.
	if fp.Size != int64(len(data)) {
		t.Fatalf("Fingerprint.Size = %d; want %d", fp.Size, len(data))
	}
	want := sha256.Sum256(data)
	if fp.SHA256 != hex.EncodeToString(want[:]) {
		t.Fatalf("Fingerprint.SHA256 = %q; want %q", fp.SHA256, hex.EncodeToString(want[:]))
	}
	if fp.ModTime.IsZero() {
		t.Fatalf("Fingerprint.ModTime is zero")
	}
	// File bytes on disk match.
	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("readfile %s: %v", target, err)
	}
	if string(got) != string(data) {
		t.Fatalf("disk bytes = %q; want %q", got, data)
	}
	// File mode is 0600.
	info, err := os.Lstat(target)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0600 {
		t.Fatalf("mode = %o; want 0600", perm)
	}
	assertNoTempFiles(t, filepath.Dir(target))
}

func TestAtomicWrite_OverwriteReplacesAndCleans(t *testing.T) {
	r, home := atomicHome(t)
	target := filepath.Join(home, ConfigDirName, ProfilesDirName, "foo.yaml")
	if _, err := AtomicWrite(r, target, []byte("v1"), AtomicWriteOptions{}); err != nil {
		t.Fatalf("first write: %v", err)
	}
	fp, err := AtomicWrite(r, target, []byte("v2 longer"), AtomicWriteOptions{})
	if err != nil {
		t.Fatalf("second write: %v", err)
	}
	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "v2 longer" {
		t.Fatalf("post-overwrite bytes = %q; want %q", got, "v2 longer")
	}
	sum := sha256.Sum256([]byte("v2 longer"))
	if fp.SHA256 != hex.EncodeToString(sum[:]) {
		t.Fatalf("SHA256 = %q; want %q", fp.SHA256, hex.EncodeToString(sum[:]))
	}
	assertNoTempFiles(t, filepath.Dir(target))
}

func TestAtomicWrite_MustNotExist_RefusesOnExisting(t *testing.T) {
	r, home := atomicHome(t)
	target := filepath.Join(home, ConfigDirName, ProfilesDirName, "foo.yaml")
	if _, err := AtomicWrite(r, target, []byte("first"), AtomicWriteOptions{}); err != nil {
		t.Fatalf("first write: %v", err)
	}
	_, err := AtomicWrite(r, target, []byte("second"), AtomicWriteOptions{MustNotExist: true})
	if err == nil {
		t.Fatal("AtomicWrite MustNotExist over existing = nil; want error")
	}
	if !errors.Is(err, ErrTargetExists) {
		t.Fatalf("err = %v; want errors.Is ErrTargetExists", err)
	}
	// Original file unchanged.
	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "first" {
		t.Fatalf("original changed: got %q; want %q", got, "first")
	}
	assertNoTempFiles(t, filepath.Dir(target))
}

func TestAtomicWrite_MustNotExist_SucceedsWhenAbsent(t *testing.T) {
	r, home := atomicHome(t)
	target := filepath.Join(home, ConfigDirName, ProfilesDirName, "fresh.yaml")
	fp, err := AtomicWrite(r, target, []byte("fresh bytes"), AtomicWriteOptions{MustNotExist: true})
	if err != nil {
		t.Fatalf("AtomicWrite MustNotExist absent = %v; want nil", err)
	}
	if fp.Size != int64(len("fresh bytes")) {
		t.Fatalf("Size = %d; want %d", fp.Size, len("fresh bytes"))
	}
	info, err := os.Lstat(target)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0600 {
		t.Fatalf("mode = %o; want 0600", perm)
	}
}

func TestAtomicWrite_RefusesWhenParentIsSymlinkOutsideHome(t *testing.T) {
	// Build two temp dirs: HOME and an OUTSIDE dir. Then place a symlink
	// inside HOME that points at OUTSIDE. A naive filepath.Clean-based
	// check would let this through because the lexical path is inside
	// HOME; only EvalSymlinks-then-Rel catches the escape.
	r, home := atomicHome(t)
	outside := t.TempDir()

	linkInHome := filepath.Join(home, ConfigDirName, ProfilesDirName, "escape")
	if err := os.Symlink(outside, linkInHome); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	target := filepath.Join(linkInHome, "foo.yaml")

	_, err := AtomicWrite(r, target, []byte("nope"), AtomicWriteOptions{})
	if err == nil {
		t.Fatal("AtomicWrite via symlink out of HOME = nil; want error")
	}
	if !errors.Is(err, ErrOutsideHome) {
		t.Fatalf("err = %v; want errors.Is ErrOutsideHome", err)
	}
	// No file was created either in HOME or in the outside dir.
	if _, statErr := os.Lstat(filepath.Join(outside, "foo.yaml")); !os.IsNotExist(statErr) {
		t.Fatalf("outside/foo.yaml exists after refused write (stat=%v)", statErr)
	}
	assertNoTempFiles(t, outside)
}

func TestAtomicWrite_ParentMustExist(t *testing.T) {
	r, home := atomicHome(t)
	target := filepath.Join(home, ConfigDirName, ProfilesDirName, "missing-subdir", "foo.yaml")
	_, err := AtomicWrite(r, target, []byte("x"), AtomicWriteOptions{})
	if err == nil {
		t.Fatal("AtomicWrite into missing parent = nil; want error")
	}
	// Parent creation is EnsureDir's job; the primitive stays minimal.
	if strings.Contains(err.Error(), "outside HOME") {
		t.Fatalf("err suggests HOME escape but parent is simply missing: %v", err)
	}
}

func TestAtomicWrite_ReturnedSHA256MatchesIndependentHash(t *testing.T) {
	r, home := atomicHome(t)
	target := filepath.Join(home, ConfigDirName, ProfilesDirName, "foo.yaml")
	// Non-trivial payload so any single-buffer bug would surface as a mismatch.
	data := []byte(strings.Repeat("claudecm-atomic ", 1024))
	fp, err := AtomicWrite(r, target, data, AtomicWriteOptions{})
	if err != nil {
		t.Fatalf("AtomicWrite = %v", err)
	}
	sum := sha256.Sum256(data)
	if fp.SHA256 != hex.EncodeToString(sum[:]) {
		t.Fatalf("Fingerprint.SHA256 = %q; want %q", fp.SHA256, hex.EncodeToString(sum[:]))
	}
	// Re-hash the on-disk bytes to prove the disk matches the returned hash.
	onDisk, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	sum2 := sha256.Sum256(onDisk)
	if fp.SHA256 != hex.EncodeToString(sum2[:]) {
		t.Fatalf("disk SHA256 differs from returned Fingerprint.SHA256")
	}
}

// TestAtomicWrite_RenameFailureLeavesOriginalUntouched proves the atomic
// contract under a rename failure: the original bytes on disk are preserved
// and no temp file is left behind. We simulate the failure by making the
// target path a *non-empty directory* — rename(regular file, non-empty dir)
// fails on Linux with EISDIR / ENOTDIR / ENOTEMPTY depending on kernel, but
// always fails. That drives the AtomicWrite error path without needing an
// injection seam.
func TestAtomicWrite_RenameFailureLeavesOriginalUntouched(t *testing.T) {
	r, home := atomicHome(t)
	targetParent := filepath.Join(home, ConfigDirName, ProfilesDirName)
	target := filepath.Join(targetParent, "foo.yaml")
	// Put a non-empty directory in the target path's slot.
	if err := os.MkdirAll(target, 0700); err != nil {
		t.Fatal(err)
	}
	sentinelPath := filepath.Join(target, "sentinel")
	if err := os.WriteFile(sentinelPath, []byte("sentinel-bytes"), 0600); err != nil {
		t.Fatal(err)
	}

	_, err := AtomicWrite(r, target, []byte("would clobber"), AtomicWriteOptions{})
	if err == nil {
		t.Fatal("AtomicWrite over non-empty directory = nil; want error")
	}
	// Directory + sentinel still intact.
	info, statErr := os.Lstat(target)
	if statErr != nil {
		t.Fatalf("target vanished after failed write: %v", statErr)
	}
	if !info.IsDir() {
		t.Fatalf("target should still be a directory; got mode %s", info.Mode())
	}
	got, err := os.ReadFile(sentinelPath)
	if err != nil {
		t.Fatalf("sentinel gone: %v", err)
	}
	if string(got) != "sentinel-bytes" {
		t.Fatalf("sentinel modified: %q", got)
	}
	assertNoTempFiles(t, targetParent)
}

func TestEnsureDir_HappyCreatesWith0700(t *testing.T) {
	r, home := atomicHome(t)
	dir := filepath.Join(home, ConfigDirName, "sub", "nested")
	if err := EnsureDir(r, dir); err != nil {
		t.Fatalf("EnsureDir = %v", err)
	}
	info, err := os.Lstat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !info.IsDir() {
		t.Fatal("not a directory")
	}
	if perm := info.Mode().Perm(); perm != 0700 {
		t.Fatalf("mode = %o; want 0700", perm)
	}
}

func TestEnsureDir_RefusesOutsideHomeViaSymlink(t *testing.T) {
	r, home := atomicHome(t)
	outside := t.TempDir()
	link := filepath.Join(home, ConfigDirName, "escape")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}
	// EnsureDir on a path under the escape-symlink must refuse: the resolved
	// ancestor lives outside HOME.
	err := EnsureDir(r, filepath.Join(link, "nested"))
	if err == nil {
		t.Fatal("EnsureDir under escape symlink = nil; want error")
	}
	if !errors.Is(err, ErrOutsideHome) {
		t.Fatalf("err = %v; want ErrOutsideHome", err)
	}
	// And no dir got created outside HOME.
	if _, statErr := os.Lstat(filepath.Join(outside, "nested")); !os.IsNotExist(statErr) {
		t.Fatalf("outside/nested exists after refused ensure (stat=%v)", statErr)
	}
}

func TestStat_MissingFile(t *testing.T) {
	r, home := atomicHome(t)
	_ = r
	fp, exists, err := Stat(filepath.Join(home, "does-not-exist.yaml"))
	if err != nil {
		t.Fatalf("Stat missing = _, _, %v; want nil error", err)
	}
	if exists {
		t.Fatal("exists = true; want false")
	}
	if fp != (Fingerprint{}) {
		t.Fatalf("Fingerprint = %+v; want zero", fp)
	}
}

func TestStat_MatchesPostWriteFingerprint(t *testing.T) {
	r, home := atomicHome(t)
	target := filepath.Join(home, ConfigDirName, ProfilesDirName, "foo.yaml")
	fpWrite, err := AtomicWrite(r, target, []byte("payload"), AtomicWriteOptions{})
	if err != nil {
		t.Fatal(err)
	}
	fpStat, exists, err := Stat(target)
	if err != nil {
		t.Fatal(err)
	}
	if !exists {
		t.Fatal("exists = false after write")
	}
	if fpWrite.SHA256 != fpStat.SHA256 || fpWrite.Size != fpStat.Size {
		t.Fatalf("post-write vs Stat mismatch: %+v vs %+v", fpWrite, fpStat)
	}
}

// TestAtomicWrite_TempFilenameCollisionResistance sanity-checks that the
// crypto/rand-suffixed temp names are unique enough that two AtomicWrite
// calls in the same test process do not collide. Without this, the O_EXCL
// guard would produce a flaky "temp file exists" error.
func TestAtomicWrite_TempFilenameCollisionResistance(t *testing.T) {
	r, home := atomicHome(t)
	target := filepath.Join(home, ConfigDirName, ProfilesDirName, "foo.yaml")
	for i := 0; i < 25; i++ {
		if _, err := AtomicWrite(r, target, []byte("iter"), AtomicWriteOptions{}); err != nil {
			t.Fatalf("iter %d: %v", i, err)
		}
	}
	assertNoTempFiles(t, filepath.Dir(target))
}

// TestAtomicWrite_FsyncErrorPath documents that we do NOT synthetically
// trigger a real fsync error. Doing so would require either a fake
// filesystem or a package-level hook. Coding standards rule 12 forbids
// package-level mutable state; introducing an unexported hook variable
// solely for tests would create a state footgun disproportionate to the
// value. Instead, the error path is verifiable by inspection of
// AtomicWrite: tmp.Sync() and fsyncDir() failures both go through the
// standard cleanup path (Close, Remove) and return a wrapped error. The
// simulated-crash coverage of the "temp cleaned up, original untouched"
// contract lives in TestAtomicWrite_RenameFailureLeavesOriginalUntouched.
func TestAtomicWrite_FsyncErrorPath(t *testing.T) {
	t.Skip("fsync error injection intentionally not implemented; see test comment")
}
