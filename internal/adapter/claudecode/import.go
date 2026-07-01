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
// AUTH_TOKEN vs API_KEY. When both env.ANTHROPIC_AUTH_TOKEN and
// env.ANTHROPIC_API_KEY are present, Import prefers AUTH_TOKEN into
// Core.APIKey (matches typical enterprise/console flow) and records
// API_KEY in the overlay's ExtraEnv map so a subsequent round-trip does
// not silently discard it. When only API_KEY is present it wins into
// Core.APIKey. When neither is present Core.APIKey stays empty.

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

	// AUTH_TOKEN vs API_KEY precedence — see file-level godoc.
	authToken, hasAuth := getEnvString("ANTHROPIC_AUTH_TOKEN")
	apiKey, hasAPIKey := getEnvString("ANTHROPIC_API_KEY")
	switch {
	case hasAuth && hasAPIKey:
		core.APIKey = authToken
		putOverlayEnv(&overlay, "ANTHROPIC_API_KEY", apiKey)
	case hasAuth:
		core.APIKey = authToken
	case hasAPIKey:
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

// verifyReadTargetInHome performs the read-side symlink containment
// check. If path exists and is a symlink, EvalSymlinks the target and
// require the result to live under HOME. Absent files and plain
// (non-symlink) files pass through; the actual read decides existence.
// A stat error other than ErrNotExist is surfaced verbatim.
//
// Duplicating checkUnderHome's semantics rather than exporting it: the
// write-path's helper is scoped to "planning a write, parent may not
// exist yet". The read path needs the simpler "the file itself must
// exist and resolve inside HOME"; conflating the two would force the
// write-path helper to grow flags. Small duplication, clear contracts.
func verifyReadTargetInHome(path string, r *storage.Resolver) error {
	info, err := os.Lstat(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil // caller will handle absence via ErrNoConfig
		}
		return fmt.Errorf("claudecode import: lstat %q: %w", path, err)
	}
	if info.Mode()&os.ModeSymlink == 0 {
		return nil // plain file; nothing to resolve
	}

	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		// Dangling symlink → treat as "no config" so the caller can
		// present the missing-config UX. Any other resolution error is
		// surfaced verbatim.
		if errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("%w: %s (dangling symlink target)", ErrNoConfig, path)
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
