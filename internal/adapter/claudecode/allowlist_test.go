package claudecode

import (
	"errors"
	"reflect"
	"sort"
	"testing"
)

// TestValidateOwnedKeys_HappyPath — the sorted-and-unique real slice
// passes; a regression here would take init() with it.
func TestValidateOwnedKeys_HappyPath(t *testing.T) {
	if err := validateOwnedKeys(OwnedKeysSettingsJSON); err != nil {
		t.Fatalf("validateOwnedKeys(OwnedKeysSettingsJSON) = %v, want nil", err)
	}
}

// TestValidateOwnedKeys_Unsorted — the sort invariant is the whole
// point of the init check; verify the error path.
func TestValidateOwnedKeys_Unsorted(t *testing.T) {
	bad := []string{"env.B", "env.A"}
	err := validateOwnedKeys(bad)
	var se *sortErr
	if !errors.As(err, &se) {
		t.Fatalf("validateOwnedKeys unsorted: err = %v, want *sortErr", err)
	}
}

// TestValidateOwnedKeys_Duplicate — matches the second half of the
// init-time panic path.
func TestValidateOwnedKeys_Duplicate(t *testing.T) {
	bad := []string{"env.A", "env.A"}
	err := validateOwnedKeys(bad)
	var de *duplicateOwnedKeyError
	if !errors.As(err, &de) {
		t.Fatalf("validateOwnedKeys duplicate: err = %v, want *duplicateOwnedKeyError", err)
	}
	if de.key != "env.A" {
		t.Errorf("duplicateOwnedKeyError.key = %q, want %q", de.key, "env.A")
	}
	// Error message is what an operator would see if this reached the
	// panic; smoke-test it stays readable.
	if got := de.Error(); got == "" {
		t.Errorf("duplicateOwnedKeyError.Error() empty")
	}
	if got := (&sortErr{}).Error(); got == "" {
		t.Errorf("sortErr.Error() empty")
	}
}

// TestOwnedKeys_IsSorted guards the invariant the package init also
// checks — running the invariant here means CI catches a hand-edit
// even when the init-time panic would fire only under `go run`.
func TestOwnedKeys_IsSorted(t *testing.T) {
	if !sort.StringsAreSorted(OwnedKeysSettingsJSON) {
		t.Fatalf("OwnedKeysSettingsJSON must be sorted, got: %v", OwnedKeysSettingsJSON)
	}
}

// TestOwnedKeys_NoDuplicates enforces the second half of the init
// invariant. A duplicate would silently double-write a key in a future
// Plan/Apply and is worth catching in the test suite too.
func TestOwnedKeys_NoDuplicates(t *testing.T) {
	seen := make(map[string]struct{}, len(OwnedKeysSettingsJSON))
	for _, k := range OwnedKeysSettingsJSON {
		if _, dup := seen[k]; dup {
			t.Fatalf("duplicate owned key: %q", k)
		}
		seen[k] = struct{}{}
	}
}

// TestOwnedKeys_ExpectedKeysPresent asserts every key the story's AC
// names is actually shipped. Missing any of these would silently drop
// a Claude Code env var from the merge-preserve owned set.
func TestOwnedKeys_ExpectedKeysPresent(t *testing.T) {
	required := []string{
		"env.ANTHROPIC_API_KEY",
		"env.ANTHROPIC_AUTH_TOKEN",
		"env.ANTHROPIC_BASE_URL",
		"env.ANTHROPIC_MODEL",
		"env.ANTHROPIC_SMALL_FAST_MODEL",
		"env.CLAUDE_CODE_USE_BEDROCK",
		"env.CLAUDE_CODE_USE_VERTEX",
	}
	have := make(map[string]struct{}, len(OwnedKeysSettingsJSON))
	for _, k := range OwnedKeysSettingsJSON {
		have[k] = struct{}{}
	}
	for _, k := range required {
		if _, ok := have[k]; !ok {
			t.Errorf("required key %q missing from OwnedKeysSettingsJSON", k)
		}
	}
}

// TestOwnedKeys_NoUnexpectedKeys pins the entire allowlist to a golden
// slice — the "adding a key requires an ADR + PRD §4.7 edit" rule
// (coding-standards rule 4) fails loudly here rather than silently in a
// merge that expands the scope of what claudecm claims to own.
func TestOwnedKeys_NoUnexpectedKeys(t *testing.T) {
	golden := []string{
		"env.ANTHROPIC_API_KEY",
		"env.ANTHROPIC_AUTH_TOKEN",
		"env.ANTHROPIC_BASE_URL",
		"env.ANTHROPIC_MODEL",
		"env.ANTHROPIC_SMALL_FAST_MODEL",
		"env.CLAUDE_CODE_USE_BEDROCK",
		"env.CLAUDE_CODE_USE_VERTEX",
	}
	if !reflect.DeepEqual(OwnedKeysSettingsJSON, golden) {
		t.Fatalf("OwnedKeysSettingsJSON drifted from golden:\n got: %v\nwant: %v", OwnedKeysSettingsJSON, golden)
	}
}
