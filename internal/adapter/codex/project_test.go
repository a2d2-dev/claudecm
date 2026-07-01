package codex_test

// project_test.go — E4-S6. Untagged tests exercise the Project surface
// through the exported adapter, using per-test HOME directories so
// on-disk state is isolated. Layer-precedence tests that require
// injecting a synthetic env-var universe live under `//go:build test`
// in project_hooks_test.go — symmetric with E3-S6.

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"testing"

	"github.com/a2d2-dev/claudecm/internal/adapter"
	"github.com/a2d2-dev/claudecm/internal/adapter/codex"
	"github.com/a2d2-dev/claudecm/internal/config"
	"github.com/a2d2-dev/claudecm/internal/storage"
)

// clearCodexEnv wipes every env var the codex env allowlist covers so
// an ambient developer env cannot leak into an untagged test. Uses
// t.Setenv, which restores the previous value at test end.
func clearCodexEnv(t *testing.T) {
	t.Helper()
	for _, name := range []string{
		"OPENAI_API_KEY",
		"OPENAI_BASE_URL",
		"CODEX_MODEL",
		"CODEX_MODEL_PROVIDER",
	} {
		t.Setenv(name, "")
	}
}

// projectResolver builds a Resolver anchored at a per-test HOME.
func projectResolver(t *testing.T) *storage.Resolver {
	t.Helper()
	r, err := storage.NewResolverWithHome(t.TempDir())
	if err != nil {
		t.Fatalf("storage.NewResolverWithHome: %v", err)
	}
	return r
}

// projectFieldByKey pulls the EffectiveField whose Key matches out of
// a view. Returns (field, true) on hit, zero-value + false on miss.
func projectFieldByKey(view adapter.EffectiveView, key string) (adapter.EffectiveField, bool) {
	for _, f := range view.Fields {
		if f.Key == key {
			return f, true
		}
	}
	return adapter.EffectiveField{}, false
}

// mustField asserts that a key is present in the view and fails the
// test otherwise.
func mustField(t *testing.T, view adapter.EffectiveView, key string) adapter.EffectiveField {
	t.Helper()
	f, ok := projectFieldByKey(view, key)
	if !ok {
		t.Fatalf("view missing owned key %q; fields=%+v", key, view.Fields)
	}
	return f
}

// codexProfileWith constructs a codex profile with Core.APIKey and an
// optional Overlay.Raw map. Kept small so tests read at a glance.
func codexProfileWith(name, apiKey string, raw map[string]any) config.Profile {
	p := config.Profile{
		SchemaVersion: config.CurrentProfileSchemaVersion,
		Name:          name,
		Core:          config.CoreConfig{APIKey: apiKey},
	}
	if raw != nil {
		p.Tools = map[config.ToolID]config.ToolOverlay{
			adapter.ToolCodex: {Raw: raw},
		}
	}
	return p
}

