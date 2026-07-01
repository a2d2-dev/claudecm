package storage

// atomic.go is the single primitive claudecm uses to put bytes on disk. Every
// profile write, state write, and (via internal/writepath in later stories)
// tool-config write funnels through AtomicWrite so the temp-file → fsync →
// rename → fsync-parent ritual is expressed once and only once.
//
// Design choices (per docs/architecture/coding-standards.md and story E1-S3):
//
//   - The API is deliberately a set of free functions taking (*Resolver, ...)
//     rather than methods on Resolver. Rationale: Resolver's job is to answer
//     "what is the absolute path for X?" — a pure function of HOME. Writing
//     bytes to disk is a different concern that only needs Resolver as a
//     HOME anchor. Keeping the primitives as package-level functions makes
//     the dependency explicit at every call site and lets Resolver stay a
//     small value type with no I/O methods.
//
//   - No package-level mutable state (coding standards rule 12). Randomness
//     for temp-file names comes from crypto/rand every call; pid + 8 bytes
//     of random hex is enough to avoid collisions within a process without
//     a shared counter.
//
//   - No fallback. crypto/rand failure surfaces as an error; we never fall
//     back to math/rand (per the global "no fallback" rule).
//
//   - Symlink escape check: EvalSymlinks the PARENT directory (the target
//     itself may not yet exist), EvalSymlinks HOME, and compare with
//     filepath.Rel — never with a string prefix. This defeats attacker-owned
//     symlinks inside HOME that point outside HOME.

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// ErrOutsideHome is returned when a write or ensure-dir target resolves to a
// path outside the Resolver's HOME after symlink evaluation. Callers can
// errors.Is-check this to distinguish policy refusals from I/O errors.
var ErrOutsideHome = errors.New("path resolves outside HOME")

// ErrTargetExists is returned by AtomicWrite when AtomicWriteOptions.MustNotExist
// is set and the target file is present on disk. Maps to NFR-C3: the first
// write against a missing target must fail rather than silently overwrite if
// the caller expected the file to be absent.
var ErrTargetExists = errors.New("target file already exists")

// Fingerprint records the identifying triple used by NFR-C2 concurrent-edit
// detection. It is captured after every successful AtomicWrite and can be
// re-captured via Stat before a subsequent write to detect out-of-band edits.
type Fingerprint struct {
	Size    int64
	ModTime time.Time
	SHA256  string // lowercase hex, no prefix
}

// AtomicWriteOptions carries the per-write knobs. Zero-value Mode defaults to
// 0600 (architecture §8 file-permission policy). MustNotExist forces the
// NFR-C3 first-write contract: refuse if the target exists on disk.
type AtomicWriteOptions struct {
	Mode         os.FileMode
	MustNotExist bool
}

// Stat computes a Fingerprint for path. The bool return is true iff the file
// exists as a regular file; on os.IsNotExist the function returns
// (Fingerprint{}, false, nil) so callers can branch cleanly on absence.
// Non-regular files (dirs, symlinks, devices) are refused: callers that want
// symlink semantics need a different tool.
func Stat(path string) (Fingerprint, bool, error) {
	info, err := os.Lstat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return Fingerprint{}, false, nil
		}
		return Fingerprint{}, false, fmt.Errorf("stat %q: %w", path, err)
	}
	if !info.Mode().IsRegular() {
		return Fingerprint{}, false, fmt.Errorf("stat %q: not a regular file (mode=%s)", path, info.Mode())
	}
	f, err := os.Open(path)
	if err != nil {
		return Fingerprint{}, false, fmt.Errorf("open %q: %w", path, err)
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return Fingerprint{}, false, fmt.Errorf("hash %q: %w", path, err)
	}
	return Fingerprint{
		Size:    n,
		ModTime: info.ModTime(),
		SHA256:  hex.EncodeToString(h.Sum(nil)),
	}, true, nil
}

