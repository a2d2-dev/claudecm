// Package writepath encodes the FR-5 locked write-path contract for every
// tool-owned config file claudecm mutates. This file (E2-S1) is types-only:
// it declares the value types that adapters (E3/E4) produce and that the
// pipeline body (E2-S2..E2-S4) consumes. The pipeline itself will live in
// this package alongside these types; the Apply stub here exists solely so
// downstream callers can compile against the contract in this PR.
//
// Nested inputs must be flattened by the caller (see Flatten) before being
// passed to Diff. Diff itself operates on a single-level map[string]any.
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
//     this shape OwnedKeys is []string. Nested-path traversal for callers
//     is provided by Flatten; ownership matching is exact-string against
//     the fully-flattened key (see OwnedKeys godoc).
//
//   - The story references a separate diff.go / apply.go layout. The
//     whole package currently lives in plan.go; the source-tree split
//     diagram in the architecture doc will catch up when the pipeline
//     lands in E2-S2..E2-S4. No behavioral impact.
//
// Everything downstream of these types (locking, backup, atomic write,
// reparse, rollback, concurrency fingerprinting) is out of scope for this
// story and MUST NOT appear here.
package writepath

import (
	"errors"
	"fmt"
	"path"
	"reflect"
	"sort"
	"strings"
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

	// OwnedKeys is the frozen allowlist of fully-flattened key paths
	// claudecm manages in this file. Ownership is EXACT string match
	// against the flattened key produced by Flatten: an OwnedKeys entry
	// of "env" does NOT own "env.ANTHROPIC_API_KEY" — those are two
	// different keys. Wildcards, prefixes, and glob syntax are out of
	// v1 scope. Everything outside this list must be preserved verbatim
	// (merge-preserve, PRD §4.7). Empty allowlist is legal (means
	// "claudecm owns nothing in this file yet"); empty strings inside
	// the slice and duplicate entries are not.
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

	// AllowUnowned tells Apply to proceed with the write even when the
	// pre-write diff reports Diff.TouchesUnowned == true. Default false:
	// touching an unowned key is refused with ErrDryRunUnownedTouched so
	// the caller (typically the FR-4 confirmation surface) must
	// explicitly opt in. Adapters and UI layers that have already
	// gathered user consent set this true to skip the guard. The
	// sentinel keeps its historic name — the guard fires on any write,
	// not only dry-runs — but the intent is "unowned keys touched
	// without opt-in", not "dry-run only". Kept name-stable for
	// errors.Is-based callers already coded against E2-S1's plan.go.
	AllowUnowned bool
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
//
// The Err* values below whose comments reference FR-5 pipeline steps are
// returned by writepath.Apply once E2-S2..E2-S4 wire the pipeline in.
// Callers should switch on the specific sentinel, not on error string.
var (
	// ErrPlanInvalid indicates a WritePlan failed cheap pre-execution
	// validation. Wrapped with the specific reason via fmt.Errorf.
	ErrPlanInvalid = errors.New("claudecm: write plan invalid")

	// ErrConcurrentEdit indicates the target changed under the lock
	// between fingerprint capture and rename. Maps to NFR-C2. Exit code 2.
	ErrConcurrentEdit = errors.New("claudecm: target changed under lock; aborted")

	// ErrLockTimeout indicates Apply could not acquire the write lock
	// within the configured timeout (NFR-C1). Distinct from
	// ErrConcurrentEdit, which fires only after a lock was obtained.
	// writepath.Apply wraps storage.ErrLockTimeout with this sentinel so
	// callers of writepath can errors.Is on this value without needing
	// to import the storage package.
	ErrLockTimeout = errors.New("claudecm: lock acquisition timed out")

	// ErrParseFailed indicates the parser refused the current on-disk
	// bytes before any write happened. Maps to FR-5 step 3 (refuse on
	// malformed) and NFR-S1 (no silent fallback rewriting). Returned by
	// writepath.Apply once E2-S2..E2-S4 wire the pipeline.
	ErrParseFailed = errors.New("claudecm: parse failed")

	// ErrOutsideHome indicates the resolved (symlink-followed) Target
	// escapes $HOME. Maps to FR-5 step 4 and NFR-S2/NFR-S3 (HOME
	// containment). Returned by writepath.Apply once E2-S2..E2-S4 wire
	// the pipeline.
	ErrOutsideHome = errors.New("claudecm: target resolves outside HOME")

	// ErrBackupFailed indicates the backup snapshot (FR-5 step 6) could
	// not be created. Apply aborts before touching the target. Returned
	// by writepath.Apply once E2-S2..E2-S4 wire the pipeline.
	ErrBackupFailed = errors.New("claudecm: backup creation failed")

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

	// ErrFlattenInvalidKey indicates Flatten was given a map whose key
	// contains a control character (NUL, newline, CR, or tab). Such
	// keys cannot legally appear in a config file we manage.
	ErrFlattenInvalidKey = errors.New("claudecm: flatten: key contains control character")
)

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
	if i := strings.IndexAny(plan.Target, "\x00\n\r\t"); i >= 0 {
		return fmt.Errorf("%w: Target contains control character at byte %d", ErrPlanInvalid, i)
	}
	// Defense-in-depth: reject any surviving ".." component after Clean.
	// Real symlink escape checks live in internal/storage/paths.go and
	// run inside Apply; this catches obvious mistakes at plan-construction
	// time so adapters fail fast.
	cleaned := path.Clean(plan.Target)
	for _, part := range strings.Split(cleaned, "/") {
		if part == ".." {
			return fmt.Errorf("%w: Target %q contains '..' component after Clean", ErrPlanInvalid, plan.Target)
		}
	}
	// A plan with neither Transform nor NewContent set would silently
	// truncate the target to zero bytes (Transform nil → newBytes =
	// plan.NewContent = nil → AtomicWrite publishes []byte{}). Refuse
	// at validation time so callers cannot express that shape at all.
	if plan.Transform == nil && plan.NewContent == nil {
		return fmt.Errorf("%w: neither Transform nor NewContent is set", ErrPlanInvalid)
	}
	seen := make(map[string]struct{}, len(plan.OwnedKeys))
	for i, k := range plan.OwnedKeys {
		if k == "" {
			return fmt.Errorf("%w: OwnedKeys[%d] is empty", ErrPlanInvalid, i)
		}
		if _, dup := seen[k]; dup {
			return fmt.Errorf("%w: OwnedKeys[%d] %q is a duplicate", ErrPlanInvalid, i, k)
		}
		seen[k] = struct{}{}
	}
	// Note: WritePlan.Transform + WritePlan.NewContent may both be set.
	// This is accepted intentionally — Transform wins at Apply time,
	// see the package doc comment. This is not a validation error.
	return nil
}