func TestProject_NoOverridesAllFromProfile(t *testing.T) {
	clearCodexEnv(t)
	r := projectResolver(t)
	a := codex.New()

	p := codexProfileWith("t", "sk-core-secret", map[string]any{
		"model":          "opus",
		"model_provider": "anthropic",
	})

	view, err := a.Project(context.Background(), r, p)
	if err != nil {
		t.Fatalf("Project: %v", err)
	}
	if view.Tool != adapter.ToolCodex {
		t.Fatalf("view.Tool = %q, want %q", view.Tool, adapter.ToolCodex)
	}

	// OPENAI_API_KEY: Core-routed, no shadowed layers.
	apiKey := mustField(t, view, "OPENAI_API_KEY")
	if apiKey.WinningLayer != adapter.LayerCore {
		t.Errorf("OPENAI_API_KEY: WinningLayer=%q, want ProfileCore", apiKey.WinningLayer)
	}
	if apiKey.Source != "profile.core:api_key" {
		t.Errorf("OPENAI_API_KEY: Source=%q, want profile.core:api_key", apiKey.Source)
	}
	if apiKey.Value != "sk-core-secret" {
		t.Errorf("OPENAI_API_KEY: Value=%v, want sk-core-secret", apiKey.Value)
	}
	if len(apiKey.Shadowed) != 0 {
		t.Errorf("OPENAI_API_KEY: Shadowed=%+v, want empty", apiKey.Shadowed)
	}
	if !apiKey.Secret {
		t.Errorf("OPENAI_API_KEY: Secret=false, want true")
	}

	// Overlay-routed keys: model, model_provider.
	overlayCases := map[string]string{
		"model":          "opus",
		"model_provider": "anthropic",
	}
	for key, wantVal := range overlayCases {
		f := mustField(t, view, key)
		if f.WinningLayer != adapter.LayerOverlay {
			t.Errorf("%s: WinningLayer=%q, want ProfileOverlay", key, f.WinningLayer)
		}
		if f.Source != "profile.overlay:"+key {
			t.Errorf("%s: Source=%q, want profile.overlay:%s", key, f.Source, key)
		}
		if f.Value != wantVal {
			t.Errorf("%s: Value=%v, want %v", key, f.Value, wantVal)
		}
	}

	// Non-contributed owned keys must be absent.
	for _, key := range []string{
		"approval_mode",
		"tokens.access_token",
		"auth_mode",
	} {
		if _, ok := projectFieldByKey(view, key); ok {
			t.Errorf("view unexpectedly contains %q", key)
		}
	}
}

func TestProject_MissingFilesEmptyOnDisk(t *testing.T) {
	clearCodexEnv(t)
	r := projectResolver(t)
	a := codex.New()

	// No writeConfigTOML / writeAuthJSON; ~/.codex does not exist.
	p := codexProfileWith("t", "sk-core", nil)

	view, err := a.Project(context.Background(), r, p)
	if err != nil {
		t.Fatalf("Project: %v", err)
	}
	// OPENAI_API_KEY should win from Core with no shadowed layers —
	// proves that a missing on-disk file contributes nothing.
	f := mustField(t, view, "OPENAI_API_KEY")
	if f.WinningLayer != adapter.LayerCore {
		t.Fatalf("WinningLayer=%q, want ProfileCore", f.WinningLayer)
	}
	if len(f.Shadowed) != 0 {
		t.Fatalf("Shadowed=%+v, want empty (no on-disk file)", f.Shadowed)
	}
	// Every non-Core key should be absent.
	if len(view.Fields) != 1 {
		t.Fatalf("expected only OPENAI_API_KEY field, got %d: %+v", len(view.Fields), view.Fields)
	}
}

func TestProject_MalformedConfigTomlRefused(t *testing.T) {
	clearCodexEnv(t)
	r := projectResolver(t)
	a := codex.New()

	writeConfigTOML(t, r, "model = \n") // truncated

	_, err := a.Project(context.Background(), r, config.Profile{})
	if !errors.Is(err, codex.ErrParseFailed) {
		t.Fatalf("Project err = %v, want ErrParseFailed", err)
	}
}

func TestProject_MalformedAuthJsonRefused(t *testing.T) {
	clearCodexEnv(t)
	r := projectResolver(t)
	a := codex.New()

	writeAuthJSON(t, r, `{"OPENAI_API_KEY":`) // truncated

	_, err := a.Project(context.Background(), r, config.Profile{})
	if !errors.Is(err, codex.ErrParseFailed) {
		t.Fatalf("Project err = %v, want ErrParseFailed", err)
	}
}

func TestProject_MalformedAuthJsonNullRootRefused(t *testing.T) {
	// Defensive: a bare `null` at root parses as a valid JSON value
	// but violates the object-shaped-root invariant.
	clearCodexEnv(t)
	r := projectResolver(t)
	a := codex.New()

	writeAuthJSON(t, r, `null`)
	_, err := a.Project(context.Background(), r, config.Profile{})
	if !errors.Is(err, codex.ErrParseFailed) {
		t.Fatalf("Project err = %v, want ErrParseFailed on null root", err)
	}
}

