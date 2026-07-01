package cmd

// current_test.go — Story E6-S1 tests for the cmd/current surface.
//
// Test isolation strategy mirrors cmd/explain_test.go:
//   1. HOME points at a per-test t.TempDir() so storage.Default() reads
//      a fresh tree and the developer's real ~/.claudecm is never
//      touched.
//   2. clearAdapterEnv (from explain_test.go) wipes the per-adapter env
//      allowlist so ambient env cannot leak into layer resolution.
//   3. resetCurrentFlags restores the cobra flag package-vars.
//   4. runCurrentCmd wraps runCurrent with bytes.Buffers for stdout /
//      stderr capture so no /dev/stdout wiring is needed.
//
// Tests are non-parallel by construction (t.Setenv), which is fine —
// current is fast and read-only. The tagged TestCurrent_EnvOverrideShownInEffective
// case relies on the same env-injection pattern; it is guarded by the
// "test" build tag only so the underlying seam can evolve without
// disturbing the untagged path.

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"github.com/a2d2-dev/claudecm/internal/adapter"
	"github.com/a2d2-dev/claudecm/internal/adapter/stateio"
	"github.com/a2d2-dev/claudecm/internal/resolver"
)

// resetCurrentFlags restores the package-level flag vars to their init()
// defaults. Every test calls this before mutating them so ordering
// between tests is irrelevant.
func resetCurrentFlags() {
	currentOutputFlag = "text"
	currentRevealFlag = false
	currentToolFlag = ""
}

// newCurrentHarness reuses newExplainHarness (identical bootstrap) and
// additionally resets the current flags. Keeping a thin wrapper so the
// test intent (this is a current test) is legible.
func newCurrentHarness(t *testing.T) *explainHarness {
	t.Helper()
	h := newExplainHarness(t)
	resetCurrentFlags()
	return h
}

// runCurrentCmd invokes runCurrent with a synthetic cobra.Command whose
// Out/Err are bytes.Buffers. Returns captured stdout, stderr, and the
// error return of runCurrent.
func runCurrentCmd(t *testing.T) (stdout, stderr string, err error) {
	t.Helper()
	var out, errBuf bytes.Buffer
	cmd := &cobra.Command{Use: "current"}
	cmd.SetOut(&out)
	cmd.SetErr(&errBuf)
	err = runCurrent(cmd, nil)
	return out.String(), errBuf.String(), err
}

// TestCurrent_ActiveProfileTextOutput: seed active profile +
// settings.json → compact block with model / base_url / api_key.
func TestCurrent_ActiveProfileTextOutput(t *testing.T) {
	h := newCurrentHarness(t)
	h.saveProfile("prod", "sk-longsecretvalue123", "https://api.anthropic.com", "opus")
	h.activate("prod")

	stdout, _, err := runCurrentCmd(t)
	if err != nil {
		t.Fatalf("runCurrent err = %v", err)
	}
	if !strings.Contains(stdout, "Profile: prod") {
		t.Errorf("stdout missing 'Profile: prod':\n%s", stdout)
	}
	if !strings.Contains(stdout, "claude_code:") {
		t.Errorf("stdout missing claude_code block:\n%s", stdout)
	}
	if !strings.Contains(stdout, "Model:") {
		t.Errorf("stdout missing Model line:\n%s", stdout)
	}
	if !strings.Contains(stdout, "Base URL: https://api.anthropic.com") {
		t.Errorf("stdout missing base URL:\n%s", stdout)
	}
	if !strings.Contains(stdout, "API Key:") {
		t.Errorf("stdout missing API Key line:\n%s", stdout)
	}
	// Redaction of profile.core secret.
	if strings.Contains(stdout, "sk-longsecretvalue123") {
		t.Errorf("stdout leaked secret; must be redacted by default:\n%s", stdout)
	}
	// Missing description → no Description line.
	if strings.Contains(stdout, "Description:") {
		t.Errorf("stdout should not contain Description when profile has none:\n%s", stdout)
	}
}

// TestCurrent_NoActiveProfileMessage: no active profile → prints the
// informational message and exits 0 (story AC).
func TestCurrent_NoActiveProfileMessage(t *testing.T) {
	newCurrentHarness(t)

	stdout, _, err := runCurrentCmd(t)
	if err != nil {
		t.Fatalf("runCurrent err = %v, want nil (informational, not error)", err)
	}
	if !strings.Contains(stdout, "no active profile") {
		t.Errorf("stdout missing 'no active profile' message:\n%s", stdout)
	}
}

