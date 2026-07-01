package storage

// backup.go is the FR-5 pre-write backup primitive. Before internal/writepath
// publishes new bytes onto a tool-owned file, it MUST capture the current
// bytes into `~/.claudecm/backups/<tool>/<basename>.bak.<ts>` via Backup so
// that a post-write reparse failure has a byte-for-byte snapshot to restore
// from. The primitive is deliberately narrow: it does one thing (copy the
// bytes on disk atomically at 0600 with a fingerprint receipt), and it does
// not know about retention (E1-S5) or restore (E6-S7).
//
// Design choices (per docs/architecture/coding-standards.md and story E1-S4):
//
//   - Same package-level-function shape as AtomicWrite: free functions that
//     take (*Resolver, ...). No methods on Resolver, no I/O baked into the
//     path type. This keeps the audit story from writepath.Apply linear:
//     "Backup then AtomicWrite" is two calls, not a hidden pipeline.
//
//   - No package-level mutable state (coding-standards rule 12). The clock
//     seam used by tests to force same-nanosecond collisions is wired via a
//     `//go:build test` companion file (see backup_clock.go and
//     backup_clock_testhook.go), matching the pattern established by
//     atomic_syncfunc.go for the fsync seam. Production binaries have no
//     swap point.
//
//   - Timestamp format is deterministic and path-safe: UTC RFC3339 collapsed
//     to `YYYYMMDDTHHMMSSZnnnnnnnnn`. No colons, no dots, no separators, no
//     control characters — passes validatePathSegment in paths.go. The
//     fixed-width layout is lexicographically ordered, so a sort by filename
//     descending gives ListBackups its "newest first" contract for free.
//
//   - Paranoid size ceiling. Real tool-configs (claude settings.json, codex
//     config.toml, auth.json) are kilobytes. If we ever see a source above
//     MaxBackupSourceBytes (32 MiB) we refuse — hoisting a runaway file into
//     memory to hash is more likely to be someone pointing the primitive at
//     the wrong path than a legitimate config that grew that large.
//
//   - No fallback. crypto/rand or fsync failures inside AtomicWrite propagate
//     verbatim; ErrTargetExists is surfaced when MustNotExist ever fires so
//     callers can decide (a same-nanosecond collision is not silently retried
//     — it means the clock went backwards or the caller invoked Backup twice
//     with a hand-supplied clock, both of which the caller must resolve).

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// MaxBackupSourceBytes is the paranoid ceiling on the size of a file Backup
// will hoist into memory. Real tool-configs (claude/codex/gemini settings)
// are measured in kilobytes; anything above this is almost certainly the
// primitive being pointed at the wrong path. Documented per the story to
// keep the ceiling reviewable without reading the code.
const MaxBackupSourceBytes int64 = 32 * 1024 * 1024

// ErrNothingToBackup is returned by Backup when the source path does not
// exist. Callers (writepath.Apply in a later story) treat this as a signal
// that the tool-config is being created for the first time — there is no
// prior state to preserve — and proceed to AtomicWrite without a backup
// receipt. It is a typed error so writepath can errors.Is-check it instead
// of parsing message strings.
var ErrNothingToBackup = errors.New("claudecm: backup source does not exist")

// ErrSourceTooLarge is returned by Backup when the source file exceeds
// MaxBackupSourceBytes. A refusal, not a partial-read fallback: FR-5 wants
// byte-for-byte snapshots, and streaming a >32 MiB file through the
// AtomicWrite temp-buffer + hasher pair defeats that guarantee.
var ErrSourceTooLarge = errors.New("claudecm: backup source exceeds size ceiling")

