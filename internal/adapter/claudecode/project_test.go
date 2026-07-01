package claudecode_test

// project_test.go — E3-S6. Untagged tests exercise the Project surface
// through the exported adapter, using t.Setenv to keep the process env
// under test control and per-test HOME directories so on-disk state is
// isolated. Layer-precedence tests that require injecting a synthetic
// env-var universe live under `//go:build test` in
// project_test_hooks.go.

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"testing"

	"github.com/a2d2-dev/claudecm/internal/adapter"
	"github.com/a2d2-dev/claudecm/internal/adapter/claudecode"
	"github.com/a2d2-dev/claudecm/internal/config"
	"github.com/a2d2-dev/claudecm/internal/storage"
)

// clearClaudeEnv wipes every env var the claudecode env-allowlist
// covers so an ambient developer env cannot leak into a table-driven
// test. Uses t.Setenv, which restores the previous value at test end.
func clearClaudeEnv(t *testing.T) {
	t.Helper()
	for _, name := range []string{
		"ANTHROPIC_API_KEY",
		"ANTHROPIC_AUTH_TOKEN",
		"ANTHROPIC_BASE_URL",
		"ANTHROPIC_MODEL",
		"ANTHROPIC_SMALL_FAST_MODEL",
		"CLAUDE_CODE_USE_BEDROCK",
		"CLAUDE_CODE_USE_VERTEX",
	} {
		t.Setenv(name, "")
	}
}

// projectFieldByKey pulls the EffectiveField whose Key matches out of
// a view. Returns (field, true) on hit, zero-value + false on miss so
// tests can assert absence without a panic.
func projectFieldByKey(view adapter.EffectiveView, key string) (adapter.EffectiveField, bool) {
	for _, f := range view.Fields {
		if f.Key == key {
			return f, true
		}
	}
	return adapter.EffectiveField{}, false
}

// mustField asserts that a key is present in the view and fails the
// test otherwise. Returns the field so the caller can chain
// assertions.
func mustField(t *testing.T, view adapter.EffectiveView, key string) adapter.EffectiveField {
	t.Helper()
	f, ok := projectFieldByKey(view, key)
	if !ok {
		t.Fatalf("view missing owned key %q; fields=%+v", key, view.Fields)
	}
	return f
}

// (writeSettings is provided by import_test.go in this same _test
// package. It takes a string body and writes ~/.claude/settings.json
// under the resolver's HOME.)

// projectResolver builds a Resolver anchored at a per-test HOME. Named
// differently from adapter_test.go's newResolver only because Go's
// same-package tests share the same file-level scope.
func projectResolver(t *testing.T) *storage.Resolver {
	t.Helper()
	r, err := storage.NewResolverWithHome(t.TempDir())
	if err != nil {
		t.Fatalf("storage.NewResolverWithHome: %v", err)
	}
	return r
}

// profileWithCore builds a Profile whose Core carries the four
// Core-routed slots. Overlay is left nil so tests that want overlay
// contribution add it explicitly.
func profileWithCore(name, baseURL, apiKey, model, smallFast string) config.Profile {
	return config.Profile{
		SchemaVersion: config.CurrentProfileSchemaVersion,
		Name:          name,
		Core: config.CoreConfig{
			BaseURL:        baseURL,
			APIKey:         apiKey,
			Model:          model,
			SmallFastModel: smallFast,
		},
	}
}

func TestProject_NoOverridesAllFromProfile(t *testing.T) {
	clearClaudeEnv(t)
	r := projectResolver(t)
	a := claudecode.New()

	p := profileWithCore("test", "https://api.example.com", "sk-core-secret", "opus", "haiku")

	view, err := a.Project(context.Background(), r, p)
	if err != nil {
		t.Fatalf("Project: %v", err)
	}
	if view.Tool != adapter.ToolClaudeCode {
		t.Fatalf("view.Tool = %q, want %q", view.Tool, adapter.ToolClaudeCode)
	}

	// Every Core-routed key must resolve into LayerCore with source
	// "profile.core" and no shadowed layers.
	cases := map[string]string{
		"env.ANTHROPIC_BASE_URL":         "https://api.example.com",
		"env.ANTHROPIC_AUTH_TOKEN":       "sk-core-secret",
		"env.ANTHROPIC_MODEL":            "opus",
		"env.ANTHROPIC_SMALL_FAST_MODEL": "haiku",
	}
	for key, wantVal := range cases {
		f := mustField(t, view, key)
		if f.WinningLayer != adapter.LayerCore {
			t.Errorf("%s: WinningLayer=%q, want ProfileCore", key, f.WinningLayer)
		}
		if f.Source != "profile.core" {
			t.Errorf("%s: Source=%q, want %q", key, f.Source, "profile.core")
		}
		if f.Value != wantVal {
			t.Errorf("%s: Value=%v, want %v", key, f.Value, wantVal)
		}
		if len(f.Shadowed) != 0 {
			t.Errorf("%s: Shadowed=%+v, want empty", key, f.Shadowed)
		}
	}

	// Overlay-only keys with no overlay contribution must be absent.
	for _, key := range []string{
		"env.ANTHROPIC_API_KEY",
		"env.CLAUDE_CODE_USE_BEDROCK",
		"env.CLAUDE_CODE_USE_VERTEX",
	} {
		if _, ok := projectFieldByKey(view, key); ok {
			t.Errorf("view unexpectedly contains %q", key)
		}
	}
}