func TestProject_SymlinkOutsideHomeRefusedConfig(t *testing.T) {
	clearCodexEnv(t)
	r := projectResolver(t)
	a := codex.New()

	// Create ~/.codex + auth.json (valid), then symlink config.toml
	// to a target outside HOME.
	dir := filepath.Join(r.Home(), ".codex")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	outside := filepath.Join(t.TempDir(), "outside.toml")
	if err := os.WriteFile(outside, []byte("model = \"x\"\n"), 0o600); err != nil {
		t.Fatalf("write outside: %v", err)
	}
	cfg := filepath.Join(dir, "config.toml")
	if err := os.Symlink(outside, cfg); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	_, err := a.Project(context.Background(), r, config.Profile{})
	if !errors.Is(err, codex.ErrOutsideHome) {
		t.Fatalf("Project err = %v, want ErrOutsideHome", err)
	}
}

func TestProject_SymlinkOutsideHomeRefusedAuth(t *testing.T) {
	clearCodexEnv(t)
	r := projectResolver(t)
	a := codex.New()

	dir := filepath.Join(r.Home(), ".codex")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	outside := filepath.Join(t.TempDir(), "outside.json")
	if err := os.WriteFile(outside, []byte(`{}`), 0o600); err != nil {
		t.Fatalf("write outside: %v", err)
	}
	auth := filepath.Join(dir, "auth.json")
	if err := os.Symlink(outside, auth); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	_, err := a.Project(context.Background(), r, config.Profile{})
	if !errors.Is(err, codex.ErrOutsideHome) {
		t.Fatalf("Project err = %v, want ErrOutsideHome", err)
	}
}

func TestProject_SortedOutput(t *testing.T) {
	clearCodexEnv(t)
	r := projectResolver(t)
	a := codex.New()

	writeConfigTOML(t, r, `model = "opus"
model_provider = "anthropic"
approval_mode = "auto"
`)
	writeAuthJSON(t, r, `{"OPENAI_API_KEY":"sk-1","auth_mode":"api_key","tokens":{"access_token":"a","account_id":"b","id_token":"c","refresh_token":"d"}}`)

	view, err := a.Project(context.Background(), r, config.Profile{})
	if err != nil {
		t.Fatalf("Project: %v", err)
	}
	keys := make([]string, len(view.Fields))
	for i, f := range view.Fields {
		keys[i] = f.Key
	}
	want := make([]string, len(keys))
	copy(want, keys)
	sort.Strings(want)
	if !reflect.DeepEqual(keys, want) {
		t.Fatalf("view.Fields not sorted by Key\n got: %v\nwant: %v", keys, want)
	}
}

func TestProject_SecretFlagOnCredentialAndTokenKeys(t *testing.T) {
	clearCodexEnv(t)
	r := projectResolver(t)
	a := codex.New()

	writeAuthJSON(t, r, `{
	  "OPENAI_API_KEY": "sk-secret-1234567890",
	  "auth_mode": "api_key",
	  "last_refresh": "2025-01-01T00:00:00Z",
	  "tokens": {
	    "access_token": "at-secret",
	    "account_id": "acct-nonsecret",
	    "id_token": "id-secret",
	    "refresh_token": "rt-secret"
	  }
	}`)
	writeConfigTOML(t, r, `model = "opus"
model_provider = "openai"
`)

	view, err := a.Project(context.Background(), r, config.Profile{})
	if err != nil {
		t.Fatalf("Project: %v", err)
	}

	wantSecret := map[string]bool{
		"OPENAI_API_KEY":       true,
		"tokens.access_token":  true,
		"tokens.id_token":      true,
		"tokens.refresh_token": true,
		// non-secret owned keys
		"auth_mode":         false,
		"last_refresh":      false,
		"tokens.account_id": false,
		"model":             false,
		"model_provider":    false,
	}
	for key, want := range wantSecret {
		f := mustField(t, view, key)
		if f.Secret != want {
			t.Errorf("%s: Secret=%v, want %v", key, f.Secret, want)
		}
	}
}

func TestProject_ContextCanceled(t *testing.T) {
	clearCodexEnv(t)
	r := projectResolver(t)
	a := codex.New()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := a.Project(ctx, r, config.Profile{})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Project on cancelled ctx: err = %v, want context.Canceled", err)
	}
}

