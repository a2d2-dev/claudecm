package cmd

// import_test.go — Story E6-S4 tests for the cmd/import surface.
//
// Test isolation strategy mirrors cmd/add_test.go / cmd/switch_test.go:
//
//  1. HOME points at a per-test t.TempDir() so storage.Default() reads
//     a fresh tree and the developer's real ~/.claudecm is never
//     touched.
//  2. resetImportFlags restores the cobra flag package-vars so ordering
//     between tests is irrelevant.
//  3. runImportInner wraps runImport with bytes.Buffers for stdout /
//     stderr capture so no /dev/stdout wiring is needed.
//  4. Timestamps are pinned via SetNowForTest to a fixed value so
//     JSON/YAML preview assertions stay stable.
//  5. clearAdapterEnv (from explain_test.go) wipes the per-adapter env
//     allowlist so ambient env cannot leak into extracted overlays.
//  6. isTerminalFn (from switch.go) is stubbed to false unless a
//     specific test needs an interactive-TTY path.
//
// t.Setenv makes each test non-parallel — the whole file runs
// sequentially by construction.

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"github.com/a2d2-dev/claudecm/internal/config"
	"github.com/a2d2-dev/claudecm/internal/storage"
)

// resetImportFlags restores the package-level flag vars to their init()
// defaults. Every test calls this before mutating them.
func resetImportFlags() {
	importNameFlag = ""
	importYesFlag = false
	importOverwriteFlag = false
	importDryRunFlag = false
	importDescriptionFlag = ""
	importOutputFlag = "text"
}

// importHarness wires the per-test HOME tree.
type importHarness struct {
	t     *testing.T
	home  string
	resv  *storage.Resolver
	store *storage.FileStorage
}

func newImportHarness(t *testing.T) *importHarness {
	t.Helper()
	clearAdapterEnv(t)
	resetImportFlags()

	home := t.TempDir()
	t.Setenv("HOME", home)
	resv, err := storage.NewResolverWithHome(home)
	if err != nil {
		t.Fatalf("NewResolverWithHome: %v", err)
	}
	if err := storage.Bootstrap(resv); err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	store := storage.NewFileStorage(resv)

	fixed := time.Date(2025, 7, 2, 12, 0, 0, 0, time.UTC)
	restore := SetNowForTest(func() time.Time { return fixed })
	t.Cleanup(restore)

	// Force non-TTY by default so tests don't hang on prompts unless
	// they explicitly ask for the interactive path.
	restoreTTY := SetIsTerminalForTest(func(*os.File) bool { return false })
	t.Cleanup(restoreTTY)

	return &importHarness{
		t:     t,
		home:  home,
		resv:  resv,
		store: store,
	}
}

// seedClaudeSettings drops ~/.claude/settings.json under the per-test
// HOME with the given body.
func (h *importHarness) seedClaudeSettings(body string) string {
	h.t.Helper()
	dir := filepath.Join(h.home, ".claude")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		h.t.Fatalf("mkdir .claude: %v", err)
	}
	path := filepath.Join(dir, "settings.json")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		h.t.Fatalf("write settings.json: %v", err)
	}
	return path
}

// seedCodexConfig writes ~/.codex/config.toml with the given body.
func (h *importHarness) seedCodexConfig(body string) string {
	h.t.Helper()
	dir := filepath.Join(h.home, ".codex")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		h.t.Fatalf("mkdir .codex: %v", err)
	}
	path := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		h.t.Fatalf("write config.toml: %v", err)
	}
	return path
}

// seedCodexAuth writes ~/.codex/auth.json with the given body.
func (h *importHarness) seedCodexAuth(body string) string {
	h.t.Helper()
	dir := filepath.Join(h.home, ".codex")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		h.t.Fatalf("mkdir .codex: %v", err)
	}
	path := filepath.Join(dir, "auth.json")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		h.t.Fatalf("write auth.json: %v", err)
	}
	return path
}

// runImportInner invokes runImport with a synthetic cobra.Command whose
// Out/Err are bytes.Buffers. Returns captured stdout, stderr, and the
// error return.
func runImportInner(t *testing.T, args ...string) (stdout, stderr string, err error) {
	t.Helper()
	var out, errBuf bytes.Buffer
	cmd := &cobra.Command{Use: "import"}
	cmd.SetOut(&out)
	cmd.SetErr(&errBuf)
	err = runImport(cmd, args)
	return out.String(), errBuf.String(), err
}

// ---------------------------------------------------------------------------
// Happy path
// ---------------------------------------------------------------------------

