// Package codex: Plan implementation (E4-S4).
//
// This file carries the Plan surface for the Codex CLI adapter. It
// lives alongside adapter.go / import.go so the write-side renderer
// stays grep-close to its read-side counterpart while adapter.go
// remains a thin dispatcher of the public contract.
//
// Design notes
// ============
//
// Two-file scope. Codex owns two files (~/.codex/auth.json and
// ~/.codex/config.toml) and Plan therefore returns a slice of length
// 2 in the auth-first order Files() advertises (architecture §5,
// two-phase commit). The auth-first ordering is a claim the whole
// commit pipeline relies on — auth-first means the write that
// contains credentials lands (or fails cleanly) before the write that
// references those credentials by provider name. Downstream commit
// (E7) will preserve this order verbatim.
//
// Special case (auth-plan elision). When the profile carries zero
// auth-related content (Core.APIKey is empty AND no overlay auth
// keys are set) AND the on-disk ~/.codex/auth.json is missing or
// whitespace-only, Plan emits ONLY the config.toml plan and returns
// a length-1 slice. Rationale: emitting an auth.json plan that
// renders to "{}" against a missing target would create a fresh
// 0600 auth.json file for no reason (writepath.Apply's first-write
// path always publishes, even for an empty diff). If auth.json HAS
// content and the profile clears it, the plan IS emitted so
// overlay-as-truth deletion applies. This is the only place Plan
// touches the filesystem: a single os.ReadFile on the auth path.
// Everything else — the Transform closures — remains pure.
//
// Overlay-as-truth (NFR-S6). Plan iterates the OWNED-KEY ALLOWLISTS
// (OwnedKeysAuthJSON, OwnedKeysConfigTOML), NOT the Profile. For
// each allowlisted key it either (a) Set with a profile-derived
// value or (b) Delete when the profile has no value in that slot.
// Iterating the Profile instead would leave stale keys in place
// when switching to a profile that omits a slot — exactly the
// failure mode NFR-S6 forbids.
//
// Auth-key sourcing. OPENAI_API_KEY comes from profile.Core.APIKey
// (the same field claudecode's Plan reads for ANTHROPIC_AUTH_TOKEN
// — the Core field is the shared "current credential" slot). All
// other auth.json owned keys (auth_mode, last_refresh, tokens.*)
// come from profile.Tools[ToolCodex].Raw. Import's inverse mapping
// (extractOwnedCodex) is symmetric: OPENAI_API_KEY → Core.APIKey,
// everything else → Overlay.Raw. Round-trip fidelity is preserved.
//
// Config-key sourcing. Every OwnedKeysConfigTOML entry lives in
// profile.Tools[ToolCodex].Raw. v1 does not promote any config.toml
// key into Core (see import.go "Core mapping conservatism").
//
// sjson merge-preserve (auth.json). The Transform closure calls
// sjson.SetBytes / sjson.DeleteBytes for each entry in
// OwnedKeysAuthJSON — never json.Unmarshal + json.Marshal.
// encoding/json reorders keys and drops comments-adjacent whitespace,
// violating PRD §4.7 merge-preserve on every non-owned key. sjson
// operates on the byte stream and preserves surrounding structure
// verbatim. The nested "tokens.*" owned keys are naturally addressed
// by sjson's dotted path syntax.
//
// Doc-model merge-preserve (config.toml). The Transform closure loads
// current bytes through codextoml.Load, iterates OwnedKeysConfigTOML,
// and calls Doc.Set (present) or Doc.Delete (absent). Non-owned keys
// round-trip byte-preserved by the Doc's raw-line preservation. See
// codex/toml package docs for the exact preservation contract and its
// documented multi-line / array-of-tables limits (NFR-S7 warnings
// surface via Doc.Warnings after Marshal).
//
// Doc.Warnings handling. When Marshal returns non-empty Warnings
// (comments/order may shift), Plan surfaces them to stderr from
// within the Transform closure BEFORE returning the rendered bytes.
// The emission is gated by a per-Plan-invocation sync.Once captured
// in the closure — writepath.Apply invokes Transform on both the
// preview and apply paths, so without the Once the same warning
// would double-print. sync.Once inside the closure is
// per-Plan-invocation state, NOT package-global mutable state.
// Warnings are informational — they do NOT abort the write
// (returning an error would be a fallback-avoidance violation the
// other way: we'd be silently refusing to complete a legitimate
// render because the wrapper heuristic couldn't prove pristine
// preservation). Logging to stderr matches storage.LoadAllProfiles's
// warning pattern. WriteReport has no Warnings field (frozen shape
// via writepath package boundary — extending it is deferred to E7
// or a future writepath enhancement); the sync.Once stderr emission
// is the v1 surfacing channel.
//
// AC #3 reconciliation. Story E4-S4 AC #3 originally spoke of
// "WritePlan.Warnings". The frozen writepath.WriteReport shape has
// no Warnings field, and expanding it is a cross-package change
// out of scope for E4-S4. The sync.Once stderr emission from the
// Transform closure supersedes that AC clause. See
// docs/plan/stories/E4-S4.md for the documented reconcile.
//
// Empty-file policy. A zero-byte or whitespace-only current file is
// interpreted as an empty document ("{}" for auth.json, empty Doc
// for config.toml). Shared with Import via the treatAsEmpty
// predicate in import.go.
//
// Refuse-on-malformed. Both Transform closures gate on codec-specific
// validation (gjson.ValidBytes + json.Valid + object-root shape for
// auth.json; codextoml.Load's parse error for config.toml). Any
// failure returns a writepath.ErrParseFailed-wrapped error so
// writepath.Apply's step 2 halts before backup + write. NFR-S1: no
// silent fallback rewrite.
//
// Provider allowlist scope. v1's OwnedKeysConfigTOML enumerates
// only "openai" and "anthropic" providers in model_providers.*. A
// profile carrying model_providers.myrelay.base_url in Overlay.Raw
// is NOT written by Plan (the key is not in the allowlist), but any
// existing model_providers.myrelay.* entries in the current file
// are preserved verbatim by the Doc's raw-line pass-through. This
// is the merge-preserve contract in action: claudecm does not own
// custom providers in v1, so it neither writes nor destroys them.

