//go:build test

package cmd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/a2d2-dev/claudecm/internal/envextract"
)

func TestAdd_FromEnvDryRunRedactsAndDoesNotWrite(t *testing.T) {
	h := newAddHarness(t)
	restore := envextract.SetLookupForTest(addEnvUniverse(map[string]string{
		"ANTHROPIC_BASE_URL":         "https://env.example.com",
		"ANTHROPIC_AUTH_TOKEN":       "sk-env-token-1234",
		"ANTHROPIC_MODEL":            "claude-env-model",
		"ANTHROPIC_SMALL_FAST_MODEL": "claude-env-small",
	}))
	t.Cleanup(restore)
	addFromEnvFlag = true
	addDryRunFlag = true

	stdout, _, err := runAddInner(t, "envprof")
	if err != nil {
		t.Fatalf("runAdd --from-env: %v", err)
	}
	for _, want := range []string{
		"base_url: https://env.example.com",
		"model: claude-env-model",
		"small_fast_model: claude-env-small",
	} {
		if !strings.Contains(stdout, want) {
			t.Fatalf("dry-run missing %q:\n%s", want, stdout)
		}
	}
	if strings.Contains(stdout, "sk-env-token-1234") {
		t.Fatalf("dry-run leaked plaintext api key:\n%s", stdout)
	}
	if !strings.Contains(stdout, "sk-e***1234") {
		t.Fatalf("dry-run missing redacted api key:\n%s", stdout)
	}
	if _, statErr := os.Stat(filepath.Join(h.home, ".claudecm", "profiles", "envprof.yaml")); !os.IsNotExist(statErr) {
		t.Fatalf("profile file written despite --dry-run: %v", statErr)
	}
}

func TestAdd_FromEnvNoKeyRefusesWithoutWrite(t *testing.T) {
	h := newAddHarness(t)
	restore := envextract.SetLookupForTest(addEnvUniverse(map[string]string{
		"ANTHROPIC_BASE_URL": "https://env.example.com",
		"ANTHROPIC_MODEL":    "claude-env-model",
	}))
	t.Cleanup(restore)
	addFromEnvFlag = true

	_, _, err := runAddInner(t, "nokey")
	if err == nil {
		t.Fatalf("--from-env without key accepted")
	}
	if !strings.Contains(err.Error(), "no API key found in environment") {
		t.Fatalf("error = %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(h.home, ".claudecm", "profiles", "nokey.yaml")); !os.IsNotExist(statErr) {
		t.Fatalf("profile file written despite missing env key: %v", statErr)
	}
}

func TestAdd_FromEnvExplicitModelOverridesEnv(t *testing.T) {
	h := newAddHarness(t)
	restore := envextract.SetLookupForTest(addEnvUniverse(map[string]string{
		"ANTHROPIC_AUTH_TOKEN": "sk-env-override-1234",
		"ANTHROPIC_MODEL":      "env-model",
	}))
	t.Cleanup(restore)
	addFromEnvFlag = true
	addModelFlag = "flag-model"

	if _, _, err := runAddInner(t, "envoverride"); err != nil {
		t.Fatalf("runAdd --from-env override: %v", err)
	}
	loaded, err := h.store.LoadProfile("envoverride")
	if err != nil {
		t.Fatalf("LoadProfile: %v", err)
	}
	if loaded.Core.Model != "flag-model" {
		t.Fatalf("Model = %q, want flag-model", loaded.Core.Model)
	}
}

func addEnvUniverse(m map[string]string) func(string) (string, bool) {
	return func(name string) (string, bool) {
		v, ok := m[name]
		return v, ok
	}
}
