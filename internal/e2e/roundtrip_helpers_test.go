//go:build test

// roundtrip_helpers.go — parse/compare shims used by roundtrip_test.go.
//
// The round-trip smoke asserts semantic equivalence on the owned-key
// scope: for every allowlisted key in the ORIGINAL fixture, the value
// in the POST-switch bytes must match. These helpers keep the tests
// declarative — parse both sides, flatten to a map[string]any, compare
// per-key.
//
// Non-owned bytes are compared as follows:
//
//   - JSON files: assert every top-level key in the original that is
//     not covered by the owned-key allowlist is still present with the
//     same value after switch.
//   - TOML files: assert every table header (unowned scope) and every
//     unowned scalar survives the round-trip via the codex TOML
//     doc-model reader.
//
// Kept as a companion to runner.go so runner.go stays generic; these
// shims are round-trip-specific and would clutter the shared setup.

package e2e

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/a2d2-dev/claudecm/internal/adapter/claudecode"
	"github.com/a2d2-dev/claudecm/internal/adapter/codex"
	codextoml "github.com/a2d2-dev/claudecm/internal/adapter/codex/toml"
	"github.com/a2d2-dev/claudecm/internal/writepath"
)

// claudecodeOwnedFlatKeys returns the frozen owned-key allowlist for
// Claude Code as a []string. Directly references the adapter package
// so the list stays in sync — a future addition lands automatically.
func claudecodeOwnedFlatKeys() []string { return claudecode.OwnedKeysSettingsJSON }

// codexAuthOwnedFlatKeys returns the frozen owned-key allowlist for
// ~/.codex/auth.json.
func codexAuthOwnedFlatKeys() []string { return codex.OwnedKeysAuthJSON }

// codexConfigOwnedFlatKeys returns the frozen owned-key allowlist for
// ~/.codex/config.toml.
func codexConfigOwnedFlatKeys() []string { return codex.OwnedKeysConfigTOML }

// parseJSON parses raw as JSON and Flatten's the top-level value.
// Zero-byte input yields an empty map (mirrors writepath.Flatten(nil)).
func parseJSON(t *testing.T, label string, raw []byte) map[string]any {
	t.Helper()
	if len(raw) == 0 {
		return map[string]any{}
	}
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatalf("parse %s as JSON: %v", label, err)
	}
	flat, err := writepath.Flatten(v)
	if err != nil {
		t.Fatalf("flatten %s: %v", label, err)
	}
	return flat
}

// parseTOML parses raw as TOML using the codex adapter's doc-model
// wrapper (the same reader Import uses) and flattens it. This is the
// authoritative shape switch's write path lands on disk with, so a
// round-trip assertion using the same reader is byte-consistent.
func parseTOML(t *testing.T, label string, raw []byte) map[string]any {
	t.Helper()
	if len(raw) == 0 {
		return map[string]any{}
	}
	doc, err := codextoml.Load(raw)
	if err != nil {
		t.Fatalf("parse %s as TOML: %v", label, err)
	}
	flat := map[string]any{}
	for _, k := range doc.Keys() {
		if v, ok := doc.Get(k); ok {
			flat[k] = v
		}
	}
	return flat
}

// assertJSONOwnedRoundTrip checks that for every allowlisted owned key
// present in the original, the same value survives to the post-switch
// bytes.
func assertJSONOwnedRoundTrip(t *testing.T, original, post []byte, ownedKeys []string) {
	t.Helper()
	origFlat := parseJSON(t, "original", original)
	postFlat := parseJSON(t, "post-switch", post)
	for _, key := range ownedKeys {
		orig, hadOrig := origFlat[key]
		if !hadOrig {
			continue // key not present in original — nothing to round-trip
		}
		got, ok := postFlat[key]
		if !ok {
			t.Errorf("owned key %q missing after switch (original=%v)", key, orig)
			continue
		}
		if !valuesEqual(orig, got) {
			t.Errorf("owned key %q drifted: original=%v post=%v", key, orig, got)
		}
	}
}

// assertJSONUnownedPreserved walks the original's top-level keys and
// asserts every unowned entry still exists (semantic equality) in
// post-switch bytes.
func assertJSONUnownedPreserved(t *testing.T, original, post []byte, ownedKeys []string) {
	t.Helper()
	// Only compare top-level keys — writepath.Flatten emits nested keys
	// too, but the write-path's preserve-unowned guarantee is stated at
	// the top-level (settings.json / auth.json entries under the JSON
	// object root). Nested unowned data is captured transitively.
	var origMap, postMap map[string]any
	if err := json.Unmarshal(original, &origMap); err != nil {
		t.Fatalf("unmarshal original JSON: %v", err)
	}
	if err := json.Unmarshal(post, &postMap); err != nil {
		t.Fatalf("unmarshal post-switch JSON: %v", err)
	}
	ownedTopLevel := topLevelOwned(ownedKeys)
	for k, want := range origMap {
		if ownedTopLevel[k] {
			continue
		}
		got, ok := postMap[k]
		if !ok {
			t.Errorf("unowned top-level key %q missing after switch (want=%v)", k, want)
			continue
		}
		if !reflect.DeepEqual(want, got) {
			t.Errorf("unowned top-level key %q drifted: want=%v got=%v", k, want, got)
		}
	}
}

