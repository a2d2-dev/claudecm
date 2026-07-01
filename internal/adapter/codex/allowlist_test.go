package codex

import (
	"errors"
	"reflect"
	"sort"
	"testing"
)

// TestValidateOwnedKeys_HappyPath — the sorted-and-unique real slices
// pass; a regression here would take init() with it.
func TestValidateOwnedKeys_HappyPath(t *testing.T) {
	if err := validateOwnedKeys(OwnedKeysConfigTOML); err != nil {
		t.Fatalf("validateOwnedKeys(OwnedKeysConfigTOML) = %v, want nil", err)
	}
	if err := validateOwnedKeys(OwnedKeysAuthJSON); err != nil {
		t.Fatalf("validateOwnedKeys(OwnedKeysAuthJSON) = %v, want nil", err)
	}
}

// TestValidateOwnedKeys_Unsorted — the sort invariant is the whole
// point of the init check; verify the error path.
func TestValidateOwnedKeys_Unsorted(t *testing.T) {
	bad := []string{"b", "a"}
	err := validateOwnedKeys(bad)
	var se *sortErr
	if !errors.As(err, &se) {
		t.Fatalf("validateOwnedKeys unsorted: err = %v, want *sortErr", err)
	}
	if got := se.Error(); got == "" {
		t.Errorf("sortErr.Error() empty")
	}
}

// TestValidateOwnedKeys_Duplicate — matches the second half of the
// init-time panic path.
func TestValidateOwnedKeys_Duplicate(t *testing.T) {
	bad := []string{"model", "model"}
	err := validateOwnedKeys(bad)
	var de *duplicateOwnedKeyError
	if !errors.As(err, &de) {
		t.Fatalf("validateOwnedKeys duplicate: err = %v, want *duplicateOwnedKeyError", err)
	}
	if de.key != "model" {
		t.Errorf("duplicateOwnedKeyError.key = %q, want %q", de.key, "model")
	}
	if got := de.Error(); got == "" {
		t.Errorf("duplicateOwnedKeyError.Error() empty")
	}
}

// TestValidateNoOverlap_HappyPath — the real allowlists must not
// overlap; if they did, one file's Plan would silently mask the other.
func TestValidateNoOverlap_HappyPath(t *testing.T) {
	if err := validateNoOverlap(OwnedKeysConfigTOML, OwnedKeysAuthJSON); err != nil {
		t.Fatalf("validateNoOverlap(config, auth) = %v, want nil", err)
	}
}

// TestValidateNoOverlap_Detected exercises the failure branch so the
// overlap detector is not a dead code path — a future refactor that
// short-circuits it would take the invariant with it.
func TestValidateNoOverlap_Detected(t *testing.T) {
	a := []string{"OPENAI_API_KEY", "model"}
	b := []string{"OPENAI_API_KEY", "tokens"}
	err := validateNoOverlap(a, b)
	var oe *overlapOwnedKeyError
	if !errors.As(err, &oe) {
		t.Fatalf("validateNoOverlap overlap: err = %v, want *overlapOwnedKeyError", err)
	}
	if oe.key != "OPENAI_API_KEY" {
		t.Errorf("overlapOwnedKeyError.key = %q, want %q", oe.key, "OPENAI_API_KEY")
	}
	if got := oe.Error(); got == "" {
		t.Errorf("overlapOwnedKeyError.Error() empty")
	}
}

// TestOwnedKeysConfigTOML_IsSorted / _NoDuplicates guard the invariants
// the package init also checks — running them here means CI catches a
// hand-edit even when the init-time panic would fire only under
// `go run`.
func TestOwnedKeysConfigTOML_IsSorted(t *testing.T) {
	if !sort.StringsAreSorted(OwnedKeysConfigTOML) {
		t.Fatalf("OwnedKeysConfigTOML must be sorted, got: %v", OwnedKeysConfigTOML)
	}
}

func TestOwnedKeysConfigTOML_NoDuplicates(t *testing.T) {
	seen := make(map[string]struct{}, len(OwnedKeysConfigTOML))
	for _, k := range OwnedKeysConfigTOML {
		if _, dup := seen[k]; dup {
			t.Fatalf("duplicate owned key: %q", k)
		}
		seen[k] = struct{}{}
	}
}

