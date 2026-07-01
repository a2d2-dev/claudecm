package claudecode_test

// import_test.go — E3-S3. Exercise Adapter.Import via the exported
// surface, using a per-test HOME (t.TempDir → NewResolverWithHome) so
// each case owns its own ~/.claude tree. Tests live in the _test
// package to mirror the shape cmd/* will consume in E5+.

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/a2d2-dev/claudecm/internal/adapter"
	"github.com/a2d2-dev/claudecm/internal/adapter/claudecode"
	"github.com/a2d2-dev/claudecm/internal/storage"
	"github.com/a2d2-dev/claudecm/internal/writepath"
)

// writeSettings ensures ~/.claude exists under the resolver's HOME and
// writes body to ~/.claude/settings.json with 0600.
func writeSettings(t *testing.T, r *storage.Resolver, body string) string {
	t.Helper()
	dir := filepath.Join(r.Home(), ".claude")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir .claude: %v", err)
	}
	path := claudecode.SettingsPath(r)
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write settings.json: %v", err)
	}
	return path
}

// runImport is a shorthand for the (adapter, ctx, resolver) triple.
// Every test uses ctx.Background() unless it is explicitly probing
// cancellation semantics.
func runImport(t *testing.T, r *storage.Resolver) (adapter.CoreFromTool, adapter.OverlayFromTool, error) {
	t.Helper()
	return claudecode.New().Import(context.Background(), r)
}

// isZeroCore reports whether core is the zero value. Written out
// long-hand so a future field addition in config.CoreConfig surfaces as
// a test compile error rather than a silent skip.
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

func TestImport_MissingFile(t *testing.T) {
	r := newResolver(t)

	core, overlay, err := runImport(t, r)
	if !errors.Is(err, claudecode.ErrNoConfig) {
		t.Fatalf("Import on fresh HOME: err = %v, want ErrNoConfig", err)
	}
	if !isZeroCore(core) {
		t.Errorf("core = %+v, want zero value", core)
	}
	if !isZeroOverlay(overlay) {
		t.Errorf("overlay = %+v, want zero value", overlay)
	}
}

func TestImport_EmptyFile(t *testing.T) {
	// Documented policy: 0-byte settings.json is interpreted as `{}`.
	// This is NOT a fallback write (Import never writes); it is a
	// read-side interpretation of an on-disk shape Claude Code
	// produces on first launch.
	r := newResolver(t)
	writeSettings(t, r, "")

	core, overlay, err := runImport(t, r)
	if err != nil {
		t.Fatalf("Import on empty file: err = %v, want nil", err)
	}
	if !isZeroCore(core) {
		t.Errorf("core = %+v, want zero value", core)
	}
	if !isZeroOverlay(overlay) {
		t.Errorf("overlay = %+v, want zero value", overlay)
	}
}

func TestImport_WhitespaceOnlyTreatedAsEmpty(t *testing.T) {
	// PR #23 review F1: prior to the treatAsEmpty consolidation the
	// two paths diverged — Import used len(data) == 0 (strict zero-
	// byte) while Plan.Transform used bytes.TrimSpace (whitespace-
	// tolerant), so a settings.json containing "   \n\t  " was
	// ErrParseFailed on Import but silently normalized to {} on Plan.
	// The shared treatAsEmpty predicate now guarantees both paths
	// agree. This test pins the Import side of that contract:
	// whitespace-only bytes → nil error, zero Core, zero Overlay
	// (same as an actual {} on disk, same as a zero-byte file).
	r := newResolver(t)
	writeSettings(t, r, "   \n\t  ")

	core, overlay, err := runImport(t, r)
	if err != nil {
		t.Fatalf("Import on whitespace-only file: err = %v, want nil", err)
	}
	if !isZeroCore(core) {
		t.Errorf("core = %+v, want zero value", core)
	}
	if !isZeroOverlay(overlay) {
		t.Errorf("overlay = %+v, want zero value", overlay)
	}
}

