// Package claudecode: Project implementation.
//
// This file carries the Project surface for the Claude Code adapter
// (E3-S6). It is deliberately split out of adapter.go so the layered
// resolver machinery does not crowd the adapter's public contract
// methods, which stay in adapter.go for grep-friendliness.
//
// Design notes
// ============
//
// Layer chain. Project resolves every owned key by walking the frozen
// precedence chain (architecture.md §6, PRD FR-7):
//
//	BuiltInDefault < ProfileCore < ProfileOverlay < OnDiskToolConfig < EnvOverride
//
// The topmost layer that carries a value for a given key WINS. All
// lower layers that also carried a value are recorded in
// EffectiveField.Shadowed in older→newer order so `explain` can
// render the full chain without a second pass through raw config.
//
// Owned-key-only projection. Project ONLY emits EffectiveField entries
// for keys in OwnedKeysSettingsJSON. Non-owned settings.json keys
// (permissions, hooks, mcpServers, model, theme, ...) round-trip
// verbatim through the write-path merge-preserve and are irrelevant to
// the claudecm effective view — projecting them would leak a shape
// operators expect us to leave alone.
//
// Env-var allowlist (NFR-E1). The per-tool env allowlist is hardcoded
// here — one entry per owned key, mapping the flat JSON key path to
// the environment variable name Claude Code reads at runtime. This is
// the SAME set of env-var names Import wired into Core/Overlay in
// E3-S3; keeping the allowlist local to this file keeps the read-side
// layer chain grep-close to its owned-key contract.
//
// ProfileCore mapping. Values in Profile.Core route into env.* slots
// via the same mapping Plan uses (plan.go collectOwnedValues), so an
// import → project of the same profile reports the layer chain a
// switch would activate. Notably:
//
//   - env.ANTHROPIC_BASE_URL       ← profile.Core.BaseURL
//   - env.ANTHROPIC_AUTH_TOKEN     ← profile.Core.APIKey
//   - env.ANTHROPIC_MODEL          ← profile.Core.Model
//   - env.ANTHROPIC_SMALL_FAST_MODEL ← profile.Core.SmallFastModel
//   - env.ANTHROPIC_API_KEY        ← (NOT a Core slot; overlay-only)
//   - env.CLAUDE_CODE_USE_BEDROCK  ← (NOT a Core slot; overlay-only)
//   - env.CLAUDE_CODE_USE_VERTEX   ← (NOT a Core slot; overlay-only)
//
// ProfileOverlay mapping. Every env-allowlisted key can additionally
// be sourced from profile.Tools[ToolClaudeCode].ExtraEnv[<envName>] —
// this is the overlay-as-truth escape hatch (NFR-S6). Presence in the
// ExtraEnv map is the source of truth ("" is a real value).
//
// OnDiskToolConfig. Read directly from settings.json bytes via gjson,
// so unowned keys are never materialised into the resolver state.
// gjson.Get returns a Result whose Exists() disambiguates "absent"
// from "present but empty string" — same shape as the ExtraEnv rule.
//
// EnvOverride. Read via envextract.Lookup — the single env-read
// primitive shared with the Codex adapter (E5-S3). A non-empty
// env-var value wins over all lower layers. Empty string on the
// process env is treated as "not set" here because Claude Code
// itself will not consume an empty env var literal to override a
// settings.json value — matching that runtime behaviour keeps the
// effective view honest. The build-tag seam that lets tests inject
// a synthetic env universe lives in internal/envextract (see
// SetLookupForTest under `//go:build test`); this adapter carries
// no per-package seam of its own.
//
// BuiltInDefault. No v1 owned key has a built-in default value (the
// architecture.md §6 default layer is empty for Claude Code today).
// The layer is still enumerated in the code path so a future default
// addition slots in without a resolver rewrite.
//
// Redaction contract. EffectiveField.Secret is set to true on the two
// credential-carrying owned keys (ANTHROPIC_API_KEY and
// ANTHROPIC_AUTH_TOKEN). Downstream renderers (cmd/current,
// cmd/explain) apply the actual redaction; Project is the adapter
// authority on which fields are secret (per the adapter.EffectiveField
// godoc redaction contract).