package codex

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"sync"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"

	"github.com/a2d2-dev/claudecm/internal/adapter"
	codextoml "github.com/a2d2-dev/claudecm/internal/adapter/codex/toml"
	"github.com/a2d2-dev/claudecm/internal/config"
	"github.com/a2d2-dev/claudecm/internal/storage"
	"github.com/a2d2-dev/claudecm/internal/writepath"
)

// planFromProfile is the core Plan body — split out of adapter.go so
// the file listing the adapter's public methods stays a grep-friendly
// index. Returns two WritePlans in auth-first order, or one plan when
// the auth-elision special case fires (see file godoc).
func (a *Adapter) planFromProfile(ctx context.Context, r *storage.Resolver, profile config.Profile) ([]writepath.WritePlan, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	// Pre-resolve every owned value from the Profile so the Transform
	// closures are pure functions of (currentBytes, pre-resolved
	// values). Capturing the raw Profile would let a caller mutate
	// profile.Tools between Plan and Apply and silently change what
	// gets written.
	authValues := collectOwnedAuthValues(profile)
	configValues := collectOwnedConfigValues(profile)

	plans := make([]writepath.WritePlan, 0, 2)

	// Auth-first ordering: the auth.json WritePlan comes before the
	// config.toml WritePlan so the two-phase commit (E7) stages
	// credentials before the config that references them.
	authPath := AuthPath(r)
	if shouldEmitAuthPlan(authPath, authValues) {
		authPlan := writepath.WritePlan{
			Tool:      string(adapter.ToolCodex),
			Target:    authPath,
			Transform: makeAuthTransform(authValues),
			Parser:    jsonParser(),
			OwnedKeys: copyStrings(OwnedKeysAuthJSON),
			Reason:    fmt.Sprintf("codex auth: switch to profile %q", profile.Name),
		}
		plans = append(plans, authPlan)
	}

	configPath := ConfigPath(r)
	configPlan := writepath.WritePlan{
		Tool:      string(adapter.ToolCodex),
		Target:    configPath,
		Transform: makeConfigTransform(configValues),
		Parser:    tomlParser(),
		OwnedKeys: copyStrings(OwnedKeysConfigTOML),
		Reason:    fmt.Sprintf("codex config: switch to profile %q", profile.Name),
	}
	plans = append(plans, configPlan)

	return plans, nil
}

