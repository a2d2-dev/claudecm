//go:build test

package claudecode_test

// project_test_hooks.go — E3-S6, migrated to the shared envextract
// seam in E5-S3. Tests here exercise layer-precedence paths that
// require injecting a synthetic env-var universe via the build-tag
// seam (envextract.SetLookupForTest). Compiled only under `-tags=test`;
// symmetric with storage.atomic_syncfunc's test hook.
//
// These tests deliberately do NOT use t.Setenv for the env layer —
// t.Setenv can only override real process env, and the goal here is
// to verify the resolver's env-var allowlist behaviour under a
// deterministic universe with no ambient noise.

import (
	"context"
	"reflect"
	"sort"
	"testing"

	"github.com/a2d2-dev/claudecm/internal/adapter"
	"github.com/a2d2-dev/claudecm/internal/adapter/claudecode"
	"github.com/a2d2-dev/claudecm/internal/config"
	"github.com/a2d2-dev/claudecm/internal/envextract"
)

// envUniverse returns a lookup shim that resolves names from the
// given map. A name is treated as present iff the map carries the
// key — this preserves the "empty string is unset" contract Claude
// Code observes at runtime (adapters call envextract.Lookup and drop
// keys where the value is ""). Passed to envextract.SetLookupForTest
// so the process env is fully insulated.
func envUniverse(m map[string]string) func(string) (string, bool) {
	return func(name string) (string, bool) {
		v, ok := m[name]
		return v, ok
	}
}

func TestProject_EnvOverrideWinsOverOnDisk(t *testing.T) {
	restore := envextract.SetLookupForTest(envUniverse(map[string]string{
		"ANTHROPIC_BASE_URL": "https://env.example",
	}))
	defer restore()

	r := projectResolver(t)
	a := claudecode.New()

	// On-disk carries the same key with a different value; profile.Core
	// carries a third value. EnvOverride must beat both, and both
	// lower layers must appear in Shadowed older→newer (Core then
	// OnDisk).
	settingsPath := writeSettings(t, r, `{"env":{"ANTHROPIC_BASE_URL":"https://disk.example"}}`)
	p := profileWithCore("t", "https://core.example", "", "", "")

	view, err := a.Project(context.Background(), r, p)
	if err != nil {
		t.Fatalf("Project: %v", err)
	}
	f := mustField(t, view, "env.ANTHROPIC_BASE_URL")
	if f.WinningLayer != adapter.LayerEnvOverride {
		t.Fatalf("WinningLayer=%q, want EnvOverride", f.WinningLayer)
	}
	if f.Value != "https://env.example" {
		t.Fatalf("Value=%v, want https://env.example", f.Value)
	}
	if f.Source != "env:ANTHROPIC_BASE_URL" {
		t.Fatalf("Source=%q, want env:ANTHROPIC_BASE_URL", f.Source)
	}

	// Shadowed carries older→newer: ProfileCore (older), then
	// OnDiskToolConfig (newer than ProfileCore but older than
	// EnvOverride).
	if len(f.Shadowed) != 2 {
		t.Fatalf("Shadowed len=%d, want 2; got=%+v", len(f.Shadowed), f.Shadowed)
	}
	if f.Shadowed[0].Layer != adapter.LayerCore || f.Shadowed[0].Value != "https://core.example" {
		t.Fatalf("Shadowed[0]=%+v, want ProfileCore/https://core.example", f.Shadowed[0])
	}
	if f.Shadowed[1].Layer != adapter.LayerOnDisk || f.Shadowed[1].Value != "https://disk.example" {
		t.Fatalf("Shadowed[1]=%+v, want OnDiskToolConfig/https://disk.example", f.Shadowed[1])
	}
	// OnDisk source must reference the actual settings.json path plus
	// the JSON pointer — otherwise `explain` cannot send an operator
	// to the file.
	wantSource := settingsPath + ":env.ANTHROPIC_BASE_URL"
	if f.Shadowed[1].Source != wantSource {
		t.Fatalf("Shadowed[1].Source=%q, want %q", f.Shadowed[1].Source, wantSource)
	}
}