// TestCurrent_NoActiveProfileJSON: no active profile, --output json →
// valid JSON with empty profile name and empty tools slice.
func TestCurrent_NoActiveProfileJSON(t *testing.T) {
	newCurrentHarness(t)

	currentOutputFlag = "json"
	stdout, _, err := runCurrentCmd(t)
	if err != nil {
		t.Fatalf("runCurrent err = %v", err)
	}
	var doc jsonCurrent
	if err := json.Unmarshal([]byte(stdout), &doc); err != nil {
		t.Fatalf("stdout not valid JSON: %v\n%s", err, stdout)
	}
	if doc.Profile.Name != "" {
		t.Errorf("json profile.name = %q, want empty", doc.Profile.Name)
	}
	if doc.Tools == nil {
		t.Errorf("json tools = nil, want empty slice")
	}
	if len(doc.Tools) != 0 {
		t.Errorf("json tools len = %d, want 0", len(doc.Tools))
	}
}

// TestCurrent_JSONOutputParses: --output json parses into jsonCurrent
// with the expected top-level shape and preserves redaction.
func TestCurrent_JSONOutputParses(t *testing.T) {
	h := newCurrentHarness(t)
	h.saveProfile("prod", "sk-longsecretvalue123", "https://api.anthropic.com", "opus")
	h.activate("prod")

	currentOutputFlag = "json"
	stdout, _, err := runCurrentCmd(t)
	if err != nil {
		t.Fatalf("runCurrent err = %v", err)
	}
	var doc jsonCurrent
	if err := json.Unmarshal([]byte(stdout), &doc); err != nil {
		t.Fatalf("stdout not valid JSON: %v\n%s", err, stdout)
	}
	if doc.Profile.Name != "prod" {
		t.Errorf("json profile.name = %q, want prod", doc.Profile.Name)
	}
	if len(doc.Tools) == 0 {
		t.Errorf("json tools empty; want at least one entry")
	}
	if strings.Contains(stdout, "sk-longsecretvalue123") {
		t.Errorf("json leaked secret without --reveal:\n%s", stdout)
	}
	// The claude_code tool must expose the expected effective keys.
	var claudeTool *jsonCurrentTool
	for i, tv := range doc.Tools {
		if tv.ID == "claude_code" {
			claudeTool = &doc.Tools[i]
			break
		}
	}
	if claudeTool == nil {
		t.Fatalf("json missing claude_code tool: %+v", doc.Tools)
	}
	wantKeys := map[string]bool{"model": false, "base_url": false, "api_key": false}
	for _, e := range claudeTool.Effective {
		if _, ok := wantKeys[e.Key]; ok {
			wantKeys[e.Key] = true
		}
	}
	for k, seen := range wantKeys {
		if !seen {
			t.Errorf("json claude_code missing effective key %q; got=%+v", k, claudeTool.Effective)
		}
	}
}

// TestCurrent_RedactsSecretsByDefault: secret plaintext does NOT
// appear in output.
func TestCurrent_RedactsSecretsByDefault(t *testing.T) {
	h := newCurrentHarness(t)
	h.saveProfile("prod", "sk-uniquesecret-abcdxyz", "https://api.anthropic.com", "opus")
	h.activate("prod")

	stdout, _, err := runCurrentCmd(t)
	if err != nil {
		t.Fatalf("runCurrent err = %v", err)
	}
	if strings.Contains(stdout, "sk-uniquesecret-abcdxyz") {
		t.Errorf("stdout leaked full secret:\n%s", stdout)
	}
	// The redacted shape (first4 + *** + last4) MUST appear for the
	// API Key line so operators can eyeball that the value is set.
	if !strings.Contains(stdout, "sk-u***dxyz") {
		t.Errorf("stdout missing redacted API key:\n%s", stdout)
	}
}

// TestCurrent_RevealShowsPlaintext: --reveal surfaces plaintext AND
// prints the stderr warning.
func TestCurrent_RevealShowsPlaintext(t *testing.T) {
	h := newCurrentHarness(t)
	h.saveProfile("prod", "sk-fullreveal-secret-xyz", "https://api.anthropic.com", "opus")
	h.activate("prod")

	currentRevealFlag = true
	stdout, stderr, err := runCurrentCmd(t)
	if err != nil {
		t.Fatalf("runCurrent err = %v", err)
	}
	if !strings.Contains(stdout, "sk-fullreveal-secret-xyz") {
		t.Errorf("--reveal did not surface plaintext:\n%s", stdout)
	}
	if !strings.Contains(stderr, "WARNING: --reveal") {
		t.Errorf("--reveal did not emit stderr warning; got=%q", stderr)
	}
}

