package storage

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// lockHome mirrors atomicHome/backupHome: fresh HOME, .claudecm/ layout
// created, Resolver returned. Every lock test funnels through here so no
// two tests share disk state.
//
// The tempdir is EvalSymlinks-canonicalized before it is handed to the
// Resolver. On macOS t.TempDir() returns "/var/folders/..." while the real
// path is "/private/var/folders/..." (/var is a symlink to /private/var).
// Production code paths (checkUnderHome) resolve symlinks, so a sidecar
// path returned by Acquire lives under "/private/var/..."; the test-side
// expectedSidecar composition would otherwise stay under "/var/..." and
// the assertion would false-fail on macOS while passing on Linux. This is
// a test-setup concern only: the production symlink guard is unchanged.
func lockHome(t *testing.T) (*Resolver, string) {
	t.Helper()
	home := t.TempDir()
	resolved, err := filepath.EvalSymlinks(home)
	if err != nil {
		t.Fatalf("EvalSymlinks(%q): %v", home, err)
	}
	r := mustResolver(t, resolved)
	if err := Bootstrap(r); err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	return r, resolved
}

func expectedSidecar(home, rel string) string {
	return filepath.Join(home, rel+lockSidecarSuffix)
}

func TestAcquire_HappyReleaseReAcquire(t *testing.T) {
	r, home := lockHome(t)
	target := "profiles/foo.yaml"

	h, err := Acquire(r, target, LockOptions{})
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	if h == nil {
		t.Fatal("Acquire returned nil handle")
	}
	sidecar := expectedSidecar(home, target)
	if h.Path() != sidecar {
		t.Fatalf("Handle.Path = %q; want %q", h.Path(), sidecar)
	}

	// Sidecar file exists at expected path with mode 0600.
	info, err := os.Lstat(sidecar)
	if err != nil {
		t.Fatalf("Lstat sidecar: %v", err)
	}
	if info.Mode().Perm() != 0600 {
		t.Fatalf("sidecar mode = %v; want 0600", info.Mode().Perm())
	}
	if !info.Mode().IsRegular() {
		t.Fatalf("sidecar is not a regular file (mode=%v)", info.Mode())
	}

	// Release drops the lock.
	if err := h.Release(); err != nil {
		t.Fatalf("Release: %v", err)
	}
	// Idempotent Release.
	if err := h.Release(); err != nil {
		t.Fatalf("second Release should be no-op, got: %v", err)
	}

	// Sequential re-acquire on the same target must succeed.
	h2, err := Acquire(r, target, LockOptions{})
	if err != nil {
		t.Fatalf("re-Acquire: %v", err)
	}
	if err := h2.Release(); err != nil {
		t.Fatalf("re-Release: %v", err)
	}
}

func TestAcquire_ContentionTimeout(t *testing.T) {
	r, _ := lockHome(t)
	target := "profiles/contended.yaml"

	h1, err := Acquire(r, target, LockOptions{Timeout: 500 * time.Millisecond})
	if err != nil {
		t.Fatalf("holder Acquire: %v", err)
	}
	defer func() { _ = h1.Release() }()

	start := time.Now()
	_, err = Acquire(r, target, LockOptions{Timeout: 100 * time.Millisecond})
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("second Acquire should have timed out")
	}
	if !errors.Is(err, ErrLockTimeout) {
		t.Fatalf("err = %v; want errors.Is ErrLockTimeout", err)
	}
	// Timeout should land in roughly [100, 250] ms — 25 ms retry cadence
	// plus scheduler jitter under load.
	if elapsed < 90*time.Millisecond {
		t.Fatalf("returned too early: %v", elapsed)
	}
	if elapsed > 500*time.Millisecond {
		t.Fatalf("returned too late: %v", elapsed)
	}
}

func TestAcquire_ContentionReleaseUnblocks(t *testing.T) {
	r, _ := lockHome(t)
	target := "profiles/rendezvous.yaml"

	h1, err := Acquire(r, target, LockOptions{})
	if err != nil {
		t.Fatalf("holder Acquire: %v", err)
	}

	acquired := make(chan time.Duration, 1)
	errCh := make(chan error, 1)
	start := time.Now()
	go func() {
		h2, err := Acquire(r, target, LockOptions{Timeout: 2 * time.Second})
		if err != nil {
			errCh <- err
			return
		}
		acquired <- time.Since(start)
		_ = h2.Release()
	}()

	// Give the goroutine a moment to enter TryLockContext, then release.
	time.Sleep(50 * time.Millisecond)
	if err := h1.Release(); err != nil {
		t.Fatalf("holder Release: %v", err)
	}

	select {
	case elapsed := <-acquired:
		// gofrs/flock polls at 25 ms, so waiter observes release within
		// roughly one retry after our Release call.
		if elapsed > 500*time.Millisecond {
			t.Fatalf("waiter took too long after release: %v", elapsed)
		}
	case err := <-errCh:
		t.Fatalf("waiter Acquire failed: %v", err)
	case <-time.After(3 * time.Second):
		t.Fatal("waiter never acquired after release")
	}
}

