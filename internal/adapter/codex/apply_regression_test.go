package codex_test

// apply_regression_test.go — hotfix regression tests for the codex
// tomlParser + writepath.Flatten dotted-key double-escape bug found
// during E4-S7 implementation.
//
// Bug summary. tomlParser() used to return a FLAT map[string]any
// keyed by dotted paths ("model_providers.openai.base_url"). But
// writepath.Apply unconditionally feeds parser output through
// writepath.Flatten, which escapes any '.' inside a single map key
// as `\.`. The resulting key ("model_providers\.openai\.base_url")
// no longer matched the unescaped OwnedKeysConfigTOML entry, so
// Diff flagged TouchesUnowned=true and Apply refused with
// ErrDryRunUnownedTouched on every nested-key change. Fix: parser
// returns a NESTED map[string]any tree so Flatten produces the
// same flat form OwnedKeysConfigTOML lists.
//
// These tests fail on the old flat parser and pass on the nested
// one. They live alongside apply_test.go (same package_test) so
// they share the bootstrappedResolver / codexOverlay helpers.

import (
	"bytes"
	"context"
	"errors"
	"os"
	"testing"

	"github.com/a2d2-dev/claudecm/internal/adapter/codex"
	"github.com/a2d2-dev/claudecm/internal/config"
	"github.com/a2d2-dev/claudecm/internal/writepath"
)

// TestApply_NestedProviderKeyChange seeds config.toml with a
// [model_providers.openai] table carrying a base_url, then applies a
// profile whose overlay changes that base_url. On the old flat
// parser this used to trip ErrDryRunUnownedTouched because Flatten
// escaped the nested key past the OwnedKeys allowlist. On the fixed
// nested parser Apply must succeed and the new value must land.
func TestApply_NestedProviderKeyChange(t *testing.T) {
	r := bootstrappedResolver(t)
	configSeed := []byte(`[model_providers.openai]
base_url = "old"
`)
	if err := os.WriteFile(codex.ConfigPath(r), configSeed, 0o600); err != nil {
		t.Fatalf("seed config.toml: %v", err)
	}

	profile := config.Profile{
		Name: "nested-change",
		Core: config.CoreConfig{APIKey: "sk"},
		Tools: codexOverlay(map[string]any{
			"model_providers.openai.base_url": "new",
		}),
	}
	plans := plansFor(t, r, profile)
	if _, err := applyAll(context.Background(), r, plans); err != nil {
		if errors.Is(err, writepath.ErrDryRunUnownedTouched) {
			t.Fatalf("Apply refused nested owned-key change as unowned: %v (double-escape bug regression)", err)
		}
		t.Fatalf("Apply: %v", err)
	}

	doc := readConfigDoc(t, r)
	if v, ok := doc.Get("model_providers.openai.base_url"); !ok || v != "new" {
		t.Errorf("model_providers.openai.base_url = %v (ok=%v), want %q", v, ok, "new")
	}

	// Sanity: overwritten bytes must contain the new value.
	got, err := os.ReadFile(codex.ConfigPath(r))
	if err != nil {
		t.Fatalf("read config.toml: %v", err)
	}
	if !bytes.Contains(got, []byte(`base_url = "new"`)) {
		t.Errorf("config.toml missing base_url = \"new\"; got:\n%s", string(got))
	}
	if bytes.Contains(got, []byte(`base_url = "old"`)) {
		t.Errorf("config.toml still contains base_url = \"old\"; got:\n%s", string(got))
	}
}

