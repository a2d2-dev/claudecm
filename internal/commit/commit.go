// Package commit is the two-phase cross-file commit orchestrator for
// claudecm. It exists to enforce coding-standards rule 13 (PRD FR-16):
// when a single command touches more than one owned file, every write
// MUST route through this package rather than sequencing
// writepath.Apply calls across files directly.
//
// E7-S2/S3 landed the real Stage + Commit pipeline. Stage acquires every
// per-target flock in canonical order (auth.json → config.toml →
// settings.json), reads current bytes, applies each plan's Transform
// (or takes NewContent verbatim), captures the pre-write fingerprint,
// and writes each backup — but does NOT rename over the target. Commit
// walks the prepared files in canonical order, re-checks the
// concurrency fingerprint under lock, then AtomicWrites each target
// via storage.AtomicWrite (writepath's underlying primitive). On any
// mid-commit failure it rolls back already-committed files in reverse
// canonical order and returns a *PartialFailure.
//
// Rollback source of truth. Rollback restores from the in-memory
// pre-Stage bytes captured under the flock (PreparedFile.CurrentBytes),
// NOT from the on-disk backup file. The backup file is retained for
// audit / operator recovery only. This keeps rollback insensitive to
// any post-Stage tampering with the backup directory and avoids the
// extra open/read syscalls the on-disk path would require. See
// rollbackFile for the mechanism and F1 in PR #44 for the rationale.
//
// Authority. This package is subordinate to:
//  1. docs/decisions/0001-direction-lock.md (ADR-0001).
//  2. docs/prd/prd-v1.md (FR-16, NFR-C1..C3).
//  3. docs/architecture.md §5 (two-phase commit + auth-first ordering).
//  4. docs/architecture/coding-standards.md rule 13 (route multi-file
//     writes through this package).
//  5. internal/writepath/plan.go — frozen WritePlan / WriteReport /
//     DiffResult types this package composes.
//  6. internal/adapter/adapter.go — frozen ToolID constants used by
//     the canonical-order function.
//
// Two-phase model. Phase 1 (Stage) acquires every per-target lock in a
// deterministic order, reads the current bytes under each lock, runs
// the WritePlan.Transform (or takes NewContent verbatim), captures the
// pre-write concurrency fingerprint, and writes a backup — but does
// NOT rename the tempfile over the target. Phase 2 (Commit) walks the
// prepared files in the fixed cross-file order and performs each
// rename. If any rename or post-write reparse fails mid-phase-2, the
// files already committed are rolled back from their backups in
// reverse order, and Commit returns a *PartialFailure whose embedded
// CommitReport enumerates per-file status. Abort discards a staged
// transaction without applying anything — used by `switch --dry-run`
// and by the negative branch of the FR-4 confirmation prompt.
//
// Ordered commit sequence: ~/.codex/auth.json → ~/.codex/config.toml →
// ~/.claude/settings.json. Rationale:
//   - auth.json first because it is the credential file. A downstream
//     config.toml or settings.json failure that fires after auth.json
//     is safely written leaves Codex self-consistent: the new auth
//     pairs with old config, which is a valid intermediate state.
//   - settings.json last because Claude Code is independent of Codex.
//     A Codex-side failure MUST NOT block a Claude Code update; a
//     Claude Code post-write reparse failure MUST NOT re-touch Codex.
//   - Rollback is in reverse (settings.json → config.toml → auth.json)
//     so lock hold order and rollback unwind order are consistent and
//     lock-order inversion (NFR-C1) is impossible.
//
// Adapter groupings: plans that share a Tool are committed in the
// order the adapter provided them. Cross-tool ordering: Codex first
// (both files), then Claude Code.
//
// Detection of "which plan is which owned file" uses only
// WritePlan.Tool and path.Base(WritePlan.Target) — this package does
// not import adapter constants for routing so a future third tool
// can be slotted in by extending canonicalCommitOrder without
// touching adapter code.
//
// State updates. This package does NOT touch state.yaml. Adapters
// that route their per-file writes through commit.Stage/Commit are
// responsible for calling stateio.RecordApplied against every
// successfully-committed WriteReport in the CommitReport.PerFile
// slice. Direct-Apply callers (existing test paths) still update state
// via adapter.Apply — commit.Commit's PerFile Report field carries a
// writepath.WriteReport shape callers can consume without change.
package commit

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"reflect"
	"sort"
	"time"

	"github.com/a2d2-dev/claudecm/internal/adapter"
	"github.com/a2d2-dev/claudecm/internal/storage"
	"github.com/a2d2-dev/claudecm/internal/writepath"
)