// TestImport_ClaudeCodeHappy seeds ~/.claude/settings.json with every
// owned key and asserts the saved profile carries the expected Core +
// Overlay values with schema_version: 1.
func TestImport_ClaudeCodeHappy(t *testing.T) {
	h := newImportHarness(t)
	h.seedClaudeSettings(`{
  "env": {
    "ANTHROPIC_BASE_URL": "https://api.example.com",
    "ANTHROPIC_AUTH_TOKEN": "sk-happy-import-tok",
    "ANTHROPIC_MODEL": "claude-opus-4-5",
    "ANTHROPIC_SMALL_FAST_MODEL": "claude-haiku-4-5",
    "CLAUDE_CODE_USE_BEDROCK": "1"
  }
}`)

	importNameFlag = "work"
	importYesFlag = true

	stdout, _, err := runImportInner(t, "claude-code")
	if err != nil {
		t.Fatalf("runImport: %v", err)
	}
	if !strings.Contains(stdout, `profile "work"`) {
		t.Fatalf("stdout missing confirmation: %q", stdout)
	}

	loaded, err := h.store.LoadProfile("work")
	if err != nil {
		t.Fatalf("LoadProfile: %v", err)
	}
	if loaded.SchemaVersion != config.CurrentProfileSchemaVersion {
		t.Errorf("SchemaVersion: got %d want %d", loaded.SchemaVersion, config.CurrentProfileSchemaVersion)
	}
	if loaded.Core.BaseURL != "https://api.example.com" {
		t.Errorf("Core.BaseURL: got %q", loaded.Core.BaseURL)
	}
	if loaded.Core.APIKey != "sk-happy-import-tok" {
		t.Errorf("Core.APIKey: got %q", loaded.Core.APIKey)
	}
	if loaded.Core.Model != "claude-opus-4-5" {
		t.Errorf("Core.Model: got %q", loaded.Core.Model)
	}
	if loaded.Core.SmallFastModel != "claude-haiku-4-5" {
		t.Errorf("Core.SmallFastModel: got %q", loaded.Core.SmallFastModel)
	}
	ov, ok := loaded.Tools[config.ToolClaudeCode]
	if !ok {
		t.Fatalf("Tools[claude_code] absent; got %+v", loaded.Tools)
	}
	if ov.ExtraEnv["CLAUDE_CODE_USE_BEDROCK"] != "1" {
		t.Errorf("Overlay ExtraEnv[CLAUDE_CODE_USE_BEDROCK]: got %q", ov.ExtraEnv["CLAUDE_CODE_USE_BEDROCK"])
	}
}

// TestImport_CodexHappyBothFiles seeds both codex files and asserts
// Core.APIKey lands from auth.json and the config.toml owned keys land
// in Overlay.Raw.
func TestImport_CodexHappyBothFiles(t *testing.T) {
	h := newImportHarness(t)
	h.seedCodexAuth(`{"OPENAI_API_KEY": "sk-codex-happy-key"}`)
	h.seedCodexConfig(`model = "gpt-5"
model_provider = "openai"
approval_mode = "auto"
`)

	importNameFlag = "codex-work"
	importYesFlag = true

	if _, _, err := runImportInner(t, "codex"); err != nil {
		t.Fatalf("runImport: %v", err)
	}

	loaded, err := h.store.LoadProfile("codex-work")
	if err != nil {
		t.Fatalf("LoadProfile: %v", err)
	}
	if loaded.Core.APIKey != "sk-codex-happy-key" {
		t.Errorf("Core.APIKey: got %q", loaded.Core.APIKey)
	}
	ov, ok := loaded.Tools[config.ToolCodex]
	if !ok {
		t.Fatalf("Tools[codex] absent; got %+v", loaded.Tools)
	}
	if got := ov.Raw["model"]; got != "gpt-5" {
		t.Errorf("Overlay Raw[model]: got %v", got)
	}
	if got := ov.Raw["model_provider"]; got != "openai" {
		t.Errorf("Overlay Raw[model_provider]: got %v", got)
	}
	if got := ov.Raw["approval_mode"]; got != "auto" {
		t.Errorf("Overlay Raw[approval_mode]: got %v", got)
	}
}

