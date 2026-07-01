package codex_test

// import_test.go — E4-S3. Exercise Adapter.Import via the exported
// surface, using a per-test HOME (t.TempDir → NewResolverWithHome)
// so each case owns its own ~/.codex tree. Tests live in the _test
// package to mirror the shape cmd/* will consume in E5+.
//
// Two-file coordination coverage. Codex Import reads BOTH
// ~/.codex/config.toml and ~/.codex/auth.json; each half is
// independently optional. Every combination is exercised:
//
//	config-missing + auth-missing         → ErrNoConfig
//	config-present + auth-missing         → happy (Overlay only)
//	config-missing + auth-present         → happy (Core.APIKey + Overlay)
//	config-present + auth-present         → happy (full extraction)
//	config-malformed                      → ErrParseFailed
//	auth-malformed                        → ErrParseFailed
//	config-empty / auth-empty             → treated as absent
//	config-whitespace / auth-whitespace   → treated as absent
//	symlink-in-HOME / symlink-out-of-HOME → follow / ErrOutsideHome
//	OPENAI_API_KEY null / empty-string    → null-safety + empty-preserve
//	pre-canceled ctx                      → ctx.Err() early

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/a2d2-dev/claudecm/internal/adapter"
	"github.com/a2d2-dev/claudecm/internal/adapter/codex"
	codextoml "github.com/a2d2-dev/claudecm/internal/adapter/codex/toml"
	"github.com/a2d2-dev/claudecm/internal/storage"
	"github.com/a2d2-dev/claudecm/internal/writepath"
)

// codexDir returns ~/.codex under the resolver HOME, creating it if
// absent. Tests that seed either file need the directory in place.
func codexDir(t *testing.T, r *storage.Resolver) string {
	t.Helper()
	dir := filepath.Join(r.Home(), ".codex")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir .codex: %v", err)
	}
	return dir
}

// writeConfigTOML seeds ~/.codex/config.toml with body at 0600.
func writeConfigTOML(t *testing.T, r *storage.Resolver, body string) string {
	t.Helper()
	codexDir(t, r)
	path := codex.ConfigPath(r)
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write config.toml: %v", err)
	}
	return path
}

// writeAuthJSON seeds ~/.codex/auth.json with body at 0600.
func writeAuthJSON(t *testing.T, r *storage.Resolver, body string) string {
	t.Helper()
	codexDir(t, r)
	path := codex.AuthPath(r)
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write auth.json: %v", err)
	}
	return path
}

// runImport is a shorthand for the (adapter, ctx, resolver) triple.
func runImport(t *testing.T, r *storage.Resolver) (adapter.CoreFromTool, adapter.OverlayFromTool, error) {
	t.Helper()
	return codex.New().Import(context.Background(), r)
}

// isZeroCore reports whether core is the zero value.
func isZeroCore(core adapter.CoreFromTool) bool {
	return core.Provider == "" &&
		core.BaseURL == "" &&
		core.APIKey == "" &&
		core.Model == "" &&
		core.SmallFastModel == "" &&
		core.ExtraEnv == nil
}

// isZeroOverlay reports whether overlay is the zero value.
func isZeroOverlay(overlay adapter.OverlayFromTool) bool {
	return overlay.BaseURL == "" &&
		overlay.APIKey == "" &&
		overlay.Model == "" &&
		overlay.SmallFastModel == "" &&
		overlay.ExtraEnv == nil &&
		overlay.Raw == nil
}

func TestImport_BothFilesMissing(t *testing.T) {
	// Fresh HOME, no ~/.codex at all → ErrNoConfig.
	r := newResolver(t)

	core, overlay, err := runImport(t, r)
	if !errors.Is(err, codex.ErrNoConfig) {
		t.Fatalf("Import on fresh HOME: err = %v, want ErrNoConfig", err)
	}
	if !isZeroCore(core) {
		t.Errorf("core = %+v, want zero value", core)
	}
	if !isZeroOverlay(overlay) {
		t.Errorf("overlay = %+v, want zero value", overlay)
	}
}