func TestImport_EmptyJSONObject(t *testing.T) {
	r := newResolver(t)
	writeSettings(t, r, "{}")

	core, overlay, err := runImport(t, r)
	if err != nil {
		t.Fatalf("Import on {}: err = %v, want nil", err)
	}
	if !isZeroCore(core) {
		t.Errorf("core = %+v, want zero value", core)
	}
	if !isZeroOverlay(overlay) {
		t.Errorf("overlay = %+v, want zero value", overlay)
	}
}

func TestImport_HappyAllOwnedKeys(t *testing.T) {
	r := newResolver(t)
	writeSettings(t, r, `{
  "env": {
    "ANTHROPIC_BASE_URL": "https://api.example.com",
    "ANTHROPIC_AUTH_TOKEN": "sk-test-123",
    "ANTHROPIC_MODEL": "claude-opus-4-5",
    "ANTHROPIC_SMALL_FAST_MODEL": "claude-haiku-4-5",
    "CLAUDE_CODE_USE_BEDROCK": "1"
  }
}`)

	core, overlay, err := runImport(t, r)
	if err != nil {
		t.Fatalf("Import: %v", err)
	}
	if core.BaseURL != "https://api.example.com" {
		t.Errorf("core.BaseURL = %q, want %q", core.BaseURL, "https://api.example.com")
	}
	if core.APIKey != "sk-test-123" {
		t.Errorf("core.APIKey = %q, want %q (from AUTH_TOKEN)", core.APIKey, "sk-test-123")
	}
	if core.Model != "claude-opus-4-5" {
		t.Errorf("core.Model = %q", core.Model)
	}
	if core.SmallFastModel != "claude-haiku-4-5" {
		t.Errorf("core.SmallFastModel = %q", core.SmallFastModel)
	}
	if got := overlay.ExtraEnv["CLAUDE_CODE_USE_BEDROCK"]; got != "1" {
		t.Errorf("overlay.ExtraEnv[CLAUDE_CODE_USE_BEDROCK] = %q, want %q", got, "1")
	}
	if _, ok := overlay.ExtraEnv["ANTHROPIC_API_KEY"]; ok {
		t.Errorf("overlay.ExtraEnv should not carry ANTHROPIC_API_KEY when only AUTH_TOKEN is present")
	}
}

func TestImport_UseVertexRoutedToOverlay(t *testing.T) {
	// Complements TestImport_HappyAllOwnedKeys — exercises the other
	// tool-specific backend toggle so both allowlist entries have a
	// happy-path assertion behind them.
	r := newResolver(t)
	writeSettings(t, r, `{
  "env": {
    "CLAUDE_CODE_USE_VERTEX": "1"
  }
}`)

	core, overlay, err := runImport(t, r)
	if err != nil {
		t.Fatalf("Import: %v", err)
	}
	if !isZeroCore(core) {
		t.Errorf("core = %+v, want zero value", core)
	}
	if got := overlay.ExtraEnv["CLAUDE_CODE_USE_VERTEX"]; got != "1" {
		t.Errorf("overlay.ExtraEnv[CLAUDE_CODE_USE_VERTEX] = %q, want %q", got, "1")
	}
}

func TestImport_APIKeyPrefersAuthToken(t *testing.T) {
	// When both AUTH_TOKEN and API_KEY are present, AUTH_TOKEN wins
	// into Core.APIKey; API_KEY is recorded in Overlay.ExtraEnv for
	// round-trip fidelity.
	r := newResolver(t)
	writeSettings(t, r, `{
  "env": {
    "ANTHROPIC_AUTH_TOKEN": "sk-auth-primary",
    "ANTHROPIC_API_KEY": "sk-api-secondary"
  }
}`)

	core, overlay, err := runImport(t, r)
	if err != nil {
		t.Fatalf("Import: %v", err)
	}
	if core.APIKey != "sk-auth-primary" {
		t.Errorf("core.APIKey = %q, want %q (AUTH_TOKEN wins)", core.APIKey, "sk-auth-primary")
	}
	if got := overlay.ExtraEnv["ANTHROPIC_API_KEY"]; got != "sk-api-secondary" {
		t.Errorf("overlay.ExtraEnv[ANTHROPIC_API_KEY] = %q, want %q", got, "sk-api-secondary")
	}
}

