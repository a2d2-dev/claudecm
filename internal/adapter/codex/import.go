// Package codex: Import implementation (E4-S3).
//
// This file carries the Import surface for the Codex CLI adapter.
// It is split out of adapter.go so the two-file (config.toml +
// auth.json) read-side machinery does not crowd the adapter's public
// contract methods, which stay in adapter.go for grep-friendliness.
//
// Design notes
// ============
//
// Two-file coordination. Unlike Claude Code's single settings.json,
// the Codex adapter owns TWO files that are read together:
//
//	~/.codex/config.toml — model routing, provider tables, approval mode
//	~/.codex/auth.json   — OPENAI_API_KEY + OAuth token bundle
//
// Each file is independently optional. A fresh install where the
// operator has only run `codex login` (no manual config edits) will
// carry auth.json without config.toml; conversely a self-hosted
// operator running against a relay may have edited config.toml before
// ever authenticating. Import must import what it can from each half
// and only refuse-with-ErrNoConfig when BOTH files are absent — a
// state that means "nothing to import" from the operator's point of
// view. See PRD FR-8 and story E4-S3 AC 2 (partial import allowed).
//
// Refuse-on-malformed. Either file's failure to parse is a hard stop
// (NFR-S1, no silent fallback). If config.toml is unparseable we do
// not proceed to auth.json — the two files are conceptually one
// operator-facing configuration and importing half of it while the
// other half is bug-signaling would let the operator continue with
// a partial profile candidate that misses provider routing they
// explicitly set. Symmetric for a malformed auth.json.
//
// Empty-file policy. Zero-byte or whitespace-only files are treated
// as absent for their respective format (an empty config.toml
// contributes nothing; an empty auth.json contributes nothing). This
// mirrors the claudecode adapter's treatAsEmpty policy for
// settings.json. It is NOT a fallback write — Import never writes.
// Predicate lives in treatAsEmpty (this file) to avoid a cross-adapter
// dependency; the shape is intentionally identical to
// claudecode/emptycheck.go so any future policy tweak (BOM handling,
// etc.) is applied to both.
//
// Unknown-key policy. Anything outside OwnedKeysConfigTOML /
// OwnedKeysAuthJSON is NOT copied into the returned Profile
// candidate: OverlayFromTool.Raw only carries owned keys. Byte-
// identical round-trip on non-owned keys is delivered by write-path
// merge-preserve at Apply time (PRD §4.7, FR-5). Recording unowned
// keys in Overlay.Raw would double-source them and produce stale
// re-emission on any hand-edit between Import and Apply.
//
// Symlink policy (read side). Import will FOLLOW a symlink at
// either owned path when the resolved target lands inside HOME.
// This is softer than the write side, which refuses to write through
// a symlink at all; a read cannot damage the target so following an
// in-HOME link matches operator intent. A symlink whose target
// escapes HOME is refused with ErrOutsideHome — reading /etc/passwd
// through a planted symlink stays an attack surface.
//
// OPENAI_API_KEY precedence. auth.json's top-level OPENAI_API_KEY is
// the ONLY key that lands in Core.APIKey. The OAuth token bundle
// (tokens.access_token / tokens.refresh_token / tokens.id_token /
// tokens.account_id) is a distinct credential shape that Codex uses
// when the API key is absent; those fields land in Overlay.Raw
// alongside auth_mode and last_refresh so a subsequent
// Import → Render → Apply reproduces the operator's authentication
// state verbatim. This differs from claudecode's AUTH_TOKEN vs
// API_KEY duel: OpenAI's OAuth bundle is not a drop-in for the API
// key at the wire level (different auth headers, different refresh
// flow), so the two paths are stored in orthogonal profile fields
// rather than one shadowing the other.
//
// Null-safety. A JSON null at OPENAI_API_KEY (a user editor cleared
// the value without deleting the key) MUST NOT populate Core.APIKey.
// Empty string ("") is a valid non-null value and IS preserved
// verbatim into Core.APIKey — same policy as claudecode. Only null
// skips the slot entirely.
//
// Core mapping conservatism. config.toml keys are NOT mapped into
// Core in v1. Codex's model / model_provider / approval_mode /
// model_providers.*.base_url are conceptually tool-specific
// (Codex is not designed as a "provider-neutral" tool the way Core
// assumes) so promoting any of them into Core would either lose
// provider-specific nuance (e.g. Anthropic's wire_api quirks) or
// force Core to grow tool-specific compensation. All config.toml
// owned keys land in Overlay.Raw with their flat dotted paths as
// map keys. Post-v1 this policy can be revisited per-key with an
// ADR + migration story if a common Core mapping emerges.

package codex

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/a2d2-dev/claudecm/internal/adapter"
	codextoml "github.com/a2d2-dev/claudecm/internal/adapter/codex/toml"
	"github.com/a2d2-dev/claudecm/internal/storage"
	"github.com/a2d2-dev/claudecm/internal/writepath"
)

