//go:build test

package cmd

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/a2d2-dev/claudecm/internal/config"
	"github.com/a2d2-dev/claudecm/internal/envextract"
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
	if !strings.Contains(stdout, "created profiles:") || !strings.Contains(stdout, "bray-neov-im") || !strings.Contains(stdout, "codex") {
		t.Fatalf("stdout missing created profile names:\n%s", stdout)
	}
	assertProfileNames(t, h, []string{"bray-neov-im", "codex"})
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
	if out.Profiles[0].Name != "json-example" {
		t.Fatalf("profile name = %q, want json-example", out.Profiles[0].Name)
	}
	if strings.Contains(stdout, "sk-json-auto-1234") {
		t.Fatalf("stdout leaked key:\n%s", stdout)
	}
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
