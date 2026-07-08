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

func TestAddAuto_EnvNewCandidateDryRun(t *testing.T) {
	newAddHarness(t)
	addAutoFlag = true
	addDryRunFlag = true
	restoreClipboard := setAddAutoClipboardForTest(func() (string, bool, error) {
		return "", false, os.ErrNotExist
	})
	defer restoreClipboard()
	restoreEnv := envextract.SetLookupForTest(addEnvUniverse(map[string]string{
		"ANTHROPIC_BASE_URL":   "https://api.anthropic.com",
		"ANTHROPIC_AUTH_TOKEN": "sk-env-auto-123456",
		"ANTHROPIC_MODEL":      "claude-opus-4-5",
	}))
	defer restoreEnv()

	stdout, _, err := runAddInner(t, "autoenv")
	if err != nil {
		t.Fatalf("runAdd --auto: %v\nstdout=%s", err, stdout)
	}
	if !strings.Contains(stdout, "environment: NEW") {
		t.Fatalf("stdout missing environment NEW marker:\n%s", stdout)
	}
	if !strings.Contains(stdout, "--- dry-run: profile YAML (not written) ---") {
		t.Fatalf("stdout missing dry-run profile:\n%s", stdout)
	}
	if strings.Contains(stdout, "sk-env-auto-123456") {
		t.Fatalf("stdout leaked plaintext key:\n%s", stdout)
	}
	if !strings.Contains(stdout, "sk-e***3456") {
		t.Fatalf("stdout missing redacted key:\n%s", stdout)
	}
}

func TestAddAuto_CodexAuthJSONCandidate(t *testing.T) {
	h := newAddHarness(t)
	addAutoFlag = true
	addDryRunFlag = true
	restoreClipboard := setAddAutoClipboardForTest(func() (string, bool, error) {
		return "", false, os.ErrNotExist
	})
	defer restoreClipboard()
	restoreEnv := envextract.SetLookupForTest(addEnvUniverse(map[string]string{}))
	defer restoreEnv()
	writeAddAutoCodexConfig(t, h.home, `model = "gpt-5"
model_provider = "openai"

[model_providers.openai]
base_url = "https://api.openai.com/v1"
`)
	writeAddAutoCodexAuth(t, h.home, `{"OPENAI_API_KEY":"sk-codex-auto-1234","auth_mode":"api_key"}`)

	stdout, _, err := runAddInner(t, "autocodex")
	if err != nil {
		t.Fatalf("runAdd --auto codex: %v\nstdout=%s", err, stdout)
	}
	if !strings.Contains(stdout, "~/.codex/auth.json + config.toml: NEW") {
		t.Fatalf("stdout missing codex NEW marker:\n%s", stdout)
	}
	if strings.Contains(stdout, "sk-codex-auto-1234") {
		t.Fatalf("stdout leaked codex key:\n%s", stdout)
	}
}

func TestAddAuto_DedupAlreadyRecorded(t *testing.T) {
	h := newAddHarness(t)
	addAutoFlag = true
	restoreClipboard := setAddAutoClipboardForTest(func() (string, bool, error) {
		return "", false, os.ErrNotExist
	})
	defer restoreClipboard()
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

	stdout, _, err := runAddInner(t, "dup")
	if err != nil {
		t.Fatalf("runAdd --auto dedup should exit 0: %v\nstdout=%s", err, stdout)
	}
	if !strings.Contains(stdout, "already recorded as existing") {
		t.Fatalf("stdout missing already-recorded marker:\n%s", stdout)
	}
	if !strings.Contains(stdout, "all discovered credentials are already recorded") {
		t.Fatalf("stdout missing all-recorded message:\n%s", stdout)
	}
	if exists, err := h.store.ProfileExists("dup"); err != nil {
		t.Fatalf("ProfileExists dup: %v", err)
	} else if exists {
		t.Fatalf("duplicate auto path wrote profile dup")
	}
}

func TestAddAuto_DedupAlreadyRecordedDefaultHTTPSPort(t *testing.T) {
	h := newAddHarness(t)
	addAutoFlag = true
	restoreClipboard := setAddAutoClipboardForTest(func() (string, bool, error) {
		return "", false, os.ErrNotExist
	})
	defer restoreClipboard()
	restoreEnv := envextract.SetLookupForTest(addEnvUniverse(map[string]string{
		"ANTHROPIC_BASE_URL":   "https://h:443/v1",
		"ANTHROPIC_AUTH_TOKEN": "sk-same-default-port",
	}))
	defer restoreEnv()
	if err := h.store.SaveProfile(&config.Profile{
		SchemaVersion: config.CurrentProfileSchemaVersion,
		Name:          "existing",
		Core: config.CoreConfig{
			Provider: "anthropic",
			BaseURL:  "https://h/v1",
			APIKey:   "sk-same-default-port",
		},
	}); err != nil {
		t.Fatalf("SaveProfile existing: %v", err)
	}

	stdout, _, err := runAddInner(t, "dupport")
	if err != nil {
		t.Fatalf("runAdd --auto default-port dedup should exit 0: %v\nstdout=%s", err, stdout)
	}
	if !strings.Contains(stdout, "already recorded as existing") {
		t.Fatalf("stdout missing already-recorded marker:\n%s", stdout)
	}
	if exists, err := h.store.ProfileExists("dupport"); err != nil {
		t.Fatalf("ProfileExists dupport: %v", err)
	} else if exists {
		t.Fatalf("default-port duplicate auto path wrote profile dupport")
	}
}