// ErrNoConfig is returned by Import when BOTH owned files are absent
// — a fresh system where Codex has never been configured. Callers use
// errors.Is to distinguish "tool not installed / nothing to import"
// from "file exists but is malformed" (ErrParseFailed).
var ErrNoConfig = errors.New("claudecm/codex: no codex config found (config.toml and auth.json both missing)")

// ErrParseFailed is returned by Import when EITHER owned file exists
// but cannot be decoded. Wraps writepath.ErrParseFailed so callers
// that already errors.Is against the shared sentinel (as the
// write-path pipeline does) keep matching without importing this
// adapter's package.
//
// For a config.toml parse failure, codextoml.ErrParseFailed is also
// in the error chain — callers can distinguish which file failed by
// checking errors.Is(err, codextoml.ErrParseFailed) if needed. For
// auth.json, only writepath.ErrParseFailed and this sentinel wrap
// the underlying json.SyntaxError.
var ErrParseFailed = fmt.Errorf("%w: claudecm/codex: config parse failed", writepath.ErrParseFailed)

// ErrOutsideHome is returned by Import when either owned file is a
// symlink whose resolved target escapes HOME. Wraps
// storage.ErrOutsideHome so callers already errors.Is against the
// shared sentinel (as the write-path does) continue to match.
var ErrOutsideHome = fmt.Errorf("%w: claudecm/codex: config file resolves outside HOME", storage.ErrOutsideHome)

// importFromCodex is the core Import body — split out so adapter.go's
// Import method stays a one-liner and so the read-side logic can be
// unit-tested via the exported Adapter.Import surface without a
// second entry point in this file.
func (a *Adapter) importFromCodex(ctx context.Context, r *storage.Resolver) (adapter.CoreFromTool, adapter.OverlayFromTool, error) {
	var (
		emptyCore    adapter.CoreFromTool
		emptyOverlay adapter.OverlayFromTool
	)

	// Honour ctx cancellation before any filesystem work. Cheap, but
	// cmd/* propagates SIGINT through here.
	if err := ctx.Err(); err != nil {
		return emptyCore, emptyOverlay, err
	}

	configPath := ConfigPath(r)
	authPath := AuthPath(r)

	// Read config.toml first, then auth.json. Order is symmetric with
	// how Files() advertises them (auth-first for the write side); on
	// the read side the order is not semantically meaningful because
	// neither file's absence disables the other's contribution.
	// config.toml first here so the OpenAI API key from auth.json
	// visibly wins the last-writer-into-Core.APIKey slot even if a
	// future config.toml owned-key expansion tries to touch it.

	configPresent, tomlDoc, err := readCodexTOML(configPath, r)
	if err != nil {
		return emptyCore, emptyOverlay, err
	}

	authPresent, authFlat, err := readCodexAuth(authPath, r)
	if err != nil {
		return emptyCore, emptyOverlay, err
	}

	if !configPresent && !authPresent {
		// Neither file exists (or both are empty/whitespace-only,
		// which is treated as absent for import purposes). Present
		// the "nothing to import" UX.
		return emptyCore, emptyOverlay, fmt.Errorf("%w: %s, %s", ErrNoConfig, configPath, authPath)
	}

	core, overlay := extractOwnedCodex(tomlDoc, authFlat)
	return core, overlay, nil
}

// readCodexTOML resolves configPath, verifies HOME containment, and
// loads the TOML doc when present. Returns:
//
//   - (false, nil, nil) when the file is absent (ErrNotExist) OR
//     present-but-empty per treatAsEmpty. Absence is not an error at
//     this level; the caller decides whether both files being absent
//     is an error.
//   - (true, doc, nil) when the file is present and parses.
//   - (false, nil, err) on any hard failure: parse error, containment
//     violation, or non-ENOENT read error.
func readCodexTOML(configPath string, r *storage.Resolver) (bool, *codextoml.Doc, error) {
	if err := verifyReadTargetInHomeCodex(configPath, r); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			// verifyReadTargetInHomeCodex surfaces ErrNotExist via
			// EvalSymlinks. Map to "absent"; the caller decides.
			return false, nil, nil
		}
		return false, nil, err
	}

	data, err := os.ReadFile(configPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil, nil
		}
		return false, nil, fmt.Errorf("codex import: read %q: %w", configPath, err)
	}

	if treatAsEmpty(data) {
		// Documented policy: zero-byte or whitespace-only config.toml
		// is treated as absent (no contribution) rather than as an
		// empty valid document. Symmetric with auth.json.
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

// readCodexAuth resolves authPath, verifies HOME containment, and
// loads + flattens the JSON when present. Same tri-state return
// contract as readCodexTOML.
//
// The flattened map is what extractOwnedCodex consumes so that
// OPENAI_API_KEY (top-level) and tokens.* (nested) can be looked up
// against OwnedKeysAuthJSON entries by exact-string match on the
// same shape writepath.Flatten produces.
func readCodexAuth(authPath string, r *storage.Resolver) (bool, map[string]any, error) {
	if err := verifyReadTargetInHomeCodex(authPath, r); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil, nil
		}
		return false, nil, err
	}

	data, err := os.ReadFile(authPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil, nil
		}
		return false, nil, fmt.Errorf("codex import: read %q: %w", authPath, err)
	}

	if treatAsEmpty(data) {
		return false, nil, nil
	}

	var root map[string]any
	if err := json.Unmarshal(data, &root); err != nil {
		return false, nil, fmt.Errorf("%w: %s: %v", ErrParseFailed, authPath, err)
	}
	if root == nil {
		// json.Unmarshal into map[string]any accepts `null` and
		// leaves the map nil. auth.json is documented as an object;
		// a null root is not a shape we should silently accept.
		return false, nil, fmt.Errorf("%w: %s: top-level value is null, want JSON object", ErrParseFailed, authPath)
	}

	flat, err := writepath.Flatten(root)
	if err != nil {
		return false, nil, fmt.Errorf("%w: %s: flatten: %v", ErrParseFailed, authPath, err)
	}
	return true, flat, nil
}