// TestImport_DoesNotAutoActivate asserts state.CurrentProfile is
// untouched by import (story AC: activation is a switch step).
func TestImport_DoesNotAutoActivate(t *testing.T) {
	h := newImportHarness(t)
	h.seedClaudeSettings(`{"env":{"ANTHROPIC_BASE_URL":"https://x","ANTHROPIC_AUTH_TOKEN":"sk-auth-1234"}}`)

	// Seed a pre-existing active profile so import cannot pretend to
	// have left an empty state alone.
	state, err := h.store.LoadState()
	if err != nil {
		t.Fatalf("LoadState: %v", err)
	}
	state.SetCurrentProfile("preexisting")
	if err := h.store.SaveState(state); err != nil {
		t.Fatalf("SaveState: %v", err)
	}

	importNameFlag = "work"
	importYesFlag = true
	if _, _, err := runImportInner(t, "claude-code"); err != nil {
		t.Fatalf("runImport: %v", err)
	}

	after, err := h.store.LoadState()
	if err != nil {
		t.Fatalf("LoadState after: %v", err)
	}
	if after.CurrentProfile != "preexisting" {
		t.Fatalf("CurrentProfile changed: got %q want %q", after.CurrentProfile, "preexisting")
	}
}

// ---------------------------------------------------------------------------
// Edge cases
// ---------------------------------------------------------------------------

// TestImport_NoConfigOnDiskErrors: no settings.json on disk → error
// mentioning the tool name.
func TestImport_NoConfigOnDiskErrors(t *testing.T) {
	newImportHarness(t)
	importNameFlag = "work"
	importYesFlag = true

	_, _, err := runImportInner(t, "claude-code")
	if err == nil {
		t.Fatal("expected error on missing config")
	}
	if !strings.Contains(err.Error(), "no claude-code configuration found") {
		t.Errorf("error = %v, want mention of 'no claude-code configuration found'", err)
	}
}

// TestImport_MalformedConfigErrors: malformed on-disk file → refuse
// with "malformed" in the error text (NFR-S1).
func TestImport_MalformedConfigErrors(t *testing.T) {
	h := newImportHarness(t)
	h.seedClaudeSettings(`{"env":`) // truncated

	importNameFlag = "work"
	importYesFlag = true

	_, _, err := runImportInner(t, "claude-code")
	if err == nil {
		t.Fatal("expected error on malformed config")
	}
	if !strings.Contains(err.Error(), "malformed") {
		t.Errorf("error = %v, want 'malformed' prefix", err)
	}

	// No profile should have been written.
	entries, _ := os.ReadDir(filepath.Join(h.home, ".claudecm", "profiles"))
	if len(entries) != 0 {
		t.Errorf("unexpected files written on malformed source: %v", entries)
	}
}

// TestImport_YesWithoutNameErrors: --yes but no --name → error.
func TestImport_YesWithoutNameErrors(t *testing.T) {
	newImportHarness(t)
	importYesFlag = true
	// no --name

	_, _, err := runImportInner(t, "claude-code")
	if err == nil {
		t.Fatal("expected error on --yes without --name")
	}
	if !strings.Contains(err.Error(), "--yes requires --name") {
		t.Errorf("error = %v, want '--yes requires --name'", err)
	}
}

// TestImport_NameCollisionRefused: profile of the same name exists
// without --overwrite → refused, original file bytes unchanged.
func TestImport_NameCollisionRefused(t *testing.T) {
	h := newImportHarness(t)
	h.seedClaudeSettings(`{"env":{"ANTHROPIC_BASE_URL":"https://a","ANTHROPIC_AUTH_TOKEN":"sk-first"}}`)

	// Save a profile by name first, then attempt import with same name.
	initial := config.NewProfile("work", "https://original.example.com", "sk-original-1234")
	initial.Core.Model = "original-model"
	if err := h.store.SaveProfile(initial); err != nil {
		t.Fatalf("SaveProfile initial: %v", err)
	}
	originalPath := filepath.Join(h.home, ".claudecm", "profiles", "work.yaml")
	originalBytes, err := os.ReadFile(originalPath)
	if err != nil {
		t.Fatalf("read original: %v", err)
	}

	importNameFlag = "work"
	importYesFlag = true

	_, _, err = runImportInner(t, "claude-code")
	if err == nil {
		t.Fatal("expected error on name collision without --overwrite")
	}
	if !strings.Contains(err.Error(), "already exists") {
		t.Errorf("error = %v, want 'already exists'", err)
	}

	// Original bytes on disk unchanged.
	afterBytes, err := os.ReadFile(originalPath)
	if err != nil {
		t.Fatalf("read after: %v", err)
	}
	if !bytes.Equal(originalBytes, afterBytes) {
		t.Errorf("collision refusal touched profile bytes:\nbefore=%s\nafter=%s", originalBytes, afterBytes)
	}
}