// TestCurrent_ToolFilterNarrows: --tool claude_code → codex section
// is absent.
func TestCurrent_ToolFilterNarrows(t *testing.T) {
	h := newCurrentHarness(t)
	h.saveProfile("prod", "sk-longsecretvalue123", "https://api.anthropic.com", "opus")
	h.activate("prod")

	currentToolFlag = "claude_code"
	stdout, _, err := runCurrentCmd(t)
	if err != nil {
		t.Fatalf("runCurrent err = %v", err)
	}
	if !strings.Contains(stdout, "claude_code:") {
		t.Errorf("stdout missing claude_code block under --tool=claude_code:\n%s", stdout)
	}
	if strings.Contains(stdout, "codex:") {
		t.Errorf("stdout still contains codex block despite --tool=claude_code:\n%s", stdout)
	}
}

// TestCurrent_DriftShown: seed State.LastAppliedPerTool with a
// mismatched SHA → drift line rendered.
func TestCurrent_DriftShown(t *testing.T) {
	h := newCurrentHarness(t)
	h.saveProfile("prod", "sk-longsecretvalue123", "https://api.anthropic.com", "opus")
	h.activate("prod")
	settingsPath := h.writeSettingsJSON(`{"env":{"ANTHROPIC_MODEL":"disk-model"}}`)

	if err := stateio.RecordApplied(h.resv, adapter.ToolClaudeCode, settingsPath, "bogus-sha", time.Now()); err != nil {
		t.Fatalf("RecordApplied: %v", err)
	}

	stdout, _, err := runCurrentCmd(t)
	if err != nil {
		t.Fatalf("runCurrent err = %v", err)
	}
	if !strings.Contains(stdout, "Drift:") {
		t.Errorf("stdout missing Drift line:\n%s", stdout)
	}
	if !strings.Contains(stdout, "externally edited") {
		t.Errorf("stdout missing 'externally edited':\n%s", stdout)
	}
}

// TestCurrent_MissingFieldsRenderCleanly: profile has an API key +
// base URL but no explicit model → the Model line still renders as
// "(not set)" because the adapter emits no env.ANTHROPIC_MODEL entry.
// Exercises the displayHighlight nil path so a partially-configured
// profile does not crash the compact renderer.
func TestCurrent_MissingFieldsRenderCleanly(t *testing.T) {
	h := newCurrentHarness(t)
	// Model empty → adapter emits no env.ANTHROPIC_MODEL projection.
	// The validator requires non-empty api_key so we cannot exercise
	// the fully-empty case at the CLI seam; the pure-function unit
	// test TestSelectHighlightFields_APIKeyFallback covers the nil
	// path for the API-key field.
	h.saveProfile("prod", "sk-longsecretvalue123", "https://api.anthropic.com", "")
	h.activate("prod")

	stdout, _, err := runCurrentCmd(t)
	if err != nil {
		t.Fatalf("runCurrent err = %v", err)
	}
	if !strings.Contains(stdout, "claude_code:") {
		t.Errorf("stdout missing claude_code block:\n%s", stdout)
	}
	if !strings.Contains(stdout, "Model: (not set)") {
		t.Errorf("stdout missing 'Model: (not set)' line:\n%s", stdout)
	}
	if !strings.Contains(stdout, "Presence: Installed=") {
		t.Errorf("stdout missing Presence line:\n%s", stdout)
	}
}

// TestCurrent_InvalidOutputFlag: --output yaml is rejected loudly.
func TestCurrent_InvalidOutputFlag(t *testing.T) {
	h := newCurrentHarness(t)
	h.saveProfile("prod", "sk-longsecretvalue123", "https://api.anthropic.com", "opus")
	h.activate("prod")

	currentOutputFlag = "yaml"
	_, _, err := runCurrentCmd(t)
	if err == nil {
		t.Fatal("runCurrent err = nil for --output yaml, want error")
	}
	if !strings.Contains(err.Error(), "invalid --output") {
		t.Errorf("error = %v, want mention of 'invalid --output'", err)
	}
}