package claudecode

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"

	"github.com/tidwall/gjson"

	"github.com/a2d2-dev/claudecm/internal/adapter"
	"github.com/a2d2-dev/claudecm/internal/config"
	"github.com/a2d2-dev/claudecm/internal/envextract"
	"github.com/a2d2-dev/claudecm/internal/storage"
)

// envVarForOwnedKey maps a flat owned-key path to the environment
// variable name Claude Code reads at runtime. Frozen alongside the
// owned-key allowlist. Adding a key requires updating both
// OwnedKeysSettingsJSON and this table in the same PR.
//
// This is the per-tool env-var allowlist required by PRD NFR-E1: only
// these variable names are read from the process environment when
// building the EffectiveView.
var envVarForOwnedKey = map[string]string{
	"env.ANTHROPIC_API_KEY":          "ANTHROPIC_API_KEY",
	"env.ANTHROPIC_AUTH_TOKEN":       "ANTHROPIC_AUTH_TOKEN",
	"env.ANTHROPIC_BASE_URL":         "ANTHROPIC_BASE_URL",
	"env.ANTHROPIC_MODEL":            "ANTHROPIC_MODEL",
	"env.ANTHROPIC_SMALL_FAST_MODEL": "ANTHROPIC_SMALL_FAST_MODEL",
	"env.CLAUDE_CODE_USE_BEDROCK":    "CLAUDE_CODE_USE_BEDROCK",
	"env.CLAUDE_CODE_USE_VERTEX":     "CLAUDE_CODE_USE_VERTEX",
}

// secretOwnedKeys lists the owned-key paths whose Value must be
// flagged Secret=true in the EffectiveField (redaction contract).
// Kept as a set for O(1) lookup during the per-key resolve loop.
var secretOwnedKeys = map[string]bool{
	"env.ANTHROPIC_API_KEY":    true,
	"env.ANTHROPIC_AUTH_TOKEN": true,
}

// init verifies that envVarForOwnedKey is a total function over
// OwnedKeysSettingsJSON. Symmetric with the sorted/no-duplicate init
// invariant in allowlist.go and the collectOwnedValues completeness
// panic in plan.go — if a future owned key lands in the allowlist
// without a paired env-var mapping here, the resolver would silently
// stop projecting a real on-disk / env / overlay contribution for that
// key. Fail LOUDLY at package load instead of degrading silently.
func init() {
	if len(envVarForOwnedKey) != len(OwnedKeysSettingsJSON) {
		panic(fmt.Errorf("claudecode project: env-var map size %d != owned keys size %d", len(envVarForOwnedKey), len(OwnedKeysSettingsJSON)))
	}
	for _, key := range OwnedKeysSettingsJSON {
		if _, ok := envVarForOwnedKey[key]; !ok {
			panic(fmt.Errorf("claudecode project: owned key %q has no env-var mapping", key))
		}
	}
}