// TestImport_NameCollisionWithOverwriteReplaces: --overwrite lets the
// second import replace the profile bytes on disk.
func TestImport_NameCollisionWithOverwriteReplaces(t *testing.T) {
	h := newImportHarness(t)
	h.seedClaudeSettings(`{"env":{"ANTHROPIC_BASE_URL":"https://replaced.example.com","ANTHROPIC_AUTH_TOKEN":"sk-replaced-token"}}`)

	initial := config.NewProfile("work", "https://original.example.com", "sk-original-1234")
	initial.Core.Model = "original-model"
	if err := h.store.SaveProfile(initial); err != nil {
		t.Fatalf("SaveProfile initial: %v", err)
	}

	importNameFlag = "work"
	importYesFlag = true
	importOverwriteFlag = true

	if _, _, err := runImportInner(t, "claude-code"); err != nil {
		t.Fatalf("runImport: %v", err)
	}

	loaded, err := h.store.LoadProfile("work")
	if err != nil {
		t.Fatalf("LoadProfile: %v", err)
	}
	if loaded.Core.BaseURL != "https://replaced.example.com" {
		t.Errorf("Core.BaseURL: got %q; want the imported value", loaded.Core.BaseURL)
	}
	if loaded.Core.APIKey != "sk-replaced-token" {
		t.Errorf("Core.APIKey: got %q", loaded.Core.APIKey)
	}
}

// TestImport_DryRunNoWrite: --dry-run prints a preview and writes
// nothing under ~/.claudecm/profiles/.
func TestImport_DryRunNoWrite(t *testing.T) {
	h := newImportHarness(t)
	h.seedClaudeSettings(`{"env":{"ANTHROPIC_BASE_URL":"https://api.example.com","ANTHROPIC_AUTH_TOKEN":"sk-dry-run-token-abc"}}`)

	importNameFlag = "work"
	importDryRunFlag = true

	stdout, _, err := runImportInner(t, "claude-code")
	if err != nil {
		t.Fatalf("runImport: %v", err)
	}
	if !strings.Contains(stdout, "dry-run") {
		t.Errorf("stdout missing 'dry-run' marker:\n%s", stdout)
	}
	if !strings.Contains(stdout, "https://api.example.com") {
		t.Errorf("preview missing base_url:\n%s", stdout)
	}
	// Secret must be redacted, never appear verbatim.
	if strings.Contains(stdout, "sk-dry-run-token-abc") {
		t.Errorf("preview leaked plaintext api key:\n%s", stdout)
	}

	// No profile file on disk.
	entries, _ := os.ReadDir(filepath.Join(h.home, ".claudecm", "profiles"))
	if len(entries) != 0 {
		t.Errorf("dry-run wrote profile files: %v", entries)
	}
}

// TestImport_InvalidNameRefused: --name ../evil → refused, no file written.
func TestImport_InvalidNameRefused(t *testing.T) {
	h := newImportHarness(t)
	h.seedClaudeSettings(`{"env":{"ANTHROPIC_BASE_URL":"https://x","ANTHROPIC_AUTH_TOKEN":"sk-invalid-name"}}`)

	importNameFlag = "../evil"
	importYesFlag = true

	_, _, err := runImportInner(t, "claude-code")
	if err == nil {
		t.Fatal("expected error on invalid name")
	}
	entries, _ := os.ReadDir(filepath.Join(h.home, ".claudecm", "profiles"))
	if len(entries) != 0 {
		t.Errorf("invalid-name run wrote files: %v", entries)
	}
}

// TestImport_UnknownToolRefused: unknown positional → error listing
// supported values.
func TestImport_UnknownToolRefused(t *testing.T) {
	newImportHarness(t)
	importNameFlag = "x"
	importYesFlag = true

	_, _, err := runImportInner(t, "gemini")
	if err == nil {
		t.Fatal("expected error on unknown tool")
	}
	if !strings.Contains(err.Error(), "unknown tool") {
		t.Errorf("error = %v, want 'unknown tool'", err)
	}
	if !strings.Contains(err.Error(), "claude-code") || !strings.Contains(err.Error(), "codex") {
		t.Errorf("error = %v, should list valid tools", err)
	}
}

