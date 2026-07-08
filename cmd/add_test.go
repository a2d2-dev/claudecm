package cmd

// add_test.go — Story E6-S3 tests for the cmd/add surface.
//
// Test isolation strategy mirrors cmd/switch_test.go /
// cmd/current_test.go / cmd/explain_test.go:
//
//  1. HOME points at a per-test t.TempDir() so storage.Default() reads
//     a fresh tree and the developer's real ~/.claudecm is never
//     touched.
//  2. resetAddFlags restores the cobra flag package-vars so ordering
//     between tests is irrelevant.
//  3. runAddInner wraps runAdd with bytes.Buffers for stdout / stderr
//     capture so no /dev/stdout wiring is needed.
//  4. Timestamps are pinned via SetNowForTest so JSON / YAML dry-run
//     assertions stay stable.
//
// t.Setenv makes each test non-parallel — the whole file runs
// sequentially by construction. add is fast (a single atomic write
// under the per-test HOME); sequential execution is fine.

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

// resetAddFlags restores the package-level flag vars to their init()
// defaults.
func resetAddFlags() {
	addDescriptionFlag = ""
	addProviderFlag = addProviderDefault
	addBaseURLFlag = ""
	addAPIKeyFlag = ""
	addModelFlag = ""
	addSmallFastModelFlag = ""
	addSetFlag = nil
	addPresetFlag = ""
	addFromEnvFlag = false
	addFromFileFlag = ""
	addListPresetsFlag = false
	addDryRunFlag = false
	addOverwriteFlag = false
	addOutputFlag = "text"
}

// addHarness wires the per-test HOME tree: TempDir, HOME rewire,
// Bootstrap, and a bound FileStorage for read-back assertions.
type addHarness struct {
	t     *testing.T
	home  string
	resv  *storage.Resolver
	store *storage.FileStorage
}

func newAddHarness(t *testing.T) *addHarness {
	t.Helper()
	resetAddFlags()

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

	// Pin timestamps for deterministic YAML/JSON assertions.
	fixed := time.Date(2025, 6, 30, 12, 0, 0, 0, time.UTC)
	restore := SetNowForTest(func() time.Time { return fixed })
	t.Cleanup(restore)

	return &addHarness{
		t:     t,
		home:  home,
		resv:  resv,
		store: store,
	}
}

// runAddInner invokes runAdd with a synthetic cobra.Command whose
// Out/Err are bytes.Buffers.
func runAddInner(t *testing.T, args ...string) (stdout, stderr string, err error) {
	t.Helper()
	var out, errBuf bytes.Buffer
	cmd := &cobra.Command{Use: "add"}
	bindSyntheticAddFlags(cmd)
	cmd.SetOut(&out)
	cmd.SetErr(&errBuf)
	err = runAdd(cmd, args)
	return out.String(), errBuf.String(), err
}

func bindSyntheticAddFlags(cmd *cobra.Command) {
	cmd.Flags().String("provider", addProviderFlag, "")
	if addProviderFlag != addProviderDefault {
		_ = cmd.Flags().Set("provider", addProviderFlag)
	}
	cmd.Flags().String("base-url", addBaseURLFlag, "")
	if addBaseURLFlag != "" {
		_ = cmd.Flags().Set("base-url", addBaseURLFlag)
	}
	cmd.Flags().String("api-key", addAPIKeyFlag, "")
	if addAPIKeyFlag != "" {
		_ = cmd.Flags().Set("api-key", addAPIKeyFlag)
	}
	cmd.Flags().String("model", addModelFlag, "")
	if addModelFlag != "" {
		_ = cmd.Flags().Set("model", addModelFlag)
	}
	cmd.Flags().String("small-fast-model", addSmallFastModelFlag, "")
	if addSmallFastModelFlag != "" {
		_ = cmd.Flags().Set("small-fast-model", addSmallFastModelFlag)
	}
}

// ---------------------------------------------------------------------------
// Happy path
// ---------------------------------------------------------------------------