// TestApply_NestedProviderKeyDelete seeds config.toml with a nested
// owned key and applies a profile whose overlay omits it. Under
// overlay-as-truth (NFR-S6) Apply must delete the key. The same
// double-escape bug used to block the delete because Diff refused
// the plan before it could run. Fixed parser lets the delete land.
func TestApply_NestedProviderKeyDelete(t *testing.T) {
	r := bootstrappedResolver(t)
	configSeed := []byte(`[model_providers.openai]
base_url = "old"
`)
	if err := os.WriteFile(codex.ConfigPath(r), configSeed, 0o600); err != nil {
		t.Fatalf("seed config.toml: %v", err)
	}

	profile := config.Profile{
		Name: "nested-delete",
		Core: config.CoreConfig{APIKey: "sk"},
		// Overlay omits model_providers.openai.base_url entirely.
		Tools: codexOverlay(map[string]any{}),
	}
	plans := plansFor(t, r, profile)
	if _, err := applyAll(context.Background(), r, plans); err != nil {
		if errors.Is(err, writepath.ErrDryRunUnownedTouched) {
			t.Fatalf("Apply refused nested owned-key delete as unowned: %v (double-escape bug regression)", err)
		}
		t.Fatalf("Apply: %v", err)
	}

	doc := readConfigDoc(t, r)
	if v, ok := doc.Get("model_providers.openai.base_url"); ok {
		t.Errorf("model_providers.openai.base_url = %v, want absent (overlay-as-truth delete)", v)
	}

	got, err := os.ReadFile(codex.ConfigPath(r))
	if err != nil {
		t.Fatalf("read config.toml: %v", err)
	}
	if bytes.Contains(got, []byte(`base_url = "old"`)) {
		t.Errorf("config.toml still contains base_url after delete; got:\n%s", string(got))
	}
}

// TestApply_KitchenSinkNestedKeys exercises the full 8-nested-key
// scope of the original E4-S7 bug: model + model_provider + both
// name/base_url/env_key/wire_api of both openai and anthropic
// provider blocks. Apply must succeed and every value must land.
func TestApply_KitchenSinkNestedKeys(t *testing.T) {
	r := bootstrappedResolver(t)

	profile := config.Profile{
		Name: "kitchen-sink",
		Core: config.CoreConfig{APIKey: "sk-ks"},
		Tools: codexOverlay(map[string]any{
			"model":                              "opus",
			"model_provider":                     "openai",
			"model_providers.openai.name":        "OpenAI",
			"model_providers.openai.base_url":    "https://api.openai.example",
			"model_providers.openai.env_key":     "OPENAI_API_KEY",
			"model_providers.openai.wire_api":    "responses",
			"model_providers.anthropic.name":     "Anthropic",
			"model_providers.anthropic.base_url": "https://api.anthropic.example",
			"model_providers.anthropic.env_key":  "ANTHROPIC_API_KEY",
			"model_providers.anthropic.wire_api": "messages",
		}),
	}
	plans := plansFor(t, r, profile)
	if _, err := applyAll(context.Background(), r, plans); err != nil {
		if errors.Is(err, writepath.ErrDryRunUnownedTouched) {
			t.Fatalf("Apply refused nested owned-key kitchen-sink as unowned: %v (double-escape bug regression)", err)
		}
		t.Fatalf("Apply: %v", err)
	}

	doc := readConfigDoc(t, r)
	want := map[string]any{
		"model":                              "opus",
		"model_provider":                     "openai",
		"model_providers.openai.name":        "OpenAI",
		"model_providers.openai.base_url":    "https://api.openai.example",
		"model_providers.openai.env_key":     "OPENAI_API_KEY",
		"model_providers.openai.wire_api":    "responses",
		"model_providers.anthropic.name":     "Anthropic",
		"model_providers.anthropic.base_url": "https://api.anthropic.example",
		"model_providers.anthropic.env_key":  "ANTHROPIC_API_KEY",
		"model_providers.anthropic.wire_api": "messages",
	}
	for k, v := range want {
		got, ok := doc.Get(k)
		if !ok {
			t.Errorf("%s: absent, want %v", k, v)
			continue
		}
		if got != v {
			t.Errorf("%s = %v, want %v", k, got, v)
		}
	}

	// Second Apply on the same profile must be a clean no-op — a
	// negative regression: if the nested parser somehow reshaped
	// the Doc, the second Apply would see a diff and re-write.
	second := plansFor(t, r, profile)
	reports, err := applyAll(context.Background(), r, second)
	if err != nil {
		t.Fatalf("second Apply: %v", err)
	}
	for i, rep := range reports {
		if !rep.Skipped {
			t.Errorf("second Apply reports[%d].Skipped = false, want true (idempotent)", i)
		}
	}
}