// BackupRecord is the receipt Backup returns to its caller (and that
// ListBackups reconstructs when enumerating a tool dir). It is intentionally
// a value type: writepath.Apply will pass it by value into the reparse-check
// step in a later story.
type BackupRecord struct {
	Tool       string    // "claude_code" / "codex" / ... (ADR-0001 frozen values; see config.ToolID). E3-S2 will retype this as adapter.ToolID for compile-time enforcement.
	Basename   string    // basename of the source file (e.g. "settings.json")
	SourcePath string    // full path to the file that was backed up
	BackupPath string    // full path to the newly written backup file
	Timestamp  time.Time // captured before the write (UTC)
	// Fingerprint.ModTime is the source's mtime at Stat time; concurrent modification of the source during copy is out of scope of this primitive.
	Fingerprint Fingerprint // fingerprint of the backed-up bytes
}

// backupTimestampLayout documents the exact width and structure of the
// timestamp suffix used in backup filenames: 8-digit date, "T", 6-digit
// time, "Z", 9-digit nanoseconds — 25 characters total. Fixed-width means
// lexicographic and chronological order agree, which is what makes
// ListBackups sort-descending correct without parsing every entry.
const backupTimestampLayout = "YYYYMMDDTHHMMSSZnnnnnnnnn" // 25 chars

// Backup copies srcPath to r.BackupPath(tool, basename, ts) atomically at
// mode 0600. See file header for the safety contract. On success it returns
// a BackupRecord whose Fingerprint hashes the actual bytes captured; on any
// error no backup file exists at the destination.
func Backup(r *Resolver, tool, basename, srcPath string) (BackupRecord, error) {
	if r == nil {
		return BackupRecord{}, errors.New("backup: resolver is nil")
	}
	if err := validatePathSegment(tool, "tool"); err != nil {
		return BackupRecord{}, fmt.Errorf("backup: %w", err)
	}
	if err := validatePathSegment(basename, "basename"); err != nil {
		return BackupRecord{}, fmt.Errorf("backup: %w", err)
	}
	if srcPath == "" {
		return BackupRecord{}, errors.New("backup: srcPath is empty")
	}
	if !filepath.IsAbs(srcPath) {
		return BackupRecord{}, fmt.Errorf("backup: srcPath %q is not absolute", srcPath)
	}
	if strings.ContainsRune(srcPath, 0x00) {
		return BackupRecord{}, fmt.Errorf("backup: srcPath %q contains a NUL byte", srcPath)
	}

	// Existence check first. If the source is genuinely absent we return
	// ErrNothingToBackup and write nothing — the writepath caller uses that
	// as "this is a first-time write, no prior state to preserve". A broken
	// symlink is treated the same way: EvalSymlinks below will fail with
	// os.ErrNotExist and we translate it here so callers see one signal.
	if _, err := os.Lstat(srcPath); err != nil {
		if os.IsNotExist(err) {
			return BackupRecord{}, ErrNothingToBackup
		}
		return BackupRecord{}, fmt.Errorf("backup: lstat %q: %w", srcPath, err)
	}

	// Symlink-escape check: EvalSymlinks-resolve srcPath and confirm it stays
	// under HOME. Same rules AtomicWrite applies to its parent dir. Wrap so
	// ErrOutsideHome is preserved for errors.Is-checks.
	// Following symlinks whose target is inside HOME is intentional; BackupRecord.SourcePath preserves the caller's raw path, while the hashed bytes are those of the resolved target.
	resolvedSrc, err := checkUnderHome(r, srcPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return BackupRecord{}, ErrNothingToBackup
		}
		return BackupRecord{}, fmt.Errorf("backup: %w", err)
	}

	// Stat the resolved (symlink-followed) path so size and mode reflect the
	// real target. We refuse non-regular files: backing up a directory or a
	// device would either fail cryptically inside io.Copy or silently produce
	// garbage bytes. Neither is acceptable for a byte-for-byte snapshot.
	info, err := os.Stat(resolvedSrc)
	if err != nil {
		if os.IsNotExist(err) {
			return BackupRecord{}, ErrNothingToBackup
		}
		return BackupRecord{}, fmt.Errorf("backup: stat %q: %w", resolvedSrc, err)
	}
	if !info.Mode().IsRegular() {
		return BackupRecord{}, fmt.Errorf("backup: %q is not a regular file (mode=%s)", srcPath, info.Mode())
	}
	if info.Size() > MaxBackupSourceBytes {
		return BackupRecord{}, fmt.Errorf("backup: %q size %d exceeds ceiling %d bytes: %w",
			srcPath, info.Size(), MaxBackupSourceBytes, ErrSourceTooLarge)
	}

	now := backupClock().UTC()
	ts := formatBackupTimestamp(now)
	dst, err := r.BackupPath(tool, basename, ts)
	if err != nil {
		return BackupRecord{}, fmt.Errorf("backup: %w", err)
	}

	// Make sure the per-tool backup dir exists (mode 0700 via EnsureDir).
	// EnsureDir walks up to the first existing ancestor and confirms it is
	// under HOME before mkdir-all — so an attacker-owned symlink at
	// backups/<tool>/ is caught before we write bytes into it.
	if err := EnsureDir(r, filepath.Dir(dst)); err != nil {
		return BackupRecord{}, fmt.Errorf("backup: %w", err)
	}

	// Read source once, MultiWriter to buffer + sha256. The Size() check
	// above bounds the buffer growth; anything unbounded is refused.
	f, err := os.Open(resolvedSrc)
	if err != nil {
		return BackupRecord{}, fmt.Errorf("backup: open %q: %w", resolvedSrc, err)
	}
	defer f.Close()

	buf := bytes.NewBuffer(make([]byte, 0, int(info.Size())))
	hasher := sha256.New()
	n, err := io.Copy(io.MultiWriter(buf, hasher), f)
	if err != nil {
		return BackupRecord{}, fmt.Errorf("backup: read %q: %w", srcPath, err)
	}

	// AtomicWrite with MustNotExist=true. A same-nanosecond collision fires
	// ErrTargetExists — we surface it verbatim rather than silently retry
	// with a fresh timestamp so callers see the real problem (clock went
	// backwards, or the seam was set to a fixed value in a test).
	if _, err := AtomicWrite(r, dst, buf.Bytes(), AtomicWriteOptions{Mode: 0o600, MustNotExist: true}); err != nil {
		return BackupRecord{}, err
	}

	return BackupRecord{
		Tool:       tool,
		Basename:   basename,
		SourcePath: srcPath,
		BackupPath: dst,
		Timestamp:  now,
		Fingerprint: Fingerprint{
			Size:    n,
			ModTime: info.ModTime(),
			SHA256:  hex.EncodeToString(hasher.Sum(nil)),
		},
	}, nil
}

