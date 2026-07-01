// Package claudecode: Import implementation.
//
// This file carries the Import surface for the Claude Code adapter
// (E3-S3). It is deliberately split out of adapter.go so the read-side
// JSON extraction machinery does not crowd the adapter's public
// contract methods, which stay in adapter.go for grep-friendliness.
//
// Design notes
// ============
//
// Empty-file policy. A zero-byte settings.json is a well-defined shape:
// Claude Code writes an empty file on its very first launch before any
// user setting has been recorded. Import interprets an empty file as
// the JSON object `{}` — same result as an actual `{}` on disk. This is
// NOT a fallback rewrite (Import never writes); it is a documented read
// policy. NFR-S1's "no silent fallback" rule addresses fallback WRITES
// on malformed input; a well-defined interpretation of an empty file at
// read time is a different question and is answered here explicitly.
//
// Unknown-key policy. Claude Code's settings.json legally carries keys
// this adapter does not own — permissions, hooks, mcpServers, model,
// theme, and any future knob. Import does NOT copy these into the
// returned Profile candidate: OverlayFromTool.Raw is left empty on the
// Import path. Byte-identical round-trip is delivered by the write-path
// merge-preserve at Apply time (PRD §4.7, FR-5): when the user later
// activates the imported profile, writepath.Apply reads the current
// on-disk settings.json and preserves every unowned key verbatim, so
// the round-trip Import → Render → Apply reproduces the original bytes
// on the owned-key scope without carrying the unowned bytes through the
// Profile round-trip. Recording unowned keys in Overlay.Raw would
// double-source them and, on any hand-edit of the on-disk file between
// Import and Apply, produce a stale re-emission.
//
// Symlink policy (read side). Import will FOLLOW a symlink at
// settings.json when the resolved target lands inside HOME. This is
// softer than the write side (E1-S3), which refuses to write through a
// symlink at all. The asymmetry is intentional: a read cannot damage
// the target file, so following an in-HOME link matches operator
// intent. A symlink whose target lands outside HOME is refused with
// ErrOutsideHome — reading /etc/passwd through a planted symlink is
// still an attack surface.
//
// AUTH_TOKEN vs API_KEY. Precedence is decided on "non-null value
// present", NOT on "key present in map". The distinction matters
// because a real Claude Code settings.json can legitimately carry
// `"ANTHROPIC_AUTH_TOKEN": null` (a user editor clearing the value
// without deleting the key) alongside `"ANTHROPIC_API_KEY": "sk-real"`
// — and a naive "was the key in the map?" check would silently zero
// Core.APIKey and demote the real API_KEY to Overlay.ExtraEnv.
//
// Rules:
//
//   - AUTH_TOKEN has non-null value → wins into Core.APIKey. If
//     API_KEY also has a non-null value, API_KEY is recorded in
//     Overlay.ExtraEnv for round-trip fidelity.
//   - Else if API_KEY has a non-null value → API_KEY wins into
//     Core.APIKey. Overlay unchanged.
//   - Else → Core.APIKey stays empty. Overlay unchanged.
//
// Empty string ("") is a valid non-null value: it is preserved
// verbatim and does NOT trigger fallback to the other slot. That
// matches operator intent — a user who explicitly writes `""` is
// asking Claude Code to run with an empty token, not asking claudecm
// to substitute a different credential. The null case is the only one
// that skips the slot entirely.

package claudecode

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/a2d2-dev/claudecm/internal/adapter"
	"github.com/a2d2-dev/claudecm/internal/storage"
	"github.com/a2d2-dev/claudecm/internal/writepath"
)

// ErrNoConfig is returned by Import when the tool's settings.json is
// simply absent — a fresh install that has never launched. Callers use
// errors.Is to distinguish "tool not installed / nothing to import" from
// "settings.json exists but is malformed", which returns ErrParseFailed.
var ErrNoConfig = errors.New("claudecm/claudecode: settings.json not found")

// ErrParseFailed is returned by Import when settings.json exists but
// cannot be decoded as JSON, or when its shape is not a top-level JSON
// object. Wraps writepath.ErrParseFailed so callers that already
// errors.Is against the writepath sentinel (the shared parse-failure
// signal for FR-5 step 3) still match without needing to know about
// this adapter's private sentinel. Downstream callers may prefer to
// switch on the writepath sentinel to stay adapter-agnostic; this
// wrapper lets both spellings match.
var ErrParseFailed = fmt.Errorf("%w: claudecm/claudecode: settings.json parse failed", writepath.ErrParseFailed)

// ErrOutsideHome is returned by Import when settings.json is a symlink
// whose resolved target escapes HOME. Wraps storage.ErrOutsideHome so
// callers that already errors.Is against the storage sentinel (as the
// write-path does) continue to match.
var ErrOutsideHome = fmt.Errorf("%w: claudecm/claudecode: settings.json resolves outside HOME", storage.ErrOutsideHome)