func TestImport_OnlyConfigTomlPresent(t *testing.T) {
	// A valid config.toml with model + model_provider, but no
	// auth.json. Import returns what it can: Overlay.Raw populated
	// with the owned config keys, Core empty (config.toml never
	// touches Core in v1 — see import.go's godoc).
	r := newResolver(t)
	writeConfigTOML(t, r, `model = "gpt-5"
model_provider = "openai"

[model_providers.openai]
name = "OpenAI"
base_url = "https://api.openai.com/v1"
env_key = "OPENAI_API_KEY"
wire_api = "chat"
`)

	core, overlay, err := runImport(t, r)
	if err != nil {
		t.Fatalf("Import: %v", err)
	}
	if !isZeroCore(core) {
		t.Errorf("core = %+v, want zero (config.toml alone contributes nothing to Core)", core)
	}
	if got := overlay.Raw["model"]; got != "gpt-5" {
		t.Errorf("overlay.Raw[model] = %v, want %q", got, "gpt-5")
	}
	if got := overlay.Raw["model_provider"]; got != "openai" {
		t.Errorf("overlay.Raw[model_provider] = %v, want %q", got, "openai")
	}
	if got := overlay.Raw["model_providers.openai.base_url"]; got != "https://api.openai.com/v1" {
		t.Errorf("overlay.Raw[model_providers.openai.base_url] = %v, want %q", got, "https://api.openai.com/v1")
	}
	if got := overlay.Raw["model_providers.openai.env_key"]; got != "OPENAI_API_KEY" {
		t.Errorf("overlay.Raw[env_key] = %v, want %q", got, "OPENAI_API_KEY")
	}
	if got := overlay.Raw["model_providers.openai.name"]; got != "OpenAI" {
		t.Errorf("overlay.Raw[name] = %v, want %q", got, "OpenAI")
	}
	if got := overlay.Raw["model_providers.openai.wire_api"]; got != "chat" {
		t.Errorf("overlay.Raw[wire_api] = %v, want %q", got, "chat")
	}
}

func TestImport_OnlyAuthJsonPresent(t *testing.T) {
	// A valid auth.json with OPENAI_API_KEY, no config.toml. Import
	// returns Core.APIKey populated and Overlay.Raw carrying the
	// auth_mode / tokens.* fields.
	r := newResolver(t)
	writeAuthJSON(t, r, `{
  "OPENAI_API_KEY": "sk-test-openai",
  "auth_mode": "api_key"
}`)

	core, overlay, err := runImport(t, r)
	if err != nil {
		t.Fatalf("Import: %v", err)
	}
	if core.APIKey != "sk-test-openai" {
		t.Errorf("core.APIKey = %q, want %q", core.APIKey, "sk-test-openai")
	}
	if got := overlay.Raw["auth_mode"]; got != "api_key" {
		t.Errorf("overlay.Raw[auth_mode] = %v, want %q", got, "api_key")
	}
	// OPENAI_API_KEY must NOT be double-booked into Overlay.Raw when
	// it was promoted to Core.APIKey.
	if _, dup := overlay.Raw["OPENAI_API_KEY"]; dup {
		t.Errorf("overlay.Raw should not carry OPENAI_API_KEY when promoted to Core.APIKey")
	}
}