// AtomicWrite writes data to path via a sibling temp file and a rename,
// enforcing the ritual documented in Architecture §4 step 7 and NFR-C3:
//
//  1. EvalSymlinks the parent directory; refuse if it resolves outside HOME.
//     The parent MUST already exist — AtomicWrite does NOT auto-create it;
//     callers use EnsureDir first. Keeping the primitive minimal makes the
//     writepath.Apply audit trail cleaner in later stories.
//  2. If opts.MustNotExist is true and the target already exists, refuse
//     with ErrTargetExists (NFR-C3).
//  3. Open a sibling temp file "<basename>.claudecm-tmp-<pid>-<rand>" with
//     O_CREATE|O_EXCL|O_WRONLY at the requested mode (default 0600). Mode
//     is re-asserted via Chmod immediately after open, per architecture §8
//     ("mode is re-asserted on every write").
//  4. Write bytes to a MultiWriter fanning to the temp file and a SHA-256
//     hasher — single-pass hash, no re-read.
//  5. fsync the temp file, close it, then os.Rename(temp, target).
//  6. fsync the parent directory so the rename is durable on ext4/xfs.
//  7. Return the post-write Fingerprint.
//
// Any error at any step deletes the temp file (best-effort) and returns.
// There is no partial-write recovery: a failed AtomicWrite leaves the
// original target file — if any — byte-for-byte untouched.
func AtomicWrite(r *Resolver, path string, data []byte, opts AtomicWriteOptions) (Fingerprint, error) {
	if r == nil {
		return Fingerprint{}, errors.New("atomic write: resolver is nil")
	}
	mode := opts.Mode
	if mode == 0 {
		mode = 0600
	}

	cleaned := filepath.Clean(path)
	if !filepath.IsAbs(cleaned) {
		return Fingerprint{}, fmt.Errorf("atomic write: path %q is not absolute", path)
	}
	parent := filepath.Dir(cleaned)
	base := filepath.Base(cleaned)
	if base == "." || base == string(filepath.Separator) || base == "" {
		return Fingerprint{}, fmt.Errorf("atomic write: bad basename in %q", path)
	}

	resolvedParent, err := checkUnderHome(r, parent)
	if err != nil {
		return Fingerprint{}, fmt.Errorf("atomic write %q: %w", path, err)
	}
	finalPath := filepath.Join(resolvedParent, base)

	if opts.MustNotExist {
		if _, err := os.Lstat(finalPath); err == nil {
			return Fingerprint{}, fmt.Errorf("atomic write %q: %w", finalPath, ErrTargetExists)
		} else if !os.IsNotExist(err) {
			return Fingerprint{}, fmt.Errorf("atomic write: lstat %q: %w", finalPath, err)
		}
	}

	tmpName, err := tempFilename(base)
	if err != nil {
		return Fingerprint{}, fmt.Errorf("atomic write: temp name: %w", err)
	}
	tmpPath := filepath.Join(resolvedParent, tmpName)

	// O_EXCL on the temp is defense-in-depth: the random suffix should make
	// this impossible, but if it ever collides we prefer to error out rather
	// than clobber a sibling.
	tmp, err := os.OpenFile(tmpPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, mode)
	if err != nil {
		return Fingerprint{}, fmt.Errorf("atomic write: open temp %q: %w", tmpPath, err)
	}
	// Umask may have masked the requested mode on OpenFile. Re-assert.
	if err := os.Chmod(tmpPath, mode); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpPath)
		return Fingerprint{}, fmt.Errorf("atomic write: chmod temp %q: %w", tmpPath, err)
	}

	hasher := sha256.New()
	mw := io.MultiWriter(tmp, hasher)
	n, werr := mw.Write(data)
	if werr != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpPath)
		return Fingerprint{}, fmt.Errorf("atomic write: write %q: %w", tmpPath, werr)
	}
	if n != len(data) {
		_ = tmp.Close()
		_ = os.Remove(tmpPath)
		return Fingerprint{}, fmt.Errorf("atomic write: short write %d/%d to %q", n, len(data), tmpPath)
	}

	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpPath)
		return Fingerprint{}, fmt.Errorf("atomic write: fsync temp %q: %w", tmpPath, err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return Fingerprint{}, fmt.Errorf("atomic write: close temp %q: %w", tmpPath, err)
	}

	if err := os.Rename(tmpPath, finalPath); err != nil {
		_ = os.Remove(tmpPath)
		return Fingerprint{}, fmt.Errorf("atomic write: rename %q -> %q: %w", tmpPath, finalPath, err)
	}

	// After rename, fsync the parent dir so the directory entry is durable.
	// On ext4/xfs without this, a crash can lose the rename even though the
	// file bytes are on disk.
	if err := fsyncDir(resolvedParent); err != nil {
		return Fingerprint{}, fmt.Errorf("atomic write: fsync parent %q: %w", resolvedParent, err)
	}

	info, err := os.Lstat(finalPath)
	if err != nil {
		return Fingerprint{}, fmt.Errorf("atomic write: post-stat %q: %w", finalPath, err)
	}
	return Fingerprint{
		Size:    int64(n),
		ModTime: info.ModTime(),
		SHA256:  hex.EncodeToString(hasher.Sum(nil)),
	}, nil
}