// ownedValue mirrors the claudecode ownedValue shape. present=false
// means "profile does not claim this slot"; under overlay-as-truth
// (NFR-S6) an absent slot triggers a Delete so the tool falls back
// to its own default rather than the previous profile's stale
// value being left on disk.
//
// value is stored as an untyped any so config.toml numeric / bool
// owned keys carry their type through to Doc.Set. auth.json owned
// keys land as string via coerceToStringCodex on the Import side;
// on the Plan side sjson.SetBytes accepts any JSON-encodable value
// and re-emits it in the natural JSON shape, so bool/number values
// from Overlay.Raw round-trip typed.
type ownedValue struct {
	present bool
	value   any
}

// collectOwnedAuthValues distills a Profile down to one ownedValue
// per entry in OwnedKeysAuthJSON. Pure.
//
// Precedence (symmetric with import.go extractOwnedCodex):
//
//   - OPENAI_API_KEY               ← profile.Core.APIKey (empty → absent)
//   - auth_mode, last_refresh,
//     tokens.access_token,
//     tokens.account_id,
//     tokens.id_token,
//     tokens.refresh_token         ← profile.Tools[ToolCodex].Raw[key]
//
// An Overlay.Raw value of nil is treated as absent (no legitimate
// caller pins a nil into Raw expecting a JSON null on disk; the
// Import side's null-owned-key policy also drops nils).
func collectOwnedAuthValues(profile config.Profile) map[string]ownedValue {
	out := make(map[string]ownedValue, len(OwnedKeysAuthJSON))

	// OPENAI_API_KEY from Core. Empty string → absent (consistent
	// with claudecode setFromCore: Core fields treat "" as "not
	// claimed", matching Profile YAML semantics where an omitted
	// key and an empty scalar are indistinguishable).
	if profile.Core.APIKey == "" {
		out["OPENAI_API_KEY"] = ownedValue{}
	} else {
		out["OPENAI_API_KEY"] = ownedValue{present: true, value: profile.Core.APIKey}
	}

	// Other owned keys from Overlay.Raw. An absent overlay entry
	// counts as "not present" and triggers Delete under
	// overlay-as-truth.
	var overlayRaw map[string]any
	if profile.Tools != nil {
		if ov, ok := profile.Tools[adapter.ToolCodex]; ok {
			overlayRaw = ov.Raw
		}
	}
	for _, key := range OwnedKeysAuthJSON {
		if key == "OPENAI_API_KEY" {
			continue
		}
		v, ok := overlayRaw[key]
		if !ok || v == nil {
			out[key] = ownedValue{}
			continue
		}
		out[key] = ownedValue{present: true, value: v}
	}
	return out
}

// collectOwnedConfigValues distills a Profile down to one ownedValue
// per entry in OwnedKeysConfigTOML. Every config.toml owned key
// sources from profile.Tools[ToolCodex].Raw — v1 does not promote
// any config.toml key into Core (see import.go "Core mapping
// conservatism"). Pure.
func collectOwnedConfigValues(profile config.Profile) map[string]ownedValue {
	out := make(map[string]ownedValue, len(OwnedKeysConfigTOML))
	var overlayRaw map[string]any
	if profile.Tools != nil {
		if ov, ok := profile.Tools[adapter.ToolCodex]; ok {
			overlayRaw = ov.Raw
		}
	}
	for _, key := range OwnedKeysConfigTOML {
		v, ok := overlayRaw[key]
		if !ok || v == nil {
			out[key] = ownedValue{}
			continue
		}
		out[key] = ownedValue{present: true, value: v}
	}
	return out
}

