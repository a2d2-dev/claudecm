package storage

// lock.go is the flock primitive that the FR-5 write-path (writepath.Apply)
// and the FR-16 two-phase commit will call. This file is deliberately a pure
// primitive: no SaveProfile/SaveState wiring, no adapter integration, no
// package-level mutable state (coding-standards rule 12). Later stories under
// E7 tie it into the write-path.
//
// Design choices (per docs/plan/stories/E1-S6.md and architecture §4 step 1):
//
//   - The lock is taken on a sidecar file "<target>.claudecm-lock" colocated
//     with the target. Rationale: AtomicWrite renames a fresh temp file over
//     the target, so any flock held on the target's fd would be dropped by
//     the rename. The sidecar shares the directory (and therefore the
//     rename+fsync ordering) but is never itself renamed away.
//
//   - Sidecar files are created 0600 and are never removed on Release. This
//     mirrors standard flock hygiene: leaving the empty file in place is
//     cheap and avoids a race where a Release-time unlink collides with a
//     concurrent Acquire (which would then create a NEW inode and lock a
//     different file than the incumbent holder).
//
//   - HOME containment is enforced twice: once via EnsureDir on the target's
//     parent (which checkUnderHome-s the first existing ancestor), and once
//     via checkUnderHome on the sidecar path itself after creation. The
//     second check catches an attacker-planted symlink at the sidecar path.
//
//   - The Resolver is required. Passing nil is refused with a clear error —
//     symmetric with AtomicWrite / EnsureDir in atomic.go.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/gofrs/flock"
)

// DefaultLockTimeout is the flock acquisition timeout when
// LockOptions.Timeout is zero. It maps to the 5-second default declared in
// docs/prd/prd-v1.md NFR-C1 and docs/plan/stories/E1-S6.md acceptance
// criteria; the future --lock-timeout flag will override it.
const DefaultLockTimeout = 5 * time.Second

// lockSidecarSuffix is the suffix appended to the target basename to form
// the sidecar lockfile name. Kept as a package constant so tests and future
// writepath integration reference the same string.
const lockSidecarSuffix = ".claudecm-lock"

// lockRetryDelay is the polling interval passed to gofrs/flock.TryLockContext.
// 25 ms is short enough that timeouts are honoured with sub-30 ms overshoot
// on a warm machine while cheap enough that contention doesn't burn a CPU.
const lockRetryDelay = 25 * time.Millisecond

// ErrLockTimeout is returned by Acquire when flock could not be obtained
// within the configured timeout. Callers can errors.Is-check it to
// distinguish contention timeouts from I/O failures. Wrapped errors carry
// the resolved sidecar path.
var ErrLockTimeout = errors.New("claudecm: lock acquisition timed out")

// LockOptions carries the per-call knobs. Zero-value Timeout maps to
// DefaultLockTimeout — see Acquire.
type LockOptions struct {
	// Timeout is the maximum time Acquire will spend polling for the lock.
	// A zero or negative value is treated as DefaultLockTimeout.
	Timeout time.Duration
}

// Handle is an acquired exclusive flock on a target's sidecar file. Release
// MUST be called (defer preferred) or the lock will remain held until the
// process exits. Handle carries no package-level state; every field is
// unexported so callers cannot manipulate the underlying fd out of band.
type Handle struct {
	fl       *flock.Flock
	path     string
	released bool
}

