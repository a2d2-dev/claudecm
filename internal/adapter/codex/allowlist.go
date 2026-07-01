// Package codex declares the frozen owned-key allowlists for Codex
// CLI's two on-disk files: config.toml (TOML) and auth.json (JSON).
//
// Owned keys are exact string matches against fully-flattened key
// paths (writepath.Flatten shape). Everything outside these lists is
// preserved verbatim by the write-path — the whole point of the
// merge-preserve contract is that a Codex user's `[history]`, MCP
// configuration, project profiles, or any future knob claudecm does
// not manage round-trip untouched.
//
// These lists are FROZEN for v1. Adding a key requires an ADR +
// migration story plus a paired PRD §4.7 edit (coding-standards
// rule 4). The `<name>` fragment in the config.toml keys is a glob:
// it stands for the arbitrary provider name declared under
// `[model_providers.<name>]` in the on-disk TOML. Plan / Apply
// (E4-S4 / E4-S5) expand it against the concrete providers present in
// the merged document. The frozen allowlist carries the templated
// spelling so a hand-edit that adds a NEW template must survive
// review — a Plan render that touches an untemplated key is a bug.
//
// Nested keys use "." as the separator (matches writepath.Flatten).
package codex

import (
	"errors"
	"sort"
)

// OwnedKeysConfigTOML is the frozen v1 owned-key allowlist for
// ~/.codex/config.toml (Architecture §3.1, PRD §4.7).
//
// The `<name>` fragment is a glob resolved at render time — v1 Plan
// walks the merged document's `model_providers.*` table and rewrites
// each templated entry per concrete provider. The templated strings
// are what this list freezes; concrete resolutions are transient.
//
// Kept in sorted order so:
//   - The init() invariant check below stays a one-liner.
//   - Downstream renderers that emit owned keys get deterministic
//     output without an extra sort at every call-site.
//   - Diffs against future v-next expansions are minimal.
//
// DO NOT mutate this slice at runtime. It is package-level only
// because Go has no read-only slice literals; treat it as constant.
var OwnedKeysConfigTOML = []string{
	"model",
	"model_provider",
	"model_providers.<name>.base_url",
	"model_providers.<name>.env_key",
	"model_providers.<name>.name",
	"model_providers.<name>.wire_api",
}

// OwnedKeysAuthJSON is the frozen v1 owned-key allowlist for
// ~/.codex/auth.json (Architecture §3.1, PRD §4.7).
//
// Codex CLI's auth.json is the current-user credential store. Its
// top-level shape is the OpenAI API key alongside the OAuth token
// bundle and its refresh timestamp — those three top-level fields are
// what a `claudecm switch` must overwrite atomically when moving
// between profiles that carry distinct OpenAI credentials, and are
// what `claudecm import codex` must be free to read.
//
// Anything else Codex CLI writes into auth.json — future top-level
// keys, per-provider extensions, comment-preserving TOML analogues —
// is out of v1 owned scope and MUST round-trip through the write-path
// untouched.
//
// Kept in sorted order for the same reasons as OwnedKeysConfigTOML.
var OwnedKeysAuthJSON = []string{
	"OPENAI_API_KEY",
	"last_refresh",
	"tokens",
}

// validateOwnedKeys checks the sorted + no-duplicates invariant on a
// candidate slice. Split out from init() so the panic paths are
// unit-testable without reaching for a runtime-init hook or mutating
// package state. Returns the first invariant violation as a plain
// error; init() upgrades it to a panic.
func validateOwnedKeys(keys []string) error {
	if !sort.StringsAreSorted(keys) {
		return errUnsortedOwnedKeys
	}
	for i := 1; i < len(keys); i++ {
		if keys[i] == keys[i-1] {
			return &duplicateOwnedKeyError{key: keys[i]}
		}
	}
	return nil
}

// validateNoOverlap enforces the invariant that no key appears in both
// files' allowlists. The two files are structurally independent —
// config.toml describes model routing, auth.json describes
// credentials — and any accidental overlap would mean the same
// logical key is owned twice, letting one file's Plan silently mask
// the other's. Cheap O(n+m) with the sorted invariant.
func validateNoOverlap(a, b []string) error {
	seen := make(map[string]struct{}, len(a))
	for _, k := range a {
		seen[k] = struct{}{}
	}
	for _, k := range b {
		if _, dup := seen[k]; dup {
			return &overlapOwnedKeyError{key: k}
		}
	}
	return nil
}

// errUnsortedOwnedKeys, duplicateOwnedKeyError, and overlapOwnedKeyError
// are the three failure shapes validateOwnedKeys / validateNoOverlap
// produce. Named types (rather than fmt.Errorf strings) so the unit
// tests can assert with errors.Is / errors.As rather than
// substring-match a panic message.
var errUnsortedOwnedKeys = &sortErr{}

type sortErr struct{}

func (*sortErr) Error() string {
	return "codex: owned-key list must be sorted"
}

type duplicateOwnedKeyError struct{ key string }

func (e *duplicateOwnedKeyError) Error() string {
	return "codex: owned-key list has a duplicate: " + e.key
}

type overlapOwnedKeyError struct{ key string }

func (e *overlapOwnedKeyError) Error() string {
	return "codex: key owned by both config.toml and auth.json: " + e.key
}

// init verifies the allowlist invariants (sorted, no duplicates, no
// cross-file overlap) at package load. A type-checker will not catch a
// hand-edit that breaks any of these properties; this init() will.
// Panic-on-bug is defense-in-depth, symmetric with
// adapter.DefaultRegistry's panic-on-duplicate rule.
func init() {
	if err := validateOwnedKeys(OwnedKeysConfigTOML); err != nil {
		panic(errors.New("codex.OwnedKeysConfigTOML: " + err.Error()))
	}
	if err := validateOwnedKeys(OwnedKeysAuthJSON); err != nil {
		panic(errors.New("codex.OwnedKeysAuthJSON: " + err.Error()))
	}
	if err := validateNoOverlap(OwnedKeysConfigTOML, OwnedKeysAuthJSON); err != nil {
		panic(err)
	}
}