func TestOwnedKeysAuthJSON_IsSorted(t *testing.T) {
	if !sort.StringsAreSorted(OwnedKeysAuthJSON) {
		t.Fatalf("OwnedKeysAuthJSON must be sorted, got: %v", OwnedKeysAuthJSON)
	}
}

func TestOwnedKeysAuthJSON_NoDuplicates(t *testing.T) {
	seen := make(map[string]struct{}, len(OwnedKeysAuthJSON))
	for _, k := range OwnedKeysAuthJSON {
		if _, dup := seen[k]; dup {
			t.Fatalf("duplicate owned key: %q", k)
		}
		seen[k] = struct{}{}
	}
}

// TestOwnedKeys_NoOverlapBetweenFiles pins the cross-file invariant so
// a hand-edit that adds `OPENAI_API_KEY` (or any auth field) to
// config.toml's allowlist fails loudly here, not silently during a
// switch that quietly stops writing the credential.
func TestOwnedKeys_NoOverlapBetweenFiles(t *testing.T) {
	seen := make(map[string]struct{}, len(OwnedKeysConfigTOML))
	for _, k := range OwnedKeysConfigTOML {
		seen[k] = struct{}{}
	}
	for _, k := range OwnedKeysAuthJSON {
		if _, dup := seen[k]; dup {
			t.Errorf("key %q owned by both config.toml and auth.json", k)
		}
	}
}

// TestOwnedKeysConfigTOML_ExpectedKeysPresent asserts every key the
// story's AC names is actually shipped. Missing any of these would
// silently drop a Codex config knob from the merge-preserve owned set.
func TestOwnedKeysConfigTOML_ExpectedKeysPresent(t *testing.T) {
	required := []string{
		"model",
		"model_provider",
		"model_providers.<name>.base_url",
		"model_providers.<name>.wire_api",
		"model_providers.<name>.env_key",
		"model_providers.<name>.name",
	}
	have := make(map[string]struct{}, len(OwnedKeysConfigTOML))
	for _, k := range OwnedKeysConfigTOML {
		have[k] = struct{}{}
	}
	for _, k := range required {
		if _, ok := have[k]; !ok {
			t.Errorf("required key %q missing from OwnedKeysConfigTOML", k)
		}
	}
}

// TestOwnedKeysAuthJSON_ExpectedKeysPresent asserts the auth top-level
// fields declared by architecture §3.1 are shipped. OPENAI_API_KEY is
// the anchor; the rest are frozen here in code per the arch
// requirement "expanded in the adapter's var ownedAuthKeys".
func TestOwnedKeysAuthJSON_ExpectedKeysPresent(t *testing.T) {
	required := []string{
		"OPENAI_API_KEY",
	}
	have := make(map[string]struct{}, len(OwnedKeysAuthJSON))
	for _, k := range OwnedKeysAuthJSON {
		have[k] = struct{}{}
	}
	for _, k := range required {
		if _, ok := have[k]; !ok {
			t.Errorf("required key %q missing from OwnedKeysAuthJSON", k)
		}
	}
}

// TestOwnedKeysConfigTOML_NoUnexpectedKeys and its auth.json sibling
// pin the entire allowlist to a golden slice — the "adding a key
// requires an ADR + PRD §4.7 edit" rule (coding-standards rule 4)
// fails loudly here rather than silently in a merge that expands the
// scope of what claudecm claims to own.
func TestOwnedKeysConfigTOML_NoUnexpectedKeys(t *testing.T) {
	golden := []string{
		"model",
		"model_provider",
		"model_providers.<name>.base_url",
		"model_providers.<name>.env_key",
		"model_providers.<name>.name",
		"model_providers.<name>.wire_api",
	}
	if !reflect.DeepEqual(OwnedKeysConfigTOML, golden) {
		t.Fatalf("OwnedKeysConfigTOML drifted from golden:\n got: %v\nwant: %v", OwnedKeysConfigTOML, golden)
	}
}

func TestOwnedKeysAuthJSON_NoUnexpectedKeys(t *testing.T) {
	golden := []string{
		"OPENAI_API_KEY",
		"last_refresh",
		"tokens",
	}
	if !reflect.DeepEqual(OwnedKeysAuthJSON, golden) {
		t.Fatalf("OwnedKeysAuthJSON drifted from golden:\n got: %v\nwant: %v", OwnedKeysAuthJSON, golden)
	}
}
