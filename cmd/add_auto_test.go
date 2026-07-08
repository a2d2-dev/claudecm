//go:build test

package cmd

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/a2d2-dev/claudecm/internal/config"
	"github.com/a2d2-dev/claudecm/internal/envextract"
	"github.com/a2d2-dev/claudecm/internal/storage"
)

func TestAddAuto_CodexAuthJSONSurvivesProjectsConfig(t *testing.T) {
	h := newAddHarness(t)
	addAutoFlag = true
	addDryRunFlag = true
	stubAddAutoEmptySources(t)
	writeAddAutoCodexConfig(t, h.home, `model = "gpt-5"
model_provider = "openai"

[model_providers.openai]
base_url = "https://api.openai.com/v1"

[projects."/data/src/github.com/a2d2-dev/claudecm"]
trust_level = "trusted"
`)
	writeAddAutoCodexAuth(t, h.home, `{"OPENAI_API_KEY":"sk-codex-projects-1234","auth_mode":"api_key"}`)

	stdout, _, err := runAddInner(t)
	if err != nil {
		t.Fatalf("runAdd --auto codex projects: %v\nstdout=%s", err, stdout)
	}
	if !strings.Contains(stdout, "codex: NEW") {
		t.Fatalf("stdout missing codex NEW marker:\n%s", stdout)
	}
	if !strings.Contains(stdout, "name: codex") {
		t.Fatalf("dry-run did not derive codex profile name:\n%s", stdout)
	}
	if strings.Contains(stdout, "sk-codex-projects-1234") {
		t.Fatalf("stdout leaked codex key:\n%s", stdout)
	}
}

func TestAddAuto_EnvAndClaudeSameKeyCollapseToOneProfile(t *testing.T) {
	h := newAddHarness(t)
	addAutoFlag = true
	addYesFlag = true
	stubAddAutoEmptySources(t)
	restoreEnv := envextract.SetLookupForTest(addEnvUniverse(map[string]string{
		"ANTHROPIC_BASE_URL":   "https://api.anthropic.com",
		"ANTHROPIC_AUTH_TOKEN": "sk-same-claude-env-1234",
	}))
	defer restoreEnv()
	writeAddAutoClaudeSettings(t, h.home, `{"env":{"ANTHROPIC_BASE_URL":"https://api.anthropic.com/","ANTHROPIC_AUTH_TOKEN":"sk-same-claude-env-1234"}}`)
	restoreTTY := SetIsTerminalForTest(func(*os.File) bool { return false })
	defer restoreTTY()

	stdout, _, err := runAddInner(t)
	if err != nil {
		t.Fatalf("runAdd --auto same env+claude: %v\nstdout=%s", err, stdout)
	}
	if !strings.Contains(stdout, "claude-code: duplicate of environment") {
		t.Fatalf("stdout missing duplicate marker:\n%s", stdout)
	}
	assertProfileNames(t, h, []string{"api-anthropic-com"})
}

func TestAddAuto_EnvAnthropicAndCodexBothNewCreateTwoProfiles(t *testing.T) {
	h := newAddHarness(t)
	addAutoFlag = true
	addYesFlag = true
	stubAddAutoEmptySources(t)
	restoreEnv := envextract.SetLookupForTest(addEnvUniverse(map[string]string{
		"ANTHROPIC_BASE_URL":   "https://bray-neov.im/v1",
		"ANTHROPIC_AUTH_TOKEN": "sk-env-anthropic-1234",
	}))
	defer restoreEnv()
	writeAddAutoCodexConfig(t, h.home, `model = "gpt-5"
model_provider = "openai"

[model_providers.openai]
base_url = "https://api.openai.com/v1"
`)
	writeAddAutoCodexAuth(t, h.home, `{"OPENAI_API_KEY":"sk-codex-two-1234"}`)
	restoreTTY := SetIsTerminalForTest(func(*os.File) bool { return false })
	defer restoreTTY()

	stdout, _, err := runAddInner(t)
	if err != nil {
		t.Fatalf("runAdd --auto two profiles: %v\nstdout=%s", err, stdout)
	}
	created := addAutoCreatedSection(stdout)
	for _, want := range []string{"bray-neov-im", "codex"} {
		if !strings.Contains(created, want) {
			t.Fatalf("created section missing %q:\nsection=%s\nstdout=%s", want, created, stdout)
		}
	}
	assertProfileNames(t, h, []string{"bray-neov-im", "codex"})
}