func TestProject_MissingSettingsJsonEmptyOnDisk(t *testing.T) {
	clearClaudeEnv(t)
	r := projectResolver(t)
	a := claudecode.New()

	// No writeSettings call; ~/.claude does not exist at all.
	p := profileWithCore("t", "https://on.example", "core-key", "sonnet", "")

	view, err := a.Project(context.Background(), r, p)
	if err != nil {
		t.Fatalf("Project: %v", err)
	}
	// BASE_URL should win from Core with no shadowed layers — proves
	// that a missing on-disk file contributes nothing to OnDisk layer.
	f := mustField(t, view, "env.ANTHROPIC_BASE_URL")
	if f.WinningLayer != adapter.LayerCore {
		t.Fatalf("WinningLayer=%q, want ProfileCore", f.WinningLayer)
	}
	if len(f.Shadowed) != 0 {
		t.Fatalf("Shadowed=%+v, want empty (no on-disk file)", f.Shadowed)
	}
}

func TestProject_MalformedSettingsJsonRefused(t *testing.T) {
	clearClaudeEnv(t)
	r := projectResolver(t)
	a := claudecode.New()

	writeSettings(t, r, `{"env":`) // truncated JSON

	p := profileWithCore("t", "https://x", "k", "", "")
	_, err := a.Project(context.Background(), r, p)
	if err == nil {
		t.Fatalf("Project: err = nil, want ErrParseFailed")
	}
	if !errors.Is(err, claudecode.ErrParseFailed) {
		t.Fatalf("Project err = %v, want errors.Is(err, ErrParseFailed)", err)
	}
}

func TestProject_MalformedSettingsJsonNullRootRefused(t *testing.T) {
	// Defensive: gjson.ValidBytes accepts a bare `null` at root, but
	// Project must refuse it — overlay-as-truth only makes sense over
	// an object. Symmetric with plan.renderSettings's check.
	clearClaudeEnv(t)
	r := projectResolver(t)
	a := claudecode.New()

	writeSettings(t, r, `null`)

	p := profileWithCore("t", "https://x", "k", "", "")
	_, err := a.Project(context.Background(), r, p)
	if !errors.Is(err, claudecode.ErrParseFailed) {
		t.Fatalf("Project err = %v, want ErrParseFailed on null root", err)
	}
}

func TestProject_SymlinkOutsideHomeRefused(t *testing.T) {
	clearClaudeEnv(t)
	r := projectResolver(t)
	a := claudecode.New()

	// Create the ~/.claude dir, then a symlink from settings.json to
	// a file outside HOME. Import's verifyReadTargetInHome is what
	// enforces this; Project must inherit the same check.
	dir := filepath.Join(r.Home(), ".claude")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	outside := filepath.Join(t.TempDir(), "outside.json")
	if err := os.WriteFile(outside, []byte(`{}`), 0o600); err != nil {
		t.Fatalf("write outside: %v", err)
	}
	settings := filepath.Join(dir, "settings.json")
	if err := os.Symlink(outside, settings); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	p := profileWithCore("t", "https://x", "k", "", "")
	_, err := a.Project(context.Background(), r, p)
	if !errors.Is(err, claudecode.ErrOutsideHome) {
		t.Fatalf("Project err = %v, want ErrOutsideHome", err)
	}
}

