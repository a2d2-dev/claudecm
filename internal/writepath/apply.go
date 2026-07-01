// apply.go implements FR-5 steps 1-7 of the locked write-path pipeline.
// It is the single legal path to disk for any tool-owned config file
// claudecm mutates. Steps 8-10 (post-write reparse, auto-rollback,
// concurrent-edit detection) land in E2-S3 and E2-S4 on top of the same
// call site.
//
// Pipeline expressed here (architecture.md §4, PRD FR-5):
//
//  1. ValidatePlan — cheap pre-execution refuse. Wraps ErrPlanInvalid.
//  2. Acquire flock via storage.WithLock on the target sidecar. Timeout
//     is derived from ctx.Deadline() when set, otherwise
//     storage.DefaultLockTimeout. Timeout maps to ErrLockTimeout.
//  3. Read current bytes (may not exist → treated as "empty" for the
//     Transform seam) and capture pre-write Fingerprint via
//     storage.Stat. Non-existence yields a zero Fingerprint with the
//     exists=false signal.
//  4. Symlink escape is enforced twice — by storage.Acquire's EnsureDir
//     under HOME and by storage.AtomicWrite on the parent — and mapped
//     to ErrOutsideHome here so callers can errors.Is on one sentinel
//     without importing internal/storage.
//  5. Compute new bytes: plan.Transform wins over plan.NewContent (see
//     plan.go package doc). Transform errors abort — no fallback to
//     NewContent, per CLAUDE.md "no fallback" rule.
//  6. Parse current + new via plan.Parser. Non-nil Parser is mandatory
//     for a meaningful Diff; a nil Parser skips diff computation and
//     the skip-on-identical-bytes shortcut becomes the only "no-op"
//     signal. On parse failure of EITHER side we wrap the parser error
//     with ErrParseFailed and abort. NFR-S1: no silent rewrite.
//  7. Flatten + Diff. Empty diff (bytes byte-identical OR parsed values
//     identical) short-circuits to Skipped=true with no backup and no
//     write; PostFingerprint mirrors PreFingerprint.
//  8. DryRun=true returns a report populated with the diff but WITHOUT
//     backing up or writing. Callers use this for FR-15.
//  9. Diff.TouchesUnowned=true AND AllowUnowned=false AND DryRun=false
//     is refused with ErrDryRunUnownedTouched. The sentinel name is
//     kept for API stability even though it fires outside dry-run.
//  10. storage.Backup snapshots the pre-write bytes. ErrNothingToBackup
//     means "first write, no prior state" and produces a zero
//     BackupRecord. Any other backup failure wraps ErrBackupFailed and
//     aborts before touching the target.
//  11. storage.AtomicWrite publishes the new bytes at mode 0600, honoring
//     MustNotExist. AtomicWrite's own ErrTargetExists propagates
//     unwrapped so callers can errors.Is against storage.ErrTargetExists.
//  12. storage.Stat captures the post-write Fingerprint for the report.
//     No post-write reparse or fingerprint-drift check happens here;
//     those are E2-S3/E2-S4.
//
// Concurrency scope: the file is held under storage.WithLock for the
// entirety of read → parse → diff → backup → write → post-stat. Panic
// safety is inherited from storage.WithLock (Release is deferred).

package writepath

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"time"

	"github.com/a2d2-dev/claudecm/internal/storage"
)

