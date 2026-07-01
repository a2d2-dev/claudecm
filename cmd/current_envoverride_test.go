//go:build test

package cmd

// current_envoverride_test.go — Story E6-S1 tagged test.
//
// Verifies that when the process env carries ANTHROPIC_MODEL, the
// highlighted Model value in `claudecm current` reflects the env
// override rather than the profile's Core.Model. Uses the envextract
// SetLookupForTest seam so the test does not rely on ambient
// os.Environ() state; symmetric with explain_shadowed_test.go.

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/a2d2-dev/claudecm/internal/envextract"
)

// TestCurrent_EnvOverrideShownInEffective seeds a profile with
// Model="opus" and injects ANTHROPIC_MODEL=env-only-model via the
// envextract seam. The compact summary must show the env value as the
// Model highlight because EnvOverride is the winning layer for that
// key.
func TestCurrent_EnvOverrideShownInEffective(t *testing.T) {
	h := newCurrentHarness(t)
	h.saveProfile("prod", "sk-longsecretvalue123", "https://api.anthropic.com", "opus")
	h.activate("prod")

	restore := envextract.SetLookupForTest(func(name string) (string, bool) {
		if name == "ANTHROPIC_MODEL" {
			return "env-only-model", true
		}
		return "", false
	})
	defer restore()

	stdout, _, err := runCurrentCmd(t)
	if err != nil {
		t.Fatalf("runCurrent err = %v", err)
	}
	if !strings.Contains(stdout, "Model: env-only-model") {
		t.Errorf("stdout missing 'Model: env-only-model'; env override should win:\n%s", stdout)
	}
	if strings.Contains(stdout, "Model: opus") {
		t.Errorf("stdout still shows profile Model 'opus'; env override should shadow it:\n%s", stdout)
	}
}

// TestCurrent_CodexBaseURLHighlight verifies F3: the codex compact
// block includes a "Base URL" line sourced from the OPENAI_BASE_URL
// env override (which shadows model_providers.openai.base_url per the
// codex adapter). The tagged env seam is the cleanest way to give the
// value a source — Profile.Core.BaseURL does NOT map to any codex
// config.toml key in v1 (see internal/adapter/codex/project.go
// "Core mapping conservatism"). Asserts both text and JSON forms.
func TestCurrent_CodexBaseURLHighlight(t *testing.T) {
	h := newCurrentHarness(t)
	h.saveProfile("prod", "sk-longsecretvalue123", "https://api.anthropic.com", "opus")
	h.activate("prod")

	restore := envextract.SetLookupForTest(func(name string) (string, bool) {
		if name == "OPENAI_BASE_URL" {
			return "https://relay.example.com", true
		}
		return "", false
	})
	defer restore()

	// Narrow to codex so the assertion is unambiguous — claude_code
	// carries its own Base URL line.
	currentToolFlag = "codex"
	stdout, _, err := runCurrentCmd(t)
	if err != nil {
		t.Fatalf("runCurrent(text) err = %v", err)
	}
	if !strings.Contains(stdout, "codex:") {
		t.Fatalf("stdout missing codex block:\n%s", stdout)
	}
	if !strings.Contains(stdout, "Base URL: https://relay.example.com") {
		t.Errorf("stdout missing 'Base URL: https://relay.example.com':\n%s", stdout)
	}

	// JSON form — base_url must appear as an effective key on the
	// codex tool with the env value.
	currentOutputFlag = "json"
	stdout, _, err = runCurrentCmd(t)
	if err != nil {
		t.Fatalf("runCurrent(json) err = %v", err)
	}
	var doc jsonCurrent
	if err := json.Unmarshal([]byte(stdout), &doc); err != nil {
		t.Fatalf("stdout not valid JSON: %v\n%s", err, stdout)
	}
	var codexTool *jsonCurrentTool
	for i, tv := range doc.Tools {
		if tv.ID == "codex" {
			codexTool = &doc.Tools[i]
			break
		}
	}
	if codexTool == nil {
		t.Fatalf("json missing codex tool: %+v", doc.Tools)
	}
	var gotBaseURL any
	found := false
	for _, e := range codexTool.Effective {
		if e.Key == "base_url" {
			gotBaseURL = e.Value
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("json codex missing base_url effective key; got=%+v", codexTool.Effective)
	}
	if gotBaseURL != "https://relay.example.com" {
		t.Errorf("json codex base_url = %v, want https://relay.example.com", gotBaseURL)
	}
}