func TestImport_APIKeyOnlyWinsWhenNoAuthToken(t *testing.T) {
	// When only API_KEY is present, it wins into Core.APIKey directly
	// and does not also land in the overlay.
	r := newResolver(t)
	writeSettings(t, r, `{
  "env": {
    "ANTHROPIC_API_KEY": "sk-api-only"
  }
}`)

	core, overlay, err := runImport(t, r)
	if err != nil {
		t.Fatalf("Import: %v", err)
	}
	if core.APIKey != "sk-api-only" {
		t.Errorf("core.APIKey = %q, want %q", core.APIKey, "sk-api-only")
	}
	if _, ok := overlay.ExtraEnv["ANTHROPIC_API_KEY"]; ok {
		t.Errorf("overlay.ExtraEnv should not double-book API_KEY when it was the only credential")
	}
}

func TestImport_UnknownKeysIgnored(t *testing.T) {
	// Non-owned keys (permissions, hooks, mcpServers, theme, and
	// unrelated env entries) are NOT extracted into Core or Overlay.
	// The write-path preserves them verbatim at Apply time via the
	// merge-preserve rule (FR-5); the Profile does not double-book.
	r := newResolver(t)
	writeSettings(t, r, `{
  "theme": "dark",
  "permissions": {"allow": ["shell:*"]},
  "hooks": {"PostToolUse": []},
  "mcpServers": {"foo": {"command": "bar"}},
  "env": {
    "ANTHROPIC_BASE_URL": "https://owned.example",
    "SOME_UNRELATED_ENV": "leave-me-alone"
  }
}`)

	core, overlay, err := runImport(t, r)
	if err != nil {
		t.Fatalf("Import: %v", err)
	}
	if core.BaseURL != "https://owned.example" {
		t.Errorf("core.BaseURL = %q, want %q", core.BaseURL, "https://owned.example")
	}
	// Non-owned env keys must NOT enter the overlay.
	if _, ok := overlay.ExtraEnv["SOME_UNRELATED_ENV"]; ok {
		t.Errorf("overlay.ExtraEnv leaked unowned env var SOME_UNRELATED_ENV")
	}
	// Non-owned top-level keys must NOT land in Overlay.Raw either —
	// merge-preserve at Apply time is the round-trip mechanism.
	if len(overlay.Raw) != 0 {
		t.Errorf("overlay.Raw = %v, want empty (merge-preserve owns round-trip, not Raw)", overlay.Raw)
	}
}

func TestImport_MalformedJSON(t *testing.T) {
	r := newResolver(t)
	writeSettings(t, r, `{"env":`) // truncated

	_, _, err := runImport(t, r)
	if err == nil {
		t.Fatalf("Import on malformed JSON: err = nil, want non-nil")
	}
	if !errors.Is(err, claudecode.ErrParseFailed) {
		t.Errorf("err = %v, want errors.Is(err, ErrParseFailed)", err)
	}
	// Adapter's ErrParseFailed wraps writepath.ErrParseFailed so
	// downstream callers that already switch on the shared sentinel
	// keep matching without importing this adapter's package.
	if !errors.Is(err, writepath.ErrParseFailed) {
		t.Errorf("err = %v, want errors.Is(err, writepath.ErrParseFailed) (wrapped by adapter sentinel)", err)
	}
}

func TestImport_NullRootRefused(t *testing.T) {
	// `null` decodes to a nil map — legal JSON but not a legal
	// settings.json shape. Refuse rather than treat as {}.
	r := newResolver(t)
	writeSettings(t, r, `null`)

	_, _, err := runImport(t, r)
	if !errors.Is(err, claudecode.ErrParseFailed) {
		t.Fatalf("Import on `null`: err = %v, want ErrParseFailed", err)
	}
}

