// Package commit is the two-phase cross-file commit orchestrator for
// claudecm. It exists to enforce coding-standards rule 13 (PRD FR-16):
// when a single command touches more than one owned file, every write
// MUST route through this package rather than sequencing
// writepath.Apply calls across files directly.
//
// This file (E7-S1) is TYPES-ONLY. It declares the Committer interface,
// its transaction handle StagedTxn, the per-file and aggregate report
// types, and the PartialFailure error surface. The pipeline body lands
// in E7-S2..E7-S5 under the contract this file freezes. The stub
// NewCommitter here returns a Committer whose Stage/Commit fail with
// ErrNotImplemented; Abort is a safe no-op (nothing was acquired). The
// zero-plan Stage path is wired end-to-end so callers can build the
// no-op branch of `switch --dry-run` against a stable contract today.
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
package commit

import (
	"context"
	"errors"
	"fmt"
	"path"
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
//
// Lifecycle. A single Committer produces a StagedTxn via Stage; the
// caller then either Commits that StagedTxn or Aborts it. Aborting is
// mandatory when Stage succeeded but the caller decides not to
// proceed (dry-run, user answered "n" to the confirmation prompt),
// because Stage holds per-target locks that Abort releases.
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
	Stage(ctx context.Context, r *storage.Resolver, plans []writepath.WritePlan) (StagedTxn, error)

	// Commit executes the ordered rename phase. It walks StagedTxn's
	// prepared files in canonicalCommitOrder, publishing each rename
	// and running each post-write reparse. On success it returns a
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

	// NewBytes are the bytes that will be renamed over the target.
	NewBytes []byte

	// PreFingerprint is the storage.Fingerprint recorded at Stage
	// time. Commit re-checks the target against this before renaming
	// (NFR-C2 concurrent-edit detection).
	PreFingerprint storage.Fingerprint

	// Backup is the pre-commit backup record. Zero for first-write
	// plans (nothing to back up).
	Backup storage.BackupRecord

	// Diff is what Stage would show in the pre-apply confirmation.
	// Callers rendering the summary read this directly rather than
	// recomputing it against the target.
	Diff writepath.DiffResult

	// Skipped is true when CurrentBytes and NewBytes are byte-equal
	// so Commit will not rename this file. Included so a no-op plan
	// still appears in the CommitReport as StatusUntouched.
	Skipped bool
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

	// RolledBack is true when Commit hit a mid-phase-2 failure and
	// rolled back at least one already-committed file from its
	// backup. Aggregate signal: the per-file detail lives in each
	// PerFileReport.Status.
	RolledBack bool
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

// ErrNotImplemented is returned by every stub method except the
// documented zero-plan Stage / zero-plan Commit / Abort no-op paths.
// Deleted in E7-S2 when the pipeline body lands.
var ErrNotImplemented = errors.New("claudecm/commit: not implemented")

// Option customizes a Committer at construction time. E7-S1 defines
// the seam without any concrete options; E7-S2 will populate it with
// test seams (WithClock, WithBackupFn, ...) without breaking existing
// callers thanks to the variadic Option shape.
type Option func(*committerCfg)

// committerCfg holds Committer construction knobs. Empty in E7-S1;
// E7-S2 will add fields backed by Option setters.
type committerCfg struct{}

// NewCommitter returns the default Committer. The E7-S1 stub
// implementation returns ErrNotImplemented from Stage / Commit for
// non-empty plan input, and nil (safe no-op) from Abort. The zero-plan
// Stage path is wired end-to-end because the story acceptance
// criterion "zero-plan input → Stage returns an empty transaction"
// applies to the stub too.
//
// The variadic Option argument is the seam E7-S2 will populate with
// test knobs (clock, backup writer, ...). Callers today pass no
// options; E7-S2 additions do not break this signature.
func NewCommitter(opts ...Option) Committer {
	cfg := committerCfg{}
	for _, opt := range opts {
		opt(&cfg)
	}
	return &stubCommitter{}
}

// stubCommitter is the E7-S1 placeholder implementation. Replaced in
// E7-S2 by the real pipeline body.
type stubCommitter struct{}

// Stage on the stub returns an empty StagedTxn + nil for zero-plan
// input; ErrNotImplemented for anything else.
func (s *stubCommitter) Stage(_ context.Context, _ *storage.Resolver, plans []writepath.WritePlan) (StagedTxn, error) {
	if len(plans) == 0 {
		return StagedTxn{StagedAt: time.Time{}}, nil
	}
	return StagedTxn{}, ErrNotImplemented
}

// Commit on the stub returns an empty CommitReport + nil for an
// empty StagedTxn (matches the zero-plan Stage contract); otherwise
// returns ErrNotImplemented.
func (s *stubCommitter) Commit(_ context.Context, txn StagedTxn) (CommitReport, error) {
	if len(txn.Plans) == 0 && len(txn.Prepared) == 0 && len(txn.Locks) == 0 {
		return CommitReport{}, nil
	}
	return CommitReport{}, ErrNotImplemented
}

// Abort on the stub is a safe no-op — the stub never acquires locks
// and never writes backups, so there is nothing to release or discard.
func (s *stubCommitter) Abort(_ StagedTxn) error { return nil }

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