// TestImport_InvalidOutputRefused: bogus --output value → error.
func TestImport_InvalidOutputRefused(t *testing.T) {
	newImportHarness(t)
	importNameFlag = "work"
	importYesFlag = true
	importOutputFlag = "toml" // unsupported

	_, _, err := runImportInner(t, "claude-code")
	if err == nil {
		t.Fatal("expected error on invalid --output")
	}
	if !strings.Contains(err.Error(), "invalid --output") {
		t.Errorf("error = %v", err)
	}
}

// TestImport_NoNameNonTTYErrors: no --name, no --yes, stdin not a TTY →
// import must fail loudly rather than hang on a prompt.
func TestImport_NoNameNonTTYErrors(t *testing.T) {
	h := newImportHarness(t)
	h.seedClaudeSettings(`{"env":{"ANTHROPIC_BASE_URL":"https://x","ANTHROPIC_AUTH_TOKEN":"sk-nonttyname"}}`)

	// No importNameFlag, no importYesFlag; harness stubs isTerminalFn
	// to false so stdin is treated as non-TTY.

	_, _, err := runImportInner(t, "claude-code")
	if err == nil {
		t.Fatal("expected error on non-TTY without --name")
	}
	if !strings.Contains(err.Error(), "--name is required") {
		t.Errorf("error = %v, want '--name is required'", err)
	}
}

// ---------------------------------------------------------------------------
// Round-trip fidelity (SM-2 at CLI layer)
// ---------------------------------------------------------------------------

// TestImport_ClaudeCodeRoundTrip seeds all owned keys, imports, and
// asserts every owned-key value is captured verbatim in the saved
// profile. This is the CLI-layer SM-2 evidence: an import → save →
// reload cycle preserves the on-disk values byte-identically for
// every owned key.
func TestImport_ClaudeCodeRoundTrip(t *testing.T) {
	h := newImportHarness(t)
	h.seedClaudeSettings(`{
  "env": {
    "ANTHROPIC_API_KEY": "sk-api-key-round",
    "ANTHROPIC_AUTH_TOKEN": "sk-auth-round-primary",
    "ANTHROPIC_BASE_URL": "https://round-trip.example.com",
    "ANTHROPIC_MODEL": "claude-round-model",
    "ANTHROPIC_SMALL_FAST_MODEL": "claude-round-small",
    "CLAUDE_CODE_USE_BEDROCK": "1",
    "CLAUDE_CODE_USE_VERTEX": "0"
  }
}`)
	importNameFlag = "round"
	importYesFlag = true

	if _, _, err := runImportInner(t, "claude-code"); err != nil {
		t.Fatalf("runImport: %v", err)
	}

	loaded, err := h.store.LoadProfile("round")
	if err != nil {
		t.Fatalf("LoadProfile: %v", err)
	}

	// AUTH_TOKEN wins Core.APIKey per adapter contract.
	if loaded.Core.APIKey != "sk-auth-round-primary" {
		t.Errorf("Core.APIKey: got %q want AUTH_TOKEN value", loaded.Core.APIKey)
	}
	if loaded.Core.BaseURL != "https://round-trip.example.com" {
		t.Errorf("Core.BaseURL: got %q", loaded.Core.BaseURL)
	}
	if loaded.Core.Model != "claude-round-model" {
		t.Errorf("Core.Model: got %q", loaded.Core.Model)
	}
	if loaded.Core.SmallFastModel != "claude-round-small" {
		t.Errorf("Core.SmallFastModel: got %q", loaded.Core.SmallFastModel)
	}

	ov := loaded.Tools[config.ToolClaudeCode]
	// ANTHROPIC_API_KEY was shadowed by AUTH_TOKEN so it's preserved in
	// Overlay.ExtraEnv per the adapter contract.
	if got := ov.ExtraEnv["ANTHROPIC_API_KEY"]; got != "sk-api-key-round" {
		t.Errorf("ExtraEnv[ANTHROPIC_API_KEY]: got %q", got)
	}
	if got := ov.ExtraEnv["CLAUDE_CODE_USE_BEDROCK"]; got != "1" {
		t.Errorf("ExtraEnv[CLAUDE_CODE_USE_BEDROCK]: got %q", got)
	}
	if got := ov.ExtraEnv["CLAUDE_CODE_USE_VERTEX"]; got != "0" {
		t.Errorf("ExtraEnv[CLAUDE_CODE_USE_VERTEX]: got %q", got)
	}
}

// ---------------------------------------------------------------------------
// JSON output
// ---------------------------------------------------------------------------