// TestCurrent_JSONRedactionAndReveal covers both JSON paths:
//   - default: secret redacted in the JSON value
//   - --reveal: secret surfaces in JSON value + stderr warning
func TestCurrent_JSONRedactionAndReveal(t *testing.T) {
	h := newCurrentHarness(t)
	h.saveProfile("prod", "sk-jsonrevealsecret-abcd", "https://api.anthropic.com", "opus")
	h.activate("prod")

	currentOutputFlag = "json"
	// default redaction
	stdout, _, err := runCurrentCmd(t)
	if err != nil {
		t.Fatalf("runCurrent err = %v", err)
	}
	if strings.Contains(stdout, "sk-jsonrevealsecret-abcd") {
		t.Errorf("json leaked secret without --reveal:\n%s", stdout)
	}
	var doc jsonCurrent
	if err := json.Unmarshal([]byte(stdout), &doc); err != nil {
		t.Fatalf("stdout not valid JSON: %v\n%s", err, stdout)
	}

	// reveal
	currentRevealFlag = true
	stdout, stderr, err := runCurrentCmd(t)
	if err != nil {
		t.Fatalf("runCurrent err = %v", err)
	}
	if !strings.Contains(stdout, "sk-jsonrevealsecret-abcd") {
		t.Errorf("json --reveal did not surface plaintext:\n%s", stdout)
	}
	if !strings.Contains(stderr, "WARNING: --reveal") {
		t.Errorf("json --reveal did not emit stderr warning; got=%q", stderr)
	}
}

// TestCurrent_ProfileWithDescriptionRendersIt: description field is
// rendered when the profile has one set.
func TestCurrent_ProfileWithDescriptionRendersIt(t *testing.T) {
	h := newCurrentHarness(t)
	p := h.saveProfile("prod", "sk-longsecretvalue123", "https://api.anthropic.com", "opus")
	p.Description = "production workspace"
	if err := h.mgr.UpdateProfile(p.Name, p); err != nil {
		t.Fatalf("UpdateProfile: %v", err)
	}
	h.activate("prod")

	stdout, _, err := runCurrentCmd(t)
	if err != nil {
		t.Fatalf("runCurrent err = %v", err)
	}
	if !strings.Contains(stdout, "Description: production workspace") {
		t.Errorf("stdout missing Description line:\n%s", stdout)
	}
}

// TestSelectHighlightFields_APIKeyFallback exercises the sentinel
// fallback path in selectHighlightFields directly: when the
// env.ANTHROPIC_AUTH_TOKEN field is absent from the ToolView but
// env.ANTHROPIC_API_KEY is present, the API Key highlight surfaces
// the fallback value. This is a pure-function test — the CLI-level
// test cannot easily produce this shape because the validator forces
// a non-empty Core.APIKey (which the adapter routes to AUTH_TOKEN).
func TestSelectHighlightFields_APIKeyFallback(t *testing.T) {
	tv := resolver.ToolView{
		Tool: adapter.ToolClaudeCode,
		Effective: adapter.EffectiveView{
			Tool: adapter.ToolClaudeCode,
			Fields: []adapter.EffectiveField{
				{Key: "env.ANTHROPIC_MODEL", Value: "opus"},
				{Key: "env.ANTHROPIC_BASE_URL", Value: "https://api.anthropic.com"},
				{Key: "env.ANTHROPIC_API_KEY", Value: "sk-fallback-value-1234", Secret: true},
			},
		},
	}
	got := selectHighlightFields(tv, true /*reveal*/)
	// Locate the API Key highlight.
	var apiKey *highlightRender
	for i := range got {
		if got[i].Label == "API Key" {
			apiKey = &got[i]
		}
	}
	if apiKey == nil {
		t.Fatalf("selectHighlightFields did not emit API Key entry; got=%+v", got)
	}
	if apiKey.Value != "sk-fallback-value-1234" {
		t.Errorf("API Key fallback value = %v, want sk-fallback-value-1234", apiKey.Value)
	}
	if !apiKey.Secret {
		t.Errorf("API Key fallback Secret = false, want true")
	}
	if apiKey.Display != "sk-fallback-value-1234" {
		t.Errorf("API Key fallback Display = %q, want plaintext (reveal=true)", apiKey.Display)
	}
}

// TestSelectHighlightFields_UnknownToolReturnsNil covers the branch
// where a ToolView carries a tool id absent from highlightSpecs.
func TestSelectHighlightFields_UnknownToolReturnsNil(t *testing.T) {
	tv := resolver.ToolView{Tool: adapter.ToolID("unknown_tool")}
	if got := selectHighlightFields(tv, false); got != nil {
		t.Errorf("selectHighlightFields for unknown tool = %+v, want nil", got)
	}
}

