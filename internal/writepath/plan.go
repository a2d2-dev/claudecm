// Package writepath encodes the FR-5 locked write-path contract for every
// tool-owned config file claudecm mutates. This file (E2-S1) is types-only:
// it declares the value types that adapters (E3/E4) produce and that the
// pipeline body (E2-S2..E2-S4) consumes. The pipeline itself lives in a
// sibling apply.go added by later stories; the Apply stub here exists solely
// so downstream callers can compile against the contract in this PR.
//
// Design deviations from the story text worth noting in code:
//
//   - The story enumerates WritePlan fields (Path, Format, OwnedKeys,
//     current bytes, intended bytes, tool ID). This file names Path as
//     "Target" and intended bytes as "NewContent", and adds a Transform
//     seam so adapters may hand writepath a pure (current -> new) function
//     when they do not want to precompute NewContent themselves. If both
//     Transform and NewContent are set, Transform wins at Apply time.
//     This is a shape-refinement, not a scope expansion: everything the
//     story lists still exists, just under refined names.
//
//   - The story names the returned struct ApplyReport. This file names it
//     WriteReport, matching how E2-S2..E2-S4 will refer to it.
//
//   - The story treats KeyPath as its own typed string with helpers. In
//     this shape OwnedKeys is []string. Nested-path helpers are deferred
//     to whichever consumer (Diff / merge-preserve) actually needs them;
//     encoding them here without a caller invites speculative surface
//     area (coding-standards §12).
//
// Everything downstream of these types (locking, backup, atomic write,
// reparse, rollback, concurrency fingerprinting) is out of scope for this
// story and MUST NOT appear here.
package writepath

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"time"

	"github.com/a2d2-dev/claudecm/internal/storage"
)

// WritePlan describes the transformation Apply will attempt on a single
// owned file. Adapters produce a WritePlan; writepath.Apply consumes it.
//
// Encodes PRD FR-5 (single locked write-path) and FR-16 (per-file plan
// unit consumed by the two-phase commit).
type WritePlan struct {
	// Tool identifies the producing adapter, e.g. "claudecode", "codex".
	// Used for backup path routing and audit-log entries. Required.
	Tool string

	// Target is the absolute path to the file to write. Required. Must
	// be absolute; symlink resolution and HOME containment live in
	// internal/storage/paths.go and happen inside Apply, not here.
	Target string

	// NewContent is the fully-serialized bytes to write. Used verbatim
	// when Transform is nil. If Transform is non-nil, NewContent is
	// ignored (Transform wins) — see the package doc comment.
	NewContent []byte

	// Transform is a pure (currentBytes) -> (newBytes, error) that
	// adapters can supply when they want Apply to fetch the current
	// bytes under lock and derive the new bytes from them. If nil,
	// Apply uses NewContent verbatim.
	Transform Transform

	// Parser turns raw file bytes into a structured view for pre-write
	// diff (FR-4 pre-apply confirmation, --dry-run) and post-write
	// reparse (FR-5 step 8). Required whenever a meaningful diff or
	// reparse is expected; may be nil for callers that only need the
	// atomic-write portion (a rare case).
	Parser Parser

	// OwnedKeys is the frozen allowlist of key paths claudecm manages
	// in this file. Everything outside this list must be preserved
	// verbatim (merge-preserve, PRD §4.7). Empty allowlist is legal
	// (means "claudecm owns nothing in this file yet"); empty strings
	// inside the slice are not.
	OwnedKeys []string

	// Reason is a human-friendly explanation attached to the backup
	// filename metadata and the audit log. Optional.
	Reason string

	// DryRun asks Apply to compute the plan (diff, would-be bytes)
	// without writing to disk. Maps to FR-15.
	DryRun bool

	// MustNotExist requests O_CREAT|O_EXCL semantics on the first
	// write against a missing target. Maps to NFR-C3.
	MustNotExist bool
}

// Transform is a pure (currentBytes) -> (newBytes, error) function that
// adapters can supply on WritePlan.Transform. If provided, Apply reads
// current bytes under lock, calls Transform, and writes the result. If
// nil, Apply uses WritePlan.NewContent verbatim. Implementations must be
// pure: no I/O, no goroutines, no package-level state.
type Transform func(current []byte) ([]byte, error)