// extractOwnedCodex distributes owned-key values from the two source
// documents into (CoreFromTool, OverlayFromTool) per the mapping
// documented in the file-level godoc.
//
//   - config.toml values → Overlay.Raw at the flat dotted key.
//   - auth.json OPENAI_API_KEY → Core.APIKey (null skips; empty
//     string preserved).
//   - auth.json auth_mode, last_refresh, tokens.* → Overlay.Raw at
//     the flat dotted key.
//
// Pure — no I/O.
func extractOwnedCodex(doc *codextoml.Doc, authFlat map[string]any) (adapter.CoreFromTool, adapter.OverlayFromTool) {
	var (
		core    adapter.CoreFromTool
		overlay adapter.OverlayFromTool
	)

	// config.toml → Overlay.Raw. Doc.Get returns (value, ok); a
	// missing key contributes nothing (the profile candidate is
	// sparse by design).
	if doc != nil {
		for _, key := range OwnedKeysConfigTOML {
			v, ok := doc.Get(key)
			if !ok {
				continue
			}
			putOverlayRaw(&overlay, key, v)
		}
	}

	// auth.json → Core.APIKey + Overlay.Raw. OPENAI_API_KEY is the
	// only key that lands in Core; everything else in the auth
	// allowlist is Overlay-only.
	if authFlat != nil {
		for _, key := range OwnedKeysAuthJSON {
			v, ok := authFlat[key]
			if !ok {
				continue
			}
			if key == "OPENAI_API_KEY" {
				// null → skip (do not zero Core.APIKey);
				// non-null (including "") → wins into Core.APIKey
				// verbatim as a string.
				if v == nil {
					continue
				}
				core.APIKey = coerceToStringCodex(v)
				continue
			}
			// Everything else in OwnedKeysAuthJSON (auth_mode,
			// last_refresh, tokens.*) → Overlay.Raw. Keep the
			// nil sentinel too so a round-trip that saw a JSON
			// null re-emits it (Codex may legitimately carry
			// `"last_refresh": null` between token refreshes).
			putOverlayRaw(&overlay, key, v)
		}
	}

	return core, overlay
}

// putOverlayRaw lazily initializes overlay.Raw and writes v under
// name. Kept small so its callers stay linear.
func putOverlayRaw(overlay *adapter.OverlayFromTool, name string, v any) {
	if overlay.Raw == nil {
		overlay.Raw = map[string]any{}
	}
	overlay.Raw[name] = v
}

// coerceToStringCodex converts a JSON- or TOML-decoded any value into
// a string suitable for Core.APIKey. Real auth.json OPENAI_API_KEY
// values are strings; a hand-edit that sneaks a non-string in is
// coerced rather than dropped, mirroring the claudecode Import
// coercion.
//
// nil is intentionally NOT handled here (callers check nil upstream
// so it can distinguish "absent slot" from "empty string").
func coerceToStringCodex(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case bool:
		if t {
			return "true"
		}
		return "false"
	case float64:
		if t == float64(int64(t)) {
			return fmt.Sprintf("%d", int64(t))
		}
		return fmt.Sprintf("%g", t)
	case int64:
		return fmt.Sprintf("%d", t)
	default:
		return fmt.Sprintf("%v", v)
	}
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

// verifyReadTargetInHomeCodex performs the read-side containment
// check for one owned file. Resolves the FULL path (not just the
// leaf) through EvalSymlinks so a symlink at ANY component — including
// a parent directory such as ~/.codex — must land inside HOME.
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
// adapter's verifyReadTargetInHome for the same reason as treatAsEmpty:
// cross-adapter dependencies are forbidden by the interface contract.
func verifyReadTargetInHomeCodex(path string, r *storage.Resolver) error {
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			// Bubble ErrNotExist to caller so it can map to "absent".
			return err
		}
		return fmt.Errorf("codex import: evalsymlinks %q: %w", path, err)
	}
	resolvedHome, err := filepath.EvalSymlinks(r.Home())
	if err != nil {
		return fmt.Errorf("codex import: evalsymlinks home %q: %w", r.Home(), err)
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
