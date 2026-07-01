// Package codex: Project implementation (E4-S6).
//
// This file carries the Project surface for the Codex CLI adapter.
// It is deliberately split out of adapter.go so the two-file (config.toml
// + auth.json) layered-resolver machinery does not crowd the adapter's
// public contract methods, which stay in adapter.go for
// grep-friendliness. Symmetric with claudecode/project.go (E3-S6).
//
// Design notes
// ============
//
// Two-file scope. Unlike Claude Code's single settings.json, Codex owns
// two files that Project walks together:
//
//	~/.codex/config.toml — model routing, provider tables, approval mode
//	~/.codex/auth.json   — OPENAI_API_KEY + OAuth token bundle
//
// The frozen owned-key allowlists (OwnedKeysConfigTOML,
// OwnedKeysAuthJSON) enumerate every projectable key. Each key is
// routed to exactly one file for the OnDisk-layer lookup — the
// allowlist init() panic on cross-file overlap guarantees the routing
// is unambiguous.
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
// for keys in OwnedKeysConfigTOML ∪ OwnedKeysAuthJSON. Non-owned keys
// (Codex's `[history]`, MCP configuration, custom providers, future
// knobs claudecm does not manage) round-trip verbatim through the
// write-path merge-preserve and are irrelevant to the claudecm
// effective view.
//
// Env-var allowlist (NFR-E1). architecture.md §6.1 authoritatively
// lists Codex's env allowlist as OPENAI_API_KEY, OPENAI_BASE_URL,
// CODEX_HOME, CODEX_MODEL, CODEX_MODEL_PROVIDER. Of those:
//
//   - OPENAI_API_KEY           shadows auth.json OPENAI_API_KEY
//   - OPENAI_BASE_URL          shadows config.toml model_providers.openai.base_url
//   - CODEX_MODEL              shadows config.toml model
//   - CODEX_MODEL_PROVIDER     shadows config.toml model_provider
//
// CODEX_HOME is a CONFIG-DIRECTORY override (it relocates the ~/.codex
// tree itself); it does not shadow any owned key value. It is
// therefore not in envVarForOwnedKey and is intentionally not surfaced
// through Project. Every other Codex owned key — the four Anthropic
// provider entries, the OpenAI provider metadata (env_key, name,
// wire_api), approval_mode, tokens.*, auth_mode, last_refresh — has NO
// env-var counterpart per the architecture allowlist and can only be
// sourced from ProfileCore, ProfileOverlay, or OnDiskToolConfig.
//
// ProfileCore mapping. The only Core-routed slot for Codex is
// OPENAI_API_KEY ← profile.Core.APIKey (symmetric with Import's
// extractOwnedCodex and Plan's collectOwnedAuthValues). Every other
// owned key sources only from ProfileOverlay (via Overlay.Raw), the
// on-disk file, or an env override where one exists. Core.BaseURL /
// Core.Model / etc. are NOT mapped into any config.toml key in v1
// (see import.go "Core mapping conservatism").
//
// ProfileOverlay mapping. Every owned key can additionally be sourced
// from profile.Tools[ToolCodex].Raw[<flat-key>] — this is the
// overlay-as-truth escape hatch (NFR-S6). Presence in the Raw map is
// the source of truth; a nil value in Raw is treated as absent
// (symmetric with Plan's collectOwnedAuthValues / collectOwnedConfigValues
// which drop nils, and Import's null-owned-key policy).
//
// OnDiskToolConfig routing. config.toml keys are looked up via
// codextoml.Doc.Get (typed values: int64, float64, string, bool
// preserved). auth.json keys are looked up via encoding/json +
// writepath.Flatten with an unflattened root peek for OPENAI_API_KEY
// (symmetric with Import). A JSON null on disk is treated as absent
// for an owned key — mirrors Import's null-owned-key rule and keeps a
// hand-edit like `"OPENAI_API_KEY": null` from shadowing a real
// ProfileCore value.
//
// EnvOverride. Read via envextract.Lookup — the single env-read
// primitive shared with the Claude Code adapter (E5-S3). A non-empty
// env-var value wins over all lower layers. Empty string on the
// process env is treated as "not set" here because Codex CLI itself
// would not consume an empty env literal to override a config.toml
// value. The build-tag seam that lets tests inject a synthetic env
// universe lives in internal/envextract (see SetLookupForTest under
// `//go:build test`); this adapter carries no per-package seam of
// its own.
//
// BuiltInDefault. No v1 owned key has a built-in default value. The
// layer is still enumerated in the code path so a future default
// addition slots in without a resolver rewrite.
//
// Redaction contract. EffectiveField.Secret is set to true on the
// credential-carrying owned keys per the file-level secretOwnedKeys
// map: OPENAI_API_KEY plus the three OAuth token secrets
// (tokens.access_token, tokens.id_token, tokens.refresh_token).
// auth_mode, last_refresh, tokens.account_id are metadata — NOT
// secret. Downstream renderers (cmd/current, cmd/explain) apply the
// actual redaction; Project is the adapter authority on which fields
// are secret (per the adapter.EffectiveField godoc redaction
// contract).
//
// Read-only. Project never writes to disk, never mutates the process
// env. Symlink-outside-HOME on either file → ErrOutsideHome; malformed
// on either file → ErrParseFailed. Missing/empty file → treated as
// absent for that file's contribution to the OnDisk layer.

