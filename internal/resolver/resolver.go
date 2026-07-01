// Package resolver aggregates per-tool EffectiveView projections across
// every registered adapter into one View that cmd/current and
// cmd/explain render. It is the join point between the adapter layer
// (internal/adapter/*, which knows how to Project a single tool's
// layered chain) and the CLI surface (cmd/*).
//
// Authority. This package is subordinate to:
//  1. docs/decisions/0001-direction-lock.md (ADR-0001).
//  2. docs/prd/prd-v1.md (FR-6, FR-7, SM-4).
//  3. docs/architecture.md §6 (Resolver contract + external drift).
//  4. internal/adapter/adapter.go — frozen shared types (EffectiveView,
//     EffectiveField, Layer, ShadowedLayer, SortFields). The resolver
//     never redefines these; it re-uses them verbatim so the wire
//     shape adapters emit is the wire shape renderers consume.
//
// V1-scope. Resolve walks the adapter.Registry (or a filtered subset)
// and stitches one ToolView per adapter into a View. It is read-only:
// it never mutates env, files, or the Profile. External drift
// detection lives in each adapter's Project (architecture §6.2); this
// package surfaces it verbatim.
package resolver

import (
	"context"
	"errors"
	"fmt"
	"regexp"

	"github.com/a2d2-dev/claudecm/internal/adapter"
	"github.com/a2d2-dev/claudecm/internal/config"
	"github.com/a2d2-dev/claudecm/internal/storage"
	"github.com/a2d2-dev/claudecm/internal/writepath"
)

// View is the aggregate projection cmd/current summarises and
// cmd/explain details. It bundles the Profile that was resolved with
// one ToolView per adapter that participated in the resolve.
//
// Tools is a slice (not a map) so ordering is a property of the data.
// Resolve emits ToolViews in deterministic order — the same order
// adapter.Registry.List returns — so runs against identical input
// produce identical output. Callers rendering to a user MUST NOT
// re-sort or re-order Tools.
type View struct {
	// Profile is the profile that was resolved. Copied by value at
	// resolve time so downstream renderers see a stable snapshot even
	// if the caller mutates its own copy afterwards.
	Profile config.Profile

	// Tools is the per-tool projection in adapter.Registry.List order
	// (lexicographic by ToolID). Empty when no adapter is registered
	// or when Filter excludes every registered tool.
	Tools []ToolView
}

// ToolView wraps a single adapter's EffectiveView with the presence
// signal Detect returned at resolve time and any non-fatal errors the
// adapter surfaced. This is the unit cmd/current renders as one block
// and cmd/explain expands into the full layer chain.
//
// Errors are per-tool and non-fatal: Resolve records them here and
// keeps walking the Registry so a single misbehaving adapter cannot
// abort the whole resolve. Top-level errors from Resolve are reserved
// for context cancellation and Registry-wide failure.
type ToolView struct {
	// Tool identifies which adapter produced this ToolView. Redundant
	// with Effective.Tool at the wire level, but hoisted here so
	// renderers do not have to reach into Effective to key on tool.
	Tool adapter.ToolID

	// Presence is the Detect result for this tool at resolve time.
	// Zero value means Detect returned its zero value or errored
	// before populating fields.
	Presence adapter.Presence

	// Effective is the layered projection this adapter produced via
	// Project (architecture §6). Fields is a slice; renderers MUST
	// call adapter.SortFields on Effective.Fields before formatting so
	// output is stable. Effective.ExternalDriftDetected /
	// ExternalDriftFile flow through verbatim.
	Effective adapter.EffectiveView

	// Errors lists non-fatal per-tool issues surfaced during the
	// resolve (Detect failure, Project failure, unreadable file
	// outside HOME, etc.). Rendered by cmd/explain; cmd/current shows
	// only a one-line summary. Empty on the clean happy path.
	Errors []ToolError
}

// ToolError is one non-fatal per-tool issue collected during Resolve.
// It never aborts the whole resolve — the resolver records the error
// on the offending ToolView and continues to the next adapter. Fatal
// conditions (context cancellation, Registry-wide failure) surface as
// the top-level error return of Resolve instead.
type ToolError struct {
	// Kind classifies the error for renderers that want to filter
	// (e.g. only show ParseFailed in cmd/explain --verbose). Constants
	// below are the closed enum.
	Kind ErrorKind

	// Message is a human-readable one-line description. Never
	// contains secrets — adapters must redact via internal/ui before
	// stuffing anything into this field.
	Message string

	// File is the absolute owned-file path this error refers to, when
	// applicable (e.g. ParseFailed on the Codex TOML config file the
	// adapter owns). Empty when the error is not tied to a specific
	// file.
	File string
}

// ErrorKind is the closed enum of per-tool resolve error categories.
// Kept as strings (rather than iota int) so JSON output from
// cmd/current --output json is stable across binary rebuilds and
// grep-friendly in operator logs — mirroring the adapter.Layer choice.
type ErrorKind string