func TestImport_BothFilesHappy(t *testing.T) {
	// Realistic Codex config: config.toml routes to OpenAI, auth.json
	// carries both an API key and an OAuth token bundle. Verify the
	// full owned-key extraction reflects both files.
	r := newResolver(t)
	writeConfigTOML(t, r, `model = "gpt-5"
model_provider = "openai"
approval_mode = "on-request"

[model_providers.openai]
name = "OpenAI"
base_url = "https://api.openai.com/v1"
env_key = "OPENAI_API_KEY"
wire_api = "chat"
`)
	writeAuthJSON(t, r, `{
  "OPENAI_API_KEY": "sk-full-happy",
  "auth_mode": "chatgpt",
  "last_refresh": "2026-01-15T10:00:00Z",
  "tokens": {
    "access_token": "at-full",
    "refresh_token": "rt-full",
    "id_token": "it-full",
    "account_id": "acct-full"
  }
}`)

	core, overlay, err := runImport(t, r)
	if err != nil {
		t.Fatalf("Import: %v", err)
	}
	if core.APIKey != "sk-full-happy" {
		t.Errorf("core.APIKey = %q, want %q", core.APIKey, "sk-full-happy")
	}
	// config.toml owned keys → Overlay.Raw.
	for _, kv := range []struct {
		key  string
		want any
	}{
		{"model", "gpt-5"},
		{"model_provider", "openai"},
		{"approval_mode", "on-request"},
		{"model_providers.openai.base_url", "https://api.openai.com/v1"},
		{"model_providers.openai.env_key", "OPENAI_API_KEY"},
		{"model_providers.openai.name", "OpenAI"},
		{"model_providers.openai.wire_api", "chat"},
	} {
		if got := overlay.Raw[kv.key]; got != kv.want {
			t.Errorf("overlay.Raw[%q] = %v, want %v", kv.key, got, kv.want)
		}
	}
	// auth.json non-API-key owned keys → Overlay.Raw.
	for _, kv := range []struct {
		key  string
		want any
	}{
		{"auth_mode", "chatgpt"},
		{"last_refresh", "2026-01-15T10:00:00Z"},
		{"tokens.access_token", "at-full"},
		{"tokens.refresh_token", "rt-full"},
		{"tokens.id_token", "it-full"},
		{"tokens.account_id", "acct-full"},
	} {
		if got := overlay.Raw[kv.key]; got != kv.want {
			t.Errorf("overlay.Raw[%q] = %v, want %v", kv.key, got, kv.want)
		}
	}
}

func TestImport_ConfigMalformed(t *testing.T) {
	// Unterminated string in config.toml → refuse with
	// ErrParseFailed. Wrapping preserves both the Codex adapter
	// sentinel and the codextoml package sentinel.
	r := newResolver(t)
	writeConfigTOML(t, r, `model = "gpt-5
`) // unterminated basic string

	_, _, err := runImport(t, r)
	if err == nil {
		t.Fatalf("Import on malformed TOML: err = nil, want non-nil")
	}
	if !errors.Is(err, codex.ErrParseFailed) {
		t.Errorf("err = %v, want errors.Is(err, ErrParseFailed)", err)
	}
	if !errors.Is(err, writepath.ErrParseFailed) {
		t.Errorf("err = %v, want errors.Is(err, writepath.ErrParseFailed) (shared sentinel)", err)
	}
	if !errors.Is(err, codextoml.ErrParseFailed) {
		t.Errorf("err = %v, want errors.Is(err, codextoml.ErrParseFailed) (TOML parser sentinel)", err)
	}
}

func TestImport_AuthMalformed(t *testing.T) {
	// Truncated JSON in auth.json → refuse with ErrParseFailed.
	r := newResolver(t)
	writeAuthJSON(t, r, `{"OPENAI_API_KEY":`) // truncated

	_, _, err := runImport(t, r)
	if err == nil {
		t.Fatalf("Import on malformed JSON: err = nil, want non-nil")
	}
	if !errors.Is(err, codex.ErrParseFailed) {
		t.Errorf("err = %v, want errors.Is(err, ErrParseFailed)", err)
	}
	if !errors.Is(err, writepath.ErrParseFailed) {
		t.Errorf("err = %v, want errors.Is(err, writepath.ErrParseFailed)", err)
	}
}