// topLevelOwned returns the set of top-level JSON keys claimed by any
// owned-key entry. "env.ANTHROPIC_MODEL" contributes "env"; a bare key
// like "OPENAI_API_KEY" contributes itself. Used to skip owned tops
// when comparing unowned entries.
//
// Note: this is intentionally conservative — a top-level "env" object
// that mixes owned and unowned entries would need per-key traversal.
// The v1 fixtures do not mix (all env.* keys in the fixture are the
// allowlisted ones), so top-level skip is safe. A future fixture that
// introduces a truly-unowned env.* entry would need to extend this
// helper.
func topLevelOwned(ownedKeys []string) map[string]bool {
	out := map[string]bool{}
	for _, k := range ownedKeys {
		if i := strings.Index(k, "."); i >= 0 {
			out[k[:i]] = true
		} else {
			out[k] = true
		}
	}
	return out
}

// assertTOMLOwnedRoundTrip is the codex config.toml twin of
// assertJSONOwnedRoundTrip.
func assertTOMLOwnedRoundTrip(t *testing.T, original, post []byte, ownedKeys []string) {
	t.Helper()
	origFlat := parseTOML(t, "original config.toml", original)
	postFlat := parseTOML(t, "post-switch config.toml", post)
	for _, key := range ownedKeys {
		orig, hadOrig := origFlat[key]
		if !hadOrig {
			continue
		}
		got, ok := postFlat[key]
		if !ok {
			t.Errorf("owned toml key %q missing after switch (original=%v)", key, orig)
			continue
		}
		if !valuesEqual(orig, got) {
			t.Errorf("owned toml key %q drifted: original=%v post=%v", key, orig, got)
		}
	}
}

// assertTOMLUnownedPreserved walks every flat TOML key from the
// original and asserts every non-owned entry still exists in post.
func assertTOMLUnownedPreserved(t *testing.T, original, post []byte, ownedKeys []string) {
	t.Helper()
	origFlat := parseTOML(t, "original config.toml", original)
	postFlat := parseTOML(t, "post-switch config.toml", post)
	owned := map[string]bool{}
	for _, k := range ownedKeys {
		owned[k] = true
	}
	for k, want := range origFlat {
		if owned[k] {
			continue
		}
		got, ok := postFlat[k]
		if !ok {
			t.Errorf("unowned toml key %q missing after switch (want=%v)", k, want)
			continue
		}
		if !valuesEqual(want, got) {
			t.Errorf("unowned toml key %q drifted: want=%v got=%v", k, want, got)
		}
	}
}

// valuesEqual is a tolerant equality check that handles the JSON /
// TOML type widening dance: an int in TOML becomes int64 through the
// doc-model reader; the same key from a JSON literal comes back as
// float64. Compare by string form as a last-resort — fine for the
// owned scalars in the v1 allowlists (all strings, bools, or numbers).
func valuesEqual(a, b any) bool {
	if reflect.DeepEqual(a, b) {
		return true
	}
	return anyToString(a) == anyToString(b)
}

// anyToString stringifies for the tolerant equality check.
func anyToString(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case bool:
		if t {
			return "true"
		}
		return "false"
	default:
		b, _ := json.Marshal(v)
		return string(b)
	}
}

// replaceJSONOwnedValue re-encodes fixture bytes with a single owned
// key's value replaced. Kept as a helper so the kitchen-sink round-trip
// mutation is deterministic — unowned bytes come back through
// json.Marshal but that reserialisation is stable enough for the
// mutate-then-switch assertion (switch itself does not depend on the
// mutated intermediate — it depends on the ORIGINAL fixture for the
// owned-key comparison and reads the mutated bytes through its own
// parser).
func replaceJSONOwnedValue(t *testing.T, fixture []byte, flatKey, newVal string) []byte {
	t.Helper()
	var root map[string]any
	if err := json.Unmarshal(fixture, &root); err != nil {
		t.Fatalf("mutate: unmarshal fixture: %v", err)
	}
	// Only "env.<VAR>" and bare "<VAR>" shapes are exercised by v1's
	// owned-key allowlist so we do not need a full nested-set helper.
	if strings.HasPrefix(flatKey, "env.") {
		envVar := strings.TrimPrefix(flatKey, "env.")
		env, _ := root["env"].(map[string]any)
		if env == nil {
			env = map[string]any{}
		}
		env[envVar] = newVal
		root["env"] = env
	} else {
		root[flatKey] = newVal
	}
	b, err := json.Marshal(root)
	if err != nil {
		t.Fatalf("mutate: marshal fixture: %v", err)
	}
	return b
}

// replaceTOMLOwnedScalar re-encodes TOML fixture bytes with a single
// scalar value replaced via the codex doc-model. Preserves unowned
// bytes (that is why we go through the doc model instead of a naive
// re-encode).
func replaceTOMLOwnedScalar(t *testing.T, fixture []byte, key, newVal string) []byte {
	t.Helper()
	doc, err := codextoml.Load(fixture)
	if err != nil {
		t.Fatalf("mutate: parse toml: %v", err)
	}
	if err := doc.Set(key, newVal); err != nil {
		t.Fatalf("mutate: set %q: %v", key, err)
	}
	b, err := doc.Marshal()
	if err != nil {
		t.Fatalf("mutate: marshal toml: %v", err)
	}
	return b
}