// Committer is the two-phase commit orchestrator. Adapters and cmd/*
// (notably cmd/switch and cmd/restore when they touch more than one
// owned file) MUST use this rather than calling writepath.Apply
// sequentially themselves — see coding-standards rule 13 / PRD FR-16.
type Committer interface {
	// Stage validates every WritePlan, acquires locks on the unique
	// targets in canonicalCommitOrder, reads current bytes under each
	// lock, applies each plan's Transform (or takes NewContent
	// verbatim), records the pre-write concurrency fingerprint, and
	// writes each backup. It does NOT rename the tempfiles over the
	// targets — that is Commit's job.
	//
	// Returns a StagedTxn describing what will change (Diff + Backup
	// + PreFingerprint per file) on success. Returns an error, and
	// releases any locks it did acquire, if any plan fails cheap
	// validation, resolves outside HOME, would touch keys not on its
	// OwnedKeys list without WritePlan.AllowUnowned == true, or if
	// a per-file backup fails.
	//
	// Zero-plan input is a documented no-op: Stage returns an empty
	// StagedTxn (zero PreparedFile / zero LockHandle slice) and a nil
	// error. This is what makes cmd/switch --dry-run against a
	// no-change profile a stable, cheap operation.
	//
	// Duplicate targets. Passing multiple plans for the same Target is
	// legal but produces confusing behavior: the second plan for that
	// target will trip drift detection during Commit (its
	// PreFingerprint was captured before the first plan wrote, and the
	// first plan's write invalidated it). Callers SHOULD merge or
	// dedupe plans by Target before calling Stage. Stage does not
	// refuse duplicate-target plans up front so callers can still
	// experiment; TestStage_DuplicateTargetPlansCauseCommitFailure
	// pins the resulting Commit-time failure.
	//
	// Backup-vs-CurrentBytes drift. The backup file on disk may drift
	// from PreparedFile.CurrentBytes if a non-claudecm process mutates
	// the target between the initial read and the storage.Backup step
	// (both happen under our flock, but the flock is advisory).
	// Rollback uses the in-memory CurrentBytes and is unaffected;
	// operators inspecting the backup file for audit should be aware.
	// A future story may add a post-Backup fingerprint check.
	Stage(ctx context.Context, r *storage.Resolver, plans []writepath.WritePlan) (StagedTxn, error)

	// Commit executes the ordered rename phase. It walks StagedTxn's
	// prepared files in canonicalCommitOrder, publishing each rename
	// under the same lock Stage acquired. On success it returns a
	// CommitReport whose PerFile slice enumerates every plan's
	// outcome. On mid-phase-2 failure it rolls back files that were
	// already committed (in reverse order) and returns a
	// *PartialFailure wrapping a CommitReport with each per-file
	// Status set to StatusCommitted, StatusRolledBack, or
	// StatusUntouched.
	//
	// Commit always releases every lock the StagedTxn held before
	// returning, even on error. Commit'ing a zero-plan StagedTxn is a
	// no-op that returns an empty CommitReport and a nil error.
	Commit(ctx context.Context, txn StagedTxn) (CommitReport, error)

	// Abort releases every lock the StagedTxn holds and discards its
	// backups without renaming anything into the target. Backups that
	// were written during Stage remain on disk under
	// ~/.claudecm/backups/ — retention pruning (NFR-R1) will remove
	// them on a later successful Commit or restore run. Abort is
	// idempotent: aborting an already-aborted or already-committed
	// StagedTxn returns nil. Aborting a zero-plan StagedTxn is a
	// no-op that returns nil.
	Abort(txn StagedTxn) error
}

// StagedTxn is the opaque handle Stage returns. It MUST be either
// Commit'd or Abort'd before it goes out of scope — the LockHandles it
// carries hold flocks that Commit / Abort release. Fields are exported
// so callers can inspect what will change (e.g. cmd/switch's
// pre-commit summary) but the txn is treated as a single unit; callers
// MUST NOT mutate the slices between Stage and Commit.
type StagedTxn struct {
	// Plans is the input WritePlan slice, retained in caller-supplied
	// order. Canonical commit ordering is derived at Commit time via
	// canonicalCommitOrder; the caller-visible slice order remains
	// the order the adapter produced.
	Plans []writepath.WritePlan

	// Locks holds one LockHandle per unique target path (deduped so a
	// single-file plan does not double-lock its target). Ordering
	// matches canonicalCommitOrder to keep lock acquisition and
	// release symmetric — release is in reverse (NFR-C1, avoids
	// lock-order inversion).
	Locks []LockHandle

	// Prepared holds one PreparedFile per plan, in Plans-order. The
	// per-file Diff, Backup, and PreFingerprint are populated here so
	// callers can render the pre-commit summary without re-reading
	// any target.
	Prepared []PreparedFile

	// StagedAt is when Stage finished. Populated on the empty-txn
	// path too so callers can log a consistent "staged at" timestamp.
	StagedAt time.Time

	// resolver is the *storage.Resolver Stage was called with. Commit
	// uses it for AtomicWrite (which requires HOME containment) and
	// for backup restore during rollback. Unexported because callers
	// treat the txn as opaque — the resolver dependency is a commit-
	// internal implementation detail, not a caller-facing knob.
	resolver *storage.Resolver
}

// LockHandle wraps a *storage.Handle so Commit and Abort can release
// it. Target is stored redundantly with the underlying handle's path
// so callers rendering the pre-commit summary do not have to poke into
// the storage handle.
//
// The underlying lock handle is unexported; only Commit/Abort may
// Release it. This prevents an external caller from prematurely
// releasing a flock that Stage still expects to hold — the commit
// package alone owns the flock lifecycle.
type LockHandle struct {
	// Target is the absolute path the lock protects.
	Target string

	// handle is the underlying flock. Commit / Abort call
	// handle.Release. Nil for the empty-txn path.
	handle *storage.Handle
}