func TestProject_OnDiskWinsOverOverlay(t *testing.T) {
	restore := envextract.SetLookupForTest(envUniverse(map[string]string{}))
	defer restore()

	r := projectResolver(t)
	a := claudecode.New()

	writeSettings(t, r, `{"env":{"CLAUDE_CODE_USE_BEDROCK":"1"}}`)
	p := config.Profile{
		SchemaVersion: config.CurrentProfileSchemaVersion,
		Name:          "t",
		Tools: map[config.ToolID]config.ToolOverlay{
			adapter.ToolClaudeCode: {ExtraEnv: map[string]string{"CLAUDE_CODE_USE_BEDROCK": "0"}},
		},
	}

	view, err := a.Project(context.Background(), r, p)
	if err != nil {
		t.Fatalf("Project: %v", err)
	}
	f := mustField(t, view, "env.CLAUDE_CODE_USE_BEDROCK")
	if f.WinningLayer != adapter.LayerOnDisk {
		t.Fatalf("WinningLayer=%q, want OnDiskToolConfig", f.WinningLayer)
	}
	if f.Value != "1" {
		t.Fatalf("Value=%v, want \"1\"", f.Value)
	}
	if len(f.Shadowed) != 1 {
		t.Fatalf("Shadowed len=%d, want 1; got=%+v", len(f.Shadowed), f.Shadowed)
	}
	if f.Shadowed[0].Layer != adapter.LayerOverlay || f.Shadowed[0].Value != "0" {
		t.Fatalf("Shadowed[0]=%+v, want ProfileOverlay/0", f.Shadowed[0])
	}
}

func TestProject_OverlayWinsOverCore(t *testing.T) {
	// The API_KEY / AUTH_TOKEN pair exercises both halves of the
	// overlay vs core routing: Core.APIKey routes into
	// env.ANTHROPIC_AUTH_TOKEN (per Plan mapping), while an overlay
	// ExtraEnv["ANTHROPIC_API_KEY"] routes into env.ANTHROPIC_API_KEY
	// and has no Core counterpart.
	restore := envextract.SetLookupForTest(envUniverse(map[string]string{}))
	defer restore()

	r := projectResolver(t)
	a := claudecode.New()

	p := config.Profile{
		SchemaVersion: config.CurrentProfileSchemaVersion,
		Name:          "t",
		Core:          config.CoreConfig{APIKey: "core-key"},
		Tools: map[config.ToolID]config.ToolOverlay{
			adapter.ToolClaudeCode: {ExtraEnv: map[string]string{"ANTHROPIC_API_KEY": "overlay-key"}},
		},
	}

	view, err := a.Project(context.Background(), r, p)
	if err != nil {
		t.Fatalf("Project: %v", err)
	}

	// AUTH_TOKEN: Core.APIKey wins (only Core contributes).
	auth := mustField(t, view, "env.ANTHROPIC_AUTH_TOKEN")
	if auth.WinningLayer != adapter.LayerCore {
		t.Fatalf("AUTH_TOKEN WinningLayer=%q, want ProfileCore", auth.WinningLayer)
	}
	if auth.Value != "core-key" {
		t.Fatalf("AUTH_TOKEN Value=%v, want core-key", auth.Value)
	}
	if !auth.Secret {
		t.Fatalf("AUTH_TOKEN Secret=false, want true")
	}

	// API_KEY: Overlay wins (only Overlay contributes to this
	// overlay-only key).
	api := mustField(t, view, "env.ANTHROPIC_API_KEY")
	if api.WinningLayer != adapter.LayerOverlay {
		t.Fatalf("API_KEY WinningLayer=%q, want ProfileOverlay", api.WinningLayer)
	}
	if api.Value != "overlay-key" {
		t.Fatalf("API_KEY Value=%v, want overlay-key", api.Value)
	}
	if !api.Secret {
		t.Fatalf("API_KEY Secret=false, want true")
	}
}

