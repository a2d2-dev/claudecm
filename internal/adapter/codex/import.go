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
// Empty-of-owned-content policy. A file that PARSES successfully but
// contributes zero owned keys (e.g. an auth.json = `{}` after a fresh
// `codex login` has not yet completed; a config.toml holding only
// comments and unowned sections) is treated as "present-with-nothing-
// owned", NOT as an error. It is a legitimate operational state:
// promoting it to an ErrNoOwnedContent sentinel would false-positive
// on real Codex installs that keep OAuth tokens under nested keys
// claudecm may not yet own. Callers wanting to distinguish "nothing
// owned" from "file missing" can compare against zero-value Core and
// Overlay themselves; the file-presence signal is separately exposed
// through Detect / Presence. This matches the treatAsEmpty policy at
// the byte level: empty parsed content is normal, not exceptional.
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
// skips the slot entirely. The OPENAI_API_KEY null-vs-empty-vs-string
// decision reads from the unflattened root map rather than from
// writepath.Flatten output: OPENAI_API_KEY is top-level and
// unambiguously scalar, so decoupling from Flatten's contract keeps
// the null-safety invariant local to this file. Flatten is still used
// for the nested tokens.* lookups where the flat dotted-path shape is
// exactly what OwnedKeysAuthJSON encodes.
//
// Null-owned-key policy (auth.json). For OWNED keys other than
// OPENAI_API_KEY (auth_mode, last_refresh, tokens.*), a JSON null is
// DROPPED during Import v1 rather than mirrored into Overlay.Raw.
// Rationale: on the write side, codex/toml.Doc.Set(k, nil) deletes
// the key. If Import preserved nil into Overlay.Raw, the same value
// would DELETE the key on re-render — the opposite of preserving it.
// Cross-format "null sentinel" plumbing (TOML has no natural null;
// JSON does) is not worth the v1 complexity for the sole realistic
// case (`last_refresh: null` between token refreshes). Legitimate
// unowned nulls in the operator's on-disk config are still preserved
// end-to-end by merge-preserve at Apply time (PRD §4.7). If a
// legitimate owned-key null needs preservation, that is a post-v1
// feature requiring a null-sentinel design + migration.
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
	"context"
	"errors"
	"fmt"

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

	configPresent, tomlDoc, _, err := readCodexTOMLWithPrefix(configPath, r, "codex import")
	if err != nil {
		return emptyCore, emptyOverlay, err
	}

	authPresent, authRoot, authFlat, _, err := readCodexAuthWithPrefix(authPath, r, "codex import")
	if err != nil {
		return emptyCore, emptyOverlay, err
	}

	if !configPresent && !authPresent {
		// Neither file exists (or both are empty/whitespace-only,
		// which is treated as absent for import purposes). Present
		// the "nothing to import" UX.
		return emptyCore, emptyOverlay, fmt.Errorf("%w: %s, %s", ErrNoConfig, configPath, authPath)
	}

	core, overlay := extractOwnedCodex(tomlDoc, authRoot, authFlat)
	return core, overlay, nil
}

// Read-side helpers (readCodexTOMLWithPrefix, readCodexAuthWithPrefix,
// treatAsEmpty, verifyReadTargetInHomeCodex) live in readers.go so
// Import and Project share one implementation of the two-file read
// discipline. See readers.go for the tri-state contract and the
// null/empty/symlink policy documented here.

// extractOwnedCodex distributes owned-key values from the two source
// documents into (CoreFromTool, OverlayFromTool) per the mapping
// documented in the file-level godoc.
//
//   - config.toml values → Overlay.Raw at the flat dotted key.
//   - auth.json OPENAI_API_KEY → Core.APIKey (null skips; empty
//     string preserved). Read from the unflattened root map so the
//     null-vs-empty-vs-string decision does not depend on
//     writepath.Flatten's nil-handling contract.
//   - auth.json auth_mode, last_refresh, tokens.* → Overlay.Raw at
//     the flat dotted key. JSON null values are DROPPED (see the
//     file-level Null-owned-key policy note) — Doc.Set(k, nil)
//     deletes on re-render, so preserving nil into Overlay.Raw
//     would be a lie.
//
// Pure — no I/O.
func extractOwnedCodex(doc *codextoml.Doc, authRoot, authFlat map[string]any) (adapter.CoreFromTool, adapter.OverlayFromTool) {
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

	// auth.json → Core.APIKey + Overlay.Raw. OPENAI_API_KEY reads
	// from the unflattened root so we do not depend on Flatten's
	// nil handling for the null-vs-empty-vs-string decision. Every
	// other owned auth key goes through the flat view where the
	// flat dotted-path shape is exactly what OwnedKeysAuthJSON
	// encodes.
	if authRoot != nil {
		if v, ok := authRoot["OPENAI_API_KEY"]; ok && v != nil {
			// null → skipped by the ok/nil guard (do NOT zero
			// Core.APIKey); non-null (including "") → wins into
			// Core.APIKey verbatim as a string.
			core.APIKey = coerceToStringCodex(v)
		}
	}
	if authFlat != nil {
		for _, key := range OwnedKeysAuthJSON {
			if key == "OPENAI_API_KEY" {
				// Handled above via the root map. Skip here so
				// we do not double-book into Overlay.Raw.
				continue
			}
			v, ok := authFlat[key]
			if !ok {
				continue
			}
			if v == nil {
				// v1 policy: null owned auth keys are DROPPED.
				// See the file-level Null-owned-key policy note.
				continue
			}
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

// treatAsEmpty and verifyReadTargetInHomeCodex live in readers.go so
// Import and Project share the empty-file and symlink-containment
// policies. See readers.go for the behavioural contract.