func TestImport_AuthNullRootRefused(t *testing.T) {
	// `null` decodes to a nil map — legal JSON but not a legal
	// auth.json shape. Refuse rather than treat as {}.
	r := newResolver(t)
	writeAuthJSON(t, r, `null`)

	_, _, err := runImport(t, r)
	if !errors.Is(err, codex.ErrParseFailed) {
		t.Fatalf("Import on `null` auth.json: err = %v, want ErrParseFailed", err)
	}
}

func TestImport_ConfigEmpty(t *testing.T) {
	// Zero-byte config.toml → treated as absent. When auth.json is
	// also missing, that leaves both files absent → ErrNoConfig.
	r := newResolver(t)
	writeConfigTOML(t, r, "")

	_, _, err := runImport(t, r)
	if !errors.Is(err, codex.ErrNoConfig) {
		t.Fatalf("Import on empty config.toml + no auth.json: err = %v, want ErrNoConfig", err)
	}
}

func TestImport_ConfigEmptyAuthPresent(t *testing.T) {
	// Zero-byte config.toml + valid auth.json → import auth
	// successfully; config contributes nothing.
	r := newResolver(t)
	writeConfigTOML(t, r, "")
	writeAuthJSON(t, r, `{"OPENAI_API_KEY":"sk-from-auth"}`)

	core, _, err := runImport(t, r)
	if err != nil {
		t.Fatalf("Import: %v", err)
	}
	if core.APIKey != "sk-from-auth" {
		t.Errorf("core.APIKey = %q, want %q", core.APIKey, "sk-from-auth")
	}
}

func TestImport_AuthEmpty(t *testing.T) {
	// Zero-byte auth.json → treated as absent. Both files absent
	// (config.toml never written) → ErrNoConfig.
	r := newResolver(t)
	writeAuthJSON(t, r, "")

	_, _, err := runImport(t, r)
	if !errors.Is(err, codex.ErrNoConfig) {
		t.Fatalf("Import on empty auth.json + no config.toml: err = %v, want ErrNoConfig", err)
	}
}

func TestImport_AuthEmptyConfigPresent(t *testing.T) {
	// Zero-byte auth.json + valid config.toml → import config
	// successfully; auth contributes nothing.
	r := newResolver(t)
	writeAuthJSON(t, r, "")
	writeConfigTOML(t, r, `model = "gpt-5"
`)

	core, overlay, err := runImport(t, r)
	if err != nil {
		t.Fatalf("Import: %v", err)
	}
	if !isZeroCore(core) {
		t.Errorf("core = %+v, want zero (auth empty, config alone contributes nothing to Core)", core)
	}
	if got := overlay.Raw["model"]; got != "gpt-5" {
		t.Errorf("overlay.Raw[model] = %v, want %q", got, "gpt-5")
	}
}

func TestImport_ConfigWhitespaceOnly(t *testing.T) {
	// Whitespace-only config.toml → treated as absent, symmetric with
	// the claudecode fixup for empty-vs-whitespace divergence.
	r := newResolver(t)
	writeConfigTOML(t, r, "  \n\t\n  ")

	_, _, err := runImport(t, r)
	if !errors.Is(err, codex.ErrNoConfig) {
		t.Fatalf("Import on whitespace-only config.toml + no auth.json: err = %v, want ErrNoConfig", err)
	}
}

func TestImport_AuthWhitespaceOnly(t *testing.T) {
	// Whitespace-only auth.json → treated as absent.
	r := newResolver(t)
	writeAuthJSON(t, r, "   \n  \t  ")

	_, _, err := runImport(t, r)
	if !errors.Is(err, codex.ErrNoConfig) {
		t.Fatalf("Import on whitespace-only auth.json + no config.toml: err = %v, want ErrNoConfig", err)
	}
}