func TestAddAuto_NoKeyRefusesWithSweptSources(t *testing.T) {
	newAddHarness(t)
	addAutoFlag = true
	restoreClipboard := setAddAutoClipboardForTest(func() (string, bool, error) {
		return "", false, os.ErrNotExist
	})
	defer restoreClipboard()
	restoreEnv := envextract.SetLookupForTest(addEnvUniverse(map[string]string{}))
	defer restoreEnv()

	stdout, _, err := runAddInner(t, "nokey")
	if err == nil {
		t.Fatalf("runAdd --auto no key unexpectedly succeeded\nstdout=%s", stdout)
	}
	msg := err.Error()
	for _, want := range []string{"clipboard", "environment", "~/.claude/settings.json", "~/.codex/auth.json + config.toml"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("error %q missing swept source %q", msg, want)
		}
	}
}

func TestAddAuto_ExplicitEmptyAPIKeyRefuses(t *testing.T) {
	h := newAddHarness(t)
	addAutoFlag = true
	addAPIKeyFlagExplicit = true

	stdout, _, err := runAddInner(t, "emptykey")
	if err == nil {
		t.Fatalf("runAdd --auto --api-key= unexpectedly succeeded\nstdout=%s", stdout)
	}
	if !strings.Contains(err.Error(), "choose only one add identity source") {
		t.Fatalf("error missing identity mutual exclusion: %v", err)
	}
	if !strings.Contains(err.Error(), "--auto") || !strings.Contains(err.Error(), "--api-key") {
		t.Fatalf("error missing conflicting flags: %v", err)
	}
	if exists, err := h.store.ProfileExists("emptykey"); err != nil {
		t.Fatalf("ProfileExists emptykey: %v", err)
	} else if exists {
		t.Fatalf("explicit empty --api-key wrote keyless profile")
	}
}

func TestAddAuto_IdentityOverridesRefused(t *testing.T) {
	for _, tc := range []struct {
		name       string
		baseURL    string
		apiKey     string
		wantFlag   string
		flagIsBase bool
	}{
		{name: "apikey", apiKey: "sk-existing-identity", wantFlag: "--api-key"},
		{name: "baseurl", baseURL: "http://x", wantFlag: "--base-url", flagIsBase: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newAddHarness(t)
			addAutoFlag = true
			if tc.flagIsBase {
				addBaseURLFlag = tc.baseURL
			} else {
				addAPIKeyFlag = tc.apiKey
			}

			stdout, _, err := runAddInner(t, "identity")
			if err == nil {
				t.Fatalf("runAdd --auto %s unexpectedly succeeded\nstdout=%s", tc.wantFlag, stdout)
			}
			if !strings.Contains(err.Error(), "choose only one add identity source") {
				t.Fatalf("error missing identity mutual exclusion: %v", err)
			}
			if !strings.Contains(err.Error(), tc.wantFlag) {
				t.Fatalf("error missing %s: %v", tc.wantFlag, err)
			}
			if exists, err := h.store.ProfileExists("identity"); err != nil {
				t.Fatalf("ProfileExists identity: %v", err)
			} else if exists {
				t.Fatalf("--auto %s wrote profile", tc.wantFlag)
			}
		})
	}
}

func TestAddAuto_ModelOverrideAllowed(t *testing.T) {
	h := newAddHarness(t)
	addAutoFlag = true
	addModelFlag = "foo"
	restoreClipboard := setAddAutoClipboardForTest(func() (string, bool, error) {
		return "", false, os.ErrNotExist
	})
	defer restoreClipboard()
	restoreEnv := envextract.SetLookupForTest(addEnvUniverse(map[string]string{
		"ANTHROPIC_BASE_URL":   "https://api.anthropic.com",
		"ANTHROPIC_AUTH_TOKEN": "sk-model-override-1234",
		"ANTHROPIC_MODEL":      "from-env",
	}))
	defer restoreEnv()

	if _, _, err := runAddInner(t, "modeloverride"); err != nil {
		t.Fatalf("runAdd --auto --model: %v", err)
	}
	loaded, err := h.store.LoadProfile("modeloverride")
	if err != nil {
		t.Fatalf("LoadProfile modeloverride: %v", err)
	}
	if loaded.Core.Model != "foo" {
		t.Fatalf("Core.Model = %q, want explicit override foo", loaded.Core.Model)
	}
}