// Flatten walks a nested map[string]any and returns a single-level map
// whose keys are the fully-qualified paths joined by '.'. Leaves (any
// non-map value: scalars, arrays, nil) are stored under their flattened
// key. Empty maps at any depth contribute no keys.
//
// Escape rule for keys that themselves contain the join delimiters:
// backslash is escaped as `\\` and '.' is escaped as `\.`. Backslash is
// escaped first, then '.'. Consumers that need to reverse the flatten
// must reverse the escape in the same order (unescape '.' first, then
// '\'). This rule matches the OwnedKeys allowlist convention: an
// adapter that owns a key literally named "a.b" would list it as `a\.b`
// in OwnedKeys, not as `a.b`.
//
// Keys containing a control character (NUL, newline, CR, or tab) are
// rejected with an error wrapping ErrFlattenInvalidKey; such keys
// cannot legally appear in a config file we manage.
//
// A non-map top-level input is treated as a leaf and returned as
// {"": v}. Adapters whose top-level document is not a map (rare —
// Codex/Claude configs are all map-shaped) should wrap explicitly
// with map[string]any{"": value} to make the caller-visible key
// deliberate rather than relying on this shortcut.
func Flatten(v any) (map[string]any, error) {
	out := make(map[string]any)
	if err := flattenInto(out, "", v); err != nil {
		return nil, err
	}
	return out, nil
}

