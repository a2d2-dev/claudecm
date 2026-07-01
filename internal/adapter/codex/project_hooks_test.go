//go:build test

package codex_test

// project_hooks_test.go — E4-S6. Tests here exercise layer-precedence
// paths that require injecting a synthetic env-var universe via the
// build-tag seam (SetLookupEnvForTest). Compiled only under
// `-tags=test`; symmetric with claudecode/project_hooks_test.go.

import (
	"context"
	"testing"

	"github.com/a2d2-dev/claudecm/internal/adapter"
	"github.com/a2d2-dev/claudecm/internal/adapter/codex"
	"github.com/a2d2-dev/claudecm/internal/config"
)

// envUniverse returns a lookupEnv shim that resolves names from the
// given map and returns "" for anything else.
func envUniverse(m map[string]string) func(string) string {
	return func(name string) string { return m[name] }
}

// TestProject_EnvOverrideWinsOverOnDisk_OPENAI_API_KEY exercises the
// full four-layer chain for the credential slot. env has one value,
// auth.json a second, profile.Core a third → EnvOverride wins with
// Shadowed = [Core, OnDisk] older→newer.
func TestProject_EnvOverrideWinsOverOnDisk_OPENAI_API_KEY(t *testing.T) {
	restore := codex.SetLookupEnvForTest(envUniverse(map[string]string{
		"OPENAI_API_KEY": "env-key",
	}))
	defer restore()

	r := projectResolver(t)
	a := codex.New()

	authPath := writeAuthJSON(t, r, `{"OPENAI_API_KEY":"disk-key"}`)
	p := codexProfileWith("t", "core-key", nil)

	view, err := a.Project(context.Background(), r, p)
	if err != nil {
		t.Fatalf("Project: %v", err)
	}
	f := mustField(t, view, "OPENAI_API_KEY")
	if f.WinningLayer != adapter.LayerEnvOverride {
		t.Fatalf("WinningLayer=%q, want EnvOverride", f.WinningLayer)
	}
	if f.Value != "env-key" {
		t.Fatalf("Value=%v, want env-key", f.Value)
	}
	if f.Source != "env:OPENAI_API_KEY" {
		t.Fatalf("Source=%q, want env:OPENAI_API_KEY", f.Source)
	}
	if !f.Secret {
		t.Fatalf("Secret=false, want true")
	}

	if len(f.Shadowed) != 2 {
		t.Fatalf("Shadowed len=%d, want 2; got=%+v", len(f.Shadowed), f.Shadowed)
	}
	if f.Shadowed[0].Layer != adapter.LayerCore || f.Shadowed[0].Value != "core-key" {
		t.Fatalf("Shadowed[0]=%+v, want ProfileCore/core-key", f.Shadowed[0])
	}
	if f.Shadowed[1].Layer != adapter.LayerOnDisk || f.Shadowed[1].Value != "disk-key" {
		t.Fatalf("Shadowed[1]=%+v, want OnDiskToolConfig/disk-key", f.Shadowed[1])
	}
	wantSource := authPath + ":OPENAI_API_KEY"
	if f.Shadowed[1].Source != wantSource {
		t.Fatalf("Shadowed[1].Source=%q, want %q", f.Shadowed[1].Source, wantSource)
	}
	// Secret propagates to shadowed entries.
	for i, sh := range f.Shadowed {
		if !sh.Secret {
			t.Fatalf("Shadowed[%d].Secret=false, want true", i)
		}
	}
}

// TestProject_EnvNotAllowlistedIgnored — env vars OUTSIDE the codex
// allowlist must not surface. CODEX_HOME is deliberately reachable
// through the shim; it is a real Codex env var but does not shadow
// any owned key value.
func TestProject_EnvNotAllowlistedIgnored(t *testing.T) {
	restore := codex.SetLookupEnvForTest(envUniverse(map[string]string{
		"OPENAI_API_KEY": "env-key",
		"CODEX_HOME":     "/should-not-leak",
		"UNRELATED_KNOB": "should-not-leak",
		"OPENAI_ORG":     "org-should-not-leak",
	}))
	defer restore()

	r := projectResolver(t)
	a := codex.New()

	view, err := a.Project(context.Background(), r, config.Profile{})
	if err != nil {
		t.Fatalf("Project: %v", err)
	}
	// OPENAI_API_KEY should be present (env wins).
	f := mustField(t, view, "OPENAI_API_KEY")
	if f.WinningLayer != adapter.LayerEnvOverride {
		t.Fatalf("WinningLayer=%q, want EnvOverride", f.WinningLayer)
	}
	// No key named CODEX_HOME / UNRELATED_KNOB / OPENAI_ORG should
	// appear.
	for _, ff := range view.Fields {
		if ff.Key == "CODEX_HOME" || ff.Key == "UNRELATED_KNOB" || ff.Key == "OPENAI_ORG" {
			t.Fatalf("view unexpectedly contains non-allowlisted key %q", ff.Key)
		}
	}
}