// TestImport_DryRunJSONOutputParses asserts --dry-run --output json emits
// a valid JSON document with the expected shape and redacted secrets.
func TestImport_DryRunJSONOutputParses(t *testing.T) {
	h := newImportHarness(t)
	h.seedClaudeSettings(`{"env":{"ANTHROPIC_BASE_URL":"https://api.json.example.com","ANTHROPIC_AUTH_TOKEN":"sk-json-plaintext-key"}}`)

	importNameFlag = "jsonprofile"
	importDryRunFlag = true
	importOutputFlag = "json"

	stdout, _, err := runImportInner(t, "claude-code")
	if err != nil {
		t.Fatalf("runImport: %v", err)
	}

	var doc struct {
		Action  string `json:"action"`
		Tool    string `json:"tool"`
		Profile struct {
			SchemaVersion int    `json:"schema_version"`
			Name          string `json:"name"`
			Core          struct {
				BaseURL string `json:"base_url"`
				APIKey  string `json:"api_key"`
			} `json:"core"`
		} `json:"profile"`
		YAML string `json:"yaml"`
	}
	if err := json.Unmarshal([]byte(stdout), &doc); err != nil {
		t.Fatalf("JSON parse: %v\n%s", err, stdout)
	}
	if doc.Action != "dry-run" {
		t.Errorf("action: got %q want dry-run", doc.Action)
	}
	if doc.Tool != "claude-code" {
		t.Errorf("tool: got %q", doc.Tool)
	}
	if doc.Profile.Name != "jsonprofile" {
		t.Errorf("Name: got %q", doc.Profile.Name)
	}
	if doc.Profile.SchemaVersion != config.CurrentProfileSchemaVersion {
		t.Errorf("SchemaVersion: got %d", doc.Profile.SchemaVersion)
	}
	if doc.Profile.Core.BaseURL != "https://api.json.example.com" {
		t.Errorf("Core.BaseURL: got %q", doc.Profile.Core.BaseURL)
	}
	if doc.Profile.Core.APIKey == "" {
		t.Error("Core.APIKey empty in JSON dry-run body")
	}
	if strings.Contains(doc.Profile.Core.APIKey, "sk-json-plaintext-key") {
		t.Errorf("JSON body leaked plaintext api key: %q", doc.Profile.Core.APIKey)
	}
	if doc.YAML == "" {
		t.Error("JSON body missing yaml field")
	}
}

// TestImport_SuccessJSONOutput asserts --yes --output json emits a
// valid "created" JSON document after saving.
func TestImport_SuccessJSONOutput(t *testing.T) {
	h := newImportHarness(t)
	h.seedClaudeSettings(`{"env":{"ANTHROPIC_BASE_URL":"https://succ.example.com","ANTHROPIC_AUTH_TOKEN":"sk-succ-token"}}`)

	importNameFlag = "successjson"
	importYesFlag = true
	importOutputFlag = "json"

	stdout, _, err := runImportInner(t, "claude-code")
	if err != nil {
		t.Fatalf("runImport: %v", err)
	}
	var doc struct {
		Action string `json:"action"`
		Tool   string `json:"tool"`
	}
	if err := json.Unmarshal([]byte(stdout), &doc); err != nil {
		t.Fatalf("JSON parse: %v\n%s", err, stdout)
	}
	if doc.Action != "created" {
		t.Errorf("action: got %q want created", doc.Action)
	}
	if doc.Tool != "claude-code" {
		t.Errorf("tool: got %q", doc.Tool)
	}

	if _, err := h.store.LoadProfile("successjson"); err != nil {
		t.Fatalf("LoadProfile after json save: %v", err)
	}
}

// TestImport_CodexNoConfigMessagesCodex ensures the ErrNoConfig message
// names the tool the operator asked about.
func TestImport_CodexNoConfigMessagesCodex(t *testing.T) {
	newImportHarness(t)
	importNameFlag = "cx"
	importYesFlag = true

	_, _, err := runImportInner(t, "codex")
	if err == nil {
		t.Fatal("expected ErrNoConfig")
	}
	if !strings.Contains(err.Error(), "no codex configuration found") {
		t.Errorf("error = %v", err)
	}
}