// ListBackups enumerates backups for the given (tool, basename), newest
// first. Path-safety rules apply to tool and basename exactly as in Backup —
// a caller cannot smuggle traversal segments through this API. A missing
// tool dir returns (nil, nil) rather than a wrapped os.ErrNotExist because
// "no backups yet" is a normal state, not an I/O error.
func ListBackups(r *Resolver, tool, basename string) ([]BackupRecord, error) {
	if r == nil {
		return nil, errors.New("list backups: resolver is nil")
	}
	if err := validatePathSegment(tool, "tool"); err != nil {
		return nil, fmt.Errorf("list backups: %w", err)
	}
	if err := validatePathSegment(basename, "basename"); err != nil {
		return nil, fmt.Errorf("list backups: %w", err)
	}

	toolDir := filepath.Join(r.BackupsRoot(), tool)
	// Confirm the tool dir (if it exists) is lexically under HOME. We do NOT
	// EvalSymlinks here because a missing dir is a valid state; the check is
	// deferred to Stat below via os.ReadDir which will surface any I/O error.
	if _, err := ensureUnderHome(toolDir, r.home); err != nil {
		return nil, fmt.Errorf("list backups: %w", err)
	}

	entries, err := os.ReadDir(toolDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("list backups: readdir %q: %w", toolDir, err)
	}

	// The ReadDir above proved the tool dir exists; now confirm the resolved
	// path still lives under HOME. Symmetric with Backup's source check:
	// ensureUnderHome above is a lexical guard on the pre-resolved path, but
	// an attacker who planted a symlink at backups/<tool>/ pointing outside
	// HOME would slip past a HasPrefix check. checkUnderHome's EvalSymlinks
	// + Rel dance is what catches that.
	if _, err := checkUnderHome(r, toolDir); err != nil {
		return nil, fmt.Errorf("list backups: %w", err)
	}

	prefix := basename + ".bak."
	records := make([]BackupRecord, 0, len(entries))
	for _, ent := range entries {
		if ent.IsDir() {
			continue
		}
		name := ent.Name()
		if !strings.HasPrefix(name, prefix) {
			continue
		}
		tsStr := strings.TrimPrefix(name, prefix)
		ts, err := parseBackupTimestamp(tsStr)
		if err != nil {
			// Foreign file that shares our prefix but has a malformed
			// timestamp: skip it silently rather than fail the whole listing.
			// Retention (E1-S5) is what audits this; ListBackups is a
			// read-only query.
			continue
		}
		fullPath := filepath.Join(toolDir, name)
		fp, ok, statErr := Stat(fullPath)
		if statErr != nil {
			return nil, fmt.Errorf("list backups: stat %q: %w", fullPath, statErr)
		}
		if !ok {
			continue
		}
		records = append(records, BackupRecord{
			Tool:        tool,
			Basename:    basename,
			BackupPath:  fullPath,
			Timestamp:   ts,
			Fingerprint: fp,
			// SourcePath is intentionally left empty: the listing operates
			// on the backup dir and does not know where the original file
			// lived. writepath will fill this in when it invokes Backup.
		})
	}

	// Fixed-width timestamp means byte-descending on filename is
	// chronological-descending on Timestamp.
	sort.Slice(records, func(i, j int) bool {
		return records[i].BackupPath > records[j].BackupPath
	})
	return records, nil
}