func flattenInto(out map[string]any, prefix string, v any) error {
	m, ok := v.(map[string]any)
	if !ok {
		out[prefix] = v
		return nil
	}
	if len(m) == 0 {
		// Empty map at any depth emits no keys. At the top level this
		// yields an empty result; at a leaf it means that subtree
		// contributes nothing to the flat view (Diff will report the
		// parent key as unchanged if both sides agree it is empty).
		return nil
	}
	for _, k := range sortedKeys(m) {
		if err := validateFlattenKey(k); err != nil {
			return err
		}
		escaped := escapeFlattenKey(k)
		var next string
		if prefix == "" {
			next = escaped
		} else {
			next = prefix + "." + escaped
		}
		if err := flattenInto(out, next, m[k]); err != nil {
			return err
		}
	}
	return nil
}

func validateFlattenKey(k string) error {
	if i := strings.IndexAny(k, "\x00\n\r\t"); i >= 0 {
		return fmt.Errorf("%w: %q (byte %d)", ErrFlattenInvalidKey, k, i)
	}
	return nil
}

func escapeFlattenKey(k string) string {
	// Order matters: escape backslash first, then dot. Reversing means
	// unescape dot first, then backslash.
	k = strings.ReplaceAll(k, `\`, `\\`)
	k = strings.ReplaceAll(k, `.`, `\.`)
	return k
}

// Diff computes a deterministic, pure DiffResult from two flat maps.
// Callers with nested configuration MUST pass their values through
// Flatten first; Diff does not descend into nested map[string]any
// values. This split keeps the ownership-match semantics simple: an
// OwnedKeys entry matches a Diff-reported key by exact string equality.
//
// Semantics:
//
//   - Diff walks the union of keys in current and next. For each key
//     it emits Removed (in current only), Added (in next only), or
//     Changed (present in both, values differ by reflect.DeepEqual).
//
//   - Two maps that compare equal via reflect.DeepEqual produce the
//     zero DiffResult.
//
//   - Passing nil for either side is legal and means "absent" — every
//     key on the other side becomes Added or Removed accordingly.
//
// TouchesUnowned is true iff any Added / Removed / Changed key is not
// present in ownedKeys.
func Diff(current, next map[string]any, ownedKeys []string) DiffResult {
	if reflect.DeepEqual(current, next) {
		return DiffResult{}
	}

	owned := sliceToSet(ownedKeys)
	var res DiffResult
	seen := make(map[string]struct{}, len(current)+len(next))
	for _, k := range sortedKeys(current) {
		seen[k] = struct{}{}
		nv, ok := next[k]
		if !ok {
			res.Removed = append(res.Removed, k)
			if _, isOwn := owned[k]; !isOwn {
				res.TouchesUnowned = true
			}
			continue
		}
		if !reflect.DeepEqual(current[k], nv) {
			res.Changed = append(res.Changed, KeyDelta{
				Key:      k,
				OldValue: current[k],
				NewValue: nv,
			})
			if _, isOwn := owned[k]; !isOwn {
				res.TouchesUnowned = true
			}
		}
	}
	for _, k := range sortedKeys(next) {
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

func sliceToSet(s []string) map[string]struct{} {
	m := make(map[string]struct{}, len(s))
	for _, v := range s {
		m[v] = struct{}{}
	}
	return m
}

// sortedKeys returns m's keys sorted lexicographically. Used to make
// Diff and Flatten deterministic across runs — critical because
// DiffResult flows into the FR-4 pre-apply confirmation surface.
func sortedKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