// projectFromProfile is the core Project body — split out of adapter.go
// so the file that lists the adapter's public methods stays a
// grep-friendly index. Returns an EffectiveView populated with one
// EffectiveField per owned key that any layer contributed a value to.
//
// Read-only: never writes to disk, never mutates the process env.
func (a *Adapter) projectFromProfile(ctx context.Context, r *storage.Resolver, profile config.Profile) (adapter.EffectiveView, error) {
	// TODO(E5/E7 state-schema evolution): populate
	// view.ExternalDriftDetected / view.ExternalDriftFile once
	// internal/config.State grows a LastAppliedPerTool[claude_code].SHA256
	// slot. See docs/plan/stories/E3-S6.md "Implementation Notes /
	// Decision" for the rationale — the EffectiveView fields exist on
	// the adapter type today but are left zero-valued by E3-S6 because
	// the state schema does not yet carry the SHA256 to compare against.
	view := adapter.EffectiveView{Tool: adapter.ToolClaudeCode}

	// Honour ctx cancellation before any filesystem or env work.
	if err := ctx.Err(); err != nil {
		return view, err
	}

	// Read on-disk settings.json (best-effort). Missing → empty {}.
	// Symlink-outside-HOME → ErrOutsideHome. Malformed → ErrParseFailed.
	settingsPath := SettingsPath(r)
	onDisk, haveOnDiskFile, err := readOnDiskSettings(settingsPath, r)
	if err != nil {
		return view, err
	}

	// Cache the overlay ExtraEnv map (may be nil) once so the per-key
	// loop can look it up without re-walking profile.Tools each time.
	overlayEnv := overlayExtraEnv(profile)

	fields := make([]adapter.EffectiveField, 0, len(OwnedKeysSettingsJSON))
	for _, key := range OwnedKeysSettingsJSON {
		envName := envVarForOwnedKey[key]

		// Gather each layer's contribution for this key. Nil = "layer
		// did not contribute a value"; a non-nil layerValue captures
		// both the value and the source string.
		defaultLV := builtInDefaultLayer(key)
		coreLV := profileCoreLayer(key, profile)
		overlayLV := profileOverlayLayer(envName, overlayEnv)
		diskLV := onDiskLayer(key, envName, onDisk, haveOnDiskFile, settingsPath)
		envLV := envOverrideLayer(envName)

		// Ordered older→newer so a linear pass finds the winner and
		// the shadowed set in one sweep.
		chain := []*layerValue{defaultLV, coreLV, overlayLV, diskLV, envLV}
		field, present := resolveChain(key, chain)
		if !present {
			// No layer had a value for this owned key → do not emit
			// an EffectiveField. Owned keys with zero layers active
			// are absent from `current` / `explain` output entirely,
			// matching the "only surfaced when something is set" UX
			// that keeps the view scannable.
			continue
		}
		if secretOwnedKeys[key] {
			field.Secret = true
			for i := range field.Shadowed {
				field.Shadowed[i].Secret = true
			}
		}
		fields = append(fields, field)
	}

	adapter.SortFields(fields)
	view.Fields = fields
	return view, nil
}

// layerValue captures a single layer's contribution for one owned key.
// A layer that did not contribute is represented by a nil *layerValue
// so the resolve loop does not need a boolean-and-value pair per slot.
type layerValue struct {
	layer  adapter.Layer
	value  any
	source string
}

// resolveChain walks the older→newer layer chain, picks the topmost
// non-nil entry as the winner, and records every lower non-nil entry
// in EffectiveField.Shadowed (older→newer). Returns (field, false)
// when NO layer contributed a value — the caller then omits the field
// entirely.
func resolveChain(key string, chain []*layerValue) (adapter.EffectiveField, bool) {
	var winner *layerValue
	var winnerIdx int
	for i, lv := range chain {
		if lv == nil {
			continue
		}
		winner = lv
		winnerIdx = i
	}
	if winner == nil {
		return adapter.EffectiveField{}, false
	}

	// Shadowed = every non-nil layer strictly BELOW the winner, in
	// older→newer order. The chain slice is already in that order so
	// we iterate 0..winnerIdx-1.
	var shadowed []adapter.ShadowedLayer
	for i := 0; i < winnerIdx; i++ {
		lv := chain[i]
		if lv == nil {
			continue
		}
		shadowed = append(shadowed, adapter.ShadowedLayer{
			Layer:  lv.layer,
			Source: lv.source,
			Value:  lv.value,
		})
	}

	return adapter.EffectiveField{
		Key:          key,
		Value:        winner.value,
		WinningLayer: winner.layer,
		Source:       winner.source,
		Shadowed:     shadowed,
	}, true
}

// builtInDefaultLayer returns the built-in default value for key, or
// nil when no v1 default exists. Reserved for future defaults —
// currently returns nil for every owned key (see file-level godoc).
func builtInDefaultLayer(_ string) *layerValue {
	// V1: no built-in defaults for any owned key. Kept as a named
	// hook so a future default addition slots in without touching
	// resolveChain.
	return nil
}