// importFromSettings is the core Import body — split out so adapter.go's
// Import method stays a one-liner and so the read-side logic can be
// unit-tested via the exported Adapter.Import surface without a second
// entry point in this file.
func (a *Adapter) importFromSettings(ctx context.Context, r *storage.Resolver) (adapter.CoreFromTool, adapter.OverlayFromTool, error) {
	var (
		emptyCore    adapter.CoreFromTool
		emptyOverlay adapter.OverlayFromTool
	)

	// Honour ctx cancellation before any filesystem work. Cheap, but
	// cmd/* propagates SIGINT through here.
	if err := ctx.Err(); err != nil {
		return emptyCore, emptyOverlay, err
	}

	path := SettingsPath(r)

	// Check symlink containment BEFORE reading. If the path is a
	// symlink whose target escapes HOME we refuse; if it's a symlink
	// with an in-HOME target we follow it (see file-level godoc).
	if err := verifyReadTargetInHome(path, r); err != nil {
		return emptyCore, emptyOverlay, err
	}

	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return emptyCore, emptyOverlay, fmt.Errorf("%w: %s", ErrNoConfig, path)
		}
		return emptyCore, emptyOverlay, fmt.Errorf("claudecode import: read %q: %w", path, err)
	}

	// Empty file → interpret as `{}` (see file-level godoc for the
	// policy rationale). Fall through to the empty flat map below.
	var root map[string]any
	if len(data) == 0 {
		root = map[string]any{}
	} else {
		if err := json.Unmarshal(data, &root); err != nil {
			return emptyCore, emptyOverlay, fmt.Errorf("%w: %s: %v", ErrParseFailed, path, err)
		}
		if root == nil {
			// json.Unmarshal into map[string]any accepts `null` and
			// leaves the map nil. Treat null as parse failure —
			// settings.json is documented as an object; a null root
			// is not a shape we should silently accept.
			return emptyCore, emptyOverlay, fmt.Errorf("%w: %s: top-level value is null, want JSON object", ErrParseFailed, path)
		}
	}

	// Shape-check "env" before flattening. A non-object env (e.g. a
	// user typo like `"env": "sk-typo"`) would flatten to the single
	// key "env" whose value is a scalar; extractOwned would never
	// find "env.ANTHROPIC_*" and would silently under-import every
	// owned credential. Refuse loudly so the operator can fix the
	// typo, matching NFR-S1's "no silent under-import" spirit.
	// A null env is treated as absent (skip); anything present that
	// is not a JSON object is a parse failure.
	if v, ok := root["env"]; ok && v != nil {
		if _, isMap := v.(map[string]any); !isMap {
			return emptyCore, emptyOverlay, fmt.Errorf("%w: %s: env key must be a JSON object, got %T", ErrParseFailed, path, v)
		}
	}

	flat, err := writepath.Flatten(root)
	if err != nil {
		return emptyCore, emptyOverlay, fmt.Errorf("%w: %s: flatten: %v", ErrParseFailed, path, err)
	}

	core, overlay := extractOwned(flat)
	return core, overlay, nil
}

// extractOwned pulls owned-key values out of a flattened settings.json
// and distributes them into (CoreFromTool, OverlayFromTool) per the
// mapping documented in the file-level godoc. Pure — no I/O.
//
// The function only READS from flat; it does not mutate it.
func extractOwned(flat map[string]any) (adapter.CoreFromTool, adapter.OverlayFromTool) {
	var (
		core    adapter.CoreFromTool
		overlay adapter.OverlayFromTool
	)

	// Local helper: fetch an owned env key as string. Returns the
	// coerced string plus whether the key was present in flat.
	getEnvString := func(envKey string) (string, bool) {
		v, ok := flat["env."+envKey]
		if !ok {
			return "", false
		}
		return coerceToString(v), true
	}

	if v, ok := getEnvString("ANTHROPIC_BASE_URL"); ok {
		core.BaseURL = v
	}
	if v, ok := getEnvString("ANTHROPIC_MODEL"); ok {
		core.Model = v
	}
	if v, ok := getEnvString("ANTHROPIC_SMALL_FAST_MODEL"); ok {
		core.SmallFastModel = v
	}

	// AUTH_TOKEN vs API_KEY precedence — see file-level godoc for the
	// full rule. The critical property is that a JSON `null` value at
	// AUTH_TOKEN must NOT shadow a real string at API_KEY. Empty
	// string is a valid non-null value and is preserved verbatim.
	hasAuth := hasNonNullEnvValue(flat, "ANTHROPIC_AUTH_TOKEN")
	hasAPIKey := hasNonNullEnvValue(flat, "ANTHROPIC_API_KEY")
	switch {
	case hasAuth && hasAPIKey:
		authToken, _ := getEnvString("ANTHROPIC_AUTH_TOKEN")
		apiKey, _ := getEnvString("ANTHROPIC_API_KEY")
		core.APIKey = authToken
		putOverlayEnv(&overlay, "ANTHROPIC_API_KEY", apiKey)
	case hasAuth:
		authToken, _ := getEnvString("ANTHROPIC_AUTH_TOKEN")
		core.APIKey = authToken
	case hasAPIKey:
		apiKey, _ := getEnvString("ANTHROPIC_API_KEY")
		core.APIKey = apiKey
	}

	// Tool-specific toggles land in the overlay's ExtraEnv. These are
	// NOT provider-neutral (they select a different backend entirely),
	// so they do not have a Core representation.
	if v, ok := getEnvString("CLAUDE_CODE_USE_BEDROCK"); ok {
		putOverlayEnv(&overlay, "CLAUDE_CODE_USE_BEDROCK", v)
	}
	if v, ok := getEnvString("CLAUDE_CODE_USE_VERTEX"); ok {
		putOverlayEnv(&overlay, "CLAUDE_CODE_USE_VERTEX", v)
	}

	return core, overlay
}

