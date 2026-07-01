// Package codex: shared read-side helpers for the two owned files
// (E4-S6 review followup).
//
// This file holds the read-only, HOME-containment-checked loaders that
// both Import (import.go) and Project (project.go) need. Prior to this
// consolidation each feature carried its own byte-identical
// readCodex{TOML,Auth} pair differing only in the log-message prefix
// baked into wrapped error strings. That duplication was flagged in the
// PR #32 review (F2): a future policy tweak (empty-file semantics, JSON
// null handling, symlink policy) would have to be applied in two
// places and staying in sync was manual. The helpers below take an
// explicit logPrefix so Import can keep emitting "codex import:" and
// Project can emit "codex project:" without either file carrying its
// own reader.
//
// Keep in lock-step with the file-level godoc in import.go
// (Empty-file policy, Symlink policy (read side), Null-safety) — those
// notes remain the authoritative narrative for the read semantics
// implemented here.

package codex

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	codextoml "github.com/a2d2-dev/claudecm/internal/adapter/codex/toml"
	"github.com/a2d2-dev/claudecm/internal/storage"
	"github.com/a2d2-dev/claudecm/internal/writepath"
)

// readCodexTOMLWithPrefix resolves configPath, verifies HOME
// containment, and loads the TOML doc when present. Returns:
//
//   - (false, nil, nil) when the file is absent (ErrNotExist) OR
//     present-but-empty per treatAsEmpty. Absence is not an error at
//     this level; the caller decides whether both files being absent
//     is an error.
//   - (true, doc, nil) when the file is present and parses.
//   - (false, nil, err) on any hard failure: parse error, containment
//     violation, or non-ENOENT read error.
//
// logPrefix is embedded verbatim into non-sentinel error strings so
// Import ("codex import") and Project ("codex project") stay
// distinguishable in operator-facing errors. Parse-failure and
// outside-HOME errors still wrap ErrParseFailed / ErrOutsideHome
// unchanged — the sentinel chain is the caller-facing contract.
func readCodexTOMLWithPrefix(configPath string, r *storage.Resolver, logPrefix string) (bool, *codextoml.Doc, error) {
	if err := verifyReadTargetInHomeCodex(configPath, r); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil, nil
		}
		return false, nil, err
	}
	data, err := os.ReadFile(configPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil, nil
		}
		return false, nil, fmt.Errorf("%s: read %q: %w", logPrefix, configPath, err)
	}
	if treatAsEmpty(data) {
		return false, nil, nil
	}
	doc, err := codextoml.Load(data)
	if err != nil {
		// Multi-%w preserves BOTH the adapter sentinel chain (through
		// ErrParseFailed → writepath.ErrParseFailed) AND the TOML
		// parser sentinel (codextoml.ErrParseFailed) so callers can
		// errors.Is against either.
		return false, nil, fmt.Errorf("%w: %s: %w", ErrParseFailed, configPath, err)
	}
	return true, doc, nil
}

// readCodexAuthWithPrefix resolves authPath, verifies HOME containment,
// and loads + flattens the JSON when present. Same tri-state return
// contract as readCodexTOMLWithPrefix.
//
// Returns the unflattened root map alongside the flattened view so
// callers can read OPENAI_API_KEY directly from the root (decoupling
// the null-vs-empty-vs-string decision from any future change to
// writepath.Flatten's nil-handling contract — OPENAI_API_KEY is
// top-level and unambiguously scalar). tokens.* lookups still go
// through the flat view where the flat dotted-path shape is exactly
// what OwnedKeysAuthJSON encodes.
func readCodexAuthWithPrefix(authPath string, r *storage.Resolver, logPrefix string) (bool, map[string]any, map[string]any, error) {
	if err := verifyReadTargetInHomeCodex(authPath, r); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil, nil, nil
		}
		return false, nil, nil, err
	}
	data, err := os.ReadFile(authPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil, nil, nil
		}
		return false, nil, nil, fmt.Errorf("%s: read %q: %w", logPrefix, authPath, err)
	}
	if treatAsEmpty(data) {
		return false, nil, nil, nil
	}
	var root map[string]any
	if err := json.Unmarshal(data, &root); err != nil {
		return false, nil, nil, fmt.Errorf("%w: %s: %v", ErrParseFailed, authPath, err)
	}
	if root == nil {
		// json.Unmarshal into map[string]any accepts `null` and leaves
		// the map nil. auth.json is documented as an object; a null
		// root is not a shape we should silently accept.
		return false, nil, nil, fmt.Errorf("%w: %s: top-level value is null, want JSON object", ErrParseFailed, authPath)
	}
	flat, err := writepath.Flatten(root)
	if err != nil {
		return false, nil, nil, fmt.Errorf("%w: %s: flatten: %v", ErrParseFailed, authPath, err)
	}
	return true, root, flat, nil
}

// treatAsEmpty reports whether the given byte payload should be
// interpreted as an absent contribution. Returns true for a zero-byte
// file or a file whose only content is JSON/TOML-insignificant
// whitespace (space, tab, newline, carriage return).
//
// Duplicated from claudecode/emptycheck.go on purpose: importing the
// claudecode package from codex would create a cross-adapter
// dependency the interface contract forbids (each adapter is opaque
// to the others). Shape is intentionally identical so any future
// tweak (BOM handling, etc.) is applied to both call sites in the
// same PR.
func treatAsEmpty(data []byte) bool {
	if len(data) == 0 {
		return true
	}
	return len(bytes.TrimSpace(data)) == 0
}

// verifyReadTargetInHomeCodex performs the read-side containment check
// for one owned file. Resolves the FULL path (not just the leaf)
// through EvalSymlinks so a symlink at ANY component — including a
// parent directory such as the codex config dir — must land inside
// HOME.
//
// Behaviour:
//
//   - EvalSymlinks succeeds → filepath.Rel against the resolved HOME.
//     A ".." prefix or absolute rel means the resolved path escapes
//     HOME → ErrOutsideHome. Otherwise nil.
//   - EvalSymlinks returns ErrNotExist → the file simply is not
//     there (missing config, or dangling symlink target). Return
//     os.ErrNotExist verbatim so the caller can distinguish absence
//     from real errors and decide (per two-file coordination) whether
//     absence is the "nothing to import" UX or just this half missing.
//   - Any other EvalSymlinks error → surface verbatim.
//
// Duplicated (with narrow shape variations) from the claudecode
// adapter's verifyReadTargetInHome for the same reason as
// treatAsEmpty: cross-adapter dependencies are forbidden by the
// interface contract.
func verifyReadTargetInHomeCodex(path string, r *storage.Resolver) error {
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			// Bubble ErrNotExist to caller so it can map to "absent".
			return err
		}
		return fmt.Errorf("codex: evalsymlinks %q: %w", path, err)
	}
	resolvedHome, err := filepath.EvalSymlinks(r.Home())
	if err != nil {
		return fmt.Errorf("codex: evalsymlinks home %q: %w", r.Home(), err)
	}
	rel, err := filepath.Rel(resolvedHome, resolved)
	if err != nil {
		return fmt.Errorf("%w: rel %q vs %q: %v", ErrOutsideHome, resolvedHome, resolved, err)
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return fmt.Errorf("%w: %q resolves to %q (not under %q)", ErrOutsideHome, path, resolved, resolvedHome)
	}
	return nil
}
