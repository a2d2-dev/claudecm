// Package claudecode declares the frozen owned-key allowlist for
// Claude Code's user-scope settings file.
//
// Owned keys are exact string matches against fully-flattened JSON key
// paths (writepath.Flatten shape). Everything outside this list is
// preserved verbatim by the write-path — permissions, hooks,
// mcpServers, model, theme, and any future Claude Code knob that
// claudecm does not manage all round-trip untouched.
//
// This list is FROZEN for v1. Adding a key requires an ADR + migration
// story plus a paired PRD §4.7 edit (coding-standards rule 4).
//
// Nested keys use "." as the separator (matches writepath.Flatten).
// Keys with a literal "." in their JSON name must be escaped as "\\."
// at the boundary; none of Claude Code's known keys currently need
// escaping, so no runtime escaping is required for the strings below.
package claudecode

import "sort"

// OwnedKeysSettingsJSON is the frozen v1 owned-key allowlist for
// ~/.claude/settings.json (Architecture §3.1, PRD §4.7).
//
// Kept in sorted order so:
//   - The init() invariant check below stays a one-liner.
//   - Downstream renderers that emit owned keys get deterministic
//     output without an extra sort at every call-site.
//   - Diffs against future v-next expansions are minimal.
//
// DO NOT mutate this slice at runtime. It is package-level only because
// Go has no read-only slice literals; treat it as constant.
var OwnedKeysSettingsJSON = []string{
	"env.ANTHROPIC_API_KEY",
	"env.ANTHROPIC_AUTH_TOKEN",
	"env.ANTHROPIC_BASE_URL",
	"env.ANTHROPIC_MODEL",
	"env.ANTHROPIC_SMALL_FAST_MODEL",
	"env.CLAUDE_CODE_USE_BEDROCK",
	"env.CLAUDE_CODE_USE_VERTEX",
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

// errUnsortedOwnedKeys and duplicateOwnedKeyError are the two failure
// shapes validateOwnedKeys produces. Named types (rather than fmt.Errorf
// strings) so the unit test can assert with errors.Is / errors.As rather
// than substring-match a panic message.
var errUnsortedOwnedKeys = &sortErr{}

type sortErr struct{}

func (*sortErr) Error() string {
	return "claudecode: OwnedKeysSettingsJSON must be sorted"
}

type duplicateOwnedKeyError struct{ key string }

func (e *duplicateOwnedKeyError) Error() string {
	return "claudecode: OwnedKeysSettingsJSON has a duplicate: " + e.key
}

// init verifies the allowlist invariants (sorted, no duplicates) at
// package load. A type-checker won't catch a hand-edit that breaks
// either property; this init() will. Panic-on-bug is defense-in-depth,
// symmetric with adapter.DefaultRegistry's panic-on-duplicate rule.
func init() {
	if err := validateOwnedKeys(OwnedKeysSettingsJSON); err != nil {
		panic(err)
	}
}