// TestAdd_HappyMinimal writes a profile with the minimum core fields
// and asserts the on-disk file carries schema_version: 1 and the flag
// values, and that the load round-trips through config.ParseProfile.
func TestAdd_HappyMinimal(t *testing.T) {
	h := newAddHarness(t)

	addBaseURLFlag = "https://api.anthropic.com"
	addAPIKeyFlag = "sk-happy-minimal-1234"
	addModelFlag = "claude-opus-4-5"

	stdout, _, err := runAddInner(t, "work")
	if err != nil {
		t.Fatalf("runAdd: %v", err)
	}
	if !strings.Contains(stdout, `Profile "work" created.`) {
		t.Fatalf("stdout missing confirmation: %q", stdout)
	}

	// File on disk.
	path := filepath.Join(h.home, ".claudecm", "profiles", "work.yaml")
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	if !strings.Contains(string(body), "schema_version: 1") {
		t.Fatalf("profile YAML missing schema_version: 1\n%s", body)
	}
	if !strings.Contains(string(body), "sk-happy-minimal-1234") {
		t.Fatalf("profile YAML missing api key\n%s", body)
	}

	// Round-trip through the parser to make sure the load path
	// accepts what add wrote.
	loaded, err := h.store.LoadProfile("work")
	if err != nil {
		t.Fatalf("LoadProfile: %v", err)
	}
	if loaded.Name != "work" {
		t.Fatalf("Name: got %q want %q", loaded.Name, "work")
	}
	if loaded.Core.BaseURL != "https://api.anthropic.com" {
		t.Fatalf("BaseURL: got %q", loaded.Core.BaseURL)
	}
	if loaded.Core.APIKey != "sk-happy-minimal-1234" {
		t.Fatalf("APIKey: got %q", loaded.Core.APIKey)
	}
	if loaded.Core.Model != "claude-opus-4-5" {
		t.Fatalf("Model: got %q", loaded.Core.Model)
	}
	if loaded.Core.Provider != "anthropic" {
		t.Fatalf("Provider: got %q; want anthropic (default)", loaded.Core.Provider)
	}
	if loaded.SchemaVersion != config.CurrentProfileSchemaVersion {
		t.Fatalf("SchemaVersion: got %d want %d", loaded.SchemaVersion, config.CurrentProfileSchemaVersion)
	}
	if loaded.CreatedAt.IsZero() || loaded.UpdatedAt.IsZero() {
		t.Fatalf("timestamps zero; got created=%v updated=%v", loaded.CreatedAt, loaded.UpdatedAt)
	}
}

// TestAdd_HappyWithOverlay writes a profile with a claude_code env
// overlay and asserts the overlay round-trips through the parser.
func TestAdd_HappyWithOverlay(t *testing.T) {
	h := newAddHarness(t)

	addBaseURLFlag = "https://api.anthropic.com"
	addAPIKeyFlag = "sk-overlay-1234"
	addSetFlag = []string{"tools.claude_code.env.CLAUDE_CODE_USE_BEDROCK=1"}

	if _, _, err := runAddInner(t, "work"); err != nil {
		t.Fatalf("runAdd: %v", err)
	}

	loaded, err := h.store.LoadProfile("work")
	if err != nil {
		t.Fatalf("LoadProfile: %v", err)
	}
	ov, ok := loaded.Tools[config.ToolClaudeCode]
	if !ok {
		t.Fatalf("Tools[claude_code] absent; got %+v", loaded.Tools)
	}
	if got := ov.ExtraEnv["CLAUDE_CODE_USE_BEDROCK"]; got != "1" {
		t.Fatalf("ExtraEnv[CLAUDE_CODE_USE_BEDROCK]: got %q want %q", got, "1")
	}
}

// TestAdd_HappyCodexOverlay writes a profile with a codex raw overlay
// and asserts the entry round-trips.
func TestAdd_HappyCodexOverlay(t *testing.T) {
	h := newAddHarness(t)

	addBaseURLFlag = "https://api.openai.com"
	addAPIKeyFlag = "sk-codex-1234"
	addSetFlag = []string{"tools.codex.raw.model=gpt-5"}

	if _, _, err := runAddInner(t, "work"); err != nil {
		t.Fatalf("runAdd: %v", err)
	}

	loaded, err := h.store.LoadProfile("work")
	if err != nil {
		t.Fatalf("LoadProfile: %v", err)
	}
	ov, ok := loaded.Tools[config.ToolCodex]
	if !ok {
		t.Fatalf("Tools[codex] absent; got %+v", loaded.Tools)
	}
	if got := ov.Raw["model"]; got != "gpt-5" {
		t.Fatalf("Raw[model]: got %v want %q", got, "gpt-5")
	}
}

// TestAdd_DoesNotAutoActivate asserts that add leaves state.yaml's
// CurrentProfile untouched. Story AC #6: activation is a switch step.
func TestAdd_DoesNotAutoActivate(t *testing.T) {
	h := newAddHarness(t)

	// Seed a state with a pre-existing CurrentProfile so we can prove
	// add did not clobber it.
	state, err := h.store.LoadState()
	if err != nil {
		t.Fatalf("LoadState: %v", err)
	}
	state.SetCurrentProfile("preexisting")
	if err := h.store.SaveState(state); err != nil {
		t.Fatalf("SaveState: %v", err)
	}

	addBaseURLFlag = "https://api.anthropic.com"
	addAPIKeyFlag = "sk-noactivate-1234"
	if _, _, err := runAddInner(t, "work"); err != nil {
		t.Fatalf("runAdd: %v", err)
	}

	after, err := h.store.LoadState()
	if err != nil {
		t.Fatalf("LoadState after add: %v", err)
	}
	if after.CurrentProfile != "preexisting" {
		t.Fatalf("CurrentProfile changed: got %q want %q", after.CurrentProfile, "preexisting")
	}
}

// TestAdd_ProviderExplicit accepts a valid --provider value.
func TestAdd_ProviderExplicit(t *testing.T) {
	h := newAddHarness(t)

	addBaseURLFlag = "https://api.openai.com"
	addAPIKeyFlag = "sk-provider-1234"
	addProviderFlag = "openai-compat"
	if _, _, err := runAddInner(t, "work"); err != nil {
		t.Fatalf("runAdd: %v", err)
	}

	loaded, err := h.store.LoadProfile("work")
	if err != nil {
		t.Fatalf("LoadProfile: %v", err)
	}
	if loaded.Core.Provider != "openai-compat" {
		t.Fatalf("Provider: got %q want openai-compat", loaded.Core.Provider)
	}
}

