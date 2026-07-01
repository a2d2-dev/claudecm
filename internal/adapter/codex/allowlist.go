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
// rule 4).
//
// Nested keys use "." as the separator (matches writepath.Flatten).
package codex

import (
	"fmt"
	"sort"
)

// OwnedKeysConfigTOML is the frozen v1 owned-key allowlist for
// ~/.codex/config.toml (Architecture §3.1, PRD §4.7).
//
// Concrete flat keys matching writepath.Flatten output. v1 supports
// 'openai' and 'anthropic' provider entries; other custom provider
// names (e.g. 'my-relay') are considered non-owned by claudecm and
// preserved via FR-5 merge-preserve. Post-v1: dynamic provider
// ownership.
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
	"approval_mode",
	"model",
	"model_provider",
	"model_providers.anthropic.base_url",
	"model_providers.anthropic.env_key",
	"model_providers.anthropic.name",
	"model_providers.anthropic.wire_api",
	"model_providers.openai.base_url",
	"model_providers.openai.env_key",
	"model_providers.openai.name",
	"model_providers.openai.wire_api",
}

// OwnedKeysAuthJSON is the frozen v1 owned-key allowlist for
// ~/.codex/auth.json (Architecture §3.1, PRD §4.7).
//
// Concrete flat keys matching writepath.Flatten output. The top-level
// OpenAI API key sits alongside the OAuth token bundle
// (tokens.access_token / tokens.refresh_token / tokens.id_token /
// tokens.account_id) and the auth-mode + last-refresh scalars. These
// are what a `claudecm switch` must overwrite atomically when moving
// between profiles that carry distinct OpenAI credentials, and are
// what `claudecm import codex` must be free to read.
//
// Anything else Codex CLI writes into auth.json — future top-level
// keys, per-provider extensions, comment-preserving TOML analogues —
// is out of v1 owned scope and MUST round-trip through the write-path
// untouched via FR-5 merge-preserve.
//
// Kept in sorted order for the same reasons as OwnedKeysConfigTOML.
var OwnedKeysAuthJSON = []string{
	"OPENAI_API_KEY",
	"auth_mode",
	"last_refresh",
	"tokens.access_token",
	"tokens.account_id",
	"tokens.id_token",
	"tokens.refresh_token",
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
//
// Errors are wrapped with fmt.Errorf %w so errors.Is / errors.As can
// still walk the chain down to the sortErr / duplicateOwnedKeyError /
// overlapOwnedKeyError sentinels when a panic recover captures the
// value.
func init() {
	if err := validateOwnedKeys(OwnedKeysConfigTOML); err != nil {
		panic(fmt.Errorf("codex.OwnedKeysConfigTOML: %w", err))
	}
	if err := validateOwnedKeys(OwnedKeysAuthJSON); err != nil {
		panic(fmt.Errorf("codex.OwnedKeysAuthJSON: %w", err))
	}
	if err := validateNoOverlap(OwnedKeysConfigTOML, OwnedKeysAuthJSON); err != nil {
		panic(err)
	}
}