func TestAddAuto_PartialSaveFailureReportsCreatedAndFailed(t *testing.T) {
	h := newAddHarness(t)
	addAutoFlag = true
	addYesFlag = true
	stubAddAutoEmptySources(t)
	restoreEnv := envextract.SetLookupForTest(addEnvUniverse(map[string]string{
		"ANTHROPIC_BASE_URL":   "https://first.example/v1",
		"ANTHROPIC_AUTH_TOKEN": "sk-first-partial-1234",
	}))
	defer restoreEnv()
	writeAddAutoCodexAuth(t, h.home, `{"OPENAI_API_KEY":"sk-second-partial-1234"}`)

	var out strings.Builder
	cmd := &cobra.Command{Use: "add"}
	cmd.SetOut(&out)
	err := runAddAuto(cmd, h.resv, failingAddAutoStore{FileStorage: h.store, failName: "codex"}, addOutputText)
	if err == nil {
		t.Fatalf("runAddAuto partial failure unexpectedly succeeded\nstdout=%s", out.String())
	}
	msg := err.Error()
	for _, want := range []string{"已创建：first-example", "失败于：codex", "injected save failure"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("partial failure error %q missing %q", msg, want)
		}
	}
	assertProfileNames(t, h, []string{"first-example"})
}

func TestAddAuto_CodexEmptyBaseURLDedupsAgainstExistingKey(t *testing.T) {
	h := newAddHarness(t)
	addAutoFlag = true
	addOutputFlag = "json"
	stubAddAutoEmptySources(t)
	if err := h.store.SaveProfile(&config.Profile{
		SchemaVersion: config.CurrentProfileSchemaVersion,
		Name:          "existing-codex",
		Core: config.CoreConfig{
			Provider: "openai-compat",
			BaseURL:  "https://api.openai.com/v1",
			APIKey:   "sk-empty-base-dedup-1234",
		},
	}); err != nil {
		t.Fatalf("SaveProfile existing-codex: %v", err)
	}
	writeAddAutoCodexAuth(t, h.home, `{"OPENAI_API_KEY":"sk-empty-base-dedup-1234"}`)

	stdout, _, err := runAddInner(t)
	if err != nil {
		t.Fatalf("runAdd --auto empty base dedup: %v\nstdout=%s", err, stdout)
	}
	var out jsonAddAutoProfiles
	if err := json.Unmarshal([]byte(stdout), &out); err != nil {
		t.Fatalf("json.Unmarshal: %v\n%s", err, stdout)
	}
	if out.Action != "already-recorded" || len(out.Created) != 0 {
		t.Fatalf("unexpected json action/created: %+v", out)
	}
	if len(out.Skipped) != 1 || out.Skipped[0].Reason != "already-recorded" || !strings.Contains(out.Skipped[0].Status, "existing-codex") {
		t.Fatalf("skipped = %+v, want already-recorded existing-codex", out.Skipped)
	}
	if strings.Contains(stdout, "sk-empty-base-dedup-1234") {
		t.Fatalf("json leaked key:\n%s", stdout)
	}
	assertProfileNames(t, h, []string{"existing-codex"})
}