// TestAdd_HappyDescriptionAndSmallFast round-trips optional fields.
func TestAdd_HappyDescriptionAndSmallFast(t *testing.T) {
	h := newAddHarness(t)

	addBaseURLFlag = "https://api.anthropic.com"
	addAPIKeyFlag = "sk-desc-1234"
	addDescriptionFlag = "primary work profile"
	addSmallFastModelFlag = "claude-haiku-4"
	if _, _, err := runAddInner(t, "work"); err != nil {
		t.Fatalf("runAdd: %v", err)
	}

	loaded, err := h.store.LoadProfile("work")
	if err != nil {
		t.Fatalf("LoadProfile: %v", err)
	}
	if loaded.Description != "primary work profile" {
		t.Fatalf("Description: got %q", loaded.Description)
	}
	if loaded.Core.SmallFastModel != "claude-haiku-4" {
		t.Fatalf("SmallFastModel: got %q", loaded.Core.SmallFastModel)
	}
}

func TestAdd_PresetMoonshotDryRunRedactedProfileDraft(t *testing.T) {
	h := newAddHarness(t)

	addPresetFlag = "moonshot"
	addAPIKeyFlag = "sk-preset-moonshot-1234"
	addDryRunFlag = true

	stdout, _, err := runAddInner(t, "work")
	if err != nil {
		t.Fatalf("runAdd: %v", err)
	}
	for _, want := range []string{
		"provider: moonshot",
		"base_url: https://api.moonshot.cn/v1",
		"model: kimi-k2-0711-preview",
		"model_provider: moonshot",
		"model_providers.moonshot.base_url: https://api.moonshot.cn/v1",
	} {
		if !strings.Contains(stdout, want) {
			t.Fatalf("dry-run missing %q:\n%s", want, stdout)
		}
	}
	if strings.Contains(stdout, "sk-preset-moonshot-1234") {
		t.Fatalf("dry-run leaked plaintext api key:\n%s", stdout)
	}
	if !strings.Contains(stdout, "sk-p***1234") {
		t.Fatalf("dry-run missing redacted api key:\n%s", stdout)
	}
	if _, err := os.Stat(filepath.Join(h.home, ".claudecm", "profiles", "work.yaml")); !os.IsNotExist(err) {
		t.Fatalf("profile file written despite --dry-run: %v", err)
	}
}

func TestAdd_PresetAllBuiltInsSaveOrdinaryProfiles(t *testing.T) {
	h := newAddHarness(t)

	for _, name := range []string{"deepseek", "glm", "moonshot", "qwen"} {
		resetAddFlags()
		addPresetFlag = name
		addAPIKeyFlag = "sk-" + name + "-1234567890"
		if _, _, err := runAddInner(t, name+"-profile"); err != nil {
			t.Fatalf("runAdd preset %q: %v", name, err)
		}
		loaded, err := h.store.LoadProfile(name + "-profile")
		if err != nil {
			t.Fatalf("LoadProfile(%q): %v", name+"-profile", err)
		}
		if loaded.SchemaVersion != config.CurrentProfileSchemaVersion {
			t.Fatalf("%s SchemaVersion = %d", name, loaded.SchemaVersion)
		}
		if loaded.Core.Provider != name {
			t.Fatalf("%s Provider = %q", name, loaded.Core.Provider)
		}
		if loaded.Core.APIKey != "sk-"+name+"-1234567890" {
			t.Fatalf("%s APIKey not stored from user input", name)
		}
		ov := loaded.Tools[config.ToolCodex]
		if got := ov.Raw["model_provider"]; got != name {
			t.Fatalf("%s codex model_provider = %v", name, got)
		}
	}
}

func TestAdd_PresetCaseInsensitiveAndOverrides(t *testing.T) {
	h := newAddHarness(t)

	addPresetFlag = "MoonShot"
	addAPIKeyFlag = "sk-override-1234"
	addModelFlag = "kimi-k2-latest"
	addBaseURLFlag = "https://override.example.com/v1"
	addSetFlag = []string{
		"tools.codex.raw.model_providers.moonshot.name=Moonshot Override",
	}
	if _, _, err := runAddInner(t, "work"); err != nil {
		t.Fatalf("runAdd: %v", err)
	}
	loaded, err := h.store.LoadProfile("work")
	if err != nil {
		t.Fatalf("LoadProfile: %v", err)
	}
	if loaded.Core.Provider != "moonshot" {
		t.Fatalf("Provider = %q, want canonical moonshot", loaded.Core.Provider)
	}
	if loaded.Core.Model != "kimi-k2-latest" {
		t.Fatalf("Core.Model = %q", loaded.Core.Model)
	}
	if loaded.Core.BaseURL != "https://override.example.com/v1" {
		t.Fatalf("Core.BaseURL = %q", loaded.Core.BaseURL)
	}
	ov := loaded.Tools[config.ToolCodex]
	if got := ov.Raw["model"]; got != "kimi-k2-latest" {
		t.Fatalf("codex raw model override = %v", got)
	}
	if got := ov.Raw["model_providers.moonshot.base_url"]; got != "https://override.example.com/v1" {
		t.Fatalf("codex raw base_url override = %v", got)
	}
	if got := ov.Raw["model_providers.moonshot.name"]; got != "Moonshot Override" {
		t.Fatalf("codex raw --set override = %v", got)
	}
}