// EnsureDir creates dir (and any missing parents) with mode 0700, refusing if
// the resolved path would land outside HOME. It walks up until the first
// existing ancestor, EvalSymlinks that ancestor, and checks it is under the
// resolved HOME — this catches attacker-owned symlinks along the path.
func EnsureDir(r *Resolver, dir string) error {
	if r == nil {
		return errors.New("ensure dir: resolver is nil")
	}
	cleaned := filepath.Clean(dir)
	if !filepath.IsAbs(cleaned) {
		return fmt.Errorf("ensure dir: %q is not absolute", dir)
	}

	// Walk up to first existing ancestor; we can only EvalSymlinks something
	// that exists.
	ancestor := cleaned
	for {
		if _, err := os.Lstat(ancestor); err == nil {
			break
		} else if !os.IsNotExist(err) {
			return fmt.Errorf("ensure dir: lstat %q: %w", ancestor, err)
		}
		parent := filepath.Dir(ancestor)
		if parent == ancestor {
			return fmt.Errorf("ensure dir: no existing ancestor for %q", cleaned)
		}
		ancestor = parent
	}
	if _, err := checkUnderHome(r, ancestor); err != nil {
		return fmt.Errorf("ensure dir %q: %w", dir, err)
	}
	if err := os.MkdirAll(cleaned, 0700); err != nil {
		return fmt.Errorf("ensure dir: mkdirall %q: %w", cleaned, err)
	}
	return nil
}

// checkUnderHome EvalSymlinks-resolves p and r.Home() and verifies that the
// resolved p is r.Home() or a subdirectory of it (using filepath.Rel, not a
// string prefix — the string-prefix approach false-positives on siblings like
// "/home/alice-evil" when HOME is "/home/alice"). Returns the resolved p on
// success. Wraps ErrOutsideHome so callers can errors.Is-check the reason.
func checkUnderHome(r *Resolver, p string) (string, error) {
	resolvedP, err := filepath.EvalSymlinks(p)
	if err != nil {
		return "", fmt.Errorf("evalsymlinks %q: %w", p, err)
	}
	resolvedHome, err := filepath.EvalSymlinks(r.home)
	if err != nil {
		return "", fmt.Errorf("evalsymlinks home %q: %w", r.home, err)
	}
	rel, err := filepath.Rel(resolvedHome, resolvedP)
	if err != nil {
		return "", fmt.Errorf("%w: rel %q vs %q: %v", ErrOutsideHome, resolvedHome, resolvedP, err)
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return "", fmt.Errorf("%w: %q is not under %q (rel=%q)", ErrOutsideHome, resolvedP, resolvedHome, rel)
	}
	return resolvedP, nil
}

// tempFilename returns "<base>.claudecm-tmp-<pid>-<8 bytes hex>". Randomness
// comes from crypto/rand; a read failure surfaces as an error rather than
// falling back to math/rand — coding standards rule 2, "no silent fallback".
func tempFilename(base string) (string, error) {
	var buf [8]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", fmt.Errorf("crypto/rand: %w", err)
	}
	return fmt.Sprintf("%s.claudecm-tmp-%d-%s", base, os.Getpid(), hex.EncodeToString(buf[:])), nil
}

// fsyncDir opens dir for reading and fsyncs it. This is the standard trick
// for making a rename durable on ext4/xfs: the file bytes are on disk after
// tmp.Sync(), but the directory entry that names them isn't durable until
// the parent inode is synced.
func fsyncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}