// PreparedFile is a single plan whose current bytes were read and
// whose transform was applied in memory. The tempfile has NOT yet
// been renamed over the target — that is Commit's phase-2 rename.
type PreparedFile struct {
	// Plan is the plan this PreparedFile derives from.
	Plan writepath.WritePlan

	// CurrentBytes are the target's contents at Stage time. Nil for
	// a first-write plan (target did not exist).
	CurrentBytes []byte

	// Exists is true when the target file existed at Stage time. Used
	// by Commit to distinguish first-write rollback (unlink) from
	// overwrite rollback (restore-via-AtomicWrite) without having to
	// consult the filesystem again under the lock.
	Exists bool

	// NewBytes are the bytes that will be renamed over the target.
	NewBytes []byte

	// PreFingerprint is the storage.Fingerprint recorded at Stage
	// time. Commit re-checks the target against this before renaming
	// (NFR-C2 concurrent-edit detection).
	PreFingerprint storage.Fingerprint

	// Backup is the pre-commit backup record. Zero for first-write
	// plans (nothing to back up) and for Skipped/DryRun plans.
	Backup storage.BackupRecord

	// Diff is what Stage would show in the pre-apply confirmation.
	// Callers rendering the summary read this directly rather than
	// recomputing it against the target.
	Diff writepath.DiffResult

	// Skipped is true when CurrentBytes and NewBytes are byte-equal
	// so Commit will not rename this file. Included so a no-op plan
	// still appears in the CommitReport as StatusUntouched.
	Skipped bool

	// DryRun is true when the input plan.DryRun was true. Commit
	// records this file as StatusUntouched without ever calling
	// AtomicWrite. FR-15.
	DryRun bool
}

// CommitReport describes what Commit did. PerFile enumerates every
// plan's outcome in Plans-order (not commit order — callers key on
// Target, not slice index). RolledBack is true when Commit hit a
// mid-phase-2 failure and unwound successfully; the underlying error
// (see PartialFailure) is where the "why" lives.
type CommitReport struct {
	// PerFile is one entry per plan in the input StagedTxn.Plans
	// order. Length == len(txn.Plans). Zero-length on empty-txn.
	PerFile []PerFileReport

	// CommittedAt is when Commit finished — populated on success and
	// on failure paths where PerFile is populated. Zero when Commit
	// never ran (e.g. Stage returned an error before Commit was
	// called).
	CommittedAt time.Time

	// RolledBack is true when Commit rolled back at least one
	// PREVIOUSLY-COMMITTED file after a mid-phase-2 failure. It does
	// NOT reflect the failing file's own post-write-reparse rollback —
	// that lives in FailingFileRolledBack. Aggregate signal only; the
	// per-file detail lives in each PerFileReport.Status.
	RolledBack bool

	// FailingFileRolledBack is true when the failing file's own
	// post-write reparse rollback ran successfully (i.e. the file was
	// AtomicWritten, reparse rejected it, and rollbackFile restored
	// the pre-Stage bytes). False on all other failure modes:
	// concurrent-edit detected before any write, storage.Stat failure,
	// AtomicWrite failure (nothing to unwind), or post-write reparse
	// rollback that itself failed. Callers distinguish "failing file
	// is now at pre-Stage bytes" from "failing file's on-disk state is
	// undefined" via this flag.
	FailingFileRolledBack bool
}

// PerFileReport carries the outcome for one WritePlan.
type PerFileReport struct {
	// Target is the absolute path this report describes.
	Target string

	// Status enumerates the per-file outcome. See FileStatus.
	Status FileStatus

	// Backup is the pre-commit backup record for this file. Zero for
	// StatusUntouched (no backup was created because nothing was
	// staged) and for first-write plans that never had a target to
	// back up.
	Backup storage.BackupRecord

	// Report is the per-file WriteReport writepath.Apply produced
	// during Commit. Populated when Status == StatusCommitted or
	// StatusRolledBack. Zero otherwise.
	Report writepath.WriteReport

	// Error is the human-readable failure reason when Status ==
	// StatusFailed. Empty otherwise. The underlying error object is
	// available on the returned *PartialFailure via Cause /
	// errors.Unwrap; this string is for rendering only.
	Error string
}

// FileStatus enumerates per-file outcomes. String values are stable
// and grep-friendly across binary rebuilds; renderers may compare
// against these constants without a translation table.
type FileStatus string

// Documented per-file commit outcomes.
const (
	// StatusCommitted means the rename fired and the post-write
	// reparse (FR-5 step 8) succeeded. The file is now at NewBytes.
	StatusCommitted FileStatus = "committed"

	// StatusRolledBack means the rename fired but a downstream file
	// failure required Commit to restore this file from its backup
	// via rename-over-target. The file is now at CurrentBytes.
	StatusRolledBack FileStatus = "rolled-back"

	// StatusUntouched means Commit chose not to rename this file
	// (either PreparedFile.Skipped was true, or Commit aborted before
	// reaching this plan). The file is unchanged from Stage time.
	StatusUntouched FileStatus = "untouched"

	// StatusFailed means the rename or post-write reparse failed on
	// this file and the file could not be rolled back cleanly. The
	// on-disk state is described by PerFileReport.Report; the
	// underlying error is on the returned *PartialFailure.
	StatusFailed FileStatus = "failed"
)