func TestAddAuto_CodexMalformedConfigStillExtractsBaseURL(t *testing.T) {
	h := newAddHarness(t)
	addAutoFlag = true
	addDryRunFlag = true
	stubAddAutoEmptySources(t)
	writeAddAutoCodexConfig(t, h.home, `bad = [

[projects."/data/src/github.com/a2d2-dev/claudecm"]
trust_level = "trusted"

[model_providers.openai]
base_url = "https://compat.example/v1"
`)
	writeAddAutoCodexAuth(t, h.home, `{"OPENAI_API_KEY":"sk-codex-lenient-base-1234"}`)

	stdout, _, err := runAddInner(t)
	if err != nil {
		t.Fatalf("runAdd --auto lenient base_url: %v\nstdout=%s", err, stdout)
	}
	if !strings.Contains(stdout, "base_url=https://compat.example/v1") || !strings.Contains(stdout, "base_url: https://compat.example/v1") {
		t.Fatalf("stdout missing leniently extracted base_url:\n%s", stdout)
	}
}

func TestAddAuto_LenientReadersRejectOutsideHomeSymlinks(t *testing.T) {
	h := newAddHarness(t)
	addAutoFlag = true
	stubAddAutoEmptySources(t)
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "auth.json"), []byte(`{"OPENAI_API_KEY":"sk-outside-codex-1234"}`), 0o600); err != nil {
		t.Fatalf("write outside auth: %v", err)
	}
	if err := os.WriteFile(filepath.Join(outside, "settings.json"), []byte(`{"env":{"ANTHROPIC_AUTH_TOKEN":"sk-outside-claude-1234"}}`), 0o600); err != nil {
		t.Fatalf("write outside settings: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(h.home, ".codex"), 0o700); err != nil {
		t.Fatalf("mkdir .codex: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(h.home, ".claude"), 0o700); err != nil {
		t.Fatalf("mkdir .claude: %v", err)
	}
	if err := os.Symlink(filepath.Join(outside, "auth.json"), filepath.Join(h.home, ".codex", "auth.json")); err != nil {
		t.Fatalf("symlink auth: %v", err)
	}
	if err := os.Symlink(filepath.Join(outside, "settings.json"), filepath.Join(h.home, ".claude", "settings.json")); err != nil {
		t.Fatalf("symlink settings: %v", err)
	}

	stdout, _, err := runAddInner(t)
	if err == nil {
		t.Fatalf("runAdd --auto outside symlinks unexpectedly succeeded\nstdout=%s", stdout)
	}
	if !strings.Contains(stdout, "auth.json skipped: read target refused") || !strings.Contains(stdout, "settings.json refused") {
		t.Fatalf("stdout missing symlink refusal notes:\n%s", stdout)
	}
	if strings.Contains(stdout, "sk-outside") || strings.Contains(err.Error(), "sk-outside") {
		t.Fatalf("output leaked outside-home key:\nstdout=%s\nerr=%v", stdout, err)
	}
}

func TestAddAuto_AlreadyRecordedSkippedAndNotRecreated(t *testing.T) {
	h := newAddHarness(t)
	addAutoFlag = true
	stubAddAutoNoClipboard(t)
	restoreEnv := envextract.SetLookupForTest(addEnvUniverse(map[string]string{
		"ANTHROPIC_BASE_URL":   "https://API.Anthropic.com/",
		"ANTHROPIC_AUTH_TOKEN": "sk-existing-auto-1234",
	}))
	defer restoreEnv()
	if err := h.store.SaveProfile(&config.Profile{
		SchemaVersion: config.CurrentProfileSchemaVersion,
		Name:          "existing",
		Core: config.CoreConfig{
			Provider: "anthropic",
			BaseURL:  "https://api.anthropic.com",
			APIKey:   "sk-existing-auto-1234",
		},
	}); err != nil {
		t.Fatalf("SaveProfile existing: %v", err)
	}

	stdout, _, err := runAddInner(t)
	if err != nil {
		t.Fatalf("runAdd --auto all recorded should exit 0: %v\nstdout=%s", err, stdout)
	}
	if !strings.Contains(stdout, "already recorded as existing") {
		t.Fatalf("stdout missing already-recorded marker:\n%s", stdout)
	}
	if !strings.Contains(stdout, "all discovered credentials are already recorded") {
		t.Fatalf("stdout missing all-recorded message:\n%s", stdout)
	}
	assertProfileNames(t, h, []string{"existing"})
}

func TestAddAuto_NameArgumentRefused(t *testing.T) {
	newAddHarness(t)
	addAutoFlag = true

	stdout, _, err := runAddInner(t, "foo")
	if err == nil {
		t.Fatalf("runAdd add foo --auto unexpectedly succeeded\nstdout=%s", stdout)
	}
	if !strings.Contains(err.Error(), "--auto does not take a profile name") || !strings.Contains(err.Error(), "names are derived") {
		t.Fatalf("error missing clear nameless-auto text: %v", err)
	}
}

func TestAddAuto_NoCredentialsRefusesWithSweptSources(t *testing.T) {
	newAddHarness(t)
	addAutoFlag = true
	stubAddAutoEmptySources(t)

	stdout, _, err := runAddInner(t)
	if err == nil {
		t.Fatalf("runAdd --auto no key unexpectedly succeeded\nstdout=%s", stdout)
	}
	msg := err.Error()
	for _, want := range []string{"clipboard", "environment", "claude-code", "codex"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("error %q missing swept source %q", msg, want)
		}
	}
}