package codex

import (
	"context"
	"fmt"

	"github.com/a2d2-dev/claudecm/internal/adapter"
	codextoml "github.com/a2d2-dev/claudecm/internal/adapter/codex/toml"
	"github.com/a2d2-dev/claudecm/internal/adapter/stateio"
	"github.com/a2d2-dev/claudecm/internal/config"
	"github.com/a2d2-dev/claudecm/internal/envextract"
	"github.com/a2d2-dev/claudecm/internal/storage"
)

// envVarForOwnedKey maps a flat owned-key path to the environment
// variable name Codex CLI reads at runtime. Frozen alongside the
// owned-key allowlists — see file godoc "Env-var allowlist (NFR-E1)".
//
// Only keys with an env counterpart appear here (architecture §6.1).
// CODEX_HOME is a config-dir override, not a key-value shadow, so it
// is intentionally absent from this table.
var envVarForOwnedKey = map[string]string{
	"OPENAI_API_KEY":                  "OPENAI_API_KEY",
	"model":                           "CODEX_MODEL",
	"model_provider":                  "CODEX_MODEL_PROVIDER",
	"model_providers.openai.base_url": "OPENAI_BASE_URL",
}

// secretOwnedKeys lists the owned-key paths whose Value must be
// flagged Secret=true in the EffectiveField (redaction contract).
// Kept as a set for O(1) lookup during the per-key resolve loop.
//
// The four entries are the auth material Codex holds: the OpenAI API
// key and the three OAuth tokens. tokens.account_id is metadata (the
// user's account identifier — not a credential) and auth_mode /
// last_refresh are also metadata; none of them are secret.
var secretOwnedKeys = map[string]bool{
	"OPENAI_API_KEY":       true,
	"tokens.access_token":  true,
	"tokens.id_token":      true,
	"tokens.refresh_token": true,
}

// ownedFileForKey identifies which of the two owned files a given
// owned key lives in. Built at init() from the two frozen allowlists
// so the resolver's per-key routing is O(1) and cannot drift out of
// sync with the allowlists.
//
// Populated with adapter.FormatTOML for config.toml keys and
// adapter.FormatJSON for auth.json keys. The allowlist init()
// no-overlap panic guarantees every key routes to exactly one file.
var ownedFileForKey = map[string]adapter.Format{}