func TestImport_SymlinkInHome(t *testing.T) {
	// settings.json is a symlink to another file inside HOME → follow
	// and read normally. This is softer than the write path (which
	// refuses to write through symlinks at all); the asymmetry is
	// documented in import.go.
	r := newResolver(t)
	if err := os.MkdirAll(filepath.Join(r.Home(), ".claude"), 0o755); err != nil {
		t.Fatalf("mkdir .claude: %v", err)
	}
	target := filepath.Join(r.Home(), ".claude", "settings-actual.json")
	if err := os.WriteFile(target, []byte(`{"env":{"ANTHROPIC_BASE_URL":"https://link.example"}}`), 0o600); err != nil {
		t.Fatalf("write target: %v", err)
	}
	link := claudecode.SettingsPath(r)
	if err := os.Symlink(target, link); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	core, _, err := runImport(t, r)
	if err != nil {
		t.Fatalf("Import on in-HOME symlink: err = %v", err)
	}
	if core.BaseURL != "https://link.example" {
		t.Errorf("core.BaseURL = %q, want %q (follow in-HOME symlink)", core.BaseURL, "https://link.example")
	}
}

func TestImport_SymlinkOutsideHome(t *testing.T) {
	// settings.json is a symlink to a target OUTSIDE HOME → refuse
	// with ErrOutsideHome. Reading /etc/passwd through a planted
	// symlink stays an attack surface even on the read side.
	r := newResolver(t)
	if err := os.MkdirAll(filepath.Join(r.Home(), ".claude"), 0o755); err != nil {
		t.Fatalf("mkdir .claude: %v", err)
	}
	// Put a target under a sibling temp dir (guaranteed outside HOME).
	outsideDir := t.TempDir()
	outsideTarget := filepath.Join(outsideDir, "settings-elsewhere.json")
	if err := os.WriteFile(outsideTarget, []byte(`{}`), 0o600); err != nil {
		t.Fatalf("write outside target: %v", err)
	}
	link := claudecode.SettingsPath(r)
	if err := os.Symlink(outsideTarget, link); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	_, _, err := runImport(t, r)
	if !errors.Is(err, claudecode.ErrOutsideHome) {
		t.Fatalf("Import on out-of-HOME symlink: err = %v, want ErrOutsideHome", err)
	}
	// Wrapping preserves storage.ErrOutsideHome so callers that
	// already errors.Is on the shared sentinel (as the write-path
	// does) keep matching.
	if !errors.Is(err, storage.ErrOutsideHome) {
		t.Errorf("err = %v, want errors.Is(err, storage.ErrOutsideHome)", err)
	}
}

func TestImport_DanglingSymlinkTreatedAsMissing(t *testing.T) {
	// A symlink whose target does not exist → present the missing
	// config UX (ErrNoConfig), not a stat error. Callers already have
	// a clean branch for "nothing to import".
	r := newResolver(t)
	if err := os.MkdirAll(filepath.Join(r.Home(), ".claude"), 0o755); err != nil {
		t.Fatalf("mkdir .claude: %v", err)
	}
	link := claudecode.SettingsPath(r)
	if err := os.Symlink(filepath.Join(r.Home(), ".claude", "does-not-exist.json"), link); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	_, _, err := runImport(t, r)
	if !errors.Is(err, claudecode.ErrNoConfig) {
		t.Fatalf("Import on dangling symlink: err = %v, want ErrNoConfig", err)
	}
}

func TestImport_ContextCanceled(t *testing.T) {
	// Pre-canceled ctx: Import must return ctx.Err() BEFORE any disk
	// I/O — mirrors Detect's contract.
	r := newResolver(t)
	writeSettings(t, r, `{"env":{"ANTHROPIC_BASE_URL":"https://never-read"}}`)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, _, err := claudecode.New().Import(ctx, r)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Import on canceled ctx: err = %v, want context.Canceled", err)
	}
}