// shouldEmitAuthPlan implements the auth-plan elision special case
// (file godoc): if the profile carries zero auth-related content
// AND the current auth.json is missing or whitespace-only, skip
// emitting the plan entirely. Otherwise emit — including the case
// where a non-empty auth.json exists and the profile clears every
// owned key, so overlay-as-truth deletion still applies.
//
// The disk read is best-effort: any error other than "file exists
// with real content" (permission denied, etc.) is treated as
// "cannot prove elision is safe, emit the plan". Better to emit an
// unneeded plan (writepath.Apply will Skip on empty diff) than to
// silently miss a legitimate deletion.
//
// Locking asymmetry vs claudecode. This read happens BEFORE
// writepath.Apply grabs its flock, which is asymmetric with
// claudecode's Plan (which does zero disk I/O in Plan). Two race
// windows exist and neither is correctness-critical:
//
//  1. auth.json is created between our read and Apply's flock: we
//     already decided config-only; the emitted config.toml plan
//     still succeeds correctly; auth.json is left unchanged. Result
//     is consistent.
//  2. auth.json is deleted between our read (which saw content) and
//     Apply's Transform: the emitted auth-plan Transform sees an
//     absent (empty) file, walks the owned-key Delete loop over the
//     "{}" seed, and writes the same "{}" (with our owned Sets
//     applied if any). Result is consistent.
//
// The only observable effect of a race is "we may emit an
// unnecessary auth plan in the elide-then-delete case", which
// writepath.Apply's diff-then-skip step handles cleanly. Accepted
// as a v1 trade-off; a future refactor could move the elision
// decision inside the flock (or express it as an in-Transform
// no-op check) if the asymmetry becomes problematic.
func shouldEmitAuthPlan(authPath string, authValues map[string]ownedValue) bool {
	// If any owned auth slot is present the plan is required.
	for _, v := range authValues {
		if v.present {
			return true
		}
	}
	// Profile carries nothing auth-related. Only elide when the
	// current file is missing or whitespace-only.
	data, err := os.ReadFile(authPath)
	if err != nil {
		// Any read error other than IsNotExist — emit the plan and
		// let Apply's pipeline surface the real problem under lock.
		return !os.IsNotExist(err)
	}
	return !treatAsEmpty(data)
}

// makeAuthTransform builds the Transform closure for the auth.json
// WritePlan. Pre-resolved authValues are captured so the closure is
// a pure function of `current`.
func makeAuthTransform(authValues map[string]ownedValue) writepath.Transform {
	return func(current []byte) ([]byte, error) {
		return renderAuth(current, authValues)
	}
}

// renderAuth is the auth.json Transform body. Reads current bytes,
// applies each owned key from authValues via sjson (Set or Delete),
// returns the new bytes. Pure — no I/O.
//
// Empty / whitespace-only current is interpreted as `{}`. Malformed
// JSON (invalid, non-object root, trailing junk) is refused with
// writepath.ErrParseFailed.
func renderAuth(current []byte, authValues map[string]ownedValue) ([]byte, error) {
	work := current
	if treatAsEmpty(work) {
		work = []byte("{}")
	}

	// Refuse-on-malformed. Belt-and-brace: gjson.ValidBytes rejects
	// most invalid JSON, encoding/json.Valid catches trailing junk
	// gjson tolerates, and an explicit root-shape check refuses
	// bare `null` / scalar / array roots (overlay-as-truth is only
	// meaningful over an object-shaped document). Symmetric with
	// claudecode/plan.go renderSettings.
	if !gjson.ValidBytes(work) {
		return nil, fmt.Errorf("%w: codex plan: current auth.json is not valid JSON", writepath.ErrParseFailed)
	}
	trimmed := bytes.TrimSpace(work)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return nil, fmt.Errorf("%w: codex plan: auth.json root must be a JSON object, got %s", writepath.ErrParseFailed, describeRoot(trimmed))
	}
	if !json.Valid(work) {
		return nil, fmt.Errorf("%w: codex plan: auth.json has trailing content after root object", writepath.ErrParseFailed)
	}

	// Iterate OwnedKeysAuthJSON so iteration order is deterministic
	// (the allowlist is sorted at package init time). Deterministic
	// output makes goldens reviewable in PRs.
	for _, key := range OwnedKeysAuthJSON {
		v, ok := authValues[key]
		if !ok {
			// collectOwnedAuthValues MUST populate every allowlist
			// entry. A silent fallback that treats missing as absent
			// would mask a future refactor that forgets to seed a
			// slot. Panic — defense-in-depth symmetric with the
			// allowlist init() panic and claudecode's Plan.
			panic(fmt.Errorf("codex plan: auth allowlist key %q missing from collectOwnedAuthValues", key))
		}

		if v.present {
			next, err := sjson.SetBytes(work, key, v.value)
			if err != nil {
				return nil, fmt.Errorf("%w: codex plan: sjson.SetBytes %q: %v", writepath.ErrParseFailed, key, err)
			}
			work = next
			continue
		}

		next, err := sjson.DeleteBytes(work, key)
		if err != nil {
			return nil, fmt.Errorf("%w: codex plan: sjson.DeleteBytes %q: %v", writepath.ErrParseFailed, key, err)
		}
		work = next
	}

	// Prune orphan empty "tokens" object. sjson.DeleteBytes on the
	// four tokens.* leaf keys leaves the parent "tokens" object in
	// place — {} when all four were owned-and-cleared, or a shrunk
	// object when unowned siblings remain. An empty tokens object is
	// confusing UI noise (users switching away from a Codex-authed
	// profile expect a clean auth.json), and Codex CLI treats {} and
	// absent identically, so no behavior change. Only prune when the
	// object is present AND empty; a tokens map with any unowned
	// sibling (e.g. tokens.other_unowned) is left intact.
	tokens := gjson.GetBytes(work, "tokens")
	if tokens.Exists() && tokens.IsObject() && len(tokens.Map()) == 0 {
		next, err := sjson.DeleteBytes(work, "tokens")
		if err != nil {
			return nil, fmt.Errorf("%w: codex plan: prune empty tokens: %v", writepath.ErrParseFailed, err)
		}
		work = next
	}

	return work, nil
}