func TestAddAuto_MultipleNewNonTTYRefusesRedactedList(t *testing.T) {
	newAddHarness(t)
	addAutoFlag = true
	restoreClipboard := setAddAutoClipboardForTest(func() (string, bool, error) {
		return "ANTHROPIC_BASE_URL=https://clip.example ANTHROPIC_AUTH_TOKEN=sk-clip-auto-1234", true, nil
	})
	defer restoreClipboard()
	restoreEnv := envextract.SetLookupForTest(addEnvUniverse(map[string]string{
		"OPENAI_BASE_URL": "https://env.example/v1",
		"OPENAI_API_KEY":  "sk-env-multi-1234",
	}))
	defer restoreEnv()
	restoreTTY := SetIsTerminalForTest(func(*os.File) bool { return false })
	defer restoreTTY()

	stdout, _, err := runAddInner(t, "multi")
	if err == nil {
		t.Fatalf("runAdd --auto multi non-tty unexpectedly succeeded\nstdout=%s", stdout)
	}
	if !strings.Contains(err.Error(), "multiple new credentials discovered") {
		t.Fatalf("error missing multi-candidate refusal: %v", err)
	}
	if !strings.Contains(stdout, "multiple new credentials discovered:") {
		t.Fatalf("stdout missing redacted list:\n%s", stdout)
	}
	for _, secret := range []string{"sk-clip-auto-1234", "sk-env-multi-1234"} {
		if strings.Contains(stdout, secret) {
			t.Fatalf("stdout leaked secret %q:\n%s", secret, stdout)
		}
	}
}

func TestAddAuto_MultipleNewNonTTYJSONOutputsRedactedCandidates(t *testing.T) {
	newAddHarness(t)
	addAutoFlag = true
	addOutputFlag = "json"
	restoreClipboard := setAddAutoClipboardForTest(func() (string, bool, error) {
		return "ANTHROPIC_BASE_URL=https://clip.example ANTHROPIC_AUTH_TOKEN=sk-clip-json-1234", true, nil
	})
	defer restoreClipboard()
	restoreEnv := envextract.SetLookupForTest(addEnvUniverse(map[string]string{
		"OPENAI_BASE_URL": "https://env-json.example/v1",
		"OPENAI_API_KEY":  "sk-env-json-1234",
	}))
	defer restoreEnv()
	restoreTTY := SetIsTerminalForTest(func(*os.File) bool { return false })
	defer restoreTTY()

	stdout, _, err := runAddInner(t, "multijson")
	if err == nil {
		t.Fatalf("runAdd --auto multi json non-tty unexpectedly succeeded\nstdout=%s", stdout)
	}
	if !strings.Contains(err.Error(), "multiple new credentials discovered") {
		t.Fatalf("error missing multi-candidate refusal: %v", err)
	}
	var out jsonAddAutoDisambiguation
	if err := json.Unmarshal([]byte(stdout), &out); err != nil {
		t.Fatalf("json.Unmarshal(%q): %v", stdout, err)
	}
	if out.Action != "auto-disambiguation-required" {
		t.Fatalf("Action = %q", out.Action)
	}
	if len(out.Candidates) != 2 {
		t.Fatalf("Candidates len = %d, want 2; stdout=%s", len(out.Candidates), stdout)
	}
	for _, candidate := range out.Candidates {
		if candidate.Status != "NEW" {
			t.Fatalf("candidate status = %q, want NEW", candidate.Status)
		}
		if candidate.Source == "" || candidate.BaseURL == "" || candidate.APIKey == "" {
			t.Fatalf("candidate missing structured fields: %+v", candidate)
		}
	}
	for _, secret := range []string{"sk-clip-json-1234", "sk-env-json-1234"} {
		if strings.Contains(stdout, secret) {
			t.Fatalf("stdout leaked secret %q:\n%s", secret, stdout)
		}
	}
	if !strings.Contains(stdout, "sk-c***1234") || !strings.Contains(stdout, "sk-e***1234") {
		t.Fatalf("stdout missing redacted keys:\n%s", stdout)
	}
}