func TestImport_NonStringEnvCoerced(t *testing.T) {
	// Claude Code writes env values as strings, but a hand-edited or
	// tool-migrated file may sneak a bool / number into an owned env
	// slot. Coerce rather than drop; the value is still round-tripped
	// through the profile. Cover every non-string arm of the
	// coerceToString switch (bool true/false, integer float64,
	// fractional float64, nil) so the coercion surface has explicit
	// asserted behaviour instead of implicit fallback.
	r := newResolver(t)
	writeSettings(t, r, `{
  "env": {
    "CLAUDE_CODE_USE_BEDROCK": true,
    "CLAUDE_CODE_USE_VERTEX": false,
    "ANTHROPIC_MODEL": 5,
    "ANTHROPIC_SMALL_FAST_MODEL": 3.14,
    "ANTHROPIC_BASE_URL": null
  }
}`)

	core, overlay, err := runImport(t, r)
	if err != nil {
		t.Fatalf("Import: %v", err)
	}
	if core.Model != "5" {
		t.Errorf("core.Model = %q, want %q (integer coerced)", core.Model, "5")
	}
	if core.SmallFastModel != "3.14" {
		t.Errorf("core.SmallFastModel = %q, want %q (float coerced)", core.SmallFastModel, "3.14")
	}
	if core.BaseURL != "" {
		t.Errorf("core.BaseURL = %q, want empty (null coerced)", core.BaseURL)
	}
	if got := overlay.ExtraEnv["CLAUDE_CODE_USE_BEDROCK"]; got != "true" {
		t.Errorf("overlay.ExtraEnv[CLAUDE_CODE_USE_BEDROCK] = %q, want %q (bool true coerced)", got, "true")
	}
	if got := overlay.ExtraEnv["CLAUDE_CODE_USE_VERTEX"]; got != "false" {
		t.Errorf("overlay.ExtraEnv[CLAUDE_CODE_USE_VERTEX] = %q, want %q (bool false coerced)", got, "false")
	}
}

func TestImport_ArrayEnvCoerced(t *testing.T) {
	// Defense-in-depth for the coerceToString default arm: an array
	// value under an owned env key. Coerce via fmt %v rather than
	// silently drop.
	r := newResolver(t)
	writeSettings(t, r, `{
  "env": {
    "ANTHROPIC_MODEL": ["a", "b"]
  }
}`)

	core, _, err := runImport(t, r)
	if err != nil {
		t.Fatalf("Import: %v", err)
	}
	if core.Model == "" {
		t.Errorf("core.Model unexpectedly empty; want a %%v-formatted array literal")
	}
}

func TestImport_NullAuthTokenDoesNotShadowAPIKey(t *testing.T) {
	// A JSON `null` at ANTHROPIC_AUTH_TOKEN is a legal Claude Code
	// shape (a user editor cleared the value without deleting the
	// key). The precedence must be decided on "non-null value
	// present", not on "key present in map", or the real API_KEY
	// gets silently demoted to Overlay.ExtraEnv while Core.APIKey is
	// zeroed. This test pins that behaviour so an accidental switch
	// back to a "has-key" check trips CI immediately.
	r := newResolver(t)
	writeSettings(t, r, `{
  "env": {
    "ANTHROPIC_AUTH_TOKEN": null,
    "ANTHROPIC_API_KEY": "sk-real"
  }
}`)

	core, overlay, err := runImport(t, r)
	if err != nil {
		t.Fatalf("Import: %v", err)
	}
	if core.APIKey != "sk-real" {
		t.Errorf("core.APIKey = %q, want %q (null AUTH_TOKEN must NOT shadow API_KEY)", core.APIKey, "sk-real")
	}
	if _, ok := overlay.ExtraEnv["ANTHROPIC_API_KEY"]; ok {
		t.Errorf("overlay.ExtraEnv should not carry ANTHROPIC_API_KEY when it was promoted to Core.APIKey; got overlay=%+v", overlay.ExtraEnv)
	}
}

func TestImport_EmptyStringAuthTokenIsPreserved(t *testing.T) {
	// Empty string is a valid non-null value: the user explicitly
	// asked Claude Code to run with an empty AUTH_TOKEN. Preserve it
	// verbatim into Core.APIKey and record the real API_KEY in the
	// overlay for round-trip fidelity. This asymmetry with the null
	// case is deliberate and documented in import.go's godoc.
	r := newResolver(t)
	writeSettings(t, r, `{
  "env": {
    "ANTHROPIC_AUTH_TOKEN": "",
    "ANTHROPIC_API_KEY": "sk-real"
  }
}`)

	core, overlay, err := runImport(t, r)
	if err != nil {
		t.Fatalf("Import: %v", err)
	}
	if core.APIKey != "" {
		t.Errorf("core.APIKey = %q, want empty (explicit empty AUTH_TOKEN wins per policy)", core.APIKey)
	}
	if got := overlay.ExtraEnv["ANTHROPIC_API_KEY"]; got != "sk-real" {
		t.Errorf("overlay.ExtraEnv[ANTHROPIC_API_KEY] = %q, want %q", got, "sk-real")
	}
}