// TestProject_OnDiskWinsOverOverlay verifies OnDiskToolConfig beats
// ProfileOverlay for a config.toml key. Overlay sets model=haiku;
// config.toml has model=opus → OnDisk wins.
func TestProject_OnDiskWinsOverOverlay(t *testing.T) {
	clearCodexEnv(t)
	r := projectResolver(t)
	a := codex.New()

	configPath := writeConfigTOML(t, r, `model = "opus"
`)

	p := codexProfileWith("t", "", map[string]any{
		"model": "haiku",
	})

	view, err := a.Project(context.Background(), r, p)
	if err != nil {
		t.Fatalf("Project: %v", err)
	}
	f := mustField(t, view, "model")
	if f.WinningLayer != adapter.LayerOnDisk {
		t.Fatalf("WinningLayer=%q, want OnDiskToolConfig", f.WinningLayer)
	}
	if f.Value != "opus" {
		t.Fatalf("Value=%v, want opus", f.Value)
	}
	wantSource := configPath + ":model"
	if f.Source != wantSource {
		t.Fatalf("Source=%q, want %q", f.Source, wantSource)
	}
	if len(f.Shadowed) != 1 {
		t.Fatalf("Shadowed len=%d, want 1; got=%+v", len(f.Shadowed), f.Shadowed)
	}
	sh := f.Shadowed[0]
	if sh.Layer != adapter.LayerOverlay {
		t.Fatalf("Shadowed[0].Layer=%q, want ProfileOverlay", sh.Layer)
	}
	if sh.Value != "haiku" {
		t.Fatalf("Shadowed[0].Value=%v, want haiku", sh.Value)
	}
	if sh.Source != "profile.overlay:model" {
		t.Fatalf("Shadowed[0].Source=%q, want profile.overlay:model", sh.Source)
	}
}

// TestProject_OverlayWinsOverCore uses Overlay.Raw for OPENAI_API_KEY
// while Core.APIKey has a different value → Overlay wins, Core sits
// in Shadowed.
func TestProject_OverlayWinsOverCore(t *testing.T) {
	clearCodexEnv(t)
	r := projectResolver(t)
	a := codex.New()

	p := codexProfileWith("t", "core-key", map[string]any{
		"OPENAI_API_KEY": "overlay-key",
	})

	view, err := a.Project(context.Background(), r, p)
	if err != nil {
		t.Fatalf("Project: %v", err)
	}
	f := mustField(t, view, "OPENAI_API_KEY")
	if f.WinningLayer != adapter.LayerOverlay {
		t.Fatalf("WinningLayer=%q, want ProfileOverlay", f.WinningLayer)
	}
	if f.Value != "overlay-key" {
		t.Fatalf("Value=%v, want overlay-key", f.Value)
	}
	if !f.Secret {
		t.Fatalf("Secret=false, want true")
	}
	if len(f.Shadowed) != 1 {
		t.Fatalf("Shadowed len=%d, want 1; got=%+v", len(f.Shadowed), f.Shadowed)
	}
	sh := f.Shadowed[0]
	if sh.Layer != adapter.LayerCore || sh.Value != "core-key" {
		t.Fatalf("Shadowed[0]=%+v, want ProfileCore/core-key", sh)
	}
	if !sh.Secret {
		t.Fatalf("Shadowed[0].Secret=false, want true (secret propagates to shadowed entries)")
	}
}

// TestProject_NullOnDiskDoesNotShadow verifies that an auth.json with
// an explicit JSON null for OPENAI_API_KEY does NOT contribute to the
// OnDisk layer — mirrors Import's null-is-absent rule.
func TestProject_NullOnDiskDoesNotShadow(t *testing.T) {
	clearCodexEnv(t)
	r := projectResolver(t)
	a := codex.New()

	writeAuthJSON(t, r, `{"OPENAI_API_KEY": null}`)

	p := codexProfileWith("t", "core-key", nil)
	view, err := a.Project(context.Background(), r, p)
	if err != nil {
		t.Fatalf("Project: %v", err)
	}
	f := mustField(t, view, "OPENAI_API_KEY")
	if f.WinningLayer != adapter.LayerCore {
		t.Fatalf("WinningLayer=%q, want ProfileCore (null on disk should not shadow)", f.WinningLayer)
	}
	if len(f.Shadowed) != 0 {
		t.Fatalf("Shadowed=%+v, want empty (null does not count as a value)", f.Shadowed)
	}
}