func TestAddAuto_NonTTYWithoutYesRefusesWithPreview(t *testing.T) {
	h := newAddHarness(t)
	addAutoFlag = true
	stubAddAutoNoClipboard(t)
	restoreEnv := envextract.SetLookupForTest(addEnvUniverse(map[string]string{
		"ANTHROPIC_BASE_URL":   "https://api.anthropic.com",
		"ANTHROPIC_AUTH_TOKEN": "sk-nontty-auto-1234",
	}))
	defer restoreEnv()
	restoreTTY := SetIsTerminalForTest(func(*os.File) bool { return false })
	defer restoreTTY()

	stdout, _, err := runAddInner(t)
	if err == nil {
		t.Fatalf("runAdd --auto non-tty without --yes unexpectedly succeeded\nstdout=%s", stdout)
	}
	if !strings.Contains(err.Error(), "pass --yes") {
		t.Fatalf("error missing --yes guidance: %v", err)
	}
	if !strings.Contains(stdout, "profiles to create:") || !strings.Contains(stdout, "api-anthropic-com") {
		t.Fatalf("stdout missing preview list:\n%s", stdout)
	}
	if exists, err := h.store.ProfileExists("api-anthropic-com"); err != nil {
		t.Fatalf("ProfileExists: %v", err)
	} else if exists {
		t.Fatalf("non-tty refusal wrote profile")
	}
}

func TestAddAuto_YesCreatesAllAndDryRunWritesNothing(t *testing.T) {
	h := newAddHarness(t)
	addAutoFlag = true
	addDryRunFlag = true
	stubAddAutoNoClipboard(t)
	restoreEnv := envextract.SetLookupForTest(addEnvUniverse(map[string]string{
		"ANTHROPIC_BASE_URL":   "https://dry.example/v1",
		"ANTHROPIC_AUTH_TOKEN": "sk-dry-auto-1234",
	}))
	defer restoreEnv()
	restoreTTY := SetIsTerminalForTest(func(*os.File) bool { return false })
	defer restoreTTY()

	stdout, _, err := runAddInner(t)
	if err != nil {
		t.Fatalf("runAdd --auto --dry-run: %v\nstdout=%s", err, stdout)
	}
	if !strings.Contains(stdout, "--- dry-run: auto profiles (not written) ---") || !strings.Contains(stdout, "name: dry-example") {
		t.Fatalf("stdout missing dry-run auto profile:\n%s", stdout)
	}
	if exists, err := h.store.ProfileExists("dry-example"); err != nil {
		t.Fatalf("ProfileExists dry-example: %v", err)
	} else if exists {
		t.Fatalf("--dry-run wrote profile")
	}

	resetAddFlags()
	addAutoFlag = true
	addYesFlag = true
	restoreEnv2 := envextract.SetLookupForTest(addEnvUniverse(map[string]string{
		"ANTHROPIC_BASE_URL":   "https://dry.example/v1",
		"ANTHROPIC_AUTH_TOKEN": "sk-dry-auto-1234",
	}))
	defer restoreEnv2()
	stdout, _, err = runAddInner(t)
	if err != nil {
		t.Fatalf("runAdd --auto --yes: %v\nstdout=%s", err, stdout)
	}
	if !strings.Contains(stdout, "created profiles:") || !strings.Contains(stdout, "dry-example") {
		t.Fatalf("stdout missing created dry-example:\n%s", stdout)
	}
	if exists, err := h.store.ProfileExists("dry-example"); err != nil {
		t.Fatalf("ProfileExists dry-example: %v", err)
	} else if !exists {
		t.Fatalf("--yes did not write profile")
	}
}