// Apply is the single legal path to disk for tool-owned config files.
// It executes FR-5 steps 1-7 under an exclusive flock and returns a
// WriteReport describing the outcome. See file header for the full
// pipeline. Callers must pass a non-nil Resolver; passing nil is a
// programming error and is refused with ErrPlanInvalid.
//
// ctx.Deadline() (when set) caps the flock acquisition timeout. A
// context without a deadline uses storage.DefaultLockTimeout. Cancel is
// NOT propagated deeper than lock acquisition in this story — the
// bounded work under the lock (read, parse, diff, backup, atomic write)
// is designed to be short enough that mid-pipeline cancellation would
// leave the code paths only partially exercised. Later stories may
// extend cancellation coverage.
func Apply(ctx context.Context, r *storage.Resolver, plan WritePlan) (WriteReport, error) {
	if err := ValidatePlan(plan); err != nil {
		return WriteReport{}, err
	}
	if r == nil {
		return WriteReport{}, fmt.Errorf("%w: resolver is nil", ErrPlanInvalid)
	}
	if ctx == nil {
		ctx = context.Background()
	}

	// storage.Acquire expects a HOME-relative path. filepath.Rel yields
	// "../foo" when Target lies outside HOME lexically; storage.Acquire
	// then refuses. Symlink-escape (Target inside HOME lexically but
	// resolves outside) is caught downstream by EnsureDir/checkUnderHome
	// and surfaces here as storage.ErrOutsideHome — mapped below.
	lockRel, relErr := filepath.Rel(r.Home(), plan.Target)
	if relErr != nil {
		return WriteReport{}, fmt.Errorf("%w: rel %q vs HOME: %v", ErrOutsideHome, plan.Target, relErr)
	}

	lockOpts := storage.LockOptions{}
	if deadline, ok := ctx.Deadline(); ok {
		if d := time.Until(deadline); d > 0 {
			lockOpts.Timeout = d
		}
	}

	var report WriteReport
	fnErr := storage.WithLock(r, lockRel, lockOpts, func() error {
		rep, aerr := applyLocked(r, plan)
		report = rep
		return aerr
	})
	if fnErr != nil {
		return WriteReport{}, mapStorageError(fnErr)
	}
	return report, nil
}