// TestImport_InteractivePromptFlow drives the TTY prompt path: fake
// isTerminalFn=true and pipe "answer\ny\n" into stdin so the prompt
// reads the profile name and confirms.
func TestImport_InteractivePromptFlow(t *testing.T) {
	h := newImportHarness(t)
	h.seedClaudeSettings(`{"env":{"ANTHROPIC_BASE_URL":"https://tty.example.com","ANTHROPIC_AUTH_TOKEN":"sk-tty-token"}}`)

	// Simulate a TTY so the interactive prompts fire.
	restore := SetIsTerminalForTest(func(*os.File) bool { return true })
	t.Cleanup(restore)

	// Pipe "myimport\ny\n" to os.Stdin so the two prompts (name +
	// confirmation) both read successfully. Restore os.Stdin after.
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	origStdin := os.Stdin
	os.Stdin = r
	t.Cleanup(func() { os.Stdin = origStdin })
	if _, err := w.Write([]byte("myimport\ny\n")); err != nil {
		t.Fatalf("write pipe: %v", err)
	}
	_ = w.Close()

	stdout, _, err := runImportInner(t, "claude-code")
	if err != nil {
		t.Fatalf("runImport: %v", err)
	}
	if !strings.Contains(stdout, "Save as [claude-code]:") {
		t.Errorf("stdout missing name prompt: %q", stdout)
	}
	if !strings.Contains(stdout, "Save this profile?") {
		t.Errorf("stdout missing confirm prompt: %q", stdout)
	}

	loaded, err := h.store.LoadProfile("myimport")
	if err != nil {
		t.Fatalf("LoadProfile: %v", err)
	}
	if loaded.Core.BaseURL != "https://tty.example.com" {
		t.Errorf("Core.BaseURL: got %q", loaded.Core.BaseURL)
	}
}

// TestImport_DryRunCodexRenderCoverage covers the codex tool arm of
// the preview renderer: Overlay.Raw entries, description, and both
// text/JSON overlay branches.
func TestImport_DryRunCodexRenderCoverage(t *testing.T) {
	h := newImportHarness(t)
	h.seedCodexAuth(`{"OPENAI_API_KEY": "sk-codex-preview", "auth_mode": "api_key"}`)
	h.seedCodexConfig(`model = "gpt-5"
model_provider = "openai"
approval_mode = "auto"
[model_providers.openai]
base_url = "https://api.example.com"
env_key = "OPENAI_API_KEY"
name = "OpenAI"
wire_api = "responses"
`)

	importNameFlag = "cx"
	importDryRunFlag = true
	importDescriptionFlag = "codex preview run"

	stdout, _, err := runImportInner(t, "codex")
	if err != nil {
		t.Fatalf("runImport: %v", err)
	}
	// Every overlay-related branch should render.
	for _, want := range []string{
		"codex preview run",
		"tools:",
		"codex:",
		"raw:",
		"model:",
		"model_provider:",
		"approval_mode:",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("stdout missing %q\n%s", want, stdout)
		}
	}
	// API key must be redacted.
	if strings.Contains(stdout, "sk-codex-preview") {
		t.Errorf("preview leaked plaintext api key:\n%s", stdout)
	}
}

// TestImport_DryRunJSONCodexOverlay drives the JSON overlay-emit
// branches: Raw, ExtraEnv entries under overlays.
func TestImport_DryRunJSONCodexOverlay(t *testing.T) {
	h := newImportHarness(t)
	h.seedCodexAuth(`{"OPENAI_API_KEY": "sk-cxjson-abc"}`)
	h.seedCodexConfig(`model = "gpt-5"
`)

	importNameFlag = "cxjson"
	importDryRunFlag = true
	importOutputFlag = "json"

	stdout, _, err := runImportInner(t, "codex")
	if err != nil {
		t.Fatalf("runImport: %v", err)
	}
	var doc struct {
		Tool    string `json:"tool"`
		Profile struct {
			Tools map[string]struct {
				Raw map[string]string `json:"raw,omitempty"`
			} `json:"tools"`
			Core struct {
				APIKey string `json:"api_key"`
			} `json:"core"`
		} `json:"profile"`
	}
	if err := json.Unmarshal([]byte(stdout), &doc); err != nil {
		t.Fatalf("JSON parse: %v\n%s", err, stdout)
	}
	if doc.Tool != "codex" {
		t.Errorf("tool: got %q", doc.Tool)
	}
	if doc.Profile.Tools == nil || doc.Profile.Tools["codex"].Raw["model"] != "gpt-5" {
		t.Errorf("expected codex raw model=gpt-5 in JSON body:\n%s", stdout)
	}
	if strings.Contains(doc.Profile.Core.APIKey, "sk-cxjson-abc") {
		t.Errorf("JSON leaked plaintext key: %q", doc.Profile.Core.APIKey)
	}
}