func TestAddAuto_NameCollisionGetsNumericSuffix(t *testing.T) {
	h := newAddHarness(t)
	addAutoFlag = true
	addYesFlag = true
	stubAddAutoEmptySources(t)
	if err := h.store.SaveProfile(&config.Profile{
		SchemaVersion: config.CurrentProfileSchemaVersion,
		Name:          "codex",
		Core: config.CoreConfig{
			Provider: "openai-compat",
			BaseURL:  "https://old.example/v1",
			APIKey:   "sk-old-codex-1234",
		},
	}); err != nil {
		t.Fatalf("SaveProfile codex: %v", err)
	}
	writeAddAutoCodexAuth(t, h.home, `{"OPENAI_API_KEY":"sk-new-codex-1234"}`)
	restoreTTY := SetIsTerminalForTest(func(*os.File) bool { return false })
	defer restoreTTY()

	stdout, _, err := runAddInner(t)
	if err != nil {
		t.Fatalf("runAdd --auto codex suffix: %v\nstdout=%s", err, stdout)
	}
	if !strings.Contains(stdout, "codex-2") {
		t.Fatalf("stdout missing suffixed name:\n%s", stdout)
	}
	assertProfileNames(t, h, []string{"codex", "codex-2"})
}

func TestAddAuto_JSONDryRunListsAllProfilesRedacted(t *testing.T) {
	newAddHarness(t)
	addAutoFlag = true
	addDryRunFlag = true
	addOutputFlag = "json"
	stubAddAutoNoClipboard(t)
	restoreEnv := envextract.SetLookupForTest(addEnvUniverse(map[string]string{
		"ANTHROPIC_BASE_URL":   "https://json.example/v1",
		"ANTHROPIC_AUTH_TOKEN": "sk-json-auto-1234",
	}))
	defer restoreEnv()

	stdout, _, err := runAddInner(t)
	if err != nil {
		t.Fatalf("runAdd --auto json dry-run: %v\nstdout=%s", err, stdout)
	}
	var out jsonAddAutoProfiles
	if err := json.Unmarshal([]byte(stdout), &out); err != nil {
		t.Fatalf("json.Unmarshal(%q): %v", stdout, err)
	}
	if out.Action != "dry-run" || len(out.Profiles) != 1 {
		t.Fatalf("unexpected json auto dry-run: %+v", out)
	}
	if len(out.Discovery) == 0 || len(out.Created) != 1 || out.Created[0] != "json-example" {
		t.Fatalf("json missing discovery/created: %+v", out)
	}
	if out.Profiles[0].Name != "json-example" {
		t.Fatalf("profile name = %q, want json-example", out.Profiles[0].Name)
	}
	if strings.Contains(stdout, "sk-json-auto-1234") {
		t.Fatalf("stdout leaked key:\n%s", stdout)
	}
}