// makeConfigTransform builds the Transform closure for the
// config.toml WritePlan. Pre-resolved configValues are captured so
// the closure is a pure function of `current`, up to a single
// per-Plan sync.Once stderr emission of any Doc.Warnings (see file
// godoc). writepath.Apply invokes Transform on both the preview and
// apply paths; without the Once, the same warning would double-print.
// The sync.Once lives INSIDE the closure so it is per-Plan-invocation
// state, not package-global mutable state.
func makeConfigTransform(configValues map[string]ownedValue) writepath.Transform {
	var warnOnce sync.Once
	return func(current []byte) ([]byte, error) {
		return renderConfig(current, configValues, &warnOnce)
	}
}

// renderConfig is the config.toml Transform body. Loads current
// through codextoml.Load, applies each owned key from configValues
// via Doc.Set / Doc.Delete, returns Marshal output. Deterministic;
// the only side effect is a single stderr emission of Doc.Warnings
// per Plan invocation, gated by the caller-supplied sync.Once.
//
// Empty / whitespace-only current becomes an empty Doc (codextoml.Load
// handles this internally). Malformed TOML is refused with
// writepath.ErrParseFailed wrapping codextoml.ErrParseFailed.
func renderConfig(current []byte, configValues map[string]ownedValue, warnOnce *sync.Once) ([]byte, error) {
	doc, err := codextoml.Load(current)
	if err != nil {
		return nil, fmt.Errorf("%w: codex plan: load current config.toml: %v", writepath.ErrParseFailed, err)
	}

	// Iterate OwnedKeysConfigTOML for deterministic emission order.
	for _, key := range OwnedKeysConfigTOML {
		v, ok := configValues[key]
		if !ok {
			panic(fmt.Errorf("codex plan: config allowlist key %q missing from collectOwnedConfigValues", key))
		}
		if v.present {
			if serr := doc.Set(key, v.value); serr != nil {
				return nil, fmt.Errorf("%w: codex plan: Doc.Set %q: %v", writepath.ErrParseFailed, key, serr)
			}
			continue
		}
		doc.Delete(key)
	}

	out, err := doc.Marshal()
	if err != nil {
		return nil, fmt.Errorf("%w: codex plan: Doc.Marshal: %v", writepath.ErrParseFailed, err)
	}

	// Surface Doc warnings to stderr, at most ONCE per Plan
	// invocation. Warnings are informational (NFR-S7
	// "comments/order may shift"); they DO NOT abort the render.
	// Logging matches storage.LoadAllProfiles's pattern. writepath.Apply
	// runs the Transform twice (preview + apply), and without the
	// sync.Once the user would see the same warnings duplicated —
	// documented AC #3 reconcile (see file godoc).
	if warnings := doc.Warnings(); len(warnings) > 0 && warnOnce != nil {
		warnOnce.Do(func() {
			for _, w := range warnings {
				fmt.Fprintln(os.Stderr, "codex plan: "+w)
			}
		})
	}

	return out, nil
}

// jsonParser returns a writepath.Parser that decodes JSON bytes into
// a Go value via encoding/json. Used by the auth.json WritePlan so
// writepath.Apply's Diff / reparse steps have a real parser to work
// with. Symmetric with claudecode/plan.go jsonParser.
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