// TestProject_EnvShadowingConfigTomlKey — CODEX_MODEL shadows the
// config.toml `model` key. Deep test: on-disk model="opus", overlay
// model="haiku", env CODEX_MODEL="sonnet" → env wins, shadowed set
// carries both older layers.
func TestProject_EnvShadowingConfigTomlKey(t *testing.T) {
	restore := codex.SetLookupEnvForTest(envUniverse(map[string]string{
		"CODEX_MODEL": "sonnet",
	}))
	defer restore()

	r := projectResolver(t)
	a := codex.New()

	writeConfigTOML(t, r, `model = "opus"
`)
	p := codexProfileWith("t", "", map[string]any{
		"model": "haiku",
	})

	view, err := a.Project(context.Background(), r, p)
	if err != nil {
		t.Fatalf("Project: %v", err)
	}
	f := mustField(t, view, "model")
	if f.WinningLayer != adapter.LayerEnvOverride {
		t.Fatalf("WinningLayer=%q, want EnvOverride", f.WinningLayer)
	}
	if f.Value != "sonnet" {
		t.Fatalf("Value=%v, want sonnet", f.Value)
	}
	if f.Source != "env:CODEX_MODEL" {
		t.Fatalf("Source=%q, want env:CODEX_MODEL", f.Source)
	}
	if len(f.Shadowed) != 2 {
		t.Fatalf("Shadowed len=%d, want 2; got=%+v", len(f.Shadowed), f.Shadowed)
	}
	if f.Shadowed[0].Layer != adapter.LayerOverlay || f.Shadowed[0].Value != "haiku" {
		t.Fatalf("Shadowed[0]=%+v, want ProfileOverlay/haiku", f.Shadowed[0])
	}
	if f.Shadowed[1].Layer != adapter.LayerOnDisk || f.Shadowed[1].Value != "opus" {
		t.Fatalf("Shadowed[1]=%+v, want OnDiskToolConfig/opus", f.Shadowed[1])
	}
}

// TestProject_EnvShadowingOpenAIBaseURL — OPENAI_BASE_URL shadows
// model_providers.openai.base_url specifically (a nested config.toml
// key). Verifies the env-var → owned-key mapping picks the correct
// dotted path.
func TestProject_EnvShadowingOpenAIBaseURL(t *testing.T) {
	restore := codex.SetLookupEnvForTest(envUniverse(map[string]string{
		"OPENAI_BASE_URL": "https://env.openai.example",
	}))
	defer restore()

	r := projectResolver(t)
	a := codex.New()

	writeConfigTOML(t, r, `[model_providers.openai]
base_url = "https://disk.openai.example"
`)

	view, err := a.Project(context.Background(), r, config.Profile{})
	if err != nil {
		t.Fatalf("Project: %v", err)
	}
	f := mustField(t, view, "model_providers.openai.base_url")
	if f.WinningLayer != adapter.LayerEnvOverride {
		t.Fatalf("WinningLayer=%q, want EnvOverride", f.WinningLayer)
	}
	if f.Value != "https://env.openai.example" {
		t.Fatalf("Value=%v, want https://env.openai.example", f.Value)
	}
	if len(f.Shadowed) != 1 {
		t.Fatalf("Shadowed len=%d, want 1; got=%+v", len(f.Shadowed), f.Shadowed)
	}
	if f.Shadowed[0].Layer != adapter.LayerOnDisk || f.Shadowed[0].Value != "https://disk.openai.example" {
		t.Fatalf("Shadowed[0]=%+v, want OnDiskToolConfig/https://disk.openai.example", f.Shadowed[0])
	}
}

// TestProject_EnvEmptyStringIgnored — an env var set to "" does not
// count as an EnvOverride contribution.
func TestProject_EnvEmptyStringIgnored(t *testing.T) {
	restore := codex.SetLookupEnvForTest(envUniverse(map[string]string{
		"CODEX_MODEL": "",
	}))
	defer restore()

	r := projectResolver(t)
	a := codex.New()

	p := codexProfileWith("t", "", map[string]any{
		"model": "opus",
	})
	view, err := a.Project(context.Background(), r, p)
	if err != nil {
		t.Fatalf("Project: %v", err)
	}
	f := mustField(t, view, "model")
	if f.WinningLayer != adapter.LayerOverlay {
		t.Fatalf("WinningLayer=%q, want ProfileOverlay (empty env should not shadow)", f.WinningLayer)
	}
	if len(f.Shadowed) != 0 {
		t.Fatalf("Shadowed=%+v, want empty", f.Shadowed)
	}
}

// TestProject_TokenFieldsFromAuthJson — all four tokens.* subkeys
// surface as OnDisk-layer entries with correct Secret flags. Under
// -tags=test so env is fully insulated.
func TestProject_TokenFieldsFromAuthJson(t *testing.T) {
	restore := codex.SetLookupEnvForTest(envUniverse(map[string]string{}))
	defer restore()

	r := projectResolver(t)
	a := codex.New()

	authPath := writeAuthJSON(t, r, `{
	  "auth_mode": "oauth",
	  "last_refresh": "2025-01-01T00:00:00Z",
	  "tokens": {
	    "access_token": "at-x",
	    "account_id":   "acct-x",
	    "id_token":     "id-x",
	    "refresh_token": "rt-x"
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
		"auth_mode":            {"oauth", false},
		"last_refresh":         {"2025-01-01T00:00:00Z", false},
		"tokens.access_token":  {"at-x", true},
		"tokens.account_id":    {"acct-x", false},
		"tokens.id_token":      {"id-x", true},
		"tokens.refresh_token": {"rt-x", true},
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