// profileCoreLayer returns the ProfileCore contribution for key, or
// nil when Core does not carry that slot (empty string counts as
// "not present" for Core, symmetric with Plan.collectOwnedValues).
func profileCoreLayer(key string, profile config.Profile) *layerValue {
	// Only the four Core-routed keys route through this layer. The
	// three overlay-only keys (API_KEY, USE_BEDROCK, USE_VERTEX)
	// deliberately return nil so a Core value can never accidentally
	// shadow an unrelated slot.
	var v string
	switch key {
	case "env.ANTHROPIC_BASE_URL":
		v = profile.Core.BaseURL
	case "env.ANTHROPIC_AUTH_TOKEN":
		v = profile.Core.APIKey
	case "env.ANTHROPIC_MODEL":
		v = profile.Core.Model
	case "env.ANTHROPIC_SMALL_FAST_MODEL":
		v = profile.Core.SmallFastModel
	default:
		return nil
	}
	if v == "" {
		return nil
	}
	return &layerValue{
		layer:  adapter.LayerCore,
		value:  v,
		source: "profile.core",
	}
}

// overlayExtraEnv returns the profile's Claude Code overlay ExtraEnv
// map, or nil when the overlay is absent. Extracted so the per-key
// loop does not re-walk profile.Tools on every iteration.
func overlayExtraEnv(profile config.Profile) map[string]string {
	if profile.Tools == nil {
		return nil
	}
	ov, ok := profile.Tools[adapter.ToolClaudeCode]
	if !ok {
		return nil
	}
	return ov.ExtraEnv
}

// profileOverlayLayer returns the ProfileOverlay contribution for
// envName, or nil when the overlay does not carry that entry. Presence
// in the ExtraEnv map is the source of truth ("" is a real value; see
// Plan.collectOwnedValues for the symmetric write-side rule).
func profileOverlayLayer(envName string, overlayEnv map[string]string) *layerValue {
	if overlayEnv == nil {
		return nil
	}
	v, ok := overlayEnv[envName]
	if !ok {
		return nil
	}
	return &layerValue{
		layer:  adapter.LayerOverlay,
		value:  v,
		source: "profile.overlay",
	}
}

// onDiskLayer returns the OnDiskToolConfig contribution for key, or
// nil when the on-disk settings.json does not carry it. Presence is
// determined via gjson.GetBytes(...).Exists() so a legitimate empty
// string on disk is preserved verbatim.
//
// When the settings.json file is absent entirely (haveOnDiskFile ==
// false) every key returns nil — an absent file cannot contribute a
// value.
func onDiskLayer(key, envName string, onDisk []byte, haveOnDiskFile bool, settingsPath string) *layerValue {
	if !haveOnDiskFile {
		return nil
	}
	// Path is a two-segment gjson query: env.<envName>. Owned keys
	// carry no literal "." inside envName so no escaping is needed.
	gjsonPath := "env." + envName
	res := gjson.GetBytes(onDisk, gjsonPath)
	if !res.Exists() {
		return nil
	}
	if res.Type == gjson.Null {
		// JSON null on disk is not a contribution — mirrors Import's
		// null-is-absent rule for AUTH_TOKEN/API_KEY precedence and
		// keeps a settings.json with `"ANTHROPIC_MODEL": null` from
		// shadowing a real ProfileCore value.
		return nil
	}
	// Preserve the on-disk type when it is a JSON primitive. Owned
	// env values are documented as strings in Claude Code's schema
	// but a real settings.json can legitimately carry a bool for the
	// USE_BEDROCK / USE_VERTEX toggles; keeping the gjson-typed value
	// means `explain` renders "true" / "false" not "1" / "0".
	value := gjsonValueToAny(res)
	// Source names the file plus the JSON pointer so `explain` can
	// send an operator directly to the line.
	return &layerValue{
		layer:  adapter.LayerOnDisk,
		value:  value,
		source: settingsPath + ":" + gjsonPath,
	}
}