func TestAddAuto_MultipleNewTTYSelectsOne(t *testing.T) {
	newAddHarness(t)
	addAutoFlag = true
	addDryRunFlag = true
	restoreClipboard := setAddAutoClipboardForTest(func() (string, bool, error) {
		return "ANTHROPIC_BASE_URL=https://clip.example ANTHROPIC_AUTH_TOKEN=sk-clip-select-1234", true, nil
	})
	defer restoreClipboard()
	restoreEnv := envextract.SetLookupForTest(addEnvUniverse(map[string]string{
		"OPENAI_BASE_URL": "https://env-select.example/v1",
		"OPENAI_API_KEY":  "sk-env-select-1234",
	}))
	defer restoreEnv()
	restoreTTY := SetIsTerminalForTest(func(*os.File) bool { return true })
	defer restoreTTY()
	restoreStdin := withOSStdin(t, "2\n")
	defer restoreStdin()

	stdout, _, err := runAddInner(t, "picked")
	if err != nil {
		t.Fatalf("runAdd --auto TTY pick: %v\nstdout=%s", err, stdout)
	}
	if !strings.Contains(stdout, "Select credential [1-2]:") {
		t.Fatalf("stdout missing selection prompt:\n%s", stdout)
	}
	if !strings.Contains(stdout, "base_url: https://env-select.example/v1") {
		t.Fatalf("dry-run did not use selected env candidate:\n%s", stdout)
	}
	if strings.Contains(stdout, "sk-env-select-1234") || strings.Contains(stdout, "sk-clip-select-1234") {
		t.Fatalf("stdout leaked selected secret:\n%s", stdout)
	}
}

func TestAddAuto_DedupsSameCredentialWithinSweep(t *testing.T) {
	newAddHarness(t)
	addAutoFlag = true
	addDryRunFlag = true
	restoreClipboard := setAddAutoClipboardForTest(func() (string, bool, error) {
		return "ANTHROPIC_BASE_URL=https://api.anthropic.com/ ANTHROPIC_AUTH_TOKEN=sk-same-auto-1234", true, nil
	})
	defer restoreClipboard()
	restoreEnv := envextract.SetLookupForTest(addEnvUniverse(map[string]string{
		"ANTHROPIC_BASE_URL":   "https://API.Anthropic.com",
		"ANTHROPIC_AUTH_TOKEN": "sk-same-auto-1234",
	}))
	defer restoreEnv()
	restoreTTY := SetIsTerminalForTest(func(*os.File) bool { return false })
	defer restoreTTY()

	stdout, _, err := runAddInner(t, "same")
	if err != nil {
		t.Fatalf("runAdd --auto same credential should not be ambiguous: %v\nstdout=%s", err, stdout)
	}
	if !strings.Contains(stdout, "environment: duplicate of clipboard") {
		t.Fatalf("stdout missing within-sweep duplicate marker:\n%s", stdout)
	}
	if strings.Contains(stdout, "multiple new credentials discovered") {
		t.Fatalf("same credential was treated as multiple new choices:\n%s", stdout)
	}
}

func TestAddAuto_NoClipboardToolDoesNotAbortEnv(t *testing.T) {
	newAddHarness(t)
	addAutoFlag = true
	addDryRunFlag = true
	restoreClipboard := setAddAutoClipboardForTest(func() (string, bool, error) {
		return "", false, os.ErrNotExist
	})
	defer restoreClipboard()
	restoreEnv := envextract.SetLookupForTest(addEnvUniverse(map[string]string{
		"ANTHROPIC_BASE_URL":   "https://api.anthropic.com",
		"ANTHROPIC_AUTH_TOKEN": "sk-env-after-clip-1234",
	}))
	defer restoreEnv()

	stdout, _, err := runAddInner(t, "clipmissing")
	if err != nil {
		t.Fatalf("runAdd --auto no clipboard tool: %v\nstdout=%s", err, stdout)
	}
	if !strings.Contains(stdout, "clipboard: skipped: file does not exist") {
		t.Fatalf("stdout missing clipboard skip reason:\n%s", stdout)
	}
	if !strings.Contains(stdout, "environment: NEW") {
		t.Fatalf("stdout missing env candidate after clipboard skip:\n%s", stdout)
	}
}

func TestAddAuto_MutuallyExclusiveWithOtherInputSources(t *testing.T) {
	newAddHarness(t)
	addAutoFlag = true
	addFromEnvFlag = true

	_, _, err := runAddInner(t, "exclusive")
	if err == nil {
		t.Fatalf("runAdd --auto --from-env unexpectedly succeeded")
	}
	if !strings.Contains(err.Error(), "choose only one add input source") {
		t.Fatalf("error missing mutual exclusion style: %v", err)
	}
	if !strings.Contains(err.Error(), "--auto") {
		t.Fatalf("error missing --auto in source list: %v", err)
	}
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

func withOSStdin(t *testing.T, input string) func() {
	t.Helper()
	old := os.Stdin
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	if _, err := w.WriteString(input); err != nil {
		t.Fatalf("write stdin pipe: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("close stdin writer: %v", err)
	}
	os.Stdin = r
	return func() {
		os.Stdin = old
		_ = r.Close()
	}
}