func TestAcquire_SymlinkEscapeParentDir(t *testing.T) {
	r, home := lockHome(t)
	// Build an outside-HOME dir and a HOME-relative symlink pointing at it.
	outside := t.TempDir()
	escapeLink := filepath.Join(home, "escape")
	if err := os.Symlink(outside, escapeLink); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	_, err := Acquire(r, "escape/foo.yaml", LockOptions{})
	if err == nil {
		t.Fatal("Acquire should refuse when parent resolves outside HOME")
	}
	if !errors.Is(err, ErrOutsideHome) {
		t.Fatalf("err = %v; want errors.Is ErrOutsideHome", err)
	}
	// No sidecar file should have leaked into HOME or the outside dir.
	if _, statErr := os.Lstat(filepath.Join(outside, "foo.yaml"+lockSidecarSuffix)); !os.IsNotExist(statErr) {
		t.Fatalf("unexpected sidecar created outside HOME: %v", statErr)
	}
}

func TestAcquire_SymlinkEscapeSidecar(t *testing.T) {
	r, home := lockHome(t)
	targetRel := "profiles/settings.json"
	targetAbs := filepath.Join(home, targetRel)
	if err := os.MkdirAll(filepath.Dir(targetAbs), 0700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	// Real target file — matches the "create a real file" step in the AC.
	if err := os.WriteFile(targetAbs, []byte("real"), 0600); err != nil {
		t.Fatalf("write target: %v", err)
	}
	// Sidecar planted as a symlink pointing outside HOME.
	outside := t.TempDir()
	attackerPath := filepath.Join(outside, "attacker")
	if err := os.WriteFile(attackerPath, []byte(""), 0600); err != nil {
		t.Fatalf("write attacker file: %v", err)
	}
	sidecar := targetAbs + lockSidecarSuffix
	if err := os.Symlink(attackerPath, sidecar); err != nil {
		t.Fatalf("symlink sidecar: %v", err)
	}

	_, err := Acquire(r, targetRel, LockOptions{})
	if err == nil {
		t.Fatal("Acquire should refuse when sidecar is a symlink escaping HOME")
	}
	if !errors.Is(err, ErrOutsideHome) {
		t.Fatalf("err = %v; want errors.Is ErrOutsideHome", err)
	}
	// Original target must be byte-untouched.
	got, err := os.ReadFile(targetAbs)
	if err != nil {
		t.Fatalf("re-read target: %v", err)
	}
	if string(got) != "real" {
		t.Fatalf("target mutated to %q; want %q", got, "real")
	}
}

func TestAcquire_PathSafety(t *testing.T) {
	r, _ := lockHome(t)
	cases := []struct {
		name   string
		target string
	}{
		{"empty", ""},
		{"absolute", "/etc/passwd"},
		{"parent-escape", "../evil"},
		{"nested-parent-escape", "profiles/../../evil"},
		{"dot", "."},
		{"nul", "foo\x00bar"},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			if _, err := Acquire(r, tc.target, LockOptions{}); err == nil {
				t.Fatalf("Acquire(%q) succeeded; want refusal", tc.target)
			}
		})
	}
}

func TestAcquire_NilResolver(t *testing.T) {
	if _, err := Acquire(nil, "profiles/foo.yaml", LockOptions{}); err == nil {
		t.Fatal("Acquire with nil resolver should be refused")
	}
}

func TestWithLock_HappyPath(t *testing.T) {
	r, _ := lockHome(t)
	target := "profiles/withlock.yaml"

	ran := 0
	err := WithLock(r, target, LockOptions{}, func() error {
		ran++
		return nil
	})
	if err != nil {
		t.Fatalf("WithLock: %v", err)
	}
	if ran != 1 {
		t.Fatalf("fn ran %d times; want 1", ran)
	}

	// Lock must be released — re-acquire immediately.
	h, err := Acquire(r, target, LockOptions{Timeout: 200 * time.Millisecond})
	if err != nil {
		t.Fatalf("re-Acquire after WithLock: %v", err)
	}
	if err := h.Release(); err != nil {
		t.Fatalf("Release: %v", err)
	}
}

func TestWithLock_FnErrorReleasesLock(t *testing.T) {
	r, _ := lockHome(t)
	target := "profiles/withlock-err.yaml"

	fnErr := errors.New("boom")
	err := WithLock(r, target, LockOptions{}, func() error { return fnErr })
	if !errors.Is(err, fnErr) {
		t.Fatalf("WithLock returned %v; want %v", err, fnErr)
	}
	// Even though fn errored, WithLock must have released the lock.
	h, err := Acquire(r, target, LockOptions{Timeout: 200 * time.Millisecond})
	if err != nil {
		t.Fatalf("Acquire after failing WithLock: %v", err)
	}
	if err := h.Release(); err != nil {
		t.Fatalf("Release: %v", err)
	}
}

func TestWithLock_NilFn(t *testing.T) {
	r, _ := lockHome(t)
	if err := WithLock(r, "profiles/foo.yaml", LockOptions{}, nil); err == nil {
		t.Fatal("WithLock with nil fn should be refused")
	}
}