// tomlParser returns a writepath.Parser that Loads TOML bytes into
// a NESTED map[string]any built from codextoml.Doc.Keys() + Doc.Get.
// Used by the config.toml WritePlan so writepath.Apply's Diff
// (flat-key semantics) and reparse steps work.
//
// Design note. writepath.Apply feeds the parser's return value to
// writepath.Flatten before diffing against plan.OwnedKeys. Flatten
// expects a nested map[string]any tree and walks it, joining path
// segments with '.' and escaping any literal '.' or '\' inside a
// single segment. OwnedKeysConfigTOML lists dotted paths as their
// UNESCAPED nested form (e.g. "model_providers.openai.base_url"),
// so the parser MUST hand Flatten a nested tree — a flat
// map[string]any keyed by "model_providers.openai.base_url" would
// be re-escaped by Flatten to "model_providers\.openai\.base_url"
// and miss the allowlist entirely, tripping TouchesUnowned=true on
// every legitimate nested-key change. That was the E4-S7 bug this
// hotfix repairs.
//
// The Doc's own Keys() returns flat dotted paths (Doc's package
// contract), so we unflatten them here into the nested shape
// Flatten expects. Non-dotted top-level keys (e.g. "model") land
// as leaves at the root; dotted keys grow intermediate
// map[string]any tables. Codex config.toml keys never contain
// literal dots or backslashes in a single segment (owned or
// unowned) — the Doc wrapper splits on '.' unconditionally
// (codex/toml/doc.go Set/Get docs) — so no escape handling is
// needed on the unflatten side.
//
// Symmetric with the auth-side parser: empty or whitespace-only
// input returns (nil, nil) so writepath.Apply's first-write path
// against a missing file stays clean.
func tomlParser() writepath.Parser {
	return writepath.ParserFunc(func(data []byte) (any, error) {
		if treatAsEmpty(data) {
			return nil, nil
		}
		doc, err := codextoml.Load(data)
		if err != nil {
			return nil, err
		}
		out := make(map[string]any)
		for _, k := range doc.Keys() {
			v, ok := doc.Get(k)
			if !ok {
				continue
			}
			if err := assignNested(out, k, v); err != nil {
				return nil, err
			}
		}
		return out, nil
	})
}

// assignNested places value at the dotted path inside root, creating
// intermediate map[string]any tables as needed. Path segments are
// split on '.' — matching codextoml.Doc's own path convention. If a
// prefix collides with a non-map value (i.e. the same segment path
// was previously assigned a scalar), the assignment returns an
// error to expose the malformed shape rather than silently
// overwriting the earlier leaf.
//
// This is only used by tomlParser; it is intentionally NOT exported
// or shared with writepath.Flatten. Its contract is narrow: undo
// the Doc's Keys() flat view so writepath.Flatten can re-flatten
// back to the same dotted keys that OwnedKeysConfigTOML lists.
func assignNested(root map[string]any, path string, value any) error {
	if path == "" {
		return fmt.Errorf("codex plan: assignNested: empty path")
	}
	segs := strings.Split(path, ".")
	cur := root
	for i, seg := range segs[:len(segs)-1] {
		next, ok := cur[seg]
		if !ok {
			nm := make(map[string]any)
			cur[seg] = nm
			cur = nm
			continue
		}
		nm, ok := next.(map[string]any)
		if !ok {
			return fmt.Errorf("codex plan: assignNested: path %q collides with non-map value at prefix %q",
				path, strings.Join(segs[:i+1], "."))
		}
		cur = nm
	}
	leaf := segs[len(segs)-1]
	if _, exists := cur[leaf]; exists {
		return fmt.Errorf("codex plan: assignNested: duplicate leaf %q at path %q", leaf, path)
	}
	cur[leaf] = value
	return nil
}

// copyStrings returns a fresh copy of s so the returned WritePlan
// does not alias the frozen OwnedKeys* package-level slice. Prevents
// a downstream caller from mutating the allowlist through the
// WritePlan.
func copyStrings(s []string) []string {
	out := make([]string, len(s))
	copy(out, s)
	return out
}

// describeRoot renders a short, quoted preview of a would-be root
// value for the "root must be a JSON object" error. Symmetric with
// claudecode/plan.go describeRoot.
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