// allOwnedKeys is the union of OwnedKeysConfigTOML and
// OwnedKeysAuthJSON in a stable sorted order, computed once at init.
// Used as the iteration order in projectFromProfile so the field
// resolution is deterministic before adapter.SortFields runs.
var allOwnedKeys []string

// init verifies that envVarForOwnedKey references only real owned
// keys and populates the routing map. Symmetric with the sorted /
// no-duplicate / no-overlap init invariant in allowlist.go and the
// collectOwnedAuthValues / collectOwnedConfigValues completeness
// panic in plan.go — if a future owned key lands in either allowlist
// without a corresponding routing entry, the resolver would silently
// stop projecting that key. Fail LOUDLY at package load.
func init() {
	for _, key := range OwnedKeysConfigTOML {
		ownedFileForKey[key] = adapter.FormatTOML
		allOwnedKeys = append(allOwnedKeys, key)
	}
	for _, key := range OwnedKeysAuthJSON {
		ownedFileForKey[key] = adapter.FormatJSON
		allOwnedKeys = append(allOwnedKeys, key)
	}
	// Assert every envVarForOwnedKey entry references a real owned
	// key. Reverse direction (every owned key mapped) is intentionally
	// NOT asserted: most Codex owned keys have NO env counterpart per
	// the architecture allowlist.
	for k := range envVarForOwnedKey {
		if _, ok := ownedFileForKey[k]; !ok {
			panic(fmt.Errorf("codex project: envVarForOwnedKey references non-owned key %q", k))
		}
	}
	// Assert every secretOwnedKeys entry references a real owned key.
	for k := range secretOwnedKeys {
		if _, ok := ownedFileForKey[k]; !ok {
			panic(fmt.Errorf("codex project: secretOwnedKeys references non-owned key %q", k))
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
	// E5-S4: external drift detection. State.LastAppliedPerTool[codex]
	// records a per-file entry (auth.json and config.toml are tracked
	// independently) with the SHA256 of the last successful Apply.
	// Project re-hashes each owned file (raw bytes, before any parse
	// or normalisation) and reports drift for each file whose SHA256
	// no longer matches. No prior state for a given file → no drift
	// reported for that file (E5-S4 AC edge case). Both files can
	// drift independently — the check runs per file so partial drift
	// (e.g. auth.json edited, config.toml intact) surfaces only the
	// touched file.
	view := adapter.EffectiveView{Tool: adapter.ToolCodex}

	// Honour ctx cancellation before any filesystem or env work.
	if err := ctx.Err(); err != nil {
		return view, err
	}

	configPath := ConfigPath(r)
	authPath := AuthPath(r)

	// Read both files (best-effort). Missing → treat as absent for
	// that file's OnDisk contribution. Malformed → ErrParseFailed.
	// Symlink-outside-HOME → ErrOutsideHome. Uses the shared helpers
	// in readers.go so the read semantics stay identical to Import's.
	// The raw-bytes tail lets the drift check hash the SAME bytes the
	// parser saw, without opening the owned file a second time
	// (F4/F5 fix; the previous driftForFile / os.ReadFile pass also
	// bypassed the readers' HOME-containment check).
	configPresent, tomlDoc, configRaw, err := readCodexTOMLWithPrefix(configPath, r, "codex project")
	if err != nil {
		return view, err
	}
	authPresent, authRoot, authFlat, authRaw, err := readCodexAuthWithPrefix(authPath, r, "codex project")
	if err != nil {
		return view, err
	}

	// Drift check. Runs AFTER the reads so we hash the same bytes the
	// parser consumed — no second os.ReadFile, no second HOME-containment
	// check. Order the slice auth-first to match Files() ordering so
	// renderers iterating ExternalDriftFiles get deterministic output.
	//
	// A file whose read helper returned raw==nil (absent, or a
	// containment/parse error propagated above) contributes no drift
	// entry: for absent files this matches the AC "absent file → no
	// drift"; for error paths we would already have returned before
	// reaching this point.
	var driftFiles []string
	if authRaw != nil && driftForFileBytes(r, adapter.ToolCodex, authPath, authRaw) {
		driftFiles = append(driftFiles, authPath)
	}
	if configRaw != nil && driftForFileBytes(r, adapter.ToolCodex, configPath, configRaw) {
		driftFiles = append(driftFiles, configPath)
	}
	if len(driftFiles) > 0 {
		view.ExternalDriftDetected = true
		view.ExternalDriftFiles = driftFiles
	}

	// Cache the overlay Raw map once so the per-key loop does not
	// re-walk profile.Tools on every iteration.
	overlayRaw := overlayRawMap(profile)

	fields := make([]adapter.EffectiveField, 0, len(allOwnedKeys))
	for _, key := range allOwnedKeys {
		envName := envVarForOwnedKey[key]

		// Gather each layer's contribution for this key. Nil = "layer
		// did not contribute a value"; a non-nil layerValue captures
		// both the value and the source string.
		defaultLV := builtInDefaultLayer(key)
		coreLV := profileCoreLayer(key, profile)
		overlayLV := profileOverlayLayer(key, overlayRaw)
		diskLV := onDiskLayer(key, tomlDoc, configPresent, configPath, authRoot, authFlat, authPresent, authPath)
		envLV := envOverrideLayer(envName)

		// Ordered older→newer so a linear pass finds the winner and
		// the shadowed set in one sweep.
		chain := []*layerValue{defaultLV, coreLV, overlayLV, diskLV, envLV}
		field, present := resolveChain(key, chain)
		if !present {
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
	return nil
}

// profileCoreLayer returns the ProfileCore contribution for key, or
// nil when Core does not carry that slot. Only OPENAI_API_KEY routes
// through Core in v1 (see file godoc "ProfileCore mapping").
//
// Empty string on Core.APIKey counts as "not present" — symmetric
// with Plan.collectOwnedAuthValues and Import's Core mapping (Core
// fields treat "" as "not claimed", matching Profile YAML semantics
// where an omitted key and an empty scalar are indistinguishable).
func profileCoreLayer(key string, profile config.Profile) *layerValue {
	if key != "OPENAI_API_KEY" {
		return nil
	}
	if profile.Core.APIKey == "" {
		return nil
	}
	return &layerValue{
		layer:  adapter.LayerCore,
		value:  profile.Core.APIKey,
		source: "profile.core:api_key",
	}
}

// overlayRawMap returns the profile's Codex overlay Raw map, or nil
// when the overlay is absent. Extracted so the per-key loop does not
// re-walk profile.Tools on every iteration.
func overlayRawMap(profile config.Profile) map[string]any {
	if profile.Tools == nil {
		return nil
	}
	ov, ok := profile.Tools[adapter.ToolCodex]
	if !ok {
		return nil
	}
	return ov.Raw
}

// profileOverlayLayer returns the ProfileOverlay contribution for
// key, or nil when the overlay does not carry that entry. Presence in
// the Raw map is the source of truth; a nil value in Raw is treated
// as absent (symmetric with Plan's collectOwnedAuthValues /
// collectOwnedConfigValues which drop nils, and Import's
// null-owned-key policy).
func profileOverlayLayer(key string, overlayRaw map[string]any) *layerValue {
	if overlayRaw == nil {
		return nil
	}
	v, ok := overlayRaw[key]
	if !ok || v == nil {
		return nil
	}
	return &layerValue{
		layer:  adapter.LayerOverlay,
		value:  v,
		source: "profile.overlay:" + key,
	}
}

// onDiskLayer returns the OnDiskToolConfig contribution for key, or
// nil when the on-disk file does not carry it. Routes to config.toml
// or auth.json based on the ownedFileForKey table.
//
// For config.toml keys: codextoml.Doc.Get preserves the original
// typed value (int64, float64, string, bool). A missing key returns
// (nil, false) and contributes nothing.
//
// For auth.json keys: OPENAI_API_KEY is read from the unflattened
// root (symmetric with Import) so the null-vs-empty-vs-string
// decision does not depend on writepath.Flatten's nil-handling
// contract. Every other auth key is looked up in the flat map, where
// the flat dotted-path shape matches OwnedKeysAuthJSON exactly. A
// JSON null value contributes nothing (mirrors Import's
// null-owned-key policy).
func onDiskLayer(
	key string,
	tomlDoc *codextoml.Doc, configPresent bool, configPath string,
	authRoot, authFlat map[string]any, authPresent bool, authPath string,
) *layerValue {
	switch ownedFileForKey[key] {
	case adapter.FormatTOML:
		if !configPresent || tomlDoc == nil {
			return nil
		}
		v, ok := tomlDoc.Get(key)
		if !ok || v == nil {
			return nil
		}
		return &layerValue{
			layer:  adapter.LayerOnDisk,
			value:  v,
			source: configPath + ":" + key,
		}
	case adapter.FormatJSON:
		if !authPresent {
			return nil
		}
		if key == "OPENAI_API_KEY" {
			if authRoot == nil {
				return nil
			}
			v, ok := authRoot["OPENAI_API_KEY"]
			if !ok || v == nil {
				return nil
			}
			return &layerValue{
				layer:  adapter.LayerOnDisk,
				value:  v,
				source: authPath + ":" + key,
			}
		}
		if authFlat == nil {
			return nil
		}
		v, ok := authFlat[key]
		if !ok || v == nil {
			return nil
		}
		return &layerValue{
			layer:  adapter.LayerOnDisk,
			value:  v,
			source: authPath + ":" + key,
		}
	default:
		// Unreachable — every owned key routes to exactly one file
		// per the init() invariant. Return nil rather than panicking
		// so a future refactor that adds a third format has one place
		// to update instead of a runtime crash.
		return nil
	}
}

// envOverrideLayer returns the EnvOverride contribution for envName,
// or nil when the process env has no non-empty value for that name.
// An empty envName means the key has no env counterpart (see
// envVarForOwnedKey) — return nil without reading anything so we do
// not read $"" or similar.
//
// Empty string on the process env is treated as absent because Codex
// itself will not consume an empty env literal to override a
// config.toml value.
func envOverrideLayer(envName string) *layerValue {
	if envName == "" {
		return nil
	}
	// envextract.Lookup is the single env-read primitive for the
	// resolver (E5-S3). We discard the "set" boolean because Codex
	// treats an empty env-var literal as absent — the effective view
	// mirrors that runtime behaviour.
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

// driftForFileBytes reports whether raw differs from the SHA256 that
// state.yaml records for (tool, filePath). Returns false when state
// has no prior entry (E5-S4 AC: no anchor → no drift) or when the
// state read itself errored — drift is informational and must never
// break `current` / `explain` (state read errors are swallowed here
// on purpose; the E5-S4 review's F4/F5 findings verified we do not
// double-read the owned file).
//
// Symmetric with the claudecode adapter's onDisk-hash path, which
// runs against stateio directly out of project.go's readOnDiskSettings.
// The codex readers surface raw bytes to keep the drift check honest
// against the parser's view of the file (no TOCTOU between parse and
// hash).
func driftForFileBytes(r *storage.Resolver, tool config.ToolID, filePath string, raw []byte) bool {
	last, ok, err := stateio.LoadLastApplied(r, tool, filePath)
	if err != nil || !ok {
		return false
	}
	return stateio.Sha256Hex(raw) != last.SHA256
}

// The tri-state read helpers formerly named readCodexTOMLForProject /
// readCodexAuthForProject were consolidated into readCodexTOMLWithPrefix
// / readCodexAuthWithPrefix in readers.go — see the readers.go file
// godoc for the behavioural contract. Project passes "codex project"
// as the log prefix so operator-facing read errors stay distinguishable
// from Import's.