// PartialFailure is returned by Commit when phase 2 fails mid-run.
// It carries a CommitReport describing every file's outcome so callers
// can render the aggregate result without re-running any reads.
//
// The Error message follows a pinned format so cmd/*'s error rendering
// stays stable across releases:
//
//	commit: partial failure on "<target>": <cause>; rolled back <N>, untouched <M>
type PartialFailure struct {
	// Report is the aggregate CommitReport at the point of failure.
	Report CommitReport

	// FailedFile is the absolute path of the target whose commit
	// failed and triggered rollback.
	FailedFile string

	// Cause is the underlying error. errors.Unwrap returns this so
	// callers can errors.Is against writepath sentinels
	// (ErrConcurrentEdit, ErrPostWriteReparse, etc.).
	Cause error

	// RolledBack lists the absolute paths of files that were rolled
	// back after the failure, in reverse-commit order (unwind order).
	RolledBack []string

	// Untouched lists the absolute paths of files that were never
	// attempted because the failure fired before Commit reached them.
	Untouched []string
}

// Error implements the error interface. The exact format is pinned in
// TestPartialFailure_MessageFormat.
func (e *PartialFailure) Error() string {
	if e == nil {
		return "<nil *commit.PartialFailure>"
	}
	return fmt.Sprintf("commit: partial failure on %q: %v; rolled back %d, untouched %d",
		e.FailedFile, e.Cause, len(e.RolledBack), len(e.Untouched))
}

// Unwrap exposes Cause so errors.Is / errors.As reach the sentinel.
func (e *PartialFailure) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Cause
}

// Option customizes a Committer at construction time. E7-S2 populates
// the seam with knobs for tests (a clock injector); callers today may
// pass zero options — the defaults are safe.
type Option func(*committerCfg)

// committerCfg holds Committer construction knobs.
type committerCfg struct {
	// now is the clock the pipeline reads for StagedAt / CommittedAt.
	// Tests may swap for determinism; nil defaults to time.Now.
	now func() time.Time
}

// WithClock injects a deterministic clock for tests. Production code
// leaves the clock defaulted to time.Now.
func WithClock(now func() time.Time) Option {
	return func(c *committerCfg) {
		c.now = now
	}
}

// NewCommitter returns the default two-phase Committer.
//
// The variadic Option argument is the seam for test-only knobs (see
// WithClock). Callers in production pass no options.
func NewCommitter(opts ...Option) Committer {
	cfg := committerCfg{now: time.Now}
	for _, opt := range opts {
		opt(&cfg)
	}
	if cfg.now == nil {
		cfg.now = time.Now
	}
	return &committer{cfg: cfg}
}

// committer is the real two-phase implementation. Zero package-level
// mutable state; every knob comes off cfg.
type committer struct {
	cfg committerCfg
}