func TestAdd_ListPresetsText(t *testing.T) {
	newAddHarness(t)
	addListPresetsFlag = true

	stdout, _, err := runAddInner(t)
	if err != nil {
		t.Fatalf("runAdd --list-presets: %v", err)
	}
	for _, name := range []string{"moonshot", "deepseek", "glm", "qwen"} {
		if !strings.Contains(stdout, name) {
			t.Fatalf("preset list missing %q:\n%s", name, stdout)
		}
	}
	if !strings.Contains(stdout, "convenience templates") || !strings.Contains(stdout, "not official provider support") {
		t.Fatalf("preset list missing boundary text:\n%s", stdout)
	}
}

func TestAdd_PresetUnknownAndMissingSecretRefuseWithoutWrite(t *testing.T) {
	h := newAddHarness(t)

	addPresetFlag = "unknown"
	_, _, err := runAddInner(t, "work")
	if err == nil {
		t.Fatalf("unknown preset accepted")
	}
	if !strings.Contains(err.Error(), "available presets: deepseek, glm, moonshot, qwen") {
		t.Fatalf("unknown preset error missing available list: %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(h.home, ".claudecm", "profiles", "work.yaml")); !os.IsNotExist(statErr) {
		t.Fatalf("profile file written after unknown preset: %v", statErr)
	}

	resetAddFlags()
	addPresetFlag = "moonshot"
	_, _, err = runAddInner(t, "work")
	if err == nil {
		t.Fatalf("preset without secret accepted")
	}
	if !strings.Contains(err.Error(), "requires --api-key") {
		t.Fatalf("missing secret error = %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(h.home, ".claudecm", "profiles", "work.yaml")); !os.IsNotExist(statErr) {
		t.Fatalf("profile file written after missing secret: %v", statErr)
	}
}

func TestAdd_FromFileDotenvDryRunRedactsAndDoesNotWrite(t *testing.T) {
	h := newAddHarness(t)

	path := filepath.Join(h.home, "provider.env")
	if err := os.WriteFile(path, []byte(strings.Join([]string{
		"ANTHROPIC_BASE_URL=https://envfile.example.com",
		"ANTHROPIC_AUTH_TOKEN=sk-file-dotenv-1234",
	}, "\n")), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	addFromFileFlag = path
	addDryRunFlag = true

	stdout, _, err := runAddInner(t, "work")
	if err != nil {
		t.Fatalf("runAdd --from-file dotenv: %v", err)
	}
	if !strings.Contains(stdout, "base_url: https://envfile.example.com") {
		t.Fatalf("dry-run missing base_url:\n%s", stdout)
	}
	if strings.Contains(stdout, "sk-file-dotenv-1234") {
		t.Fatalf("dry-run leaked plaintext api key:\n%s", stdout)
	}
	if !strings.Contains(stdout, "sk-f***1234") {
		t.Fatalf("dry-run missing redacted api key:\n%s", stdout)
	}
	if _, statErr := os.Stat(filepath.Join(h.home, ".claudecm", "profiles", "work.yaml")); !os.IsNotExist(statErr) {
		t.Fatalf("profile file written despite --dry-run: %v", statErr)
	}
}

func TestAdd_FromFileJSONPopulatesProfile(t *testing.T) {
	h := newAddHarness(t)

	path := filepath.Join(h.home, "provider.json")
	if err := os.WriteFile(path, []byte(`{"base_url":"https://json.example.com","api_key":"sk-json-file-1234","model":"json-model"}`), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	addFromFileFlag = path

	if _, _, err := runAddInner(t, "jsonfile"); err != nil {
		t.Fatalf("runAdd --from-file json: %v", err)
	}
	loaded, err := h.store.LoadProfile("jsonfile")
	if err != nil {
		t.Fatalf("LoadProfile: %v", err)
	}
	if loaded.Core.BaseURL != "https://json.example.com" {
		t.Fatalf("BaseURL = %q", loaded.Core.BaseURL)
	}
	if loaded.Core.APIKey != "sk-json-file-1234" {
		t.Fatalf("APIKey = %q", loaded.Core.APIKey)
	}
	if loaded.Core.Model != "json-model" {
		t.Fatalf("Model = %q", loaded.Core.Model)
	}
}

func TestAdd_FromFileShellExportModelParsed(t *testing.T) {
	h := newAddHarness(t)

	path := filepath.Join(h.home, "exports.sh")
	if err := os.WriteFile(path, []byte(strings.Join([]string{
		"export ANTHROPIC_AUTH_TOKEN=sk-shell-file-1234",
		"export ANTHROPIC_MODEL=claude-shell-model",
	}, "\n")), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	addFromFileFlag = path

	if _, _, err := runAddInner(t, "shellfile"); err != nil {
		t.Fatalf("runAdd --from-file shell: %v", err)
	}
	loaded, err := h.store.LoadProfile("shellfile")
	if err != nil {
		t.Fatalf("LoadProfile: %v", err)
	}
	if loaded.Core.Model != "claude-shell-model" {
		t.Fatalf("Model = %q", loaded.Core.Model)
	}
}

func TestAdd_FromFileExplicitModelOverridesParsedValue(t *testing.T) {
	h := newAddHarness(t)

	path := filepath.Join(h.home, "provider.json")
	if err := os.WriteFile(path, []byte(`{"api_key":"sk-file-override-1234","model":"file-model"}`), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	addFromFileFlag = path
	addModelFlag = "flag-model"

	if _, _, err := runAddInner(t, "overridefile"); err != nil {
		t.Fatalf("runAdd --from-file override: %v", err)
	}
	loaded, err := h.store.LoadProfile("overridefile")
	if err != nil {
		t.Fatalf("LoadProfile: %v", err)
	}
	if loaded.Core.Model != "flag-model" {
		t.Fatalf("Model = %q, want flag-model", loaded.Core.Model)
	}
}

func TestAdd_FromFileKeylessWithExplicitAPIKeyAllowed(t *testing.T) {
	h := newAddHarness(t)

	path := filepath.Join(h.home, "provider.json")
	if err := os.WriteFile(path, []byte(`{"base_url":"https://json.example.com","model":"json-model"}`), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	addFromFileFlag = path
	addAPIKeyFlag = "sk-flag-file-1234"

	if _, _, err := runAddInner(t, "fileflagkey"); err != nil {
		t.Fatalf("runAdd --from-file --api-key: %v", err)
	}
	loaded, err := h.store.LoadProfile("fileflagkey")
	if err != nil {
		t.Fatalf("LoadProfile: %v", err)
	}
	if loaded.Core.APIKey != "sk-flag-file-1234" {
		t.Fatalf("APIKey = %q, want flag value", loaded.Core.APIKey)
	}
}

func TestAdd_FromFileKeylessWithoutExplicitAPIKeyRefuses(t *testing.T) {
	h := newAddHarness(t)

	path := filepath.Join(h.home, "provider.json")
	if err := os.WriteFile(path, []byte(`{"base_url":"https://json.example.com","model":"json-model"}`), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	addFromFileFlag = path

	_, _, err := runAddInner(t, "filewithoutkey")
	if err == nil {
		t.Fatalf("keyless --from-file accepted without --api-key")
	}
	if !strings.Contains(err.Error(), "no API key found in input source") {
		t.Fatalf("error = %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(h.home, ".claudecm", "profiles", "filewithoutkey.yaml")); !os.IsNotExist(statErr) {
		t.Fatalf("profile file written despite missing file key: %v", statErr)
	}
}

func TestAdd_FromFileUnreadableAndGarbageRefuseWithoutWrite(t *testing.T) {
	h := newAddHarness(t)

	addFromFileFlag = filepath.Join(h.home, "missing.env")
	_, _, err := runAddInner(t, "missing")
	if err == nil {
		t.Fatalf("nonexistent --from-file accepted")
	}
	if !strings.Contains(err.Error(), "cannot read file") {
		t.Fatalf("missing file error = %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(h.home, ".claudecm", "profiles", "missing.yaml")); !os.IsNotExist(statErr) {
		t.Fatalf("profile file written after missing file: %v", statErr)
	}

	resetAddFlags()
	path := filepath.Join(h.home, "garbage.bin")
	if err := os.WriteFile(path, []byte{0x00, 0xff, 0x01}, 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	addFromFileFlag = path
	_, _, err = runAddInner(t, "garbage")
	if err == nil {
		t.Fatalf("garbage --from-file accepted")
	}
	if !strings.Contains(err.Error(), "unrecognized config format") {
		t.Fatalf("garbage file error = %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(h.home, ".claudecm", "profiles", "garbage.yaml")); !os.IsNotExist(statErr) {
		t.Fatalf("profile file written after garbage file: %v", statErr)
	}
}

// ---------------------------------------------------------------------------
// Edge cases
// ---------------------------------------------------------------------------

// TestAdd_InvalidNameRefused refuses a traversal-shaped name and does
// not write any file.
func TestAdd_InvalidNameRefused(t *testing.T) {
	h := newAddHarness(t)

	addBaseURLFlag = "https://api.anthropic.com"
	addAPIKeyFlag = "sk-invalid-1234"
	_, _, err := runAddInner(t, "../evil")
	if err == nil {
		t.Fatalf("runAdd(\"../evil\"): got nil err")
	}

	// No profiles directory entries should exist.
	entries, err := os.ReadDir(filepath.Join(h.home, ".claudecm", "profiles"))
	if err != nil {
		t.Fatalf("read profiles dir: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("unexpected files written: %v", entries)
	}
}

// TestAdd_InvalidNameEmpty refuses an empty name.
func TestAdd_InvalidNameEmpty(t *testing.T) {
	newAddHarness(t)
	addBaseURLFlag = "https://x"
	addAPIKeyFlag = "sk-x"
	_, _, err := runAddInner(t, "")
	if err == nil {
		t.Fatalf("expected error on empty name")
	}
}

// TestAdd_DuplicateNameRefused refuses to overwrite an existing
// profile without --overwrite, and the original bytes stay intact.
func TestAdd_DuplicateNameRefused(t *testing.T) {
	h := newAddHarness(t)

	// First write.
	addBaseURLFlag = "https://api.first.example.com"
	addAPIKeyFlag = "sk-first-1234"
	if _, _, err := runAddInner(t, "work"); err != nil {
		t.Fatalf("first runAdd: %v", err)
	}
	path := filepath.Join(h.home, ".claudecm", "profiles", "work.yaml")
	original, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read original: %v", err)
	}

	// Second write without --overwrite → error.
	resetAddFlags()
	addBaseURLFlag = "https://api.second.example.com"
	addAPIKeyFlag = "sk-second-9999"
	_, _, err = runAddInner(t, "work")
	if err == nil {
		t.Fatalf("second runAdd: got nil err; want duplicate refusal")
	}
	if !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("second runAdd err: %v; want mention of 'already exists'", err)
	}

	// Bytes on disk unchanged.
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read after: %v", err)
	}
	if !bytes.Equal(original, after) {
		t.Fatalf("profile bytes changed despite refusal\nbefore:\n%s\nafter:\n%s", original, after)
	}
}

// TestAdd_DuplicateWithOverwriteReplaces replaces an existing profile
// when --overwrite is set.
func TestAdd_DuplicateWithOverwriteReplaces(t *testing.T) {
	h := newAddHarness(t)

	addBaseURLFlag = "https://api.first.example.com"
	addAPIKeyFlag = "sk-first-1234"
	if _, _, err := runAddInner(t, "work"); err != nil {
		t.Fatalf("first runAdd: %v", err)
	}

	resetAddFlags()
	addBaseURLFlag = "https://api.second.example.com"
	addAPIKeyFlag = "sk-second-9999"
	addOverwriteFlag = true
	if _, _, err := runAddInner(t, "work"); err != nil {
		t.Fatalf("overwrite runAdd: %v", err)
	}

	loaded, err := h.store.LoadProfile("work")
	if err != nil {
		t.Fatalf("LoadProfile: %v", err)
	}
	if loaded.Core.BaseURL != "https://api.second.example.com" {
		t.Fatalf("BaseURL not replaced: got %q", loaded.Core.BaseURL)
	}
	if loaded.Core.APIKey != "sk-second-9999" {
		t.Fatalf("APIKey not replaced: got %q", loaded.Core.APIKey)
	}
}

// TestAdd_DryRunNoWrite runs --dry-run and asserts the profile file is
// NOT written but the YAML body is printed.
func TestAdd_DryRunNoWrite(t *testing.T) {
	h := newAddHarness(t)

	addBaseURLFlag = "https://api.dryrun.example.com"
	addAPIKeyFlag = "sk-dryrun-1234"
	addModelFlag = "claude-opus-4-5"
	addDryRunFlag = true

	stdout, _, err := runAddInner(t, "work")
	if err != nil {
		t.Fatalf("runAdd: %v", err)
	}

	if !strings.Contains(stdout, "schema_version: 1") {
		t.Fatalf("stdout missing YAML body; got %q", stdout)
	}
	if strings.Contains(stdout, "sk-dryrun-1234") {
		t.Fatalf("stdout leaked plaintext api key; got %q", stdout)
	}
	if !strings.Contains(stdout, "sk-d***1234") {
		t.Fatalf("stdout missing redacted api key; got %q", stdout)
	}

	path := filepath.Join(h.home, ".claudecm", "profiles", "work.yaml")
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("profile file written despite --dry-run: err=%v", err)
	}
}

// TestAdd_DryRunJSONOutputParses runs --dry-run --output json and
// asserts the emitted document is well-formed JSON with the expected
// shape.
func TestAdd_DryRunJSONOutputParses(t *testing.T) {
	newAddHarness(t)

	addBaseURLFlag = "https://api.dryrun.example.com"
	addAPIKeyFlag = "sk-dryrun-json-1234"
	addModelFlag = "claude-opus-4-5"
	addDryRunFlag = true
	addOutputFlag = "json"

	stdout, _, err := runAddInner(t, "work")
	if err != nil {
		t.Fatalf("runAdd: %v", err)
	}

	var out jsonAddDryRun
	if err := json.Unmarshal([]byte(stdout), &out); err != nil {
		t.Fatalf("json.Unmarshal(%q): %v", stdout, err)
	}
	if out.Action != "dry-run" {
		t.Fatalf("Action: got %q want dry-run", out.Action)
	}
	if out.Profile.Name != "work" {
		t.Fatalf("Profile.Name: got %q", out.Profile.Name)
	}
	if out.Profile.SchemaVersion != config.CurrentProfileSchemaVersion {
		t.Fatalf("SchemaVersion: got %d want %d", out.Profile.SchemaVersion, config.CurrentProfileSchemaVersion)
	}
	if out.Profile.Core.APIKey != "sk-d***1234" {
		t.Fatalf("Core.APIKey: got %q, want redacted", out.Profile.Core.APIKey)
	}
	if strings.Contains(out.YAML, "sk-dryrun-json-1234") || strings.Contains(stdout, "sk-dryrun-json-1234") {
		t.Fatalf("dry-run JSON leaked plaintext api key:\n%s", stdout)
	}
	if !strings.Contains(out.YAML, "schema_version: 1") {
		t.Fatalf("YAML field missing schema_version; got %q", out.YAML)
	}
}

// TestAdd_JSONSuccessOutputParses asserts the non-dry-run JSON body is
// well-formed and carries the created profile.
func TestAdd_JSONSuccessOutputParses(t *testing.T) {
	h := newAddHarness(t)

	addBaseURLFlag = "https://api.anthropic.com"
	addAPIKeyFlag = "sk-json-success-1234"
	addOutputFlag = "json"

	stdout, _, err := runAddInner(t, "work")
	if err != nil {
		t.Fatalf("runAdd: %v", err)
	}
	var out jsonAddSuccess
	if err := json.Unmarshal([]byte(stdout), &out); err != nil {
		t.Fatalf("json.Unmarshal(%q): %v", stdout, err)
	}
	if out.Action != "created" {
		t.Fatalf("Action: got %q want created", out.Action)
	}
	if out.Profile.Name != "work" {
		t.Fatalf("Profile.Name: got %q", out.Profile.Name)
	}
	// File actually exists.
	path := filepath.Join(h.home, ".claudecm", "profiles", "work.yaml")
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("profile file missing: %v", err)
	}
}

// TestAdd_InvalidSetPathRefused refuses --set entries whose path is
// not one of the two supported prefixes.
func TestAdd_InvalidSetPathRefused(t *testing.T) {
	h := newAddHarness(t)

	addBaseURLFlag = "https://x"
	addAPIKeyFlag = "sk-x"
	addSetFlag = []string{"tools.claude_code.malformed=X"}

	_, _, err := runAddInner(t, "work")
	if err == nil {
		t.Fatalf("expected error on unsupported --set path")
	}
	if !strings.Contains(err.Error(), "unsupported path") {
		t.Fatalf("error should mention 'unsupported path': %v", err)
	}
	if !strings.Contains(err.Error(), setPrefixClaudeCodeEnv) || !strings.Contains(err.Error(), setPrefixCodexRaw) {
		t.Fatalf("error should list supported prefixes: %v", err)
	}
	// Nothing written.
	if _, statErr := os.Stat(filepath.Join(h.home, ".claudecm", "profiles", "work.yaml")); !os.IsNotExist(statErr) {
		t.Fatalf("profile file written despite --set error: %v", statErr)
	}
}

// TestAdd_InvalidSetTopLevelPrefixRefused rejects a --set whose path
// does not even begin with "tools.".
func TestAdd_InvalidSetTopLevelPrefixRefused(t *testing.T) {
	newAddHarness(t)
	addBaseURLFlag = "https://x"
	addAPIKeyFlag = "sk-x"
	addSetFlag = []string{"core.model=x"}
	_, _, err := runAddInner(t, "work")
	if err == nil {
		t.Fatalf("expected error on non-tools --set path")
	}
}

// TestAdd_MissingNameArg → cobra rejects at Args validation. The
// Args validator lives on addCmd; call it directly with zero
// positionals to prove the guard is wired.
func TestAdd_MissingNameArg(t *testing.T) {
	newAddHarness(t)
	if err := addCmd.Args(addCmd, []string{}); err == nil {
		t.Fatalf("expected error on missing name arg")
	}
	if err := addCmd.Args(addCmd, []string{"a", "b"}); err == nil {
		t.Fatalf("expected error on too many positionals")
	}
}

// TestAdd_MalformedSetValue refuses a --set entry with no "=".
func TestAdd_MalformedSetValue(t *testing.T) {
	newAddHarness(t)
	addBaseURLFlag = "https://x"
	addAPIKeyFlag = "sk-x"
	addSetFlag = []string{"no-equals-here"}
	_, _, err := runAddInner(t, "work")
	if err == nil {
		t.Fatalf("expected error on malformed --set")
	}
	if !strings.Contains(err.Error(), "malformed") {
		t.Fatalf("error should mention 'malformed': %v", err)
	}
}

// TestAdd_SetEnvNameLeadingDigit refuses env var names that begin with
// a digit (POSIX + typo protection).
func TestAdd_SetEnvNameLeadingDigit(t *testing.T) {
	newAddHarness(t)
	addBaseURLFlag = "https://x"
	addAPIKeyFlag = "sk-x"
	addSetFlag = []string{"tools.claude_code.env.9BAD=x"}
	_, _, err := runAddInner(t, "work")
	if err == nil {
		t.Fatalf("expected error on leading-digit env var name")
	}
}

// TestAdd_SetEmptyEnvName refuses tools.claude_code.env.=x (empty
// after the trailing dot).
func TestAdd_SetEmptyEnvName(t *testing.T) {
	newAddHarness(t)
	addBaseURLFlag = "https://x"
	addAPIKeyFlag = "sk-x"
	addSetFlag = []string{"tools.claude_code.env.=x"}
	_, _, err := runAddInner(t, "work")
	if err == nil {
		t.Fatalf("expected error on empty env var name")
	}
}

// TestAdd_SetEmptyCodexKey refuses tools.codex.raw.=x.
func TestAdd_SetEmptyCodexKey(t *testing.T) {
	newAddHarness(t)
	addBaseURLFlag = "https://x"
	addAPIKeyFlag = "sk-x"
	addSetFlag = []string{"tools.codex.raw.=x"}
	_, _, err := runAddInner(t, "work")
	if err == nil {
		t.Fatalf("expected error on empty codex raw key")
	}
}

// TestAdd_InvalidProviderRefused refuses --provider outside the enum.
func TestAdd_InvalidProviderRefused(t *testing.T) {
	newAddHarness(t)
	addBaseURLFlag = "https://x"
	addAPIKeyFlag = "sk-x"
	addProviderFlag = "not-a-provider"
	_, _, err := runAddInner(t, "work")
	if err == nil {
		t.Fatalf("expected error on invalid --provider")
	}
	if !strings.Contains(err.Error(), "invalid --provider") {
		t.Fatalf("error should mention 'invalid --provider': %v", err)
	}
}

// TestAdd_InvalidOutputRefused refuses --output outside text|json.
func TestAdd_InvalidOutputRefused(t *testing.T) {
	newAddHarness(t)
	addBaseURLFlag = "https://x"
	addAPIKeyFlag = "sk-x"
	addOutputFlag = "xml"
	_, _, err := runAddInner(t, "work")
	if err == nil {
		t.Fatalf("expected error on invalid --output")
	}
}

// TestParseSetEntries_MultipleTools exercises the pure --set parser
// with entries on both tools.
func TestParseSetEntries_MultipleTools(t *testing.T) {
	out, err := parseSetEntries([]string{
		"tools.claude_code.env.ANTHROPIC_MODEL=claude-opus-4-5",
		"tools.claude_code.env.CLAUDE_CODE_USE_BEDROCK=1",
		"tools.codex.raw.model_provider=openai",
		"tools.codex.raw.model_providers.openai.base_url=https://api.openai.com",
	})
	if err != nil {
		t.Fatalf("parseSetEntries: %v", err)
	}
	cc, ok := out[config.ToolClaudeCode]
	if !ok || cc.ExtraEnv["ANTHROPIC_MODEL"] != "claude-opus-4-5" ||
		cc.ExtraEnv["CLAUDE_CODE_USE_BEDROCK"] != "1" {
		t.Fatalf("claude_code overlay wrong: %+v", cc)
	}
	cx, ok := out[config.ToolCodex]
	if !ok || cx.Raw["model_provider"] != "openai" ||
		cx.Raw["model_providers.openai.base_url"] != "https://api.openai.com" {
		t.Fatalf("codex overlay wrong: %+v", cx)
	}
}

// TestParseSetEntries_Empty returns nil so the marshaled YAML does not
// carry an empty tools: {} block.
func TestParseSetEntries_Empty(t *testing.T) {
	out, err := parseSetEntries(nil)
	if err != nil {
		t.Fatalf("parseSetEntries(nil): %v", err)
	}
	if out != nil {
		t.Fatalf("parseSetEntries(nil): got %+v want nil", out)
	}
}

// TestParseAddOutput_Cases exercises the parser directly.
func TestParseAddOutput_Cases(t *testing.T) {
	cases := []struct {
		in      string
		want    addOutputFormat
		wantErr bool
	}{
		{"", addOutputText, false},
		{"text", addOutputText, false},
		{"TEXT", addOutputText, false},
		{"json", addOutputJSON, false},
		{"JSON", addOutputJSON, false},
		{"xml", "", true},
	}
	for _, tc := range cases {
		got, err := parseAddOutput(tc.in)
		if tc.wantErr && err == nil {
			t.Fatalf("parseAddOutput(%q): want err", tc.in)
		}
		if !tc.wantErr && err != nil {
			t.Fatalf("parseAddOutput(%q): unexpected err: %v", tc.in, err)
		}
		if !tc.wantErr && got != tc.want {
			t.Fatalf("parseAddOutput(%q): got %q want %q", tc.in, got, tc.want)
		}
	}
}

// TestValidateEnvVarName_Cases exercises the pure validator.
func TestValidateEnvVarName_Cases(t *testing.T) {
	if err := validateEnvVarName("ANTHROPIC_API_KEY"); err != nil {
		t.Fatalf("valid name refused: %v", err)
	}
	if err := validateEnvVarName(""); err == nil {
		t.Fatalf("empty name accepted")
	}
	if err := validateEnvVarName("BAD NAME"); err == nil {
		t.Fatalf("space accepted")
	}
	if err := validateEnvVarName("1LEADING"); err == nil {
		t.Fatalf("leading digit accepted")
	}
}

// TestSetNowForTestRestores confirms the seam is deterministic and the
// restore closure works.
func TestSetNowForTestRestores(t *testing.T) {
	orig := nowFn
	restore := SetNowForTest(func() time.Time { return time.Unix(42, 0).UTC() })
	if nowFn().Unix() != 42 {
		t.Fatalf("seam not applied")
	}
	restore()
	// After restore, nowFn should NOT be the test lambda anymore.
	if nowFn().Unix() == 42 {
		t.Fatalf("seam not restored")
	}
	// And it should be the original again.
	_ = orig
}