func TestImport_UnknownKeysIgnored(t *testing.T) {
	// Non-owned keys in either file are NOT extracted. Byte-identical
	// round-trip on unowned bytes is delivered by write-path
	// merge-preserve at Apply time; the Profile candidate stays sparse.
	r := newResolver(t)
	writeConfigTOML(t, r, `model = "gpt-5"

[history]
persistence = "save-all"

[mcp_servers.foo]
command = "bar"
`)
	writeAuthJSON(t, r, `{
  "OPENAI_API_KEY": "sk-unknown-keys",
  "future_field": "leave-me-alone",
  "some_random_config": {"nested": true}
}`)

	core, overlay, err := runImport(t, r)
	if err != nil {
		t.Fatalf("Import: %v", err)
	}
	if core.APIKey != "sk-unknown-keys" {
		t.Errorf("core.APIKey = %q, want %q", core.APIKey, "sk-unknown-keys")
	}
	// Non-owned keys must NOT enter Overlay.Raw.
	for _, badKey := range []string{
		"history.persistence",
		"mcp_servers.foo.command",
		"future_field",
		"some_random_config.nested",
	} {
		if _, leaked := overlay.Raw[badKey]; leaked {
			t.Errorf("overlay.Raw leaked non-owned key %q: %v", badKey, overlay.Raw[badKey])
		}
	}
	// Owned key still lands correctly.
	if got := overlay.Raw["model"]; got != "gpt-5" {
		t.Errorf("overlay.Raw[model] = %v, want %q", got, "gpt-5")
	}
}

func TestImport_TokensExtraction(t *testing.T) {
	// Full OAuth token bundle with no OPENAI_API_KEY → Core.APIKey
	// stays empty, all four tokens.* fields land in Overlay.Raw.
	r := newResolver(t)
	writeAuthJSON(t, r, `{
  "auth_mode": "chatgpt",
  "last_refresh": "2026-06-30T00:00:00Z",
  "tokens": {
    "access_token": "at-abc",
    "refresh_token": "rt-def",
    "id_token": "it-ghi",
    "account_id": "acct-jkl"
  }
}`)

	core, overlay, err := runImport(t, r)
	if err != nil {
		t.Fatalf("Import: %v", err)
	}
	if core.APIKey != "" {
		t.Errorf("core.APIKey = %q, want empty (no OPENAI_API_KEY present)", core.APIKey)
	}
	for _, kv := range []struct {
		key  string
		want any
	}{
		{"auth_mode", "chatgpt"},
		{"last_refresh", "2026-06-30T00:00:00Z"},
		{"tokens.access_token", "at-abc"},
		{"tokens.refresh_token", "rt-def"},
		{"tokens.id_token", "it-ghi"},
		{"tokens.account_id", "acct-jkl"},
	} {
		if got := overlay.Raw[kv.key]; got != kv.want {
			t.Errorf("overlay.Raw[%q] = %v, want %v", kv.key, got, kv.want)
		}
	}
}

func TestImport_APIKeyPresentAndOAuth(t *testing.T) {
	// Both OPENAI_API_KEY and tokens.* present → API_KEY wins into
	// Core.APIKey and the full token bundle still lands in
	// Overlay.Raw. This matches operator expectations for
	// self-hosted flows that keep both credential shapes on disk.
	r := newResolver(t)
	writeAuthJSON(t, r, `{
  "OPENAI_API_KEY": "sk-both",
  "auth_mode": "hybrid",
  "tokens": {
    "access_token": "at-both",
    "refresh_token": "rt-both",
    "id_token": "it-both",
    "account_id": "acct-both"
  }
}`)

	core, overlay, err := runImport(t, r)
	if err != nil {
		t.Fatalf("Import: %v", err)
	}
	if core.APIKey != "sk-both" {
		t.Errorf("core.APIKey = %q, want %q", core.APIKey, "sk-both")
	}
	if got := overlay.Raw["tokens.access_token"]; got != "at-both" {
		t.Errorf("overlay.Raw[tokens.access_token] = %v, want %q", got, "at-both")
	}
	if got := overlay.Raw["tokens.refresh_token"]; got != "rt-both" {
		t.Errorf("overlay.Raw[tokens.refresh_token] = %v, want %q", got, "rt-both")
	}
	if got := overlay.Raw["tokens.id_token"]; got != "it-both" {
		t.Errorf("overlay.Raw[tokens.id_token] = %v, want %q", got, "it-both")
	}
	if got := overlay.Raw["tokens.account_id"]; got != "acct-both" {
		t.Errorf("overlay.Raw[tokens.account_id] = %v, want %q", got, "acct-both")
	}
	if got := overlay.Raw["auth_mode"]; got != "hybrid" {
		t.Errorf("overlay.Raw[auth_mode] = %v, want %q", got, "hybrid")
	}
}