// TestWithLock_PanicReleasesLock covers AC3 (panic safety): if fn panics,
// the deferred Release must still fire, so a subsequent Acquire on the
// same target succeeds quickly instead of timing out. We also assert the
// panic propagates (WithLock does NOT swallow it) and that the sidecar
// path resolved by the second Acquire matches — proving no path
// collision or renamed sidecar.
func TestWithLock_PanicReleasesLock(t *testing.T) {
	r, home := lockHome(t)
	target := "profiles/withlock-panic.yaml"
	panicMsg := "boom-in-fn"

	func() {
		defer func() {
			rec := recover()
			if rec == nil {
				t.Fatal("expected panic to propagate out of WithLock")
			}
			if got, ok := rec.(string); !ok || got != panicMsg {
				t.Fatalf("recovered %v; want %q", rec, panicMsg)
			}
		}()
		_ = WithLock(r, target, LockOptions{}, func() error {
			panic(panicMsg)
		})
	}()

	// If Release fired mid-panic, this Acquire returns almost immediately.
	// If it didn't, we time out at 500 ms.
	h, err := Acquire(r, target, LockOptions{Timeout: 500 * time.Millisecond})
	if err != nil {
		t.Fatalf("Acquire after panicking WithLock: %v", err)
	}
	if got, want := h.Path(), expectedSidecar(home, target); got != want {
		t.Fatalf("sidecar path after re-Acquire = %q; want %q", got, want)
	}
	if err := h.Release(); err != nil {
		t.Fatalf("Release: %v", err)
	}
}

// TestWithLock_ConcurrencyStress: 8 goroutines increment a shared counter
// under WithLock on the same target. Serialization is the whole point — if
// the lock ever fails to serialize, the counter drops below 8 (race on the
// non-atomic read-modify-write) or the sequence bookkeeping detects overlap.
func TestWithLock_ConcurrencyStress(t *testing.T) {
	r, _ := lockHome(t)
	target := "profiles/stress.yaml"

	const N = 8
	var (
		counter    int
		inCritical int32
		wg         sync.WaitGroup
		errs       = make(chan error, N)
	)
	wg.Add(N)
	for i := 0; i < N; i++ {
		go func() {
			defer wg.Done()
			err := WithLock(r, target, LockOptions{Timeout: 3 * time.Second}, func() error {
				if !atomic.CompareAndSwapInt32(&inCritical, 0, 1) {
					return fmt.Errorf("critical section entered concurrently")
				}
				// Non-atomic RMW deliberately: proves the lock serializes.
				c := counter
				time.Sleep(2 * time.Millisecond)
				counter = c + 1
				atomic.StoreInt32(&inCritical, 0)
				return nil
			})
			if err != nil {
				errs <- err
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("goroutine error: %v", err)
	}
	if counter != N {
		t.Fatalf("counter = %d; want %d — lock failed to serialize", counter, N)
	}
}

// TestAcquire_TimeoutDefault verifies LockOptions{Timeout: 0} uses
// DefaultLockTimeout: a contender that would have timed out under a very
// short deadline instead waits long enough to observe a release ~600 ms
// later. If the default were e.g. 100 ms, this test would fail with
// ErrLockTimeout.
func TestAcquire_TimeoutDefault(t *testing.T) {
	if DefaultLockTimeout < time.Second {
		t.Fatalf("DefaultLockTimeout = %v; this test presumes >= 1s", DefaultLockTimeout)
	}
	r, _ := lockHome(t)
	target := "profiles/default-timeout.yaml"

	h1, err := Acquire(r, target, LockOptions{})
	if err != nil {
		t.Fatalf("holder Acquire: %v", err)
	}
	releaseAfter := 600 * time.Millisecond
	go func() {
		time.Sleep(releaseAfter)
		_ = h1.Release()
	}()

	start := time.Now()
	// Zero Timeout → should use DefaultLockTimeout (5s), long enough to
	// outlast the 600 ms release.
	h2, err := Acquire(r, target, LockOptions{Timeout: 0})
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("waiter Acquire with default timeout: %v", err)
	}
	defer func() { _ = h2.Release() }()
	if elapsed < releaseAfter-100*time.Millisecond {
		t.Fatalf("waiter returned too early: %v", elapsed)
	}
	if elapsed > 2*time.Second {
		t.Fatalf("waiter returned too late: %v (default timeout may be misconfigured)", elapsed)
	}
}

// TestAcquire_SidecarPersistsAcrossReleases confirms the sidecar file is
// NOT removed on Release — standard flock hygiene per the file-level
// comment. Removing on Release would race with concurrent Acquire.
func TestAcquire_SidecarPersistsAcrossReleases(t *testing.T) {
	r, home := lockHome(t)
	target := "profiles/persistent.yaml"
	sidecar := expectedSidecar(home, target)

	h, err := Acquire(r, target, LockOptions{})
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	if err := h.Release(); err != nil {
		t.Fatalf("Release: %v", err)
	}
	if _, err := os.Lstat(sidecar); err != nil {
		t.Fatalf("sidecar removed on Release (should persist): %v", err)
	}
}