// TestSelectHighlightFields_AllMissing verifies that when NEITHER
// AUTH_TOKEN nor API_KEY resolves, the API Key highlight surfaces as
// nil Value and Display "(not set)".
func TestSelectHighlightFields_AllMissing(t *testing.T) {
	tv := resolver.ToolView{
		Tool: adapter.ToolClaudeCode,
		Effective: adapter.EffectiveView{
			Tool:   adapter.ToolClaudeCode,
			Fields: nil,
		},
	}
	got := selectHighlightFields(tv, false)
	for _, h := range got {
		if h.Value != nil {
			t.Errorf("highlight %q Value = %v, want nil", h.Label, h.Value)
		}
		if h.Display != "(not set)" {
			t.Errorf("highlight %q Display = %q, want '(not set)'", h.Label, h.Display)
		}
	}
}

// TestCurrent_UnknownToolInFilterEmptyBlock: filtering on an unknown
// tool ID produces the "(no tools resolved)" banner without erroring.
func TestCurrent_UnknownToolInFilterEmptyBlock(t *testing.T) {
	h := newCurrentHarness(t)
	h.saveProfile("prod", "sk-longsecretvalue123", "https://api.anthropic.com", "opus")
	h.activate("prod")

	currentToolFlag = "does_not_exist"
	stdout, _, err := runCurrentCmd(t)
	if err != nil {
		t.Fatalf("runCurrent err = %v", err)
	}
	if !strings.Contains(stdout, "(no tools resolved)") {
		t.Errorf("stdout missing 'no tools resolved' banner:\n%s", stdout)
	}
}

// ---------------------------------------------------------------------------
// Pure-function unit tests
// ---------------------------------------------------------------------------

// TestParseCurrentOutput_Values mirrors the explain equivalent.
func TestParseCurrentOutput_Values(t *testing.T) {
	cases := []struct {
		in      string
		wantFmt currentOutputFormat
		wantErr bool
	}{
		{"", currentOutputText, false},
		{"text", currentOutputText, false},
		{"TEXT", currentOutputText, false},
		{"json", currentOutputJSON, false},
		{"JSON", currentOutputJSON, false},
		{"yaml", "", true},
		{"garbage", "", true},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			got, err := parseCurrentOutput(tc.in)
			if tc.wantErr {
				if err == nil {
					t.Errorf("parseCurrentOutput(%q) err = nil, want error", tc.in)
				}
				return
			}
			if err != nil {
				t.Errorf("parseCurrentOutput(%q) err = %v, want nil", tc.in, err)
			}
			if got != tc.wantFmt {
				t.Errorf("parseCurrentOutput(%q) = %q, want %q", tc.in, got, tc.wantFmt)
			}
		})
	}
}

// TestDisplayHighlight_Cases locks the missing/secret/reveal
// combinatorics for the text renderer.
func TestDisplayHighlight_Cases(t *testing.T) {
	cases := []struct {
		name   string
		v      any
		secret bool
		reveal bool
		want   string
	}{
		{"nil-nonsecret", nil, false, false, "(not set)"},
		{"nil-secret", nil, true, false, "(not set)"},
		{"plain-string", "opus", false, false, "opus"},
		{"plain-bool", true, false, false, "true"},
		{"secret-hidden", "sk-longsecretvalue123", true, false, "sk-l***e123"},
		{"secret-revealed", "sk-longsecretvalue123", true, true, "sk-longsecretvalue123"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := displayHighlight(tc.v, tc.secret, tc.reveal); got != tc.want {
				t.Errorf("displayHighlight(%v, secret=%v, reveal=%v) = %q, want %q",
					tc.v, tc.secret, tc.reveal, got, tc.want)
			}
		})
	}
}

// TestJSONCurrentValue_Cases locks the JSON-side missing/secret/reveal
// combinatorics. Missing values become "" (empty string) so the JSON
// key is always present.
func TestJSONCurrentValue_Cases(t *testing.T) {
	cases := []struct {
		name   string
		v      any
		secret bool
		reveal bool
		want   any
	}{
		{"nil-nonsecret", nil, false, false, ""},
		{"nil-secret", nil, true, false, ""},
		{"plain-string", "opus", false, false, "opus"},
		{"plain-bool", true, false, false, true},
		{"secret-hidden", "sk-longsecretvalue123", true, false, "sk-l***e123"},
		{"secret-revealed", "sk-longsecretvalue123", true, true, "sk-longsecretvalue123"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := jsonCurrentValue(tc.v, tc.secret, tc.reveal)
			if got != tc.want {
				t.Errorf("jsonCurrentValue(%v, secret=%v, reveal=%v) = %v, want %v",
					tc.v, tc.secret, tc.reveal, got, tc.want)
			}
		})
	}
}