func TestAddAuto_JSONCreatedListsDiscoveryCreatedAndSkippedRedacted(t *testing.T) {
	h := newAddHarness(t)
	addAutoFlag = true
	addYesFlag = true
	addOutputFlag = "json"
	stubAddAutoNoClipboard(t)
	restoreEnv := envextract.SetLookupForTest(addEnvUniverse(map[string]string{
		"ANTHROPIC_BASE_URL":   "https://json-create.example/v1",
		"ANTHROPIC_AUTH_TOKEN": "sk-json-create-1234",
	}))
	defer restoreEnv()
	if err := h.store.SaveProfile(&config.Profile{
		SchemaVersion: config.CurrentProfileSchemaVersion,
		Name:          "existing",
		Core:          config.CoreConfig{Provider: "openai-compat", APIKey: "sk-json-existing-1234"},
	}); err != nil {
		t.Fatalf("SaveProfile existing: %v", err)
	}
	writeAddAutoCodexAuth(t, h.home, `{"OPENAI_API_KEY":"sk-json-existing-1234"}`)

	stdout, _, err := runAddInner(t)
	if err != nil {
		t.Fatalf("runAdd --auto json created: %v\nstdout=%s", err, stdout)
	}
	var out jsonAddAutoProfiles
	if err := json.Unmarshal([]byte(stdout), &out); err != nil {
		t.Fatalf("json.Unmarshal: %v\n%s", err, stdout)
	}
	if out.Action != "created" || len(out.Discovery) == 0 || len(out.Created) != 1 || out.Created[0] != "json-create-example" {
		t.Fatalf("unexpected json created output: %+v", out)
	}
	if len(out.Skipped) != 1 || out.Skipped[0].Reason != "already-recorded" {
		t.Fatalf("json skipped = %+v, want one already-recorded", out.Skipped)
	}
	if strings.Contains(stdout, "sk-json-create-1234") || strings.Contains(stdout, "sk-json-existing-1234") {
		t.Fatalf("json leaked key:\n%s", stdout)
	}
	assertProfileNames(t, h, []string{"existing", "json-create-example"})
}

func TestAddAuto_InteractiveNamingAcceptsDefault(t *testing.T) {
	h := newAddHarness(t)
	addAutoFlag = true
	stubAddAutoNoClipboard(t)
	restoreEnv := envextract.SetLookupForTest(addEnvUniverse(map[string]string{
		"ANTHROPIC_BASE_URL":   "https://interactive-default.example/v1",
		"ANTHROPIC_AUTH_TOKEN": "sk-interactive-default-1234",
	}))
	defer restoreEnv()
	restoreTTY := SetIsTerminalForTest(func(*os.File) bool { return true })
	defer restoreTTY()
	restoreStdin := pipeAddAutoStdin(t, "\ny\n")
	defer restoreStdin()

	stdout, _, err := runAddInner(t)
	if err != nil {
		t.Fatalf("runAdd --auto interactive default: %v\nstdout=%s", err, stdout)
	}
	if !strings.Contains(stdout, "Save profile for environment, sk-i***1234 as [interactive-default-example]:") {
		t.Fatalf("stdout missing naming prompt:\n%s", stdout)
	}
	assertProfileNames(t, h, []string{"interactive-default-example"})
}

func TestAddAuto_InteractiveNamingUsesCustomName(t *testing.T) {
	h := newAddHarness(t)
	addAutoFlag = true
	stubAddAutoNoClipboard(t)
	restoreEnv := envextract.SetLookupForTest(addEnvUniverse(map[string]string{
		"ANTHROPIC_BASE_URL":   "https://interactive-custom.example/v1",
		"ANTHROPIC_AUTH_TOKEN": "sk-interactive-custom-1234",
	}))
	defer restoreEnv()
	restoreTTY := SetIsTerminalForTest(func(*os.File) bool { return true })
	defer restoreTTY()
	restoreStdin := pipeAddAutoStdin(t, "mine-custom\ny\n")
	defer restoreStdin()

	stdout, _, err := runAddInner(t)
	if err != nil {
		t.Fatalf("runAdd --auto interactive custom: %v\nstdout=%s", err, stdout)
	}
	if !strings.Contains(stdout, "mine-custom") {
		t.Fatalf("stdout missing custom created name:\n%s", stdout)
	}
	assertProfileNames(t, h, []string{"mine-custom"})
}