// applyLocked runs steps 3-12 under the assumption the lock is held.
// Split out so Apply itself remains a linear lock+dispatch shell.
func applyLocked(r *storage.Resolver, plan WritePlan) (WriteReport, error) {
	now := time.Now()

	currentBytes, exists, err := readAll(plan.Target)
	if err != nil {
		return WriteReport{}, err
	}
	preFP, _, err := storage.Stat(plan.Target)
	if err != nil {
		return WriteReport{}, err
	}

	// Step 5: compute new bytes. Transform wins over NewContent; a
	// Transform error is fatal (no fallback rewrite).
	var newBytes []byte
	if plan.Transform != nil {
		nb, terr := plan.Transform(currentBytes)
		if terr != nil {
			return WriteReport{}, fmt.Errorf("writepath: transform for %q: %w", plan.Target, terr)
		}
		newBytes = nb
	} else {
		newBytes = plan.NewContent
	}

	// Step 6: parse current + new. Skipped entirely when Parser is nil.
	// When the file did not exist, the "current" side is an empty flat
	// map (not the result of Flatten(nil), which yields {"": nil} — see
	// Flatten's non-map top-level rule). Skipping parse+flatten on the
	// current side keeps the first-write diff clean: every key on the
	// next side is a legitimate Added, and TouchesUnowned is decided
	// against next's keys alone.
	var diff DiffResult
	if plan.Parser != nil {
		curFlat := map[string]any{}
		if exists {
			curParsed, perr := plan.Parser.Parse(currentBytes)
			if perr != nil {
				return WriteReport{}, fmt.Errorf("%w: parse current %q: %v", ErrParseFailed, plan.Target, perr)
			}
			cf, ferr := Flatten(curParsed)
			if ferr != nil {
				return WriteReport{}, fmt.Errorf("%w: flatten current %q: %v", ErrParseFailed, plan.Target, ferr)
			}
			curFlat = cf
		}
		newParsed, perr := plan.Parser.Parse(newBytes)
		if perr != nil {
			return WriteReport{}, fmt.Errorf("%w: parse new %q: %v", ErrParseFailed, plan.Target, perr)
		}
		newFlat, ferr := Flatten(newParsed)
		if ferr != nil {
			return WriteReport{}, fmt.Errorf("%w: flatten new %q: %v", ErrParseFailed, plan.Target, ferr)
		}
		diff = Diff(curFlat, newFlat, plan.OwnedKeys)
	}

	// Step 7: skip iff bytes byte-identical AND file existed, or
	// (Parser present AND parsed diff is empty). The "&& exists"
	// guard makes "current absent + newBytes empty" NOT skip — a
	// legitimate zero-byte first write still goes through the atomic
	// publish so the file appears at 0600.
	bytesEqual := exists && bytes.Equal(currentBytes, newBytes)
	diffEmpty := plan.Parser != nil && reflect.DeepEqual(diff, DiffResult{})
	if bytesEqual || diffEmpty {
		return WriteReport{
			Tool:            plan.Tool,
			Target:          plan.Target,
			Skipped:         true,
			PreFingerprint:  preFP,
			PostFingerprint: preFP,
			Diff:            diff,
			AppliedAt:       now,
		}, nil
	}

	// Step 8: dry-run short-circuits before backup and write.
	if plan.DryRun {
		return WriteReport{
			Tool:           plan.Tool,
			Target:         plan.Target,
			DryRun:         true,
			Diff:           diff,
			PreFingerprint: preFP,
			AppliedAt:      now,
		}, nil
	}

	// Step 9: unowned-touched guard. Refuse the write unless the caller
	// pre-authorized via AllowUnowned. Sentinel name kept for API
	// stability (see plan.go ErrDryRunUnownedTouched doc).
	if diff.TouchesUnowned && !plan.AllowUnowned {
		return WriteReport{}, fmt.Errorf("%w: %s", ErrDryRunUnownedTouched, plan.Target)
	}

	// Step 10: backup pre-write state. ErrNothingToBackup is fine —
	// first write against a missing target. Anything else is fatal.
	var backup storage.BackupRecord
	brec, berr := storage.Backup(r, plan.Tool, filepath.Base(plan.Target), plan.Target)
	switch {
	case berr == nil:
		backup = brec
	case errors.Is(berr, storage.ErrNothingToBackup):
		// Zero-value BackupRecord left in place.
	default:
		return WriteReport{}, fmt.Errorf("%w: %v", ErrBackupFailed, berr)
	}

	// Step 11: atomic publish. Mode 0600 is re-asserted regardless of
	// umask by storage.AtomicWrite itself.
	postFP, werr := storage.AtomicWrite(r, plan.Target, newBytes, storage.AtomicWriteOptions{
		Mode:         0o600,
		MustNotExist: plan.MustNotExist,
	})
	if werr != nil {
		return WriteReport{}, werr
	}

	return WriteReport{
		Tool:            plan.Tool,
		Target:          plan.Target,
		Backup:          backup,
		PreFingerprint:  preFP,
		PostFingerprint: postFP,
		Diff:            diff,
		AppliedAt:       now,
	}, nil
}

// mapStorageError translates storage-layer sentinels to writepath ones
// so callers can errors.Is against a single sentinel surface without
// importing internal/storage. Unrecognized errors pass through
// unmodified; errors.Is on storage.ErrTargetExists continues to work
// because AtomicWrite's own wrapper is preserved.
func mapStorageError(err error) error {
	switch {
	case errors.Is(err, storage.ErrLockTimeout):
		return fmt.Errorf("%w: %v", ErrLockTimeout, err)
	case errors.Is(err, storage.ErrOutsideHome) && !errors.Is(err, ErrOutsideHome):
		return fmt.Errorf("%w: %v", ErrOutsideHome, err)
	default:
		return err
	}
}

// readAll reads the file at path. exists=false iff the file is absent;
// any other Stat/Read error surfaces as a non-nil error. Returned
// bytes are nil when the file does not exist so callers can pass
// currentBytes through Transform without allocating an empty slice.
func readAll(path string) ([]byte, bool, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("read %q: %w", path, err)
	}
	return b, true, nil
}