func TestImport_APIKeyNull(t *testing.T) {
	// JSON `null` at OPENAI_API_KEY is a legal shape (a user editor
	// cleared the value without deleting the key). Precedence must
	// be decided on "non-null value present", NOT on "key present in
	// map", or Core.APIKey gets silently zeroed while a real OAuth
	// bundle is still on disk. Mirror claudecode's null-safety
	// behaviour so a regression to a "has-key" check trips CI.
	r := newResolver(t)
	writeAuthJSON(t, r, `{
  "OPENAI_API_KEY": null,
  "tokens": {
    "access_token": "at-null",
    "refresh_token": "rt-null",
    "id_token": "it-null",
    "account_id": "acct-null"
  }
}`)

	core, overlay, err := runImport(t, r)
	if err != nil {
		t.Fatalf("Import: %v", err)
	}
	if core.APIKey != "" {
		t.Errorf("core.APIKey = %q, want empty (null OPENAI_API_KEY skips the slot)", core.APIKey)
	}
	// tokens.* still extracted regardless of the null API key.
	if got := overlay.Raw["tokens.access_token"]; got != "at-null" {
		t.Errorf("overlay.Raw[tokens.access_token] = %v, want %q", got, "at-null")
	}
	if got := overlay.Raw["tokens.refresh_token"]; got != "rt-null" {
		t.Errorf("overlay.Raw[tokens.refresh_token] = %v, want %q", got, "rt-null")
	}
	// The null OPENAI_API_KEY must NOT leak into Overlay.Raw either
	// — it's not carried anywhere; a subsequent Render will emit the
	// slot based on the (empty) Core.APIKey. Precedence policy is
	// documented in import.go's godoc.
	if _, dup := overlay.Raw["OPENAI_API_KEY"]; dup {
		t.Errorf("overlay.Raw carries OPENAI_API_KEY on null: %v; want absent", overlay.Raw["OPENAI_API_KEY"])
	}
}

func TestImport_APIKeyEmptyStringPreserved(t *testing.T) {
	// Empty string is a valid non-null value: the user explicitly
	// asked Codex to run with an empty API key (say, to force the
	// OAuth path). Preserve it verbatim into Core.APIKey rather
	// than silently substituting anything. Asymmetric with the null
	// case above, and documented in import.go.
	r := newResolver(t)
	writeAuthJSON(t, r, `{
  "OPENAI_API_KEY": "",
  "auth_mode": "chatgpt"
}`)

	core, _, err := runImport(t, r)
	if err != nil {
		t.Fatalf("Import: %v", err)
	}
	if core.APIKey != "" {
		t.Errorf("core.APIKey = %q, want empty string preserved verbatim", core.APIKey)
	}
}