// Acquire takes an exclusive advisory lock (flock LOCK_EX) on a sidecar
// lockfile colocated with target. See docs/plan/stories/E1-S6.md and
// architecture §4 step 1.
//
// target is a HOME-relative path (e.g. "profiles/foo.yaml" or
// ".claude/settings.json"). Absolute inputs, empty strings, and paths that
// walk outside HOME via ".." are refused before any filesystem call.
//
// Guarantees:
//   - The parent directory of target is created under HOME if missing
//     (via EnsureDir), which itself refuses on outside-HOME symlinks.
//   - The sidecar path is verified to stay under HOME after EvalSymlinks
//     (defense in depth against attacker-planted symlinks at the sidecar).
//   - The sidecar file is created 0600 with O_EXCL on first sighting so a
//     symlink planted between the initial Lstat and the create call fails
//     the create — closing the TOCTOU window.
//   - On timeout the returned error wraps ErrLockTimeout with the sidecar
//     path so operators can act on it.
func Acquire(r *Resolver, target string, opts LockOptions) (*Handle, error) {
	if r == nil {
		return nil, errors.New("lock acquire: resolver is nil")
	}
	if target == "" {
		return nil, errors.New("lock acquire: target is empty")
	}
	if filepath.IsAbs(target) {
		return nil, fmt.Errorf("lock acquire: target %q must be HOME-relative", target)
	}
	if strings.ContainsRune(target, 0x00) {
		return nil, fmt.Errorf("lock acquire: target %q contains a NUL byte", target)
	}
	cleaned := filepath.Clean(target)
	// Reject any target that resolves to "." or escapes HOME lexically.
	// A leading ".." after Clean means we would climb above r.home; a bare
	// "." means the target degenerated to the HOME directory itself, which
	// cannot be a lock target (there is no basename to hang the sidecar on).
	if cleaned == "." || cleaned == ".." ||
		strings.HasPrefix(cleaned, ".."+string(filepath.Separator)) {
		return nil, fmt.Errorf("lock acquire: target %q escapes HOME", target)
	}
	base := filepath.Base(cleaned)
	if err := validatePathSegment(base, "lock target basename"); err != nil {
		return nil, fmt.Errorf("lock acquire: %w", err)
	}
	absParent := filepath.Join(r.home, filepath.Dir(cleaned))

	// EnsureDir walks up to the first existing ancestor and checkUnderHome-s
	// it. This is the first line of defense against a parent-dir symlink
	// that points outside HOME (the AC "Symlink escape via parent dir").
	if err := EnsureDir(r, absParent); err != nil {
		return nil, fmt.Errorf("lock acquire: %w", err)
	}
	resolvedParent, err := checkUnderHome(r, absParent)
	if err != nil {
		return nil, fmt.Errorf("lock acquire: %w", err)
	}
	sidecar := filepath.Join(resolvedParent, base+lockSidecarSuffix)

	// Defense in depth: refuse if the sidecar path already exists as a
	// symlink. Lstat sees the symlink itself (not the target); if we
	// followed it via Open we could touch a file outside HOME. Detecting
	// here means gofrs/flock never opens the escaping path.
	if info, statErr := os.Lstat(sidecar); statErr == nil {
		if info.Mode()&os.ModeSymlink != 0 {
			return nil, fmt.Errorf(
				"lock acquire: sidecar %q is a symlink: %w",
				sidecar, ErrOutsideHome,
			)
		}
	} else if !os.IsNotExist(statErr) {
		return nil, fmt.Errorf("lock acquire: lstat sidecar %q: %w", sidecar, statErr)
	} else {
		// Create with O_EXCL: on Linux this fails with EEXIST if a symlink
		// (dangling or otherwise) exists at the path — closing the TOCTOU
		// window between the Lstat above and the Open here. On EEXIST we
		// fall through to the flock path; a race with a concurrent
		// legitimate Acquire is fine because both parties end up locking
		// the same inode.
		f, cerr := os.OpenFile(sidecar, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0600)
		if cerr != nil && !errors.Is(cerr, os.ErrExist) {
			return nil, fmt.Errorf("lock acquire: create sidecar %q: %w", sidecar, cerr)
		}
		if cerr == nil {
			_ = f.Close()
		}
	}
	// Re-assert 0600 — umask may have masked the requested mode on create,
	// and the sidecar may have been created by an earlier Acquire under a
	// different umask (architecture §8: mode re-asserted on every write).
	if err := os.Chmod(sidecar, 0600); err != nil {
		return nil, fmt.Errorf("lock acquire: chmod sidecar %q: %w", sidecar, err)
	}
	// Post-condition: EvalSymlinks the sidecar and verify it stays under
	// HOME. This is the second guard: it catches the case where a symlink
	// slipped in between the initial Lstat and the create call succeeded
	// because the symlink target happened to satisfy O_EXCL semantics on
	// an unusual filesystem.
	if _, err := checkUnderHome(r, sidecar); err != nil {
		return nil, fmt.Errorf("lock acquire: sidecar %q: %w", sidecar, err)
	}

	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = DefaultLockTimeout
	}
	fl := flock.New(sidecar)
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	locked, lockErr := fl.TryLockContext(ctx, lockRetryDelay)
	if lockErr != nil {
		if errors.Is(lockErr, context.DeadlineExceeded) {
			return nil, fmt.Errorf("%w: %s", ErrLockTimeout, sidecar)
		}
		return nil, fmt.Errorf("lock acquire: flock %q: %w", sidecar, lockErr)
	}
	if !locked {
		return nil, fmt.Errorf("%w: %s", ErrLockTimeout, sidecar)
	}
	return &Handle{fl: fl, path: sidecar}, nil
}

// Path returns the resolved sidecar path this Handle owns. Exposed for
// tests and future writepath diagnostics; callers must not open, unlink,
// or otherwise touch the file behind this path.
func (h *Handle) Path() string {
	if h == nil {
		return ""
	}
	return h.path
}

// Release drops the flock and closes the underlying fd. Idempotent: a
// second Release returns nil without touching the fd. Does NOT remove the
// sidecar file — see the file-level comment for the reasoning.
//
// Not safe for concurrent Release from multiple goroutines; call once
// (typically via defer).
func (h *Handle) Release() error {
	if h == nil || h.released {
		return nil
	}
	h.released = true
	if h.fl == nil {
		return nil
	}
	unlockErr := h.fl.Unlock()
	closeErr := h.fl.Close()
	// errors.Join is nil-safe: returns nil when both args are nil, and a
	// single non-nil arg when only one errored. This ensures a closeErr is
	// never silently dropped just because unlockErr fired first.
	if joined := errors.Join(unlockErr, closeErr); joined != nil {
		return fmt.Errorf("lock release %q: %w", h.path, joined)
	}
	return nil
}

// WithLock is a convenience: Acquire → fn → Release. Release is deferred
// so a panic inside fn still drops the lock (AC3 panic safety). The
// returned error is fn's error if fn failed; a Release error is joined
// onto it via errors.Join when both fire, and returned alone when only
// Release failed.
func WithLock(r *Resolver, target string, opts LockOptions, fn func() error) (err error) {
	if fn == nil {
		return errors.New("lock: WithLock fn is nil")
	}
	h, acqErr := Acquire(r, target, opts)
	if acqErr != nil {
		return acqErr
	}
	defer func() {
		relErr := h.Release()
		if err == nil {
			err = relErr
		} else if relErr != nil {
			err = errors.Join(err, relErr)
		}
	}()
	return fn()
}