func TestProject_AllFiveLayers(t *testing.T) {
	// Construct a scenario where OnDisk, Overlay, and Core all
	// contribute env.ANTHROPIC_MODEL, plus an env var wins. Since
	// v1 has no BuiltInDefault for any owned key, the shadowed set
	// carries three entries: Core → Overlay → OnDisk (older→newer).
	restore := envextract.SetLookupForTest(envUniverse(map[string]string{
		"ANTHROPIC_MODEL": "env-model",
	}))
	defer restore()

	r := projectResolver(t)
	a := claudecode.New()

	writeSettings(t, r, `{"env":{"ANTHROPIC_MODEL":"disk-model"}}`)
	p := config.Profile{
		SchemaVersion: config.CurrentProfileSchemaVersion,
		Name:          "t",
		Core:          config.CoreConfig{Model: "core-model"},
		Tools: map[config.ToolID]config.ToolOverlay{
			adapter.ToolClaudeCode: {ExtraEnv: map[string]string{"ANTHROPIC_MODEL": "overlay-model"}},
		},
	}

	view, err := a.Project(context.Background(), r, p)
	if err != nil {
		t.Fatalf("Project: %v", err)
	}
	f := mustField(t, view, "env.ANTHROPIC_MODEL")
	if f.WinningLayer != adapter.LayerEnvOverride {
		t.Fatalf("WinningLayer=%q, want EnvOverride", f.WinningLayer)
	}
	if f.Value != "env-model" {
		t.Fatalf("Value=%v, want env-model", f.Value)
	}
	if len(f.Shadowed) != 3 {
		t.Fatalf("Shadowed len=%d, want 3; got=%+v", len(f.Shadowed), f.Shadowed)
	}
	wantLayers := []adapter.Layer{adapter.LayerCore, adapter.LayerOverlay, adapter.LayerOnDisk}
	wantVals := []string{"core-model", "overlay-model", "disk-model"}
	gotLayers := make([]adapter.Layer, len(f.Shadowed))
	gotVals := make([]string, len(f.Shadowed))
	for i, sh := range f.Shadowed {
		gotLayers[i] = sh.Layer
		gotVals[i] = sh.Value.(string)
	}
	if !reflect.DeepEqual(gotLayers, wantLayers) {
		t.Fatalf("Shadowed layers=%v, want %v", gotLayers, wantLayers)
	}
	if !reflect.DeepEqual(gotVals, wantVals) {
		t.Fatalf("Shadowed values=%v, want %v", gotVals, wantVals)
	}
}

// TestProject_EnvEmptyStringIgnored verifies that an env var set to
// "" does not count as an EnvOverride contribution — Claude Code
// itself would not consume it, and the resolver mirrors that.
func TestProject_EnvEmptyStringIgnored(t *testing.T) {
	restore := envextract.SetLookupForTest(envUniverse(map[string]string{
		"ANTHROPIC_MODEL": "",
	}))
	defer restore()

	r := projectResolver(t)
	a := claudecode.New()

	p := config.Profile{
		SchemaVersion: config.CurrentProfileSchemaVersion,
		Name:          "t",
		Core:          config.CoreConfig{Model: "core-model"},
	}
	view, err := a.Project(context.Background(), r, p)
	if err != nil {
		t.Fatalf("Project: %v", err)
	}
	f := mustField(t, view, "env.ANTHROPIC_MODEL")
	if f.WinningLayer != adapter.LayerCore {
		t.Fatalf("WinningLayer=%q, want ProfileCore (empty env should not shadow)", f.WinningLayer)
	}
	if len(f.Shadowed) != 0 {
		t.Fatalf("Shadowed=%+v, want empty", f.Shadowed)
	}
}

// TestProject_EnvAllowlistOnly verifies that env vars OUTSIDE the
// per-tool allowlist are ignored even if set. Sets a bogus var that
// matches no owned key and confirms the view is unchanged.
func TestProject_EnvAllowlistOnly(t *testing.T) {
	restore := envextract.SetLookupForTest(envUniverse(map[string]string{
		"ANTHROPIC_MODEL":       "env-model",
		"SOME_OTHER_VAR":        "should-not-leak",
		"CLAUDE_UNRELATED_KNOB": "should-not-leak",
	}))
	defer restore()

	r := projectResolver(t)
	a := claudecode.New()

	view, err := a.Project(context.Background(), r, config.Profile{})
	if err != nil {
		t.Fatalf("Project: %v", err)
	}

	// Only ANTHROPIC_MODEL should be surfaced; no key named
	// SOME_OTHER_VAR / CLAUDE_UNRELATED_KNOB should exist.
	if _, ok := projectFieldByKey(view, "env.ANTHROPIC_MODEL"); !ok {
		t.Fatalf("view missing env.ANTHROPIC_MODEL")
	}
	for _, f := range view.Fields {
		if f.Key == "env.SOME_OTHER_VAR" || f.Key == "env.CLAUDE_UNRELATED_KNOB" {
			t.Fatalf("view unexpectedly contains non-allowlisted key %q", f.Key)
		}
	}
	// Sanity: keys are still sorted.
	keys := make([]string, len(view.Fields))
	for i, f := range view.Fields {
		keys[i] = f.Key
	}
	want := make([]string, len(keys))
	copy(want, keys)
	sort.Strings(want)
	if !reflect.DeepEqual(keys, want) {
		t.Fatalf("view keys not sorted: got=%v want=%v", keys, want)
	}
}
