// apply.go implements FR-5 steps 1-10 of the locked write-path pipeline
// (Story E2-S2 steps 1-7 + Story E2-S3 steps 8-10; E2-S4 concurrent-edit
// drift detection still deferred). It is the single legal path to disk
// for any tool-owned config file claudecm mutates.
//
// OS-Rename discipline: only internal/storage/atomic.go and this file
// may call os.Rename on tool-owned paths. Enforced by
// scripts/lint-osrename.sh (wired into `make lint`).
//
// Story E2-S2 steps 1-7 (implemented here):
//
//  1. ValidatePlan — cheap pre-execution refuse. Wraps ErrPlanInvalid.
//  2. Acquire flock via storage.WithLock on the target sidecar. Timeout
//     is derived from ctx.Deadline() when set, otherwise
//     storage.DefaultLockTimeout. Timeout maps to ErrLockTimeout.
//     ctx.Deadline() (when set) caps the flock acquisition timeout; an
//     already-expired deadline returns ErrLockTimeout without attempting
//     to acquire.
//  3. Read current bytes (may not exist → treated as "empty" for the
//     Transform seam) and capture pre-write Fingerprint via
//     storage.Stat. Non-existence yields a zero Fingerprint with the
//     exists=false signal. Symlink escape is enforced twice — by
//     storage.Acquire's EnsureDir under HOME and by storage.AtomicWrite
//     on the parent — and mapped to ErrOutsideHome here so callers can
//     errors.Is on one sentinel without importing internal/storage.
//  4. Compute new bytes: plan.Transform wins over plan.NewContent (see
//     plan.go package doc). Transform errors abort — no fallback to
//     NewContent, per CLAUDE.md "no fallback" rule.
//  5. Parse current + new via plan.Parser. Non-nil Parser is mandatory
//     for a meaningful Diff; a nil Parser skips diff computation and
//     the skip-on-identical-bytes shortcut becomes the only "no-op"
//     signal. On parse failure of EITHER side we wrap the parser error
//     with ErrParseFailed and abort. NFR-S1: no silent rewrite. Then
//     Flatten + Diff. A byte-identical current==new against an existing
//     file, OR a parsed-empty diff against an existing file, short-
//     circuits to Skipped=true with no backup and no write;
//     PostFingerprint mirrors PreFingerprint. A first write (no prior
//     file) is NEVER skipped even if diff is empty — the atomic publish
//     still creates the file at 0600.
//  6. DryRun=true returns a report populated with the diff but WITHOUT
//     backing up or writing. Callers use this for FR-15.
//     Diff.TouchesUnowned=true AND AllowUnowned=false AND DryRun=false
//     is refused with ErrDryRunUnownedTouched. The sentinel name is
//     kept for API stability even though it fires outside dry-run.
//  7. storage.Backup snapshots the pre-write bytes. ErrNothingToBackup
//     means "first write, no prior state" and produces a zero
//     BackupRecord. Any other backup failure wraps ErrBackupFailed and
//     aborts before touching the target. Then storage.AtomicWrite
//     publishes the new bytes at mode 0600, honoring MustNotExist.
//     AtomicWrite's own ErrTargetExists propagates unwrapped so callers
//     can errors.Is against storage.ErrTargetExists. storage.Stat
//     captures the post-write Fingerprint for the report.
//
// Story E2-S3 steps 8-10 (implemented here):
//
//  8. Post-write reparse: re-read the target bytes from disk via
//     os.ReadFile, then feed them through plan.Parser. Skipped entirely
//     when plan.Parser == nil (adapter opts out; adapter takes
//     responsibility for post-write correctness). Also skipped when
//     Skipped=true (no write happened) and when DryRun=true (nothing
//     was written). Any reparse failure — read error or parse error —
//     joins into ErrPostWriteReparse and triggers step 10.
//  9. (E2-S4, deferred) Concurrent-edit fingerprint drift check.
//  10. Auto-rollback from the step-7 backup on any step-8 failure:
//      - When Backup is zero-value (first-write case), rollback =
//        os.Remove(plan.Target). A Remove failure surfaces as
//        ErrRollbackFailed joined with ErrPostWriteReparse and the
//        original failure.
//      - Otherwise, restore the backup bytes over the target via
//        storage.AtomicWrite (mode 0600, MustNotExist=false). A restore
//        failure surfaces as ErrRollbackFailed joined with
//        ErrPostWriteReparse and the original failure. No WriteReport
//        is returned in this state; on-disk state is undefined and the
//        caller must not retry blindly.
//      - On a successful rollback, Apply returns a WriteReport whose
//        RolledBack=true and PostFingerprint mirrors PreFingerprint
//        (state restored), plus errors.Join(ErrPostWriteReparse,
//        ErrRollback, originalErr) so callers can errors.Is against
//        any of the three.
//      Rollback runs INSIDE the flock held by WithLock; it does NOT
//      trigger a fresh Backup (we already have the one we need).
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
		d := time.Until(deadline)
		if d <= 0 {
			// An already-expired deadline must not silently degrade to
			// storage.DefaultLockTimeout — that would cost the caller ~5s
			// of blocking on a context they already gave up on. Short-
			// circuit with ErrLockTimeout joined to ctx.Err() so callers
			// can errors.Is against either sentinel.
			return WriteReport{}, errors.Join(ErrLockTimeout, ctx.Err())
		}
		lockOpts.Timeout = d
	}

	var report WriteReport
	fnErr := storage.WithLock(r, lockRel, lockOpts, func() error {
		rep, aerr := applyLocked(r, plan)
		report = rep
		return aerr
	})
	if fnErr != nil {
		// Preserve the report on a successful rollback so callers can
		// see RolledBack=true and read PreFingerprint/Backup metadata.
		// A failed rollback (or any non-rollback error) returns a zero
		// report — callers must not confuse "state restored" with
		// "state undefined".
		if report.RolledBack {
			return report, mapStorageError(fnErr)
		}
		return WriteReport{}, mapStorageError(fnErr)
	}
	return report, nil
}

