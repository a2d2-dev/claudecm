// Package claudecode: Plan implementation.
//
// This file carries the Plan surface for the Claude Code adapter
// (E3-S4). It lives alongside adapter.go / import.go so the write-side
// renderer stays grep-close to its read-side counterpart while
// adapter.go remains a thin dispatcher of the public contract.
//
// Design notes
// ============
//
// One-file scope. Claude Code (user scope, v1) owns exactly one file:
// ~/.claude/settings.json. Plan therefore always returns a slice of
// length 1 — never zero, never two. If the project ever grows a second
// owned file this becomes a real slice; until then the shape is
// deliberately fixed so callers can index [0] without a length check
// and downstream commit ordering (auth-first) is a no-op.
//
// sjson merge-preserve. The Transform closure calls sjson.SetBytes /
// sjson.DeleteBytes for each entry in OwnedKeysSettingsJSON — never
// json.Unmarshal + json.Marshal. That is the whole point of the sjson
// dependency: encoding/json reorders keys, drops comments-adjacent
// whitespace, and would violate PRD §4.7 merge-preserve on every
// non-owned key. sjson operates on the byte stream and preserves
// surrounding structure verbatim.
//
// Overlay-as-truth (NFR-S6). Plan iterates the OWNED-KEY ALLOWLIST,
// not the Profile. For each allowlisted key it either (a) SetBytes
// with the profile-derived value, or (b) DeleteBytes when the profile
// has no value in that slot. Iterating the Profile instead would leave
// stale keys in place when switching to a profile that omits a slot —
// exactly the failure mode NFR-S6 forbids.
//
// Empty file policy. Claude Code writes a zero-byte settings.json on
// its first launch before any user setting is recorded. Transform
// interprets an empty (or whitespace-only) current as `{}` so
// sjson.SetBytes has something to graft onto. The predicate lives in
// treatAsEmpty (emptycheck.go) and is shared with Import so the two
// paths cannot diverge — a round-trip Import → Plan → Apply of a fresh
// install would otherwise silently drop through different branches.
//
// Malformed current. sjson.SetBytes returns an error when the current
// bytes are not valid JSON. Transform gates every render on
// gjson.ValidBytes first and, additionally, requires the parsed root to
// be a JSON object — a bare `null`, scalar, array, or trailing-junk
// document is refused with ErrParseFailed even though gjson.ValidBytes
// accepts some of those shapes (null, scalars, arrays). Overlay-as-truth
// is only meaningful over an object-shaped document; letting sjson
// operate on a root scalar would either silently succeed writing over
// `null` (masking corruption) or wander into sjson error paths that
// don't surface the actual shape violation. This is the FR-5 step-3
// refuse-on-malformed guarantee for the Claude Code path — no silent
// fallback rewrite (NFR-S1).
//
// APIKey dual housing. Import mirrors ANTHROPIC_AUTH_TOKEN into
// Core.APIKey and, when a real API_KEY was ALSO present, records it
// in Overlay.ExtraEnv["ANTHROPIC_API_KEY"] for round-trip fidelity.
// Plan restores both halves: Core.APIKey → env.ANTHROPIC_AUTH_TOKEN
// AND (if present) Overlay.ExtraEnv["ANTHROPIC_API_KEY"] →
// env.ANTHROPIC_API_KEY. A profile that carries only Core.APIKey (the
// common case) writes only AUTH_TOKEN, leaving API_KEY unset.

package claudecode

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"

	"github.com/a2d2-dev/claudecm/internal/adapter"
	"github.com/a2d2-dev/claudecm/internal/config"
	"github.com/a2d2-dev/claudecm/internal/storage"
	"github.com/a2d2-dev/claudecm/internal/writepath"
)

// planFromProfile is the core Plan body — split out of adapter.go so
// the file that lists the adapter's public methods stays a
// grep-friendly index. Returns a single-entry []WritePlan targeting
// SettingsPath(r). Pure — no filesystem I/O.
//
// The returned WritePlan.Transform is the sjson-driven merge-preserve
// renderer; NewContent is left nil (Transform wins at Apply time —
// see writepath.plan.go package doc).
func (a *Adapter) planFromProfile(_ context.Context, r *storage.Resolver, profile config.Profile) ([]writepath.WritePlan, error) {
	target := SettingsPath(r)

	// Snapshot the effective values before building the closure so the
	// Transform is a pure function of `current`. Capturing the raw
	// Profile would let a caller mutate profile.Tools after Plan
	// returned and silently change what Apply writes.
	values := collectOwnedValues(profile)

	transform := func(current []byte) ([]byte, error) {
		return renderSettings(current, values)
	}

	// OwnedKeys is exposed to writepath so the Diff step (FR-5 step 5)
	// knows which flat-key paths this adapter claims. It is the same
	// frozen allowlist Files() advertises.
	owned := make([]string, len(OwnedKeysSettingsJSON))
	copy(owned, OwnedKeysSettingsJSON)

	plan := writepath.WritePlan{
		Tool:      string(adapter.ToolClaudeCode),
		Target:    target,
		Transform: transform,
		Parser:    jsonParser(),
		OwnedKeys: owned,
		Reason:    fmt.Sprintf("switch to profile %q", profile.Name),
	}
	return []writepath.WritePlan{plan}, nil
}