func TestImport_SymlinkInHome(t *testing.T) {
	// ~/.codex/config.toml is a symlink to another file inside HOME
	// → follow and read normally. Softer than the write path (which
	// refuses to write through symlinks at all); the asymmetry is
	// documented in import.go.
	r := newResolver(t)
	codexDir(t, r)
	target := filepath.Join(r.Home(), ".codex", "config-actual.toml")
	if err := os.WriteFile(target, []byte(`model = "gpt-symlink"`), 0o600); err != nil {
		t.Fatalf("write target: %v", err)
	}
	link := codex.ConfigPath(r)
	if err := os.Symlink(target, link); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	// Also need SOMETHING valid so ErrNoConfig is not what we hit.
	// The symlink IS the config.toml. auth.json stays absent — Import
	// should still succeed on the config-only half.
	_, overlay, err := runImport(t, r)
	if err != nil {
		t.Fatalf("Import on in-HOME symlink: err = %v", err)
	}
	if got := overlay.Raw["model"]; got != "gpt-symlink" {
		t.Errorf("overlay.Raw[model] = %v, want %q (in-HOME symlink followed)", got, "gpt-symlink")
	}
}

func TestImport_ConfigSymlinkOutsideHome(t *testing.T) {
	// config.toml is a symlink to a target OUTSIDE HOME → refuse
	// with ErrOutsideHome. Reading /etc/passwd through a planted
	// symlink stays an attack surface on the read side too.
	r := newResolver(t)
	codexDir(t, r)
	outsideDir := t.TempDir()
	outsideTarget := filepath.Join(outsideDir, "elsewhere.toml")
	if err := os.WriteFile(outsideTarget, []byte(`model = "leak"`), 0o600); err != nil {
		t.Fatalf("write outside target: %v", err)
	}
	link := codex.ConfigPath(r)
	if err := os.Symlink(outsideTarget, link); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	_, _, err := runImport(t, r)
	if !errors.Is(err, codex.ErrOutsideHome) {
		t.Fatalf("Import on out-of-HOME config symlink: err = %v, want ErrOutsideHome", err)
	}
	if !errors.Is(err, storage.ErrOutsideHome) {
		t.Errorf("err = %v, want errors.Is(err, storage.ErrOutsideHome) (shared sentinel)", err)
	}
}

func TestImport_AuthSymlinkOutsideHome(t *testing.T) {
	// auth.json is a symlink to a target OUTSIDE HOME → refuse
	// with ErrOutsideHome. Same as the config-side test.
	r := newResolver(t)
	codexDir(t, r)
	outsideDir := t.TempDir()
	outsideTarget := filepath.Join(outsideDir, "elsewhere.json")
	if err := os.WriteFile(outsideTarget, []byte(`{"OPENAI_API_KEY":"leak"}`), 0o600); err != nil {
		t.Fatalf("write outside target: %v", err)
	}
	link := codex.AuthPath(r)
	if err := os.Symlink(outsideTarget, link); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	_, _, err := runImport(t, r)
	if !errors.Is(err, codex.ErrOutsideHome) {
		t.Fatalf("Import on out-of-HOME auth symlink: err = %v, want ErrOutsideHome", err)
	}
	if !errors.Is(err, storage.ErrOutsideHome) {
		t.Errorf("err = %v, want errors.Is(err, storage.ErrOutsideHome)", err)
	}
}

func TestImport_DanglingConfigSymlinkTreatedAsMissing(t *testing.T) {
	// A symlink whose target does not exist → treat as absent (not a
	// stat error). Combined with a missing auth.json, this reproduces
	// the "nothing to import" UX (ErrNoConfig).
	r := newResolver(t)
	codexDir(t, r)
	link := codex.ConfigPath(r)
	if err := os.Symlink(filepath.Join(r.Home(), ".codex", "does-not-exist.toml"), link); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	_, _, err := runImport(t, r)
	if !errors.Is(err, codex.ErrNoConfig) {
		t.Fatalf("Import on dangling config symlink + no auth: err = %v, want ErrNoConfig", err)
	}
}