// TestImport_InteractivePreviewCoverage exercises the interactive
// preview branch (--name provided + TTY + confirm=y) so the
// renderImportPreview + renderImportProfileText paths are covered under
// the non-dry-run branch too.
func TestImport_InteractivePreviewCoverage(t *testing.T) {
	h := newImportHarness(t)
	h.seedClaudeSettings(`{"env":{"ANTHROPIC_BASE_URL":"https://iv.example.com","ANTHROPIC_AUTH_TOKEN":"sk-preview-iv","ANTHROPIC_MODEL":"m","CLAUDE_CODE_USE_BEDROCK":"1"}}`)

	restore := SetIsTerminalForTest(func(*os.File) bool { return true })
	t.Cleanup(restore)

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	origStdin := os.Stdin
	os.Stdin = r
	t.Cleanup(func() { os.Stdin = origStdin })
	if _, err := w.Write([]byte("y\n")); err != nil {
		t.Fatalf("write pipe: %v", err)
	}
	_ = w.Close()

	importNameFlag = "iv"
	stdout, _, err := runImportInner(t, "claude-code")
	if err != nil {
		t.Fatalf("runImport: %v", err)
	}
	// Preview headers and overlay section should render.
	for _, want := range []string{
		"preview",
		"core:",
		"tools:",
		"claude_code:",
		"extra_env:",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("stdout missing %q\n%s", want, stdout)
		}
	}
	if _, err := h.store.LoadProfile("iv"); err != nil {
		t.Fatalf("LoadProfile: %v", err)
	}
}

// TestImport_InteractivePreviewJSON covers renderImportPreview JSON
// branch (interactive but --output json).
func TestImport_InteractivePreviewJSON(t *testing.T) {
	h := newImportHarness(t)
	h.seedClaudeSettings(`{"env":{"ANTHROPIC_BASE_URL":"https://ivj.example.com","ANTHROPIC_AUTH_TOKEN":"sk-preview-jvj"}}`)

	restore := SetIsTerminalForTest(func(*os.File) bool { return true })
	t.Cleanup(restore)

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	origStdin := os.Stdin
	os.Stdin = r
	t.Cleanup(func() { os.Stdin = origStdin })
	if _, err := w.Write([]byte("y\n")); err != nil {
		t.Fatalf("write pipe: %v", err)
	}
	_ = w.Close()

	importNameFlag = "ivj"
	importOutputFlag = "json"
	stdout, _, err := runImportInner(t, "claude-code")
	if err != nil {
		t.Fatalf("runImport: %v", err)
	}
	// Both preview and success documents appear in stdout order (one
	// document each — technically two adjacent JSON docs). Ensure the
	// preview shape is present as a valid JSON prefix.
	if !strings.Contains(stdout, `"action": "preview"`) {
		t.Errorf("stdout missing preview action in JSON mode:\n%s", stdout)
	}
	if !strings.Contains(stdout, `"action": "created"`) {
		t.Errorf("stdout missing created action:\n%s", stdout)
	}
	if _, err := h.store.LoadProfile("ivj"); err != nil {
		t.Fatalf("LoadProfile: %v", err)
	}
}

// TestImport_InteractivePromptAbortsOnNo drives the TTY prompt path
// with "y\nn\n" (accept default name, then reject the save) and
// asserts no profile is written.
func TestImport_InteractivePromptAbortsOnNo(t *testing.T) {
	h := newImportHarness(t)
	h.seedClaudeSettings(`{"env":{"ANTHROPIC_BASE_URL":"https://abort.example.com","ANTHROPIC_AUTH_TOKEN":"sk-abort-token"}}`)

	restore := SetIsTerminalForTest(func(*os.File) bool { return true })
	t.Cleanup(restore)

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	origStdin := os.Stdin
	os.Stdin = r
	t.Cleanup(func() { os.Stdin = origStdin })
	// Empty line for name (accept default = "claude-code") + "n" for
	// the confirm prompt.
	if _, err := w.Write([]byte("\nn\n")); err != nil {
		t.Fatalf("write pipe: %v", err)
	}
	_ = w.Close()

	_, _, err = runImportInner(t, "claude-code")
	if err == nil {
		t.Fatal("expected 'aborted by user' error on 'n' answer")
	}
	if !strings.Contains(err.Error(), "aborted") {
		t.Errorf("error = %v", err)
	}
	entries, _ := os.ReadDir(filepath.Join(h.home, ".claudecm", "profiles"))
	if len(entries) != 0 {
		t.Errorf("abort still wrote files: %v", entries)
	}
}