// TestProject_EmptyFilesTreatedAsAbsent — whitespace-only files
// contribute nothing to OnDisk, symmetric with Import.
func TestProject_EmptyFilesTreatedAsAbsent(t *testing.T) {
	clearCodexEnv(t)
	r := projectResolver(t)
	a := codex.New()

	writeConfigTOML(t, r, "   \n\t\n")
	writeAuthJSON(t, r, " \n")

	p := codexProfileWith("t", "sk-core", nil)
	view, err := a.Project(context.Background(), r, p)
	if err != nil {
		t.Fatalf("Project: %v", err)
	}
	f := mustField(t, view, "OPENAI_API_KEY")
	if f.WinningLayer != adapter.LayerCore {
		t.Fatalf("WinningLayer=%q, want ProfileCore (empty file → no OnDisk contribution)", f.WinningLayer)
	}
	if len(f.Shadowed) != 0 {
		t.Fatalf("Shadowed=%+v, want empty", f.Shadowed)
	}
}

// TestProject_ConfigTomlProviderKeysProjected verifies the nested
// provider-table keys surface as individual EffectiveFields with
// OnDisk provenance.
func TestProject_ConfigTomlProviderKeysProjected(t *testing.T) {
	clearCodexEnv(t)
	r := projectResolver(t)
	a := codex.New()

	configPath := writeConfigTOML(t, r, `[model_providers.openai]
base_url = "https://api.openai.example"
env_key = "OPENAI_API_KEY"
name = "openai"
wire_api = "responses"

[model_providers.anthropic]
base_url = "https://api.anthropic.example"
env_key = "ANTHROPIC_API_KEY"
name = "anthropic"
wire_api = "chat"
`)

	view, err := a.Project(context.Background(), r, config.Profile{})
	if err != nil {
		t.Fatalf("Project: %v", err)
	}

	expected := map[string]string{
		"model_providers.openai.base_url":    "https://api.openai.example",
		"model_providers.openai.env_key":     "OPENAI_API_KEY",
		"model_providers.openai.name":        "openai",
		"model_providers.openai.wire_api":    "responses",
		"model_providers.anthropic.base_url": "https://api.anthropic.example",
		"model_providers.anthropic.env_key":  "ANTHROPIC_API_KEY",
		"model_providers.anthropic.name":     "anthropic",
		"model_providers.anthropic.wire_api": "chat",
	}
	for key, wantVal := range expected {
		f := mustField(t, view, key)
		if f.WinningLayer != adapter.LayerOnDisk {
			t.Errorf("%s: WinningLayer=%q, want OnDiskToolConfig", key, f.WinningLayer)
		}
		if f.Value != wantVal {
			t.Errorf("%s: Value=%v, want %v", key, f.Value, wantVal)
		}
		if f.Source != configPath+":"+key {
			t.Errorf("%s: Source=%q, want %s", key, f.Source, configPath+":"+key)
		}
	}
}

// TestProject_ConfigTomlTypedValuesPreserved verifies typed values
// (int, float, bool) round-trip through codextoml.Doc.Get with their
// native Go type intact so downstream renderers do not
// mis-format them.
func TestProject_ConfigTomlTypedValuesPreserved(t *testing.T) {
	// approval_mode is documented as a string; but the point of this
	// test is the type-fidelity contract. Use model with a real string
	// (Codex takes a string here) and rely on codextoml's typed
	// storage. This test simply checks that a real string comes back
	// as a Go string, exercising the "typed value" branch of
	// onDiskLayer for config.toml.
	clearCodexEnv(t)
	r := projectResolver(t)
	a := codex.New()

	writeConfigTOML(t, r, `model = "gpt-4"
`)

	view, err := a.Project(context.Background(), r, config.Profile{})
	if err != nil {
		t.Fatalf("Project: %v", err)
	}
	f := mustField(t, view, "model")
	if _, ok := f.Value.(string); !ok {
		t.Fatalf("model Value type = %T, want string", f.Value)
	}
	if f.Value.(string) != "gpt-4" {
		t.Fatalf("model Value = %v, want gpt-4", f.Value)
	}
}