// ownedValue is a single owned-key slot pre-resolved from the Profile.
// present=false means "profile does not claim this slot"; under
// overlay-as-truth (NFR-S6) an absent slot triggers sjson.DeleteBytes
// so the tool falls back to its own default, rather than the previous
// profile's stale value being left on disk.
//
// value is captured as a Go string; every owned key in Claude Code's
// settings.json is a string in the JSON schema (env.* map values), so
// there is no legitimate non-string owned value to represent.
type ownedValue struct {
	present bool
	value   string
}

// collectOwnedValues distills a Profile down to one ownedValue per
// entry in OwnedKeysSettingsJSON. Pure. Split out so Transform is a
// pure function of pre-resolved data — no Profile deref inside the
// closure.
//
// Precedence (matches Import's inverse mapping so import→plan
// round-trips are stable):
//
//   - env.ANTHROPIC_BASE_URL     ← profile.Core.BaseURL
//   - env.ANTHROPIC_AUTH_TOKEN   ← profile.Core.APIKey
//   - env.ANTHROPIC_MODEL        ← profile.Core.Model
//   - env.ANTHROPIC_SMALL_FAST_MODEL ← profile.Core.SmallFastModel
//   - env.ANTHROPIC_API_KEY      ← overlay.ExtraEnv["ANTHROPIC_API_KEY"]
//   - env.CLAUDE_CODE_USE_BEDROCK← overlay.ExtraEnv["CLAUDE_CODE_USE_BEDROCK"]
//   - env.CLAUDE_CODE_USE_VERTEX ← overlay.ExtraEnv["CLAUDE_CODE_USE_VERTEX"]
//
// Where "overlay" is profile.Tools[ToolClaudeCode]. Empty-string
// Core.* values are treated as "not present" — the profile does not
// own that slot right now, so the key is deleted. This matches
// overlay-as-truth: a caller who genuinely wants an empty string in
// settings.json should not go through Core (the config layer's Core
// field is documented as "empty means unset"); the escape hatch is
// Overlay.ExtraEnv, which does treat "" as a real value.
func collectOwnedValues(profile config.Profile) map[string]ownedValue {
	out := make(map[string]ownedValue, len(OwnedKeysSettingsJSON))

	// Core-driven slots. Empty string → absent (see godoc).
	setFromCore := func(key, v string) {
		if v == "" {
			out[key] = ownedValue{present: false}
			return
		}
		out[key] = ownedValue{present: true, value: v}
	}
	setFromCore("env.ANTHROPIC_BASE_URL", profile.Core.BaseURL)
	setFromCore("env.ANTHROPIC_AUTH_TOKEN", profile.Core.APIKey)
	setFromCore("env.ANTHROPIC_MODEL", profile.Core.Model)
	setFromCore("env.ANTHROPIC_SMALL_FAST_MODEL", profile.Core.SmallFastModel)

	// Overlay ExtraEnv-driven slots. Empty string ("") IS a real value
	// here — Overlay.ExtraEnv is the escape hatch that lets an
	// operator pin an empty env var literal into settings.json. The
	// map-key presence check is the source of truth for these slots.
	//
	// The overlay comes from profile.Tools[ToolClaudeCode]. If the
	// profile has no overlay for this tool (map absent or entry
	// missing), all overlay-driven slots default to absent.
	var overlayEnv map[string]string
	if profile.Tools != nil {
		if ov, ok := profile.Tools[adapter.ToolClaudeCode]; ok {
			overlayEnv = ov.ExtraEnv
		}
	}
	setFromOverlay := func(key, envName string) {
		if overlayEnv == nil {
			out[key] = ownedValue{present: false}
			return
		}
		v, ok := overlayEnv[envName]
		if !ok {
			out[key] = ownedValue{present: false}
			return
		}
		out[key] = ownedValue{present: true, value: v}
	}
	setFromOverlay("env.ANTHROPIC_API_KEY", "ANTHROPIC_API_KEY")
	setFromOverlay("env.CLAUDE_CODE_USE_BEDROCK", "CLAUDE_CODE_USE_BEDROCK")
	setFromOverlay("env.CLAUDE_CODE_USE_VERTEX", "CLAUDE_CODE_USE_VERTEX")

	return out
}