func TestImport_ParentSymlinkOutsideHomeRefused(t *testing.T) {
	// The parent directory ~/.claude is a symlink to an out-of-HOME
	// target. An earlier revision only Lstat'd the leaf settings.json
	// and would happily read through the parent symlink. The FULL
	// path must be resolved via EvalSymlinks and checked against
	// HOME; anything escaping HOME is refused with ErrOutsideHome.
	r := newResolver(t)
	outsideDir := t.TempDir()
	outsideClaude := filepath.Join(outsideDir, ".claude")
	if err := os.MkdirAll(outsideClaude, 0o755); err != nil {
		t.Fatalf("mkdir outside .claude: %v", err)
	}
	if err := os.WriteFile(filepath.Join(outsideClaude, "settings.json"), []byte(`{}`), 0o600); err != nil {
		t.Fatalf("write outside settings.json: %v", err)
	}
	link := filepath.Join(r.Home(), ".claude")
	if err := os.Symlink(outsideClaude, link); err != nil {
		t.Fatalf("symlink parent: %v", err)
	}

	_, _, err := runImport(t, r)
	if !errors.Is(err, claudecode.ErrOutsideHome) {
		t.Fatalf("Import through parent symlink out of HOME: err = %v, want ErrOutsideHome", err)
	}
	if !errors.Is(err, storage.ErrOutsideHome) {
		t.Errorf("err = %v, want errors.Is(err, storage.ErrOutsideHome)", err)
	}
}

func TestImport_EnvKeyNotObjectRefused(t *testing.T) {
	// A user typo like `"env": "sk-real"` (scalar instead of object)
	// would flatten to the single key "env" and every owned
	// env.ANTHROPIC_* lookup would miss — silent under-import. Refuse
	// loudly with ErrParseFailed so the operator sees the typo.
	r := newResolver(t)
	writeSettings(t, r, `{"env": "sk-typo"}`)

	_, _, err := runImport(t, r)
	if !errors.Is(err, claudecode.ErrParseFailed) {
		t.Fatalf("Import on non-object env: err = %v, want ErrParseFailed", err)
	}
	if !errors.Is(err, writepath.ErrParseFailed) {
		t.Errorf("err = %v, want errors.Is(err, writepath.ErrParseFailed) (wrapped)", err)
	}
	if !strings.Contains(err.Error(), "env") {
		t.Errorf("err = %v, want message to mention 'env' so the operator can locate the typo", err)
	}
}

func TestImport_UnreadableFilePermission(t *testing.T) {
	// A settings.json that exists but is unreadable (mode 0) exercises
	// the "read %q: %w" branch — a permission error that is neither
	// ErrNotExist (which maps to ErrNoConfig) nor a parse failure.
	// Running as root bypasses mode 0 so we skip; skipping keeps the
	// test honest rather than lying with a t.Skip("...") elsewhere.
	if os.Geteuid() == 0 {
		t.Skip("running as root; mode 0 does not block reads")
	}
	r := newResolver(t)
	path := writeSettings(t, r, `{}`)
	if err := os.Chmod(path, 0); err != nil {
		t.Fatalf("chmod 0: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(path, 0o600) })

	_, _, err := runImport(t, r)
	if err == nil {
		t.Fatalf("Import on unreadable file: err = nil, want a permission error")
	}
	if errors.Is(err, claudecode.ErrNoConfig) || errors.Is(err, claudecode.ErrParseFailed) || errors.Is(err, claudecode.ErrOutsideHome) {
		t.Errorf("err = %v, want a non-sentinel read error (raw filesystem error, not remapped)", err)
	}
}