func TestProject_SortedOutput(t *testing.T) {
	clearClaudeEnv(t)
	r := projectResolver(t)
	a := claudecode.New()

	// Populate several slots so we get a multi-field view whose sort
	// order is observably deterministic.
	writeSettings(t, r, `{
	  "env": {
	    "CLAUDE_CODE_USE_BEDROCK": "1"
	  }
	}`)
	p := profileWithCore("t", "https://z.example", "sk-token", "opus", "haiku")
	view, err := a.Project(context.Background(), r, p)
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

func TestProject_SecretFlagOnCredentialKeys(t *testing.T) {
	clearClaudeEnv(t)
	r := projectResolver(t)
	a := claudecode.New()

	// Populate every owned key so we can assert Secret on all of them.
	writeSettings(t, r, `{
	  "env": {
	    "ANTHROPIC_API_KEY": "sk-api-key-1234567890abcdef",
	    "ANTHROPIC_AUTH_TOKEN": "sk-auth-token-1234567890abcdef",
	    "ANTHROPIC_BASE_URL": "https://on.example",
	    "ANTHROPIC_MODEL": "opus",
	    "ANTHROPIC_SMALL_FAST_MODEL": "haiku",
	    "CLAUDE_CODE_USE_BEDROCK": "1",
	    "CLAUDE_CODE_USE_VERTEX": "0"
	  }
	}`)
	view, err := a.Project(context.Background(), r, config.Profile{})
	if err != nil {
		t.Fatalf("Project: %v", err)
	}

	wantSecret := map[string]bool{
		"env.ANTHROPIC_API_KEY":          true,
		"env.ANTHROPIC_AUTH_TOKEN":       true,
		"env.ANTHROPIC_BASE_URL":         false,
		"env.ANTHROPIC_MODEL":            false,
		"env.ANTHROPIC_SMALL_FAST_MODEL": false,
		"env.CLAUDE_CODE_USE_BEDROCK":    false,
		"env.CLAUDE_CODE_USE_VERTEX":     false,
	}
	for key, want := range wantSecret {
		f := mustField(t, view, key)
		if f.Secret != want {
			t.Errorf("%s: Secret=%v, want %v", key, f.Secret, want)
		}
	}
}

func TestProject_ContextCanceled(t *testing.T) {
	clearClaudeEnv(t)
	r := projectResolver(t)
	a := claudecode.New()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := a.Project(ctx, r, config.Profile{})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Project on cancelled ctx: err = %v, want context.Canceled", err)
	}
}

// TestProject_OnDiskShadowsProfileCore verifies that when both
// ProfileCore and OnDiskToolConfig contribute a value, OnDisk wins
// and Core is recorded in Shadowed. Untagged because it uses
// t.Setenv to clear the process env — no build-tag seam needed for
// the OnDisk vs Core precedence pair.
func TestProject_OnDiskShadowsProfileCore(t *testing.T) {
	clearClaudeEnv(t)
	r := projectResolver(t)
	a := claudecode.New()

	writeSettings(t, r, `{"env":{"ANTHROPIC_BASE_URL":"https://disk.example"}}`)

	p := profileWithCore("t", "https://core.example", "", "", "")
	view, err := a.Project(context.Background(), r, p)
	if err != nil {
		t.Fatalf("Project: %v", err)
	}
	f := mustField(t, view, "env.ANTHROPIC_BASE_URL")
	if f.WinningLayer != adapter.LayerOnDisk {
		t.Fatalf("WinningLayer=%q, want OnDiskToolConfig", f.WinningLayer)
	}
	if f.Value != "https://disk.example" {
		t.Fatalf("Value=%v, want https://disk.example", f.Value)
	}
	if len(f.Shadowed) != 1 {
		t.Fatalf("Shadowed len=%d, want 1; got=%+v", len(f.Shadowed), f.Shadowed)
	}
	sh := f.Shadowed[0]
	if sh.Layer != adapter.LayerCore {
		t.Fatalf("Shadowed[0].Layer=%q, want ProfileCore", sh.Layer)
	}
	if sh.Value != "https://core.example" {
		t.Fatalf("Shadowed[0].Value=%v, want https://core.example", sh.Value)
	}
	if sh.Source != "profile.core" {
		t.Fatalf("Shadowed[0].Source=%q, want profile.core", sh.Source)
	}
}