// renderSettings is the Transform body: read `current`, apply each
// owned key from `values` via sjson (Set or Delete), return the new
// bytes. Pure — no I/O.
//
// Empty `current` is interpreted as `{}` (see file-level godoc).
// Malformed JSON in `current` triggers an ErrParseFailed-wrapped
// error on the first sjson call that hits it.
func renderSettings(current []byte, values map[string]ownedValue) ([]byte, error) {
	// Treat empty / whitespace-only bytes as an empty JSON object.
	// Shared predicate treatAsEmpty keeps this in lockstep with Import.
	work := current
	if treatAsEmpty(work) {
		work = []byte("{}")
	}

	// Refuse-on-malformed (NFR-S1 / FR-5 step 3). sjson.SetBytes is
	// permissive on malformed input — it will happily emit garbage
	// rather than fail — so we gate every render on gjson.ValidBytes.
	// gjson is a strict JSON validator; any failure here means the
	// current settings.json is not valid JSON and the operator must
	// resolve it before claudecm will overwrite (no silent fallback
	// rewrite).
	if !gjson.ValidBytes(work) {
		return nil, fmt.Errorf("%w: claudecode plan: current settings.json is not valid JSON", writepath.ErrParseFailed)
	}

	// Additional shape check: gjson.ValidBytes accepts a bare `null`,
	// scalar, or array at the JSON root, and it also accepts a document
	// with trailing junk after the first valid value on some releases.
	// Overlay-as-truth (NFR-S6) only makes sense over an object-shaped
	// document — a bare `null` root would let sjson silently succeed
	// writing over it, masking corruption. Require the trimmed root to
	// start with '{'; refuse anything else with ErrParseFailed. Also
	// re-validate on the trimmed slice: gjson.ValidBytes tolerates
	// trailing whitespace but not trailing non-whitespace junk, and
	// json.Valid is stricter about the "single top-level value" rule
	// than gjson is, so we belt-and-brace with encoding/json.Valid.
	trimmed := bytes.TrimSpace(work)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return nil, fmt.Errorf("%w: claudecode plan: settings.json root must be a JSON object, got %s", writepath.ErrParseFailed, describeRoot(trimmed))
	}
	if !json.Valid(work) {
		return nil, fmt.Errorf("%w: claudecode plan: settings.json has trailing content after root object", writepath.ErrParseFailed)
	}

	// Iterate OwnedKeysSettingsJSON (NOT the values map) so iteration
	// order is deterministic — the allowlist is sorted at package
	// init. Deterministic output makes goldens reviewable in PRs.
	for _, key := range OwnedKeysSettingsJSON {
		v, ok := values[key]
		if !ok {
			// collectOwnedValues MUST populate every allowlist entry —
			// that is the E3 invariant that ties Files() / Plan /
			// Import together. A silent `v = ownedValue{present:
			// false}` fallback here would mask a future refactor that
			// forgets to seed a slot: the slot would be quietly
			// DeleteBytes'd on every write, and the operator would
			// only notice by grepping settings.json for a missing key.
			// Panic instead — defense-in-depth symmetric with the
			// allowlist init() panic in allowlist.go. Callers who
			// legitimately want "not present" set it via
			// collectOwnedValues; there is no legitimate path where
			// the map is missing a key.
			panic(fmt.Errorf("claudecode plan: allowlist key %q missing from collectOwnedValues — E3 invariant broken", key))
		}

		if v.present {
			next, err := sjson.SetBytes(work, key, v.value)
			if err != nil {
				return nil, fmt.Errorf("%w: claudecode plan: sjson.SetBytes %q: %v", writepath.ErrParseFailed, key, err)
			}
			work = next
			continue
		}

		next, err := sjson.DeleteBytes(work, key)
		if err != nil {
			return nil, fmt.Errorf("%w: claudecode plan: sjson.DeleteBytes %q: %v", writepath.ErrParseFailed, key, err)
		}
		work = next
	}

	return work, nil
}

// jsonParser returns a writepath.Parser that decodes JSON bytes into a
// Go value via encoding/json. Used by Plan so writepath.Apply's Diff /
// reparse steps have a real parser to work with.
//
// The parser accepts an empty byte slice (returns nil, nil) so
// writepath.Apply's step-3 parse of a first-write "no current file"
// path does not need special-casing here — a nil current maps to a
// nil parsed value, and Diff against the new bytes' parsed shape
// still reports the correct Added / Changed set.
//
// Constructed as a function rather than a package-level var so tests
// can build fresh parsers without inheriting mutable state — matches
// the discipline in synthetic_adapter_test.go.
func jsonParser() writepath.Parser {
	return writepath.ParserFunc(func(data []byte) (any, error) {
		if treatAsEmpty(data) {
			return nil, nil
		}
		var v any
		if err := json.Unmarshal(data, &v); err != nil {
			return nil, err
		}
		return v, nil
	})
}

// describeRoot renders a short, quoted preview of a would-be root value
// for the "root must be a JSON object" error message. Kept tiny and
// non-allocating on the hot path: at most 16 bytes of trimmed content
// are surfaced, plus an ellipsis marker when truncated. Returning
// "<empty>" for a zero-length slice (rather than an empty quoted string)
// keeps the operator-facing message unambiguous.
func describeRoot(trimmed []byte) string {
	if len(trimmed) == 0 {
		return "<empty>"
	}
	const max = 16
	if len(trimmed) > max {
		return fmt.Sprintf("%q...", trimmed[:max])
	}
	return fmt.Sprintf("%q", trimmed)
}