// hasNonNullEnvValue reports whether flat carries an env.<key> entry
// whose value is NOT JSON null. Used by the AUTH_TOKEN vs API_KEY
// precedence to distinguish three shapes:
//
//   - key absent from map            → returns false
//   - key present with value == nil  → returns false (JSON null)
//   - key present with any other v   → returns true (including "")
//
// Empty string is deliberately treated as "present, valid value" so
// that a user who explicitly clears AUTH_TOKEN by writing `""` is not
// second-guessed by claudecm silently promoting API_KEY into its slot.
// See the file-level godoc for the policy rationale.
func hasNonNullEnvValue(flat map[string]any, envKey string) bool {
	v, ok := flat["env."+envKey]
	if !ok {
		return false
	}
	return v != nil
}

// putOverlayEnv lazily initializes overlay.ExtraEnv and writes v under
// name. Kept small so its callers stay linear.
func putOverlayEnv(overlay *adapter.OverlayFromTool, name, v string) {
	if overlay.ExtraEnv == nil {
		overlay.ExtraEnv = map[string]string{}
	}
	overlay.ExtraEnv[name] = v
}

// coerceToString converts a JSON-decoded any value into the string
// shape env vars take on the wire. Real Claude Code settings.json env
// values are strings, but the schema does not forbid a bool ("true") or
// a number ("1") sneaking in; a targeted coercion is friendlier than
// dropping the value.
//
// Nil is coerced to the empty string; slice/map/complex types round-trip
// through fmt to produce a deterministic textual form. Callers that
// require strict-string enforcement should apply it upstream.
func coerceToString(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case bool:
		if t {
			return "true"
		}
		return "false"
	case float64:
		// JSON numbers round-trip through float64 by default. Emit an
		// integer literal when the number is integral to avoid the
		// misleading "1.000000" surface.
		if t == float64(int64(t)) {
			return fmt.Sprintf("%d", int64(t))
		}
		return fmt.Sprintf("%g", t)
	case nil:
		return ""
	default:
		return fmt.Sprintf("%v", v)
	}
}

// verifyReadTargetInHome performs the read-side containment check.
// It resolves the FULL path (not just the leaf) through EvalSymlinks
// so a symlink at ANY component — including a parent directory such
// as ~/.claude — must land inside HOME. An earlier revision only
// Lstat'd the leaf; that contradicted the file-level symlink policy
// because a hostile parent symlink (e.g. ~/.claude → /etc) would slip
// through and Import would happily read /etc/settings.json.
//
// Behaviour:
//
//   - EvalSymlinks succeeds → filepath.Rel against the resolved HOME.
//     A ".." prefix or absolute rel means the resolved path escapes
//     HOME → ErrOutsideHome. Otherwise nil.
//   - EvalSymlinks returns ErrNotExist → the file simply is not
//     there (missing config, or dangling symlink target). Return
//     ErrNoConfig so the caller presents the "nothing to import" UX.
//   - Any other EvalSymlinks error → surface verbatim.
//
// The write-path's checkUnderHome does something similar but is
// scoped to "planning a write, parent may not exist yet". The read
// side always requires the file to already exist, so it is simpler
// to inline the semantics than to grow the write-path helper.
func verifyReadTargetInHome(path string, r *storage.Resolver) error {
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		// ErrNotExist covers both "file/parent absent" and "dangling
		// symlink target". Both map to ErrNoConfig; the caller does
		// not care which shape produced the absence.
		if errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("%w: %s", ErrNoConfig, path)
		}
		return fmt.Errorf("claudecode import: evalsymlinks %q: %w", path, err)
	}
	resolvedHome, err := filepath.EvalSymlinks(r.Home())
	if err != nil {
		return fmt.Errorf("claudecode import: evalsymlinks home %q: %w", r.Home(), err)
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
