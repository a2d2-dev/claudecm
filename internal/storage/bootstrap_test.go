package storage

import (
	"os"
	"path/filepath"
	"testing"
)

// assertMode is a package-local mode assertion helper. It is NOT exported —
// tests in this package share it directly. Kept in this file (rather than a
// new testutil_test.go) because bootstrap tests are its sole caller today;
// promote if a second caller appears.
func assertMode(t *testing.T, path string, want os.FileMode) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	if got := info.Mode().Perm(); got != want.Perm() {
		t.Fatalf("%s mode = %#o; want %#o", path, got, want.Perm())
	}
}

func TestBootstrap_FreshHome_CreatesDirs(t *testing.T) {
	home := t.TempDir()
	r := mustResolver(t, home)

	if err := Bootstrap(r); err != nil {
		t.Fatalf("Bootstrap = %v; want nil", err)
	}

	// Every directory the layout requires must exist at 0700.
	for _, sub := range []string{
		ConfigDirName,
		filepath.Join(ConfigDirName, ProfilesDirName),
		filepath.Join(ConfigDirName, BackupsDirName),
	} {
		p := filepath.Join(home, sub)
		info, err := os.Stat(p)
		if err != nil {
			t.Fatalf("stat %s: %v", sub, err)
		}
		if !info.IsDir() {
			t.Fatalf("%s is not a directory", sub)
		}
		assertMode(t, p, 0700)
	}

	// state.yaml and audit.log must NOT exist yet — they are lazily created
	// by SaveState / retention respectively, matching the E1-S7 story's
	// "no silent file creation" posture.
	statePath, err := r.StatePath()
	if err != nil {
		t.Fatalf("StatePath = _, %v", err)
	}
	if _, err := os.Stat(statePath); !os.IsNotExist(err) {
		t.Fatalf("state.yaml exists after Bootstrap: stat err = %v; want ErrNotExist", err)
	}
	if _, err := os.Stat(r.AuditLogPath()); !os.IsNotExist(err) {
		t.Fatalf("audit.log exists after Bootstrap: stat err = %v; want ErrNotExist", err)
	}
}

func TestBootstrap_Idempotent(t *testing.T) {
	home := t.TempDir()
	r := mustResolver(t, home)

	if err := Bootstrap(r); err != nil {
		t.Fatalf("Bootstrap #1 = %v; want nil", err)
	}
	// Snapshot mtimes and modes before the second call so we can prove the
	// second call did not chmod, did not mkdir, and did not create anything
	// new.
	type snap struct {
		mode  os.FileMode
		mtime int64
	}
	before := map[string]snap{}
	dirs := []string{r.ConfigDir(), r.ProfilesDir(), r.BackupsRoot()}
	for _, d := range dirs {
		info, err := os.Stat(d)
		if err != nil {
			t.Fatalf("stat %s: %v", d, err)
		}
		before[d] = snap{info.Mode().Perm(), info.ModTime().UnixNano()}
	}

	if err := Bootstrap(r); err != nil {
		t.Fatalf("Bootstrap #2 = %v; want nil", err)
	}

	for _, d := range dirs {
		info, err := os.Stat(d)
		if err != nil {
			t.Fatalf("stat %s: %v", d, err)
		}
		if got := info.Mode().Perm(); got != before[d].mode {
			t.Fatalf("%s mode drifted: before=%#o after=%#o", d, before[d].mode, got)
		}
	}

	// Confirm the ConfigDir contents did not grow — no state.yaml, no
	// audit.log magically appeared on the second call.
	entries, err := os.ReadDir(r.ConfigDir())
	if err != nil {
		t.Fatalf("readdir %s: %v", r.ConfigDir(), err)
	}
	got := map[string]bool{}
	for _, e := range entries {
		got[e.Name()] = true
	}
	for _, unexpected := range []string{StateFileName, AuditLogFileName} {
		if got[unexpected] {
			t.Fatalf("Bootstrap created %s; want no lazy-file creation", unexpected)
		}
	}
	// The three subdirs are the only entries we expect. profiles/ and
	// backups/ are direct children of ConfigDir; ConfigDir itself is not.
	for _, expected := range []string{ProfilesDirName, BackupsDirName} {
		if !got[expected] {
			t.Fatalf("Bootstrap dropped %s on second call", expected)
		}
	}
}

// TestBootstrap_ExistingDirsWithLooseMode covers the real threat vector: a
// user whose umask is 022 first-ran claudecm before Bootstrap enforced 0700,
// or manually chmod'd the config dir. Bootstrap must tighten the mode down
// to 0700 rather than silently accept the looser mode — accepting a looser
// mode would violate NFR-S4.
func TestBootstrap_ExistingDirsWithLooseMode(t *testing.T) {
	home := t.TempDir()
	r := mustResolver(t, home)

	// Pre-create the layout at 0755 (what os.MkdirAll would produce under
	// umask 022 without our chmod-down enforcement).
	for _, d := range []string{r.ConfigDir(), r.ProfilesDir(), r.BackupsRoot()} {
		if err := os.MkdirAll(d, 0755); err != nil {
			t.Fatalf("pre-mkdir %s: %v", d, err)
		}
		// os.MkdirAll respects umask, so force-set 0755 explicitly to make
		// the test's premise reliable regardless of the runner's umask.
		if err := os.Chmod(d, 0755); err != nil {
			t.Fatalf("pre-chmod %s: %v", d, err)
		}
	}

	if err := Bootstrap(r); err != nil {
		t.Fatalf("Bootstrap = %v; want nil", err)
	}

	assertMode(t, r.ConfigDir(), 0700)
	assertMode(t, r.ProfilesDir(), 0700)
	assertMode(t, r.BackupsRoot(), 0700)
}

// TestBootstrap_NilResolver documents the defensive nil guard. Bootstrap
// never dereferences a nil Resolver; the guard is a fast, clear failure
// for a coding mistake at the cmd/* wiring layer.
func TestBootstrap_NilResolver(t *testing.T) {
	if err := Bootstrap(nil); err == nil {
		t.Fatal("Bootstrap(nil) = nil; want error")
	}
}

// TestBootstrap_RefusesOutsideHome documents where the HOME-bounds check
// lives. Bootstrap does not re-validate HOME — the Resolver constructor
// already refuses HOME=/, missing dirs, non-directory targets, and (when
// running non-root) root-owned HOME. This test proves that the refusal
// happens BEFORE Bootstrap is even reachable, so no filesystem side effects
// can leak from a bad HOME. See paths_test.go TestResolver_Refuses* for the
// full matrix.
func TestBootstrap_RefusesOutsideHome(t *testing.T) {
	if _, err := NewResolverWithHome("/"); err == nil {
		t.Fatal(`NewResolverWithHome("/") = nil; want refuse before Bootstrap`)
	}
	if _, err := NewResolverWithHome(""); err == nil {
		t.Fatal(`NewResolverWithHome("") = nil; want refuse before Bootstrap`)
	}
	if _, err := NewResolverWithHome("relative"); err == nil {
		t.Fatal(`NewResolverWithHome("relative") = nil; want refuse before Bootstrap`)
	}
}