// Stage implements Committer.Stage.
func (c *committer) Stage(ctx context.Context, r *storage.Resolver, plans []writepath.WritePlan) (StagedTxn, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return StagedTxn{}, err
	}
	if len(plans) == 0 {
		return StagedTxn{StagedAt: c.cfg.now()}, nil
	}
	if r == nil {
		return StagedTxn{}, fmt.Errorf("%w: resolver is nil", writepath.ErrPlanInvalid)
	}

	// Cheap validation first — fail before touching disk.
	for i := range plans {
		if err := writepath.ValidatePlan(plans[i]); err != nil {
			return StagedTxn{}, err
		}
	}

	orderIdx := canonicalCommitOrder(plans)

	// Determine the per-txn lock timeout. Honor ctx.Deadline() when
	// set; otherwise use storage.DefaultLockTimeout. An already-
	// expired deadline short-circuits with ErrLockTimeout joined to
	// ctx.Err() — matching writepath.Apply's contract.
	lockOpts := storage.LockOptions{}
	if deadline, ok := ctx.Deadline(); ok {
		d := time.Until(deadline)
		if d <= 0 {
			return StagedTxn{}, errors.Join(writepath.ErrLockTimeout, ctx.Err())
		}
		lockOpts.Timeout = d
	}

	// Acquire flocks in canonical order, deduped per target. If any
	// acquire fails, release the locks we already hold and return.
	locks := make([]LockHandle, 0, len(orderIdx))
	seen := make(map[string]struct{}, len(orderIdx))
	for _, idx := range orderIdx {
		target := plans[idx].Target
		if _, dup := seen[target]; dup {
			continue
		}
		seen[target] = struct{}{}

		lockRel, relErr := filepath.Rel(r.Home(), target)
		if relErr != nil {
			releaseLocks(locks)
			return StagedTxn{}, fmt.Errorf("%w: rel %q vs HOME: %v", writepath.ErrOutsideHome, target, relErr)
		}
		h, aerr := storage.Acquire(r, lockRel, lockOpts)
		if aerr != nil {
			releaseLocks(locks)
			if errors.Is(aerr, storage.ErrLockTimeout) {
				return StagedTxn{}, fmt.Errorf("%w: %v", writepath.ErrLockTimeout, aerr)
			}
			if errors.Is(aerr, storage.ErrOutsideHome) {
				return StagedTxn{}, fmt.Errorf("%w: %v", writepath.ErrOutsideHome, aerr)
			}
			return StagedTxn{}, aerr
		}
		locks = append(locks, LockHandle{Target: target, handle: h})
	}

	// Phase-1 per-plan work. Prepared stays in Plans-order so callers
	// can key by input index without re-sorting; the visit order is
	// canonical (auth-first) so lock hold order and work order agree.
	prepared := make([]PreparedFile, len(plans))
	for _, idx := range orderIdx {
		plan := plans[idx]

		currentBytes, exists, err := readAll(plan.Target)
		if err != nil {
			releaseLocks(locks)
			return StagedTxn{}, err
		}
		preFP, _, err := storage.Stat(plan.Target)
		if err != nil {
			releaseLocks(locks)
			return StagedTxn{}, err
		}

		// Compute new bytes. Transform wins over NewContent; a
		// Transform error is fatal — no fallback to NewContent
		// (CLAUDE.md "no fallback" rule / NFR-S1).
		var newBytes []byte
		if plan.Transform != nil {
			nb, terr := plan.Transform(currentBytes)
			if terr != nil {
				releaseLocks(locks)
				return StagedTxn{}, fmt.Errorf("commit: transform for %q: %w", plan.Target, terr)
			}
			newBytes = nb
		} else {
			newBytes = plan.NewContent
		}

		// Diff via Flatten + Diff, gated on Parser presence.
		var diff writepath.DiffResult
		if plan.Parser != nil {
			curFlat := map[string]any{}
			if exists {
				curParsed, perr := plan.Parser.Parse(currentBytes)
				if perr != nil {
					releaseLocks(locks)
					return StagedTxn{}, fmt.Errorf("%w: parse current %q: %v", writepath.ErrParseFailed, plan.Target, perr)
				}
				cf, ferr := writepath.Flatten(curParsed)
				if ferr != nil {
					releaseLocks(locks)
					return StagedTxn{}, fmt.Errorf("%w: flatten current %q: %v", writepath.ErrParseFailed, plan.Target, ferr)
				}
				curFlat = cf
			}
			newParsed, perr := plan.Parser.Parse(newBytes)
			if perr != nil {
				releaseLocks(locks)
				return StagedTxn{}, fmt.Errorf("%w: parse new %q: %v", writepath.ErrParseFailed, plan.Target, perr)
			}
			newFlat, ferr := writepath.Flatten(newParsed)
			if ferr != nil {
				releaseLocks(locks)
				return StagedTxn{}, fmt.Errorf("%w: flatten new %q: %v", writepath.ErrParseFailed, plan.Target, ferr)
			}
			diff = writepath.Diff(curFlat, newFlat, plan.OwnedKeys)
		}

		bytesEqual := exists && bytes.Equal(currentBytes, newBytes)
		diffEmpty := plan.Parser != nil && exists && reflect.DeepEqual(diff, writepath.DiffResult{})
		skipped := bytesEqual || diffEmpty

		// Unowned-touched guard mirrors writepath's step-5 check. The
		// guard fires whether or not DryRun is set — a dry-run over
		// unowned keys is still a diff we refuse to render without opt-
		// in, matching writepath's behavior for parity across the two
		// code paths. Skipped plans (no bytes will move) bypass the
		// guard: TouchesUnowned against unchanged content is a no-op.
		if !skipped && diff.TouchesUnowned && !plan.AllowUnowned && !plan.DryRun {
			releaseLocks(locks)
			return StagedTxn{}, fmt.Errorf("%w: %s", writepath.ErrDryRunUnownedTouched, plan.Target)
		}

		pf := PreparedFile{
			Plan:           plan,
			CurrentBytes:   currentBytes,
			Exists:         exists,
			NewBytes:       newBytes,
			PreFingerprint: preFP,
			Diff:           diff,
			Skipped:        skipped,
			DryRun:         plan.DryRun,
		}

		// Backup only when we will actually write during Commit. A
		// Skipped file will not be renamed, and a DryRun file will
		// never be renamed by contract (FR-15) — no need to snapshot
		// pre-write bytes.
		if !skipped && !plan.DryRun {
			brec, berr := storage.Backup(r, plan.Tool, filepath.Base(plan.Target), plan.Target)
			switch {
			case berr == nil:
				pf.Backup = brec
			case errors.Is(berr, storage.ErrNothingToBackup):
				// First write; zero backup left in place. Rollback
				// path will os.Remove the target rather than restore.
			default:
				releaseLocks(locks)
				return StagedTxn{}, fmt.Errorf("%w: %v", writepath.ErrBackupFailed, berr)
			}
		}

		prepared[idx] = pf
	}

	return StagedTxn{
		Plans:    plans,
		Locks:    locks,
		Prepared: prepared,
		StagedAt: c.cfg.now(),
		resolver: r,
	}, nil
}