// Per-tool error categories. The set is closed at v1: adding a value
// here is a code + fixture-matrix update, matching the adapter.Layer
// discipline in internal/adapter/adapter.go.
const (
	// ErrorDetectFailed marks a Detect call that returned a
	// non-nil error. Resolve records the error and carries whatever
	// Presence value Detect returned (best-effort — may be zero, may
	// be partially populated).
	ErrorDetectFailed ErrorKind = "DetectFailed"

	// ErrorProjectFailed marks a Project call that returned a
	// non-nil error. Resolve records it and emits a ToolView with
	// the adapter's zero EffectiveView.
	ErrorProjectFailed ErrorKind = "ProjectFailed"

	// ErrorParseFailed marks an unparseable owned file. Adapters
	// bubble it up from their Project implementation; the resolver
	// carries it here so cmd/explain can render the offending file
	// with a "refuse-not-guess" banner (NFR-S1).
	ErrorParseFailed ErrorKind = "ParseFailed"

	// ErrorOutsideHome marks an owned file whose resolved symlink
	// target is outside the HOME the storage.Resolver is anchored
	// on (NFR-S2). Never followed silently.
	ErrorOutsideHome ErrorKind = "OutsideHome"

	// ErrorCanceled marks a per-tool step that returned
	// context.Canceled or context.DeadlineExceeded. The per-tool
	// Project (or Detect) returned a context-derived error. Resolve
	// records this on the ToolView and continues to the next
	// iteration; the outer walk aborts only if the parent ctx is
	// also canceled (checked at the top of each iteration).
	ErrorCanceled ErrorKind = "Canceled"
)

// Filter narrows Resolve to a subset of tools. A zero Filter (Tools
// nil or empty) means "resolve every registered adapter" — the
// common cmd/current path. A non-empty Tools list restricts the
// resolve to those ToolIDs; unknown IDs are silently skipped so a
// caller can pass a superset without a pre-flight check.
//
// Deliberately a struct (not a []adapter.ToolID) so future v1.x flags
// like verbose-mode toggles can land here without breaking Resolve's
// signature.
type Filter struct {
	// Tools is the allowlist. Nil or empty == all registered
	// adapters. Order is not significant; Resolve always walks the
	// Registry in its own lexicographic order for determinism.
	Tools []adapter.ToolID
}

// Allows reports whether the given tool passes the filter. Pure — no
// I/O, no allocation past what the receiver already holds. An empty
// filter allows everything; a populated filter allows exactly the
// listed ToolIDs.
func (f Filter) Allows(tool adapter.ToolID) bool {
	if len(f.Tools) == 0 {
		return true
	}
	for _, t := range f.Tools {
		if t == tool {
			return true
		}
	}
	return false
}

// filePathRE extracts the first double-quoted absolute path from an
// error message. Adapter error wraps use the shape
//
//	codex apply "<absolute-owned-file-path>": <inner>
//	claudecode apply "<absolute-owned-file-path>": <inner>
//	claudecm: parse failed: parse current "<absolute-owned-file-path>": <inner>
//
// so a quoted absolute path is the reliable signal. Non-absolute
// quotes (e.g. dotted key names) are rejected by the leading "/" so
// the match never returns a non-path string. Best-effort — an empty
// return means the resolver could not identify a specific file, which
// is fine because ToolError.Message already carries the full text.
//
// This intentionally picks the FIRST quoted path, which is the
// adapter-owned file we care about (the file the adapter was applying
// or projecting when the error surfaced). Deeper wrapped paths in the
// message may be intermediate stat targets or symlink resolutions;
// the full Message field carries them for operator inspection.
var filePathRE = regexp.MustCompile(`"(/[^"]+)"`)

// extractFilePath best-effort lifts an absolute file path from an
// adapter error message. Returns "" when no absolute-path quoted
// substring is found.
func extractFilePath(msg string) string {
	m := filePathRE.FindStringSubmatch(msg)
	if len(m) < 2 {
		return ""
	}
	return m[1]
}

// classifyAdapterError maps an adapter-returned error (from either
// Detect or Project) onto the ErrorKind enum for the categorized
// kinds: Canceled, OutsideHome, ParseFailed. Returns the empty
// ErrorKind ("") when the error does not match any categorized
// sentinel — callers must supply their own fallthrough (typically
// ErrorProjectFailed for Project, ErrorDetectFailed for Detect).
//
// Order matters: context cancellation is checked first because a
// canceled parse would otherwise be mis-classified as ParseFailed;
// OutsideHome is checked before ParseFailed because the two writepath
// sentinels are distinct but adapters may wrap both in the same call
// site.
func classifyAdapterError(err error) ErrorKind {
	switch {
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return ErrorCanceled
	case errors.Is(err, storage.ErrOutsideHome), errors.Is(err, writepath.ErrOutsideHome):
		return ErrorOutsideHome
	case errors.Is(err, writepath.ErrParseFailed):
		return ErrorParseFailed
	default:
		return ""
	}
}