func TestImport_ContextCanceled(t *testing.T) {
	// Pre-canceled ctx: Import must return ctx.Err() BEFORE any
	// disk I/O — mirrors claudecode's contract.
	r := newResolver(t)
	writeAuthJSON(t, r, `{"OPENAI_API_KEY":"sk-never-read"}`)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, _, err := codex.New().Import(ctx, r)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Import on canceled ctx: err = %v, want context.Canceled", err)
	}
}

func TestImport_NonStringAPIKeyCoerced(t *testing.T) {
	// A hand-edited auth.json that sneaks a non-string into
	// OPENAI_API_KEY (e.g. a boolean or numeric literal) must be
	// coerced rather than dropped, so the profile candidate carries
	// SOMETHING recognizable. Real Codex writes strings only; the
	// coercion is defense-in-depth for anomalous inputs and covers
	// every arm of coerceToStringCodex's type switch:
	//   - integer float64 (JSON numbers arrive as float64)
	//   - fractional float64
	//   - bool true / bool false
	//   - default arm (arrays, objects) via fmt %v
	cases := []struct {
		name    string
		body    string
		wantKey string
	}{
		{"integer", `{"OPENAI_API_KEY": 12345}`, "12345"},
		{"fractional", `{"OPENAI_API_KEY": 3.14}`, "3.14"},
		{"bool_true", `{"OPENAI_API_KEY": true}`, "true"},
		{"bool_false", `{"OPENAI_API_KEY": false}`, "false"},
		{"array_via_default", `{"OPENAI_API_KEY": ["a","b"]}`, "[a b]"},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			r := newResolver(t)
			writeAuthJSON(t, r, tc.body)

			core, _, err := runImport(t, r)
			if err != nil {
				t.Fatalf("Import: %v", err)
			}
			if core.APIKey != tc.wantKey {
				t.Errorf("core.APIKey = %q, want %q", core.APIKey, tc.wantKey)
			}
		})
	}
}

func TestImport_UnreadableFilePermission(t *testing.T) {
	// A config.toml that exists but is unreadable (mode 0) exercises
	// the "read %q: %w" branch — a permission error that is neither
	// ErrNotExist nor a parse failure. Skip when running as root
	// (mode 0 doesn't block root reads).
	if os.Geteuid() == 0 {
		t.Skip("running as root; mode 0 does not block reads")
	}
	r := newResolver(t)
	path := writeConfigTOML(t, r, `model = "gpt-5"`)
	if err := os.Chmod(path, 0); err != nil {
		t.Fatalf("chmod 0: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(path, 0o600) })

	_, _, err := runImport(t, r)
	if err == nil {
		t.Fatalf("Import on unreadable config.toml: err = nil, want a permission error")
	}
	if errors.Is(err, codex.ErrNoConfig) || errors.Is(err, codex.ErrParseFailed) || errors.Is(err, codex.ErrOutsideHome) {
		t.Errorf("err = %v, want a non-sentinel read error (raw filesystem error, not remapped)", err)
	}
}

func TestImport_NonStringTOMLValuePreserved(t *testing.T) {
	// TOML permits non-string owned-key values in a couple of the
	// slot shapes claudecm names (bool, integer). None of the v1
	// Codex owned keys expect non-string values but a hand-edited
	// or future-tool-written file might sneak one in. Verify the
	// wrapper's typed value is preserved in Overlay.Raw as-is (the
	// downstream renderer type-switches on it).
	r := newResolver(t)
	// approval_mode is documented as a string; force a bool for the
	// defense-in-depth check.
	writeConfigTOML(t, r, `approval_mode = true
model = "gpt-5"
`)

	_, overlay, err := runImport(t, r)
	if err != nil {
		t.Fatalf("Import: %v", err)
	}
	if got := overlay.Raw["approval_mode"]; got != true {
		t.Errorf("overlay.Raw[approval_mode] = %v (%T), want bool true", got, got)
	}
	if got := overlay.Raw["model"]; got != "gpt-5" {
		t.Errorf("overlay.Raw[model] = %v, want %q", got, "gpt-5")
	}
}