// Parser turns raw file bytes into a comparable value. Used for
// pre-write diff and post-write reparse. Return an error on malformed
// bytes — no fallback. Per coding-standards rule 2 (NFR-S1), silent
// fallback rewriting on parse failure is forbidden.
type Parser interface {
	Parse(data []byte) (any, error)
}

// ParserFunc adapts an ordinary function to the Parser interface.
type ParserFunc func(data []byte) (any, error)

// Parse implements Parser.
func (f ParserFunc) Parse(data []byte) (any, error) { return f(data) }

// DiffResult captures what will change when the plan is applied. Rendered
// by `cmd/explain` (FR-7 context) and by `switch`'s FR-4 pre-apply
// confirmation.
type DiffResult struct {
	Added          []string   // key paths that will appear
	Removed        []string   // key paths that will disappear
	Changed        []KeyDelta // key paths whose value differs
	TouchesUnowned bool       // true if any Added/Removed/Changed key is not in OwnedKeys
}

// KeyDelta records a single key-path change.
type KeyDelta struct {
	Key      string
	OldValue any
	NewValue any
}

// WriteReport describes what Apply actually did on a single file.
// Corresponds to the story's ApplyReport; renamed for clarity relative
// to the two-phase CommitReport that wraps a slice of these.
type WriteReport struct {
	Tool            string
	Target          string
	DryRun          bool
	Skipped         bool                 // no-op: current bytes already match intent
	Backup          storage.BackupRecord // zero value on skipped/dry-run/first-write
	PreFingerprint  storage.Fingerprint  // captured under lock before write
	PostFingerprint storage.Fingerprint  // captured after successful atomic write
	Diff            DiffResult
	AppliedAt       time.Time
}

// Sentinel error kinds. Adapters and cmd/* switch on these via errors.Is;
// each maps to a documented exit-code / rollback path.
var (
	// ErrPlanInvalid indicates a WritePlan failed cheap pre-execution
	// validation. Wrapped with the specific reason via fmt.Errorf.
	ErrPlanInvalid = errors.New("claudecm: write plan invalid")

	// ErrConcurrentEdit indicates the target changed under the lock
	// between fingerprint capture and rename. Maps to NFR-C2. Exit code 2.
	ErrConcurrentEdit = errors.New("claudecm: target changed under lock; aborted")

	// ErrPostWriteReparse indicates FR-5 step 8 reparse failed or an
	// owned key disagreed with intent.
	ErrPostWriteReparse = errors.New("claudecm: post-write reparse failed")

	// ErrRollback indicates auto-rollback succeeded following a
	// post-write reparse failure — target is restored, callers should
	// surface both the original failure and the successful rollback.
	ErrRollback = errors.New("claudecm: rolled back to backup after reparse failure")

	// ErrRollbackFailed indicates auto-rollback itself failed. On-disk
	// state is undefined; callers must not retry blindly.
	ErrRollbackFailed = errors.New("claudecm: rollback attempt failed; on-disk state is undefined")

	// ErrDryRunUnownedTouched indicates a dry-run diff touched keys not
	// in OwnedKeys and the caller did not pre-authorize with --yes.
	ErrDryRunUnownedTouched = errors.New("claudecm: dry-run touched unowned keys; requires --yes")

	// ErrNotImplemented is returned by the E2-S1 Apply stub. E2-S2
	// replaces the stub and this sentinel is removed.
	ErrNotImplemented = errors.New("claudecm: writepath.Apply not implemented (see E2-S2)")
)

// Apply is the single legal path to disk for tool-owned files. This story
// (E2-S1) ships only the signature and a stub that returns a typed
// not-implemented error, so adapter code and cmd/* callers can compile
// against the final contract while E2-S2..E2-S4 land the pipeline.
//
// The stub deliberately does NOT lock, back up, or write. Do not fill it
// in here; that is the entire scope of E2-S2.
func Apply(ctx context.Context, r *storage.Resolver, plan WritePlan) (WriteReport, error) {
	_ = ctx
	_ = r
	_ = plan
	return WriteReport{}, ErrNotImplemented
}