// Resolve builds a View for the given Profile by walking the adapter
// Registry (or, when reg is nil, adapter.DefaultRegistry) and
// stitching each adapter's EffectiveView into one aggregate. Read-
// only: never mutates env, files, or the Profile.
//
// Contract:
//   - Walk the Registry (or reg) in deterministic order — the
//     lexicographic ToolID order adapter.Registry.List already
//     returns. This makes runs against identical input produce
//     identical View.Tools ordering.
//   - Apply Filter.Allows before touching an adapter. Excluded tools
//     never appear in View.Tools; they are not zero-filled.
//   - For each adapter that passes the filter: call Detect first,
//     then Project. Detect errors are captured as ErrorDetectFailed
//     on the ToolView and the walk continues with a zero Presence.
//     Project errors are captured as ErrorProjectFailed on the
//     ToolView (or one of the more specific ErrorParseFailed /
//     ErrorOutsideHome / ErrorCanceled kinds when classifiable) and
//     the walk continues with a zero EffectiveView.
//   - Parse failures the adapter bubbles up appear as
//     ErrorParseFailed with the offending file path when it can be
//     lifted from the error message. External drift the adapter
//     surfaces on EffectiveView.ExternalDrift* flows through
//     verbatim — this package never re-hashes files.
//   - Top-level error return is reserved for context cancellation
//     (ctx.Err() before or between tools) and Registry-wide failure
//     (List returned an id that Get cannot construct). A single
//     misbehaving adapter never aborts the whole resolve.
//
// Parameters:
//   - ctx: cancellation from cmd/*.
//   - r:   the sole legal source of HOME-anchored paths
//     (coding-standards rule 3); adapters receive it verbatim.
//   - reg: adapter Registry to walk; nil means adapter.DefaultRegistry.
//   - profile: the Profile to project (copied by value into the View).
//   - f:   Filter restricting which tools participate.
func Resolve(ctx context.Context, r *storage.Resolver, reg *adapter.Registry, profile config.Profile, f Filter) (View, error) {
	view := View{Profile: profile}

	// Top-level cancellation check before any work. One of the only
	// two allowed top-level errors per the E5-S1 contract.
	if err := ctx.Err(); err != nil {
		return view, err
	}

	if reg == nil {
		reg = adapter.DefaultRegistry
	}

	ids := reg.List() // already sorted lexicographically by ToolID.

	for _, id := range ids {
		// Re-check cancellation between tools so a signal that
		// arrives mid-walk aborts promptly with a coherent partial
		// View rather than continuing to hammer adapters.
		if err := ctx.Err(); err != nil {
			return view, err
		}

		if !f.Allows(id) {
			// Filter-excluded tools do not appear in View.Tools at
			// all — no zero-filled ToolView. Callers relying on
			// len(View.Tools) as a "which tools participated"
			// signal see the honest answer.
			continue
		}

		adap, ok := reg.Get(id)
		if !ok || adap == nil {
			// Registry inconsistency: List returned this id but Get
			// cannot construct an Adapter for it. The other allowed
			// top-level error per E5-S1 contract. This is
			// defensive — Register panics on nil ctor and there is
			// no Unregister — but it costs nothing to detect and
			// tells operators the Registry is in an impossible
			// state rather than silently skipping the tool.
			//
			// Return the view built so far (Profile-preserving,
			// partial Tools) so caller diagnostics benefit from
			// "even on error, tell me what profile was attempted
			// and which tools we made it through".
			return view, fmt.Errorf("claudecm/resolver: registry inconsistency: List returned %q but Get failed", id)
		}

		tv := ToolView{Tool: id}

		presence, derr := adap.Detect(ctx, r)
		if derr != nil {
			kind := classifyAdapterError(derr)
			if kind == "" {
				kind = ErrorDetectFailed
			}
			te := ToolError{
				Kind:    kind,
				Message: derr.Error(),
			}
			if kind == ErrorParseFailed || kind == ErrorOutsideHome {
				te.File = extractFilePath(derr.Error())
			}
			tv.Errors = append(tv.Errors, te)
			// Presence carries whatever Detect returned; adapters
			// may still fill best-effort fields on error, so we do
			// not force it to the zero value.
		}
		tv.Presence = presence

		effective, perr := adap.Project(ctx, r, profile)
		if perr != nil {
			kind := classifyAdapterError(perr)
			if kind == "" {
				kind = ErrorProjectFailed
			}
			te := ToolError{
				Kind:    kind,
				Message: perr.Error(),
			}
			if kind == ErrorParseFailed || kind == ErrorOutsideHome {
				te.File = extractFilePath(perr.Error())
			}
			tv.Errors = append(tv.Errors, te)
			// Effective is whatever Project returned — usually the
			// adapter's zero EffectiveView on error; carry it as-is
			// so renderers do not have to distinguish "no data"
			// from "cleared data".
		}
		tv.Effective = effective

		view.Tools = append(view.Tools, tv)
	}

	return view, nil
}