// TestProject_NullOnDiskDoesNotShadow verifies that a settings.json
// with an explicit JSON null for an owned key does NOT contribute to
// the OnDisk layer — mirrors Import's null-is-absent rule.
func TestProject_NullOnDiskDoesNotShadow(t *testing.T) {
	clearClaudeEnv(t)
	r := projectResolver(t)
	a := claudecode.New()

	writeSettings(t, r, `{"env":{"ANTHROPIC_MODEL":null}}`)

	p := profileWithCore("t", "", "", "opus", "")
	view, err := a.Project(context.Background(), r, p)
	if err != nil {
		t.Fatalf("Project: %v", err)
	}
	f := mustField(t, view, "env.ANTHROPIC_MODEL")
	if f.WinningLayer != adapter.LayerCore {
		t.Fatalf("WinningLayer=%q, want ProfileCore (null on disk should not shadow)", f.WinningLayer)
	}
	if len(f.Shadowed) != 0 {
		t.Fatalf("Shadowed=%+v, want empty (null does not count as a value)", f.Shadowed)
	}
}

// TestProject_EmptyOnDiskTreatedAsEmptyObject exercises the shared
// treatAsEmpty predicate: a whitespace-only settings.json must be
// interpreted as `{}` for the OnDisk layer, symmetric with Import.
func TestProject_EmptyOnDiskTreatedAsEmptyObject(t *testing.T) {
	clearClaudeEnv(t)
	r := projectResolver(t)
	a := claudecode.New()

	writeSettings(t, r, "   \n\t\n")

	p := profileWithCore("t", "https://core.example", "", "", "")
	view, err := a.Project(context.Background(), r, p)
	if err != nil {
		t.Fatalf("Project: %v", err)
	}
	f := mustField(t, view, "env.ANTHROPIC_BASE_URL")
	if f.WinningLayer != adapter.LayerCore {
		t.Fatalf("WinningLayer=%q, want ProfileCore (empty file → no OnDisk contribution)", f.WinningLayer)
	}
	if len(f.Shadowed) != 0 {
		t.Fatalf("Shadowed=%+v, want empty", f.Shadowed)
	}
}

// TestProject_OverlayShadowsCore uses Overlay.ExtraEnv on an
// overlay-only key while asserting the same Overlay carries no effect
// on a Core-routed key it does not touch. Overlay for env.ANTHROPIC_MODEL
// via ExtraEnv should win over Core.Model.
func TestProject_OverlayShadowsCore(t *testing.T) {
	clearClaudeEnv(t)
	r := projectResolver(t)
	a := claudecode.New()

	p := profileWithCore("t", "", "", "opus-core", "")
	p.Tools = map[config.ToolID]config.ToolOverlay{
		adapter.ToolClaudeCode: {
			ExtraEnv: map[string]string{"ANTHROPIC_MODEL": "opus-overlay"},
		},
	}

	view, err := a.Project(context.Background(), r, p)
	if err != nil {
		t.Fatalf("Project: %v", err)
	}
	f := mustField(t, view, "env.ANTHROPIC_MODEL")
	if f.WinningLayer != adapter.LayerOverlay {
		t.Fatalf("WinningLayer=%q, want ProfileOverlay", f.WinningLayer)
	}
	if f.Value != "opus-overlay" {
		t.Fatalf("Value=%v, want opus-overlay", f.Value)
	}
	if len(f.Shadowed) != 1 || f.Shadowed[0].Layer != adapter.LayerCore || f.Shadowed[0].Value != "opus-core" {
		t.Fatalf("Shadowed=%+v, want single ProfileCore=opus-core", f.Shadowed)
	}
}

// TestProject_OverlayEmptyStringIsRealValue verifies that Overlay's
// "" is a real value (not "absent") — symmetric with Plan's rule.
func TestProject_OverlayEmptyStringIsRealValue(t *testing.T) {
	clearClaudeEnv(t)
	r := projectResolver(t)
	a := claudecode.New()

	p := profileWithCore("t", "", "", "opus-core", "")
	p.Tools = map[config.ToolID]config.ToolOverlay{
		adapter.ToolClaudeCode: {
			ExtraEnv: map[string]string{"ANTHROPIC_MODEL": ""},
		},
	}
	view, err := a.Project(context.Background(), r, p)
	if err != nil {
		t.Fatalf("Project: %v", err)
	}
	f := mustField(t, view, "env.ANTHROPIC_MODEL")
	if f.WinningLayer != adapter.LayerOverlay {
		t.Fatalf("WinningLayer=%q, want ProfileOverlay", f.WinningLayer)
	}
	if f.Value != "" {
		t.Fatalf("Value=%v, want empty string", f.Value)
	}
}