// Commit implements Committer.Commit. See the interface doc for the
// contract. The txn's locks are released before Commit returns (either
// via the deferred releaseLocks, or explicitly on the ctx-cancel bail).
func (c *committer) Commit(ctx context.Context, txn StagedTxn) (CommitReport, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if len(txn.Plans) == 0 && len(txn.Prepared) == 0 && len(txn.Locks) == 0 {
		return CommitReport{}, nil
	}
	if err := ctx.Err(); err != nil {
		releaseLocks(txn.Locks)
		return CommitReport{}, err
	}
	// Every path below releases the locks the txn holds. Deferring
	// here is safer than sprinkling releaseLocks(txn.Locks) at every
	// return.
	defer releaseLocks(txn.Locks)

	if txn.resolver == nil {
		// Only possible if a caller synthesised a StagedTxn by hand;
		// the real Stage always populates the resolver. Refuse loudly.
		return CommitReport{}, fmt.Errorf("commit: staged txn has no resolver (was Stage bypassed?)")
	}
	// Shape check: hand-crafted StagedTxns (e.g. from tests) can carry
	// mismatched Plans/Prepared lengths, which would panic later at
	// txn.Prepared[idx]. Reject loudly instead. Real Stage always
	// produces len(Plans) == len(Prepared).
	if len(txn.Prepared) != len(txn.Plans) {
		return CommitReport{}, fmt.Errorf("commit: staged txn shape mismatch: %d plans vs %d prepared", len(txn.Plans), len(txn.Prepared))
	}

	// One clock read for the whole commit — every per-file AppliedAt
	// and the aggregate CommittedAt share this instant so timestamps
	// are monotonic within a single Commit call (F6).
	now := c.cfg.now()

	orderIdx := canonicalCommitOrder(txn.Plans)
	perFile := make([]PerFileReport, len(txn.Plans))
	// committedOrderPositions records the canonical-order positions
	// (indices into orderIdx) whose targets have been published. On a
	// mid-run failure we walk this slice in reverse to roll back.
	committedOrderPositions := make([]int, 0, len(orderIdx))

	for canonPos, idx := range orderIdx {
		pf := txn.Prepared[idx]
		plan := pf.Plan

		if pf.Skipped || pf.DryRun {
			// F8: for Skipped files, re-Stat the target and verify the
			// pre-Stage fingerprint. A non-claudecm actor could have
			// mutated the target between Stage and Commit even though
			// we will not write it — the operator deserves to see that
			// drift promoted to StatusFailed rather than a silent
			// StatusUntouched. DryRun keeps the fast path: we are not
			// writing, so drift does not compromise the report's
			// honesty (the diff shown was against the Stage-time
			// bytes; a DryRun over a since-modified file is still a
			// truthful "here is what we WOULD have done, from that
			// pre-Stage baseline").
			if pf.Skipped {
				curFP, curExists, cerr := storage.Stat(plan.Target)
				if cerr != nil {
					return c.failCommit(canonPos, idx, orderIdx, txn, perFile, committedOrderPositions, cerr, now, false)
				}
				if driftErr := detectDrift(plan.Target, pf.Exists, pf.PreFingerprint, curExists, curFP); driftErr != nil {
					wrapped := fmt.Errorf("external drift on skipped file: %w", driftErr)
					return c.failCommit(canonPos, idx, orderIdx, txn, perFile, committedOrderPositions, wrapped, now, false)
				}
			}
			perFile[idx] = PerFileReport{
				Target: plan.Target,
				Status: StatusUntouched,
				Backup: pf.Backup,
				Report: writepath.WriteReport{
					Tool:            plan.Tool,
					Target:          plan.Target,
					DryRun:          pf.DryRun,
					Skipped:         pf.Skipped,
					Backup:          pf.Backup,
					PreFingerprint:  pf.PreFingerprint,
					PostFingerprint: pf.PreFingerprint,
					Diff:            pf.Diff,
					AppliedAt:       now,
				},
			}
			continue
		}

		// Concurrent-edit re-check (NFR-C2, AC E7-S3 #1). Under the
		// lock nothing else claudecm-owned can race us, but a non-
		// claudecm actor could have written the target between Stage
		// and Commit.
		curFP, curExists, cerr := storage.Stat(plan.Target)
		if cerr != nil {
			return c.failCommit(canonPos, idx, orderIdx, txn, perFile, committedOrderPositions, cerr, now, false)
		}
		if driftErr := detectDrift(plan.Target, pf.Exists, pf.PreFingerprint, curExists, curFP); driftErr != nil {
			return c.failCommit(canonPos, idx, orderIdx, txn, perFile, committedOrderPositions, driftErr, now, false)
		}

		// Publish. storage.AtomicWrite is the primitive writepath.Apply
		// also funnels through; the lock is held externally by us
		// (writepath's Apply owns its own lock — we own ours here).
		postFP, werr := storage.AtomicWrite(txn.resolver, plan.Target, pf.NewBytes, storage.AtomicWriteOptions{
			Mode:         0o600,
			MustNotExist: plan.MustNotExist,
		})
		if werr != nil {
			return c.failCommit(canonPos, idx, orderIdx, txn, perFile, committedOrderPositions, werr, now, false)
		}

		// Post-write reparse when Parser is present. Reparse failure
		// triggers rollback of THIS file (which was just AtomicWritten
		// but has bad bytes on disk) plus every previously-committed
		// file, matching writepath.Apply's step-8 policy. The status
		// stays StatusFailed on the failing file — the on-disk bytes
		// are restored for atomicity, but the caller-facing signal is
		// still "this file's write failed"; StatusRolledBack is for
		// files that committed successfully and were rolled back only
		// because a LATER file failed. The two statuses are
		// deliberately distinct so operators can tell the failing file
		// apart from the collateral-damage ones.
		if plan.Parser != nil {
			if rerr := reparseTarget(plan); rerr != nil {
				errMsg := rerr.Error()
				rbErr := rollbackFile(txn.resolver, pf)
				failingRolledBack := rbErr == nil
				if rbErr != nil {
					errMsg = fmt.Sprintf("%v; rollback also failed: %v", rerr, rbErr)
				}
				perFile[idx] = PerFileReport{
					Target: plan.Target,
					Status: StatusFailed,
					Backup: pf.Backup,
					Error:  errMsg,
				}
				return c.failCommit(canonPos, idx, orderIdx, txn, perFile, committedOrderPositions, rerr, now, failingRolledBack)
			}
		}

		perFile[idx] = PerFileReport{
			Target: plan.Target,
			Status: StatusCommitted,
			Backup: pf.Backup,
			Report: writepath.WriteReport{
				Tool:            plan.Tool,
				Target:          plan.Target,
				Backup:          pf.Backup,
				PreFingerprint:  pf.PreFingerprint,
				PostFingerprint: postFP,
				Diff:            pf.Diff,
				AppliedAt:       now,
			},
		}
		committedOrderPositions = append(committedOrderPositions, canonPos)
	}

	return CommitReport{
		PerFile:     perFile,
		CommittedAt: now,
	}, nil
}