// ValidatePlan performs cheap pre-execution checks. It is pure and does
// no I/O; anything requiring disk access (symlink resolution, HOME
// containment, existence) belongs in Apply, not here. Errors returned
// wrap ErrPlanInvalid so callers can use errors.Is.
func ValidatePlan(plan WritePlan) error {
	if plan.Tool == "" {
		return fmt.Errorf("%w: Tool is empty", ErrPlanInvalid)
	}
	if plan.Target == "" {
		return fmt.Errorf("%w: Target is empty", ErrPlanInvalid)
	}
	if !isAbsolute(plan.Target) {
		return fmt.Errorf("%w: Target %q is not absolute", ErrPlanInvalid, plan.Target)
	}
	for i, k := range plan.OwnedKeys {
		if k == "" {
			return fmt.Errorf("%w: OwnedKeys[%d] is empty", ErrPlanInvalid, i)
		}
	}
	// Note: WritePlan.Transform + WritePlan.NewContent may both be set.
	// This is accepted intentionally — Transform wins at Apply time,
	// see the package doc comment. This is not a validation error.
	return nil
}

// Diff computes a deterministic, pure DiffResult from two parsed values.
// Both inputs come from a Parser; nil inputs are treated as "absent".
//
// Semantics:
//
//   - If current and next are both map[string]any, Diff walks the union
//     of keys at the top level and reports Added / Removed / Changed.
//     This is a shallow diff on purpose: v1 owned-key allowlists are
//     dotted paths ("env.ANTHROPIC_API_KEY"), which the caller will
//     have already flattened into map[string]any before invoking Diff
//     if they need nested comparison.
//
//   - If either side is not a map, Diff falls back to a single-scalar
//     comparison with an empty Key. If the two scalars are equal, Diff
//     returns the zero DiffResult.
//
//   - Two values that compare equal via reflect.DeepEqual produce the
//     zero DiffResult.
//
// TouchesUnowned is true iff any Added / Removed / Changed key is not
// present in ownedKeys.
func Diff(current, next any, ownedKeys []string) DiffResult {
	if reflect.DeepEqual(current, next) {
		return DiffResult{}
	}

	curMap, curOK := current.(map[string]any)
	nextMap, nextOK := next.(map[string]any)
	if !curOK || !nextOK {
		// Scalar fallback: one Changed entry keyed to the empty
		// string, so callers can still render "value differs".
		res := DiffResult{
			Changed: []KeyDelta{{Key: "", OldValue: current, NewValue: next}},
		}
		res.TouchesUnowned = !isOwned("", ownedKeys)
		return res
	}

	owned := sliceToSet(ownedKeys)
	var res DiffResult
	seen := make(map[string]struct{}, len(curMap)+len(nextMap))
	for _, k := range sortedKeys(curMap) {
		seen[k] = struct{}{}
		nv, ok := nextMap[k]
		if !ok {
			res.Removed = append(res.Removed, k)
			if _, isOwn := owned[k]; !isOwn {
				res.TouchesUnowned = true
			}
			continue
		}
		if !reflect.DeepEqual(curMap[k], nv) {
			res.Changed = append(res.Changed, KeyDelta{
				Key:      k,
				OldValue: curMap[k],
				NewValue: nv,
			})
			if _, isOwn := owned[k]; !isOwn {
				res.TouchesUnowned = true
			}
		}
	}
	for _, k := range sortedKeys(nextMap) {
		if _, ok := seen[k]; ok {
			continue
		}
		res.Added = append(res.Added, k)
		if _, isOwn := owned[k]; !isOwn {
			res.TouchesUnowned = true
		}
	}
	return res
}

// isAbsolute checks whether a path is absolute. We avoid importing
// path/filepath purely for its Windows semantics; v1 targets Unix and
// FR-5's Target is a POSIX absolute path in every architecture ref.
func isAbsolute(p string) bool { return len(p) > 0 && p[0] == '/' }

// isOwned reports whether key is present in ownedKeys.
func isOwned(key string, ownedKeys []string) bool {
	for _, k := range ownedKeys {
		if k == key {
			return true
		}
	}
	return false
}

func sliceToSet(s []string) map[string]struct{} {
	m := make(map[string]struct{}, len(s))
	for _, v := range s {
		m[v] = struct{}{}
	}
	return m
}

// sortedKeys returns m's keys sorted lexicographically. Used to make
// Diff deterministic across runs — critical because DiffResult flows
// into the FR-4 pre-apply confirmation surface.
func sortedKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