func TestAddAuto_YesUsesDerivedNameWithoutPrompt(t *testing.T) {
	h := newAddHarness(t)
	addAutoFlag = true
	addYesFlag = true
	stubAddAutoNoClipboard(t)
	restoreEnv := envextract.SetLookupForTest(addEnvUniverse(map[string]string{
		"ANTHROPIC_BASE_URL":   "https://yes-derived.example/v1",
		"ANTHROPIC_AUTH_TOKEN": "sk-yes-derived-1234",
	}))
	defer restoreEnv()
	restoreTTY := SetIsTerminalForTest(func(*os.File) bool { return true })
	defer restoreTTY()

	stdout, _, err := runAddInner(t)
	if err != nil {
		t.Fatalf("runAdd --auto --yes derived: %v\nstdout=%s", err, stdout)
	}
	if strings.Contains(stdout, "Save profile for") {
		t.Fatalf("--yes unexpectedly prompted:\n%s", stdout)
	}
	assertProfileNames(t, h, []string{"yes-derived-example"})
}

type failingAddAutoStore struct {
	*storage.FileStorage
	failName string
}

func (s failingAddAutoStore) SaveProfile(profile *config.Profile) error {
	if profile != nil && profile.Name == s.failName {
		return errors.New("injected save failure")
	}
	return s.FileStorage.SaveProfile(profile)
}

func stubAddAutoNoClipboard(t *testing.T) {
	t.Helper()
	restoreClipboard := setAddAutoClipboardForTest(func() (string, bool, error) {
		return "", false, os.ErrNotExist
	})
	t.Cleanup(restoreClipboard)
}

func stubAddAutoEmptySources(t *testing.T) {
	t.Helper()
	stubAddAutoNoClipboard(t)
	restoreEnv := envextract.SetLookupForTest(addEnvUniverse(map[string]string{}))
	t.Cleanup(restoreEnv)
}

func writeAddAutoCodexConfig(t *testing.T, home, body string) {
	t.Helper()
	dir := filepath.Join(home, ".codex")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir .codex: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "config.toml"), []byte(body), 0o600); err != nil {
		t.Fatalf("write config.toml: %v", err)
	}
}

func writeAddAutoCodexAuth(t *testing.T, home, body string) {
	t.Helper()
	dir := filepath.Join(home, ".codex")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir .codex: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "auth.json"), []byte(body), 0o600); err != nil {
		t.Fatalf("write auth.json: %v", err)
	}
}

func writeAddAutoClaudeSettings(t *testing.T, home, body string) {
	t.Helper()
	dir := filepath.Join(home, ".claude")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir .claude: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "settings.json"), []byte(body), 0o600); err != nil {
		t.Fatalf("write settings.json: %v", err)
	}
}

func addAutoCreatedSection(stdout string) string {
	idx := strings.Index(stdout, "created profiles:")
	if idx < 0 {
		return ""
	}
	section := stdout[idx:]
	if next := strings.Index(section[len("created profiles:"):], "\n\n"); next >= 0 {
		return section[:len("created profiles:")+next]
	}
	return section
}

func pipeAddAutoStdin(t *testing.T, input string) func() {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	orig := os.Stdin
	os.Stdin = r
	if _, err := w.WriteString(input); err != nil {
		t.Fatalf("write stdin pipe: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("close stdin pipe writer: %v", err)
	}
	return func() {
		os.Stdin = orig
		_ = r.Close()
	}
}

func assertProfileNames(t *testing.T, h *addHarness, want []string) {
	t.Helper()
	gotProfiles, err := h.store.LoadAllProfiles()
	if err != nil {
		t.Fatalf("LoadAllProfiles: %v", err)
	}
	got := make(map[string]struct{}, len(gotProfiles))
	for _, profile := range gotProfiles {
		got[profile.Name] = struct{}{}
	}
	if len(got) != len(want) {
		t.Fatalf("profile count = %d, want %d; got=%v", len(got), len(want), got)
	}
	for _, name := range want {
		if _, ok := got[name]; !ok {
			t.Fatalf("missing profile %q; got=%v", name, got)
		}
	}
}