// gjsonValueToAny coerces a gjson.Result into the Go type
// EffectiveField.Value carries. Strings stay strings; JSON bools stay
// bools; JSON numbers stay float64 (matching encoding/json's decode
// shape so downstream renderers can type-switch uniformly). Anything
// else (array, object) round-trips as its raw JSON text — arrays and
// objects are not currently valid shapes for any owned env value but
// preserving the raw text keeps `explain` diagnostic when a
// hand-edited file goes off-schema.
func gjsonValueToAny(res gjson.Result) any {
	switch res.Type {
	case gjson.String:
		return res.Str
	case gjson.True:
		return true
	case gjson.False:
		return false
	case gjson.Number:
		return res.Num
	default:
		return res.Raw
	}
}

// envOverrideLayer returns the EnvOverride contribution for envName,
// or nil when the process env has no non-empty value for that name.
// Empty string is treated as absent because Claude Code will not use
// an empty env var literal to override a settings.json value.
func envOverrideLayer(envName string) *layerValue {
	// envextract.Lookup is the single env-read primitive for the
	// resolver (E5-S3). We discard the "set" boolean because Claude
	// Code treats an empty env-var literal as absent — the effective
	// view mirrors that runtime behaviour.
	v, _ := envextract.Lookup(envName)
	if v == "" {
		return nil
	}
	return &layerValue{
		layer:  adapter.LayerEnvOverride,
		value:  v,
		source: "env:" + envName,
	}
}

// readOnDiskSettings reads settings.json off disk with the same
// symlink-containment and malformed-refusal rules Import uses. Returns
// (bytes, present, err) where present=false means "no file on disk"
// (fresh install, treat every OnDisk lookup as absent). A malformed
// file returns ErrParseFailed; a symlink outside HOME returns
// ErrOutsideHome. Both errors match errors.Is against the same
// package sentinels Import surfaces.
//
// Empty (or whitespace-only) bytes are interpreted as `{}` so gjson
// lookups behave uniformly regardless of a fresh-install zero-byte
// shape (shared predicate treatAsEmpty keeps this in lockstep with
// Import / Plan).
func readOnDiskSettings(path string, r *storage.Resolver) ([]byte, bool, error) {
	// Symlink containment mirrors Import's read-side behaviour. When
	// the file is absent (EvalSymlinks ENOENT), Import maps to
	// ErrNoConfig; Project instead treats that as "no on-disk layer"
	// and returns (nil, false, nil) so a fresh install still produces
	// an EffectiveView driven by profile+env.
	if err := verifyReadTargetInHome(path, r); err != nil {
		if errors.Is(err, ErrNoConfig) {
			return nil, false, nil
		}
		return nil, false, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("claudecode project: read %q: %w", path, err)
	}
	if treatAsEmpty(data) {
		return []byte("{}"), true, nil
	}
	// Shape-check the same way Plan does: require an object root.
	// gjson.ValidBytes tolerates a bare `null`/scalar/array at the
	// root, but overlay-as-truth (and OnDisk-layer semantics) only
	// make sense over an object. Refuse anything else with
	// ErrParseFailed so a corrupt file cannot silently under-report
	// the layer chain.
	if !gjson.ValidBytes(data) {
		return nil, false, fmt.Errorf("%w: %s: not valid JSON", ErrParseFailed, path)
	}
	if !json.Valid(data) {
		return nil, false, fmt.Errorf("%w: %s: trailing content after root value", ErrParseFailed, path)
	}
	// Peek at the root byte after trimming; refuse non-object roots.
	trimmed := trimJSONLeadingSpace(data)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return nil, false, fmt.Errorf("%w: %s: settings.json root must be a JSON object", ErrParseFailed, path)
	}
	return data, true, nil
}

// trimJSONLeadingSpace returns data with leading ASCII whitespace
// stripped. Kept local so this file does not import bytes just for a
// one-liner; the JSON spec's insignificant-whitespace set (space, tab,
// LF, CR) matches ASCII whitespace which is what we skip here.
func trimJSONLeadingSpace(data []byte) []byte {
	i := 0
	for i < len(data) {
		c := data[i]
		if c == ' ' || c == '\t' || c == '\n' || c == '\r' {
			i++
			continue
		}
		break
	}
	return data[i:]
}