// failCommit handles a mid-commit failure at canonical-order position
// canonPos (input-slice index failedIdx). It records the failed file
// as StatusFailed (unless already recorded by the caller), rolls back
// every previously-committed file in reverse canonical order, marks
// every not-yet-visited file as StatusUntouched, and returns a
// *PartialFailure wrapping the CommitReport.
//
// The now parameter is captured once at the top of Commit and reused
// for CommittedAt so a failed-commit report shares its parent's clock
// read (F6). failingRolledBack signals whether the failing file's own
// unwind ran (only true on the post-write reparse path where the file
// was AtomicWritten and rollbackFile succeeded); it drives the
// CommitReport.FailingFileRolledBack UX flag (F4).
//
// Rollback source: each per-file rollback restores from
// PreparedFile.CurrentBytes captured under the flock at Stage time —
// NOT from the on-disk backup file. See the package godoc "Rollback
// source of truth" paragraph (F1).
func (c *committer) failCommit(
	canonPos int,
	failedIdx int,
	orderIdx []int,
	txn StagedTxn,
	perFile []PerFileReport,
	committedOrderPositions []int,
	cause error,
	now time.Time,
	failingRolledBack bool,
) (CommitReport, error) {
	// Ensure the failed file has a row. Callers that pre-populated
	// (post-write reparse path already set StatusFailed with the
	// combined error string) are respected; only overwrite when the
	// row is still zero-value (concurrent edit / AtomicWrite failure
	// paths where nothing was published).
	if perFile[failedIdx].Status == "" {
		perFile[failedIdx] = PerFileReport{
			Target: txn.Prepared[failedIdx].Plan.Target,
			Status: StatusFailed,
			Backup: txn.Prepared[failedIdx].Backup,
			Error:  cause.Error(),
		}
	}
	rolled := make([]string, 0, len(committedOrderPositions))

	// Roll back previously-committed files in reverse canonical
	// order. Each rollback restores from the in-memory
	// PreparedFile.CurrentBytes captured under the flock at Stage
	// time; on the first-write case (Exists=false) rollback removes
	// the target. The on-disk backup file is audit-only (F1).
	for i := len(committedOrderPositions) - 1; i >= 0; i-- {
		pos := committedOrderPositions[i]
		idx := orderIdx[pos]
		pf := txn.Prepared[idx]
		if rerr := rollbackFile(txn.resolver, pf); rerr != nil {
			// Rollback failure: state is undefined for this file.
			// Mark it StatusFailed with the rollback cause and keep
			// walking — a later rollback may still succeed.
			perFile[idx] = PerFileReport{
				Target: pf.Plan.Target,
				Status: StatusFailed,
				Backup: pf.Backup,
				Error:  fmt.Sprintf("rollback failed: %v", rerr),
			}
			continue
		}
		perFile[idx].Status = StatusRolledBack
		rolled = append(rolled, pf.Plan.Target)
	}

	// Not-yet-visited files stay untouched (they were staged but
	// never renamed to their target — the on-disk state is still the
	// pre-Stage bytes).
	untouched := make([]string, 0)
	for pos := canonPos + 1; pos < len(orderIdx); pos++ {
		idx := orderIdx[pos]
		pf := txn.Prepared[idx]
		perFile[idx] = PerFileReport{
			Target: pf.Plan.Target,
			Status: StatusUntouched,
			Backup: pf.Backup,
		}
		untouched = append(untouched, pf.Plan.Target)
	}

	report := CommitReport{
		PerFile:               perFile,
		CommittedAt:           now,
		RolledBack:            len(rolled) > 0,
		FailingFileRolledBack: failingRolledBack,
	}
	return report, &PartialFailure{
		Report:     report,
		FailedFile: txn.Prepared[failedIdx].Plan.Target,
		Cause:      cause,
		RolledBack: rolled,
		Untouched:  untouched,
	}
}

// rollbackFile restores a previously-committed PreparedFile to its
// pre-Stage state. Overwrite case: AtomicWrite the CurrentBytes back
// over the target. First-write case (Exists=false): unlink the target.
// Called under the txn's flock — no fresh lock is acquired.
func rollbackFile(r *storage.Resolver, pf PreparedFile) error {
	if !pf.Exists {
		if err := os.Remove(pf.Plan.Target); err != nil {
			return fmt.Errorf("remove %q: %w", pf.Plan.Target, err)
		}
		return nil
	}
	if _, err := storage.AtomicWrite(r, pf.Plan.Target, pf.CurrentBytes, storage.AtomicWriteOptions{
		Mode:         0o600,
		MustNotExist: false,
	}); err != nil {
		return fmt.Errorf("restore %q: %w", pf.Plan.Target, err)
	}
	return nil
}