// formatBackupTimestamp renders t in the canonical backup timestamp layout
// documented at backupTimestampLayout. Always UTC, always 25 characters,
// no colons or dots so the result is safe as a path segment.
func formatBackupTimestamp(t time.Time) string {
	t = t.UTC()
	return fmt.Sprintf("%04d%02d%02dT%02d%02d%02dZ%09d",
		t.Year(), int(t.Month()), t.Day(),
		t.Hour(), t.Minute(), t.Second(),
		t.Nanosecond())
}

// parseBackupTimestamp is the inverse of formatBackupTimestamp. It rejects
// malformed inputs so ListBackups can silently skip foreign files that
// share the "<basename>.bak." prefix without conflating them with valid
// backups written by an older claudecm build.
func parseBackupTimestamp(s string) (time.Time, error) {
	// Expected shape: 8 date + 'T' + 6 time + 'Z' + 9 nanos = 25 chars.
	if len(s) != 25 || s[8] != 'T' || s[15] != 'Z' {
		return time.Time{}, fmt.Errorf("backup timestamp %q does not match layout %q", s, backupTimestampLayout)
	}
	base, err := time.Parse("20060102T150405Z", s[:16])
	if err != nil {
		return time.Time{}, fmt.Errorf("backup timestamp %q: %w", s, err)
	}
	// ParseUint (not Atoi) so a signed nanosecond field like "+00000000" or
	// "-00000001" is rejected. formatBackupTimestamp always emits nine
	// unsigned digits; anything else is a foreign entry.
	nanos, err := strconv.ParseUint(s[16:], 10, 32)
	if err != nil {
		return time.Time{}, fmt.Errorf("backup timestamp %q nanoseconds: %w", s, err)
	}
	return base.Add(time.Duration(nanos) * time.Nanosecond).UTC(), nil
}