// applyLocked runs steps 3-7 under the assumption the lock is held.
// Split out so Apply itself remains a linear lock+dispatch shell.
func applyLocked(r *storage.Resolver, plan WritePlan) (WriteReport, error) {
	now := time.Now()

	// Step 3: read current bytes + pre-write fingerprint.
	currentBytes, exists, err := readAll(plan.Target)
	if err != nil {
		return WriteReport{}, err
	}
	preFP, _, err := storage.Stat(plan.Target)
	if err != nil {
		return WriteReport{}, err
	}

	// Step 4: compute new bytes. Transform wins over NewContent; a
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

	// Step 5 (parse+diff): parse current + new. Skipped entirely when Parser is nil.
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

	// Step 5 (skip guard): skip iff bytes byte-identical AND file existed,
	// or (Parser present AND parsed diff is empty AND file existed). Both
	// halves gate on `exists` because on a first write, "empty diff
	// against nothing" MUST still publish the file. Story E2-S2 AC:
	// legitimate zero-byte / empty-doc first write still goes through
	// the atomic publish so the file appears at 0600.
	bytesEqual := exists && bytes.Equal(currentBytes, newBytes)
	diffEmpty := plan.Parser != nil && exists && reflect.DeepEqual(diff, DiffResult{})
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

	// Step 6 (dry-run): short-circuits before backup and write.
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

	// Step 6 (unowned-touched guard). Refuse the write unless the caller
	// pre-authorized via AllowUnowned. Sentinel name kept for API
	// stability (see plan.go ErrDryRunUnownedTouched doc).
	if diff.TouchesUnowned && !plan.AllowUnowned {
		return WriteReport{}, fmt.Errorf("%w: %s", ErrDryRunUnownedTouched, plan.Target)
	}

	// Step 7 (backup): backup pre-write state. ErrNothingToBackup is fine —
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

	// Step 7 (atomic publish): mode 0600 is re-asserted regardless of
	// umask by storage.AtomicWrite itself. Post-write Stat captures the
	// PostFingerprint for the report.
	postFP, werr := storage.AtomicWrite(r, plan.Target, newBytes, storage.AtomicWriteOptions{
		Mode:         0o600,
		MustNotExist: plan.MustNotExist,
	})
	if werr != nil {
		return WriteReport{}, werr
	}

	// Steps 8-10: post-write reparse + auto-rollback. Skipped entirely
	// when plan.Parser is nil (adapter opts out; the same rule as the
	// pre-write parse). Rollback runs inside the flock we already hold,
	// against the just-taken backup — no fresh backup is captured.
	if plan.Parser != nil {
		if reparseErr := reparseTarget(plan); reparseErr != nil {
			rep, rerr := rollback(r, plan, currentBytes, exists, backup, preFP, diff, now, reparseErr)
			return rep, rerr
		}
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

// reparseTarget re-reads plan.Target from disk and runs plan.Parser
// against the fresh bytes. A read failure or a parse failure both wrap
// ErrPostWriteReparse so the caller can errors.Is on the single sentinel.
// Called only when plan.Parser != nil.
func reparseTarget(plan WritePlan) error {
	b, err := os.ReadFile(plan.Target)
	if err != nil {
		return fmt.Errorf("%w: reread %q: %v", ErrPostWriteReparse, plan.Target, err)
	}
	if _, err := plan.Parser.Parse(b); err != nil {
		return fmt.Errorf("%w: parse %q: %v", ErrPostWriteReparse, plan.Target, err)
	}
	return nil
}

// rollback restores the pre-write state after a post-write reparse
// failure. It is called inside the flock; no fresh Backup is taken.
//
//   - First-write case (backup zero-value): remove the target. The
//     step-3 read reported the file did not exist, so removing brings
//     the tree back to that state.
//   - Overwrite case: restore the pre-write bytes over the target via
//     AtomicWrite (mode 0600, MustNotExist=false). currentBytes was
//     captured under the same lock in step 3, so it's the authoritative
//     pre-write payload; we prefer it over re-reading the backup file
//     (fewer failure modes, and the backup path is retained on the
//     WriteReport regardless).
//
// On success returns a WriteReport with RolledBack=true and
// PostFingerprint == PreFingerprint plus
// errors.Join(ErrPostWriteReparse, ErrRollback, reparseErr). On failure
// returns a zero WriteReport plus errors.Join(ErrPostWriteReparse,
// ErrRollbackFailed, reparseErr).
func rollback(
	r *storage.Resolver,
	plan WritePlan,
	currentBytes []byte,
	exists bool,
	backup storage.BackupRecord,
	preFP storage.Fingerprint,
	diff DiffResult,
	appliedAt time.Time,
	reparseErr error,
) (WriteReport, error) {
	if !exists {
		// First-write case: revert to "no file". Zero-value backup is
		// expected here (Backup returned ErrNothingToBackup in step 7).
		if err := os.Remove(plan.Target); err != nil {
			return WriteReport{}, errors.Join(
				reparseErr,
				fmt.Errorf("%w: remove %q: %v", ErrRollbackFailed, plan.Target, err),
			)
		}
		return WriteReport{
			Tool:            plan.Tool,
			Target:          plan.Target,
			Backup:          backup,
			PreFingerprint:  preFP,
			PostFingerprint: preFP,
			Diff:            diff,
			AppliedAt:       appliedAt,
			RolledBack:      true,
		}, errors.Join(reparseErr, ErrRollback)
	}
	if _, err := storage.AtomicWrite(r, plan.Target, currentBytes, storage.AtomicWriteOptions{
		Mode:         0o600,
		MustNotExist: false,
	}); err != nil {
		return WriteReport{}, errors.Join(
			reparseErr,
			fmt.Errorf("%w: restore %q: %v", ErrRollbackFailed, plan.Target, err),
		)
	}
	return WriteReport{
		Tool:            plan.Tool,
		Target:          plan.Target,
		Backup:          backup,
		PreFingerprint:  preFP,
		PostFingerprint: preFP,
		Diff:            diff,
		AppliedAt:       appliedAt,
		RolledBack:      true,
	}, errors.Join(reparseErr, ErrRollback)
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