// reparseTarget mirrors writepath.reparseTarget: re-read the just-
// written target and feed it through plan.Parser. Read/parse failures
// both wrap ErrPostWriteReparse. Kept as a local helper so this
// package does not depend on writepath's package-private surface.
func reparseTarget(plan writepath.WritePlan) error {
	b, exists, err := readAll(plan.Target)
	if err != nil {
		return fmt.Errorf("%w: reread %q: %v", writepath.ErrPostWriteReparse, plan.Target, err)
	}
	if !exists {
		return fmt.Errorf("%w: target %q vanished after atomic write", writepath.ErrPostWriteReparse, plan.Target)
	}
	if _, err := plan.Parser.Parse(b); err != nil {
		return fmt.Errorf("%w: parse %q: %v", writepath.ErrPostWriteReparse, plan.Target, err)
	}
	return nil
}

// Abort implements Committer.Abort. See the interface doc for the
// contract.
func (c *committer) Abort(txn StagedTxn) error {
	releaseLocks(txn.Locks)
	return nil
}

// releaseLocks releases every non-nil handle in the slice. Errors are
// intentionally swallowed: Abort/Commit's callers treat a lock-release
// failure as diagnostic noise (the process is exiting or moving on)
// rather than a fatal error, and the E7-S1 LockHandle godoc pins
// Release as idempotent.
func releaseLocks(handles []LockHandle) {
	// Release in reverse order — NFR-C1 lock-order inversion avoidance.
	for i := len(handles) - 1; i >= 0; i-- {
		if handles[i].handle != nil {
			_ = handles[i].handle.Release()
		}
	}
}

// readAll reads the file at path. exists=false iff the file is absent;
// any other Stat/Read error surfaces as a non-nil error. Returned
// bytes are nil when the file does not exist. Mirrors writepath.readAll
// verbatim — kept as a local helper so this package does not depend on
// writepath's package-private surface.
func readAll(p string) ([]byte, bool, error) {
	b, err := os.ReadFile(p)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("read %q: %w", p, err)
	}
	return b, true, nil
}

// detectDrift is the commit-package sibling of writepath.detectDrift.
// See writepath/apply.go for the policy narrative; kept as a local
// helper so this package does not depend on writepath's package-
// private surface.
func detectDrift(target string, prevExists bool, preFP storage.Fingerprint, curExists bool, curFP storage.Fingerprint) error {
	if !prevExists && !curExists {
		return nil
	}
	if !prevExists && curExists {
		return fmt.Errorf(
			"%w: target %q appeared under lock (pre-write did not exist; current size=%d sha256=%s mtime=%s)",
			writepath.ErrConcurrentEdit, target,
			curFP.Size, curFP.SHA256, curFP.ModTime.Format(time.RFC3339Nano),
		)
	}
	if prevExists && !curExists {
		return fmt.Errorf(
			"%w: target %q vanished under lock (pre-write size=%d sha256=%s mtime=%s)",
			writepath.ErrConcurrentEdit, target,
			preFP.Size, preFP.SHA256, preFP.ModTime.Format(time.RFC3339Nano),
		)
	}
	if preFP.Size != curFP.Size || preFP.SHA256 != curFP.SHA256 || !preFP.ModTime.Equal(curFP.ModTime) {
		return fmt.Errorf(
			"%w: target %q fingerprint drift under lock: pre=(size=%d sha256=%s mtime=%s) current=(size=%d sha256=%s mtime=%s)",
			writepath.ErrConcurrentEdit, target,
			preFP.Size, preFP.SHA256, preFP.ModTime.Format(time.RFC3339Nano),
			curFP.Size, curFP.SHA256, curFP.ModTime.Format(time.RFC3339Nano),
		)
	}
	return nil
}

// canonicalCommitOrder returns indices into plans sorted into the
// fixed cross-file commit order documented in the package godoc:
//
//	0: codex ~/.codex/auth.json
//	1: codex ~/.codex/config.toml
//	2: claude_code ~/.claude/settings.json
//	3: any other tool/file combination (stable order by index)
//
// Detection uses only WritePlan.Tool and path.Base(WritePlan.Target)
// so this package stays adapter-agnostic — a future third tool slots
// in by extending this function alone.
//
// The sort is stable so within the same priority bucket, adapter-
// supplied ordering is preserved.
func canonicalCommitOrder(plans []writepath.WritePlan) []int {
	indices := make([]int, len(plans))
	for i := range plans {
		indices[i] = i
	}
	sort.SliceStable(indices, func(a, b int) bool {
		return commitPriority(plans[indices[a]]) < commitPriority(plans[indices[b]])
	})
	return indices
}

// commitPriority returns the canonical priority bucket for one plan.
// Lower is earlier. Bucket 3 catches "unknown" (unrecognized tool or
// unrecognized basename); such plans commit last in their input
// order.
//
// The tool comparison routes through the typed adapter.ToolCodex /
// adapter.ToolClaudeCode constants (via string() because WritePlan.Tool
// is a bare string field, not a ToolID) so that a rename of either
// constant's underlying value breaks this file at compile time — or,
// failing that, is caught by TestCommitPriority_TracksAdapterConstants
// below. That drift detector is deliberate: commit ordering is auth-
// first, and a silent adapter ToolID change would otherwise re-order
// writes without any test failure.
func commitPriority(p writepath.WritePlan) int {
	base := path.Base(p.Target)
	switch adapter.ToolID(p.Tool) {
	case adapter.ToolCodex:
		switch base {
		case "auth.json":
			return 0
		case "config.toml":
			return 1
		}
	case adapter.ToolClaudeCode:
		if base == "settings.json" {
			return 2
		}
	}
	return 3
}