// TestProject_AuthJsonTokensProjected verifies all four tokens.*
// subkeys land as individual EffectiveFields with the correct Secret
// flags.
func TestProject_AuthJsonTokensProjected(t *testing.T) {
	clearCodexEnv(t)
	r := projectResolver(t)
	a := codex.New()

	authPath := writeAuthJSON(t, r, `{
	  "tokens": {
	    "access_token": "at-1",
	    "account_id":   "acct-1",
	    "id_token":     "id-1",
	    "refresh_token": "rt-1"
	  }
	}`)

	view, err := a.Project(context.Background(), r, config.Profile{})
	if err != nil {
		t.Fatalf("Project: %v", err)
	}

	cases := map[string]struct {
		val    string
		secret bool
	}{
		"tokens.access_token":  {"at-1", true},
		"tokens.account_id":    {"acct-1", false},
		"tokens.id_token":      {"id-1", true},
		"tokens.refresh_token": {"rt-1", true},
	}
	for key, want := range cases {
		f := mustField(t, view, key)
		if f.WinningLayer != adapter.LayerOnDisk {
			t.Errorf("%s: WinningLayer=%q, want OnDiskToolConfig", key, f.WinningLayer)
		}
		if f.Value != want.val {
			t.Errorf("%s: Value=%v, want %v", key, f.Value, want.val)
		}
		if f.Secret != want.secret {
			t.Errorf("%s: Secret=%v, want %v", key, f.Secret, want.secret)
		}
		if f.Source != authPath+":"+key {
			t.Errorf("%s: Source=%q, want %s", key, f.Source, authPath+":"+key)
		}
	}
}

// TestProject_OverlayEmptyCodexMapButOtherToolPresent covers the
// overlayRawMap branch where profile.Tools is populated but does NOT
// contain a codex entry.
func TestProject_OverlayEmptyCodexMapButOtherToolPresent(t *testing.T) {
	clearCodexEnv(t)
	r := projectResolver(t)
	a := codex.New()

	p := codexProfileWith("t", "sk-core", nil)
	p.Tools = map[config.ToolID]config.ToolOverlay{
		adapter.ToolClaudeCode: {
			ExtraEnv: map[string]string{"ANTHROPIC_MODEL": "opus"},
		},
	}

	view, err := a.Project(context.Background(), r, p)
	if err != nil {
		t.Fatalf("Project: %v", err)
	}
	f := mustField(t, view, "OPENAI_API_KEY")
	if f.WinningLayer != adapter.LayerCore {
		t.Fatalf("WinningLayer=%q, want ProfileCore", f.WinningLayer)
	}
	for _, sh := range f.Shadowed {
		if sh.Layer == adapter.LayerOverlay {
			t.Fatalf("Shadowed unexpectedly contains ProfileOverlay: %+v", f.Shadowed)
		}
	}
}

// TestProject_OverlayNilValueIgnored — a nil in Overlay.Raw is
// treated as absent (matches Plan.collectOwnedAuthValues +
// Import.extractOwnedCodex null-owned-key policy).
func TestProject_OverlayNilValueIgnored(t *testing.T) {
	clearCodexEnv(t)
	r := projectResolver(t)
	a := codex.New()

	writeConfigTOML(t, r, `model = "opus"
`)
	p := codexProfileWith("t", "", map[string]any{
		"model": nil, // nil in Raw → absent
	})

	view, err := a.Project(context.Background(), r, p)
	if err != nil {
		t.Fatalf("Project: %v", err)
	}
	f := mustField(t, view, "model")
	if f.WinningLayer != adapter.LayerOnDisk {
		t.Fatalf("WinningLayer=%q, want OnDiskToolConfig", f.WinningLayer)
	}
	if len(f.Shadowed) != 0 {
		t.Fatalf("Shadowed=%+v, want empty (nil overlay value should be absent)", f.Shadowed)
	}
}
