package cmd

// explain_test.go — Story E5-S5 tests for the cmd/explain surface.
//
// Test isolation strategy:
//   1. Every test switches HOME to a per-test t.TempDir() so storage.Default()
//      reads a fresh tree — the developer's real ~/.claudecm is never touched.
//   2. Every test clears the per-adapter env-var allowlist (via clearAdapterEnv)
//      before running so an ambient developer env cannot leak into an
//      assertion.
//   3. Every test resets the package-level cobra flag vars (via
//      resetExplainFlags) — those are set from init() and mutated by cobra's
//      flag binding; explain tests bypass cobra flag parsing and set them
//      directly.
//   4. Tests use runExplain(cmd, args) directly with a synthetic
//      cobra.Command whose Out/Err are bytes.Buffers so output capture is
//      deterministic across platforms (no /dev/stdout wiring, no golden
//      files).
//
// t.Setenv makes each test non-parallel — the whole file runs
// sequentially by construction. That is acceptable here because
// (a) explain is fast (no writes, no locks), and (b) HOME rewiring is
// inherently process-global.

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"github.com/a2d2-dev/claudecm/internal/adapter"
	"github.com/a2d2-dev/claudecm/internal/adapter/stateio"
	"github.com/a2d2-dev/claudecm/internal/config"
	"github.com/a2d2-dev/claudecm/internal/storage"
)

// adapterEnvVarNames is the union of the two v1 adapters' owned env-var
// names. Cleared at the top of every test via t.Setenv so an ambient dev
// env (an operator with ANTHROPIC_API_KEY exported at their shell)
// cannot leak into the layer chain under test. Kept local to the test
// file — production code uses ownedEnvVarNames() (explain.go) for the
// diagnostic-filter allowlist.
var adapterEnvVarNames = []string{
	"ANTHROPIC_API_KEY",
	"ANTHROPIC_AUTH_TOKEN",
	"ANTHROPIC_BASE_URL",
	"ANTHROPIC_MODEL",
	"ANTHROPIC_SMALL_FAST_MODEL",
	"CLAUDE_CODE_USE_BEDROCK",
	"CLAUDE_CODE_USE_VERTEX",
	"OPENAI_API_KEY",
	"OPENAI_BASE_URL",
	"CODEX_MODEL",
	"CODEX_MODEL_PROVIDER",
}

// clearAdapterEnv wipes every env var either adapter treats as an
// EnvOverride source. Symmetric with claudecode/project_test.go's
// clearClaudeEnv but broader — explain resolves BOTH tools.
func clearAdapterEnv(t *testing.T) {
	t.Helper()
	for _, name := range adapterEnvVarNames {
		t.Setenv(name, "")
	}
	// Also clear the diagnostic-prefix names the --all-env tests inject
	// (RANDOM_VAR, ANTHROPIC_UNKNOWN) so leaked developer env cannot
	// spoof either assertion.
	t.Setenv("RANDOM_VAR", "")
	t.Setenv("ANTHROPIC_UNKNOWN", "")
}

// resetExplainFlags restores the package-level flag vars to their init()
// defaults. Every test calls this before mutating them so ordering
// between tests is irrelevant.
func resetExplainFlags() {
	explainOutputFlag = "text"
	explainRevealFlag = false
	explainToolFlag = ""
	explainAllEnvFlag = false
}

// explainHarness wires the per-test HOME tree: TempDir, HOME rewire,
// Bootstrap, and a Manager backed by FileStorage. Every explain test
// starts here so profile / state / on-disk seeding stays one-liner
// simple.
type explainHarness struct {
	t     *testing.T
	home  string
	resv  *storage.Resolver
	store *storage.FileStorage
	mgr   *config.Manager
}

func newExplainHarness(t *testing.T) *explainHarness {
	t.Helper()
	clearAdapterEnv(t)
	resetExplainFlags()

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
	mgr := config.NewManager(store, config.NewValidator())
	return &explainHarness{
		t:     t,
		home:  home,
		resv:  resv,
		store: store,
		mgr:   mgr,
	}
}

// saveProfile persists a fully-populated Profile via FileStorage. The
// caller supplies Core fields inline; overlays are absent unless the
// test explicitly adds them post-return.
func (h *explainHarness) saveProfile(name, apiKey, baseURL, model string) *config.Profile {
	h.t.Helper()
	p := config.NewProfile(name, baseURL, apiKey)
	p.Core.Model = model
	if err := h.mgr.AddProfile(p); err != nil {
		h.t.Fatalf("AddProfile(%q): %v", name, err)
	}
	return p
}

// activate sets state.CurrentProfile via mgr.SetActive.
func (h *explainHarness) activate(name string) {
	h.t.Helper()
	if err := h.mgr.SetActive(name); err != nil {
		h.t.Fatalf("SetActive(%q): %v", name, err)
	}
}

// writeSettingsJSON seeds ~/.claude/settings.json with the given bytes,
// creating the parent directory when absent. Used to give the
// OnDiskToolConfig layer a value in tests exercising the claude_code
// adapter.
func (h *explainHarness) writeSettingsJSON(body string) string {
	h.t.Helper()
	dir := filepath.Join(h.home, ".claude")
	if err := os.MkdirAll(dir, 0700); err != nil {
		h.t.Fatalf("mkdir %s: %v", dir, err)
	}
	path := filepath.Join(dir, "settings.json")
	if err := os.WriteFile(path, []byte(body), 0600); err != nil {
		h.t.Fatalf("write settings.json: %v", err)
	}
	return path
}

// runExplainCmd is the test entry point that mimics what cobra would
// pass to the RunE callback. Returns captured stdout, stderr, and the
// error return of runExplain.
func runExplainCmd(t *testing.T, args []string) (stdout, stderr string, err error) {
	t.Helper()
	var out, errBuf bytes.Buffer
	cmd := &cobra.Command{Use: "explain"}
	cmd.SetOut(&out)
	cmd.SetErr(&errBuf)
	err = runExplain(cmd, args)
	return out.String(), errBuf.String(), err
}

// ---------------------------------------------------------------------------
// Tests
// ---------------------------------------------------------------------------

// TestExplain_ActiveProfileDefaultText: seed active profile + settings.json,
// call explain with no positional args and default flags → each expected
// per-tool section renders and secrets are redacted.
func TestExplain_ActiveProfileDefaultText(t *testing.T) {
	h := newExplainHarness(t)
	h.saveProfile("prod", "sk-longsecretvalue123", "https://api.anthropic.com", "opus")
	h.activate("prod")
	h.writeSettingsJSON(`{"env":{"ANTHROPIC_MODEL":"disk-model"}}`)

	stdout, stderr, err := runExplainCmd(t, nil)
	if err != nil {
		t.Fatalf("runExplain err = %v, want nil; stderr=%s", err, stderr)
	}

	if !strings.Contains(stdout, "Profile: prod") {
		t.Errorf("stdout missing 'Profile: prod':\n%s", stdout)
	}
	if !strings.Contains(stdout, "Tool: claude_code") {
		t.Errorf("stdout missing claude_code section:\n%s", stdout)
	}
	if !strings.Contains(stdout, "Tool: codex") {
		t.Errorf("stdout missing codex section:\n%s", stdout)
	}
	if strings.Contains(stdout, "sk-longsecretvalue123") {
		t.Errorf("stdout leaked secret; must be redacted by default:\n%s", stdout)
	}
	if !strings.Contains(stdout, "env.ANTHROPIC_MODEL") {
		t.Errorf("stdout missing owned key env.ANTHROPIC_MODEL:\n%s", stdout)
	}
}

// TestExplain_NamedProfile: passing a positional arg names an explicit
// profile whose chain is what explain renders.
func TestExplain_NamedProfile(t *testing.T) {
	h := newExplainHarness(t)
	h.saveProfile("prod", "sk-prodkey12345678", "https://api.anthropic.com", "opus")
	h.saveProfile("dev", "sk-devkey87654321", "https://dev.anthropic.com", "haiku")
	h.activate("prod") // active is prod, but arg forces dev

	stdout, _, err := runExplainCmd(t, []string{"dev"})
	if err != nil {
		t.Fatalf("runExplain err = %v", err)
	}
	if !strings.Contains(stdout, "Profile: dev") {
		t.Errorf("stdout missing 'Profile: dev':\n%s", stdout)
	}
	if strings.Contains(stdout, "Profile: prod") {
		t.Errorf("stdout should not contain 'Profile: prod' when dev is named:\n%s", stdout)
	}
}

// TestExplain_NoActiveProfileErrors: no profiles saved, no active,
// no positional arg → error with actionable message.
func TestExplain_NoActiveProfileErrors(t *testing.T) {
	newExplainHarness(t)

	_, _, err := runExplainCmd(t, nil)
	if err == nil {
		t.Fatal("runExplain err = nil, want error")
	}
	if !strings.Contains(err.Error(), "no active profile") {
		t.Errorf("error = %v, want mention of 'no active profile'", err)
	}
}

// TestExplain_MissingProfileErrors: naming a profile that does not exist
// surfaces a not-found error.
func TestExplain_MissingProfileErrors(t *testing.T) {
	newExplainHarness(t)

	_, _, err := runExplainCmd(t, []string{"nonexistent"})
	if err == nil {
		t.Fatal("runExplain err = nil, want error")
	}
	if !strings.Contains(err.Error(), "nonexistent") {
		t.Errorf("error = %v, want mention of profile name", err)
	}
}

// TestExplain_JSONOutputParses: --output json produces valid JSON with
// the expected top-level shape.
func TestExplain_JSONOutputParses(t *testing.T) {
	h := newExplainHarness(t)
	h.saveProfile("prod", "sk-longsecretvalue123", "https://api.anthropic.com", "opus")
	h.activate("prod")

	explainOutputFlag = "json"
	stdout, _, err := runExplainCmd(t, nil)
	if err != nil {
		t.Fatalf("runExplain err = %v", err)
	}
	var doc jsonExplain
	if err := json.Unmarshal([]byte(stdout), &doc); err != nil {
		t.Fatalf("stdout is not valid JSON: %v\n%s", err, stdout)
	}
	if doc.Profile.Name != "prod" {
		t.Errorf("json profile.name = %q, want prod", doc.Profile.Name)
	}
	if len(doc.Tools) == 0 {
		t.Errorf("json tools empty; want at least one entry")
	}
	// Every field in JSON is checked for redaction: no revealed secret
	// substring may appear.
	if strings.Contains(stdout, "sk-longsecretvalue123") {
		t.Errorf("json leaked secret without --reveal:\n%s", stdout)
	}
}

// TestExplain_RedactsSecretsByDefault: an api_key set only via Core
// still surfaces as a Secret field with a redacted value.
func TestExplain_RedactsSecretsByDefault(t *testing.T) {
	h := newExplainHarness(t)
	h.saveProfile("prod", "sk-uniquesecret-abcdxyz", "https://api.anthropic.com", "opus")
	h.activate("prod")

	stdout, _, err := runExplainCmd(t, nil)
	if err != nil {
		t.Fatalf("runExplain err = %v", err)
	}
	if strings.Contains(stdout, "sk-uniquesecret-abcdxyz") {
		t.Errorf("stdout leaked full secret; want redaction. output:\n%s", stdout)
	}
	if !strings.Contains(stdout, "[SECRET]") {
		t.Errorf("stdout missing [SECRET] marker:\n%s", stdout)
	}
}

// TestExplain_RevealFlagEmitsStderrNotice: --reveal prints the plaintext
// value AND a stderr warning (NFR-S8).
func TestExplain_RevealFlagEmitsStderrNotice(t *testing.T) {
	h := newExplainHarness(t)
	h.saveProfile("prod", "sk-fullreveal-secret-xyz", "https://api.anthropic.com", "opus")
	h.activate("prod")

	explainRevealFlag = true
	stdout, stderr, err := runExplainCmd(t, nil)
	if err != nil {
		t.Fatalf("runExplain err = %v", err)
	}
	if !strings.Contains(stdout, "sk-fullreveal-secret-xyz") {
		t.Errorf("--reveal did not surface plaintext in stdout:\n%s", stdout)
	}
	if !strings.Contains(stderr, "WARNING: --reveal") {
		t.Errorf("--reveal did not emit stderr warning; got: %q", stderr)
	}
}

// TestExplain_ToolFilterNarrows: --tool claude_code excludes the codex
// section entirely.
func TestExplain_ToolFilterNarrows(t *testing.T) {
	h := newExplainHarness(t)
	h.saveProfile("prod", "sk-longsecretvalue123", "https://api.anthropic.com", "opus")
	h.activate("prod")

	explainToolFlag = "claude_code"
	stdout, _, err := runExplainCmd(t, nil)
	if err != nil {
		t.Fatalf("runExplain err = %v", err)
	}
	if !strings.Contains(stdout, "Tool: claude_code") {
		t.Errorf("stdout missing claude_code section under --tool=claude_code:\n%s", stdout)
	}
	if strings.Contains(stdout, "Tool: codex") {
		t.Errorf("stdout still contains codex section despite --tool=claude_code:\n%s", stdout)
	}
}

// TestExplain_DriftWarningRendered: seed state with a mismatched SHA256
// for ~/.claude/settings.json so the drift check fires.
func TestExplain_DriftWarningRendered(t *testing.T) {
	h := newExplainHarness(t)
	h.saveProfile("prod", "sk-longsecretvalue123", "https://api.anthropic.com", "opus")
	h.activate("prod")

	settingsPath := h.writeSettingsJSON(`{"env":{"ANTHROPIC_MODEL":"disk-model"}}`)

	// Record a bogus prior SHA256 so the drift check flags a mismatch.
	if err := stateio.RecordApplied(h.resv, adapter.ToolClaudeCode, settingsPath, "bogus-sha-does-not-match", time.Now()); err != nil {
		t.Fatalf("RecordApplied: %v", err)
	}

	stdout, _, err := runExplainCmd(t, nil)
	if err != nil {
		t.Fatalf("runExplain err = %v", err)
	}
	if !strings.Contains(stdout, "External drift") {
		t.Errorf("stdout missing external drift banner:\n%s", stdout)
	}
	if !strings.Contains(stdout, "externally edited") {
		t.Errorf("stdout missing drift explanation:\n%s", stdout)
	}
}

// TestExplain_ErrorsSectionRendersToolErrors: a malformed on-disk
// settings.json makes the claudecode adapter surface a ParseFailed
// error which explain renders under the tool's Errors section.
func TestExplain_ErrorsSectionRendersToolErrors(t *testing.T) {
	h := newExplainHarness(t)
	h.saveProfile("prod", "sk-longsecretvalue123", "https://api.anthropic.com", "opus")
	h.activate("prod")
	// Malformed JSON triggers ErrParseFailed inside the adapter, which
	// the resolver classifies as ErrorParseFailed on the ToolView.
	h.writeSettingsJSON(`{"env":`)

	stdout, _, err := runExplainCmd(t, nil)
	if err != nil {
		t.Fatalf("runExplain err = %v", err)
	}
	if !strings.Contains(stdout, "Errors:") {
		t.Errorf("stdout missing per-tool Errors section:\n%s", stdout)
	}
	if !strings.Contains(stdout, "ParseFailed") {
		t.Errorf("stdout missing ParseFailed error kind:\n%s", stdout)
	}
}

// TestExplain_EnvOverrideShownAsWinning: set ANTHROPIC_API_KEY in the
// process env → its EffectiveField shows EnvOverride winning over any
// lower layer. Uses t.Setenv (via clearAdapterEnv + explicit setenv) so
// no build-tag seam is required.
func TestExplain_EnvOverrideShownAsWinning(t *testing.T) {
	h := newExplainHarness(t)
	h.saveProfile("prod", "sk-profilecore-secret-xy", "https://api.anthropic.com", "opus")
	h.activate("prod")

	t.Setenv("ANTHROPIC_API_KEY", "sk-env-override-value-9x")

	explainRevealFlag = true
	stdout, _, err := runExplainCmd(t, nil)
	if err != nil {
		t.Fatalf("runExplain err = %v", err)
	}
	// env override wins for ANTHROPIC_API_KEY. Since --reveal is set,
	// plaintext is present.
	if !strings.Contains(stdout, "env.ANTHROPIC_API_KEY") {
		t.Errorf("stdout missing env.ANTHROPIC_API_KEY field:\n%s", stdout)
	}
	if !strings.Contains(stdout, "sk-env-override-value-9x") {
		t.Errorf("stdout missing env-override value:\n%s", stdout)
	}
	if !strings.Contains(stdout, "winning: EnvOverride") {
		t.Errorf("stdout does not report EnvOverride as winning layer:\n%s", stdout)
	}
}

// TestExplain_AllEnvListsMatchingVars: --all-env exposes env vars whose
// names match a per-tool prefix but are not adapter-owned. We assert
// against the JSON diagnostic_env map rather than the text output so
// the check is hermetic to whatever ambient CODEX_* / ANTHROPIC_* vars
// the CI runner or the developer's shell happens to export — those may
// still appear in the map but do not affect the specific asserts here.
func TestExplain_AllEnvListsMatchingVars(t *testing.T) {
	h := newExplainHarness(t)
	h.saveProfile("prod", "sk-longsecretvalue123", "https://api.anthropic.com", "opus")
	h.activate("prod")

	// The diagnostic dump reads live os.Environ(); inject via t.Setenv.
	// clearAdapterEnv already zeroed RANDOM_VAR / ANTHROPIC_UNKNOWN so
	// the assertions below are hermetic no matter what the ambient
	// shell exported.
	t.Setenv("ANTHROPIC_UNKNOWN", "unknown-value-xyz")
	t.Setenv("RANDOM_VAR", "should-not-appear")

	explainOutputFlag = "json"
	explainAllEnvFlag = true
	stdout, _, err := runExplainCmd(t, nil)
	if err != nil {
		t.Fatalf("runExplain err = %v", err)
	}
	var doc jsonExplain
	if err := json.Unmarshal([]byte(stdout), &doc); err != nil {
		t.Fatalf("stdout is not valid JSON: %v\n%s", err, stdout)
	}
	got, ok := doc.DiagnosticEnv["ANTHROPIC_UNKNOWN"]
	if !ok {
		t.Errorf("diagnostic_env missing ANTHROPIC_UNKNOWN; got=%v", doc.DiagnosticEnv)
	}
	if got != "unknown-value-xyz" {
		t.Errorf("diagnostic_env[ANTHROPIC_UNKNOWN]=%q, want %q", got, "unknown-value-xyz")
	}
	if _, hit := doc.DiagnosticEnv["RANDOM_VAR"]; hit {
		t.Errorf("diagnostic_env leaked RANDOM_VAR (no prefix match); got=%v", doc.DiagnosticEnv)
	}
	// Also: an owned env var must never appear in the diagnostic map
	// regardless of ambient environment.
	if _, hit := doc.DiagnosticEnv["ANTHROPIC_API_KEY"]; hit {
		t.Errorf("diagnostic_env leaked owned ANTHROPIC_API_KEY; got=%v", doc.DiagnosticEnv)
	}
}

// ---------------------------------------------------------------------------
// Pure-function unit tests (no HOME, no resolver)
// ---------------------------------------------------------------------------

// TestRedactValue_ShapeMatches locks the redaction shape:
//
//	len >= 8 → first4 + "***" + last4
//	shorter → "***"
//
// Any change to this contract has downstream consequences for
// screenshot goldens, docs, and the JSON wire.
func TestRedactValue_ShapeMatches(t *testing.T) {
	cases := []struct {
		name string
		in   any
		want string
	}{
		{"nil", nil, "***"},
		{"empty", "", "***"},
		{"short", "abc", "***"},
		{"seven", "abcdefg", "***"},
		{"eight", "abcdefgh", "abcd***efgh"},
		{"long", "sk-longsecretvalue123", "sk-l***e123"},
		{"bool", true, "***"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := redactValue(tc.in); got != tc.want {
				t.Errorf("redactValue(%v) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestParseExplainOutput_Values verifies the --output flag accepts the
// declared string set and rejects everything else.
func TestParseExplainOutput_Values(t *testing.T) {
	cases := []struct {
		in      string
		wantFmt explainOutputFormat
		wantErr bool
	}{
		{"", explainOutputText, false},
		{"text", explainOutputText, false},
		{"TEXT", explainOutputText, false},
		{"json", explainOutputJSON, false},
		{"JSON", explainOutputJSON, false},
		{"yaml", "", true},
		{"garbage", "", true},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			got, err := parseExplainOutput(tc.in)
			if tc.wantErr {
				if err == nil {
					t.Errorf("parseExplainOutput(%q) err = nil, want error", tc.in)
				}
				return
			}
			if err != nil {
				t.Errorf("parseExplainOutput(%q) err = %v, want nil", tc.in, err)
			}
			if got != tc.wantFmt {
				t.Errorf("parseExplainOutput(%q) = %q, want %q", tc.in, got, tc.wantFmt)
			}
		})
	}
}

// TestParseToolFilter_Splits verifies the CSV split shape.
func TestParseToolFilter_Splits(t *testing.T) {
	cases := []struct {
		in   string
		want []adapter.ToolID
	}{
		{"", nil},
		{"   ", nil},
		{"claude_code", []adapter.ToolID{"claude_code"}},
		{"claude_code,codex", []adapter.ToolID{"claude_code", "codex"}},
		{" claude_code , codex ", []adapter.ToolID{"claude_code", "codex"}},
		{",,claude_code,,", []adapter.ToolID{"claude_code"}},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			got := parseToolFilter(tc.in)
			if len(got) != len(tc.want) {
				t.Fatalf("parseToolFilter(%q) len = %d, want %d (got=%v)", tc.in, len(got), len(tc.want), got)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Errorf("parseToolFilter(%q)[%d] = %q, want %q", tc.in, i, got[i], tc.want[i])
				}
			}
		})
	}
}

// TestCollectDiagnosticEnv_FiltersOwnedAndPrefixed sanity-checks the
// pure filter used by --all-env.
func TestCollectDiagnosticEnv_FiltersOwnedAndPrefixed(t *testing.T) {
	env := []string{
		"ANTHROPIC_API_KEY=owned-and-filtered", // owned → excluded
		"ANTHROPIC_UNKNOWN=diagnostic-hit",     // prefix match, unowned
		"CLAUDE_CODE_EXPERIMENTAL=hit",         // prefix match, unowned
		"OPENAI_UNKNOWN=hit",                   // prefix match, unowned
		"CODEX_UNKNOWN=hit",                    // prefix match, unowned
		"RANDOM_VAR=no-hit",                    // no prefix match
		"malformed-no-equals",                  // ignored
	}
	got := collectDiagnosticEnv(env)
	want := map[string]string{
		"ANTHROPIC_UNKNOWN":        "diagnostic-hit",
		"CLAUDE_CODE_EXPERIMENTAL": "hit",
		"OPENAI_UNKNOWN":           "hit",
		"CODEX_UNKNOWN":            "hit",
	}
	if len(got) != len(want) {
		t.Fatalf("collectDiagnosticEnv len = %d, want %d; got=%v", len(got), len(want), got)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("collectDiagnosticEnv[%q] = %q, want %q", k, got[k], v)
		}
	}
	if _, hit := got["ANTHROPIC_API_KEY"]; hit {
		t.Errorf("collectDiagnosticEnv leaked owned key ANTHROPIC_API_KEY")
	}
	if _, hit := got["RANDOM_VAR"]; hit {
		t.Errorf("collectDiagnosticEnv leaked non-prefixed RANDOM_VAR")
	}
}

// TestFormatValueForDisplay ensures the secret+reveal combinatorics
// produce the expected shape.
func TestFormatValueForDisplay(t *testing.T) {
	cases := []struct {
		name   string
		v      any
		secret bool
		reveal bool
		want   string
	}{
		{"nonsecret-string", "opus", false, false, "opus"},
		{"nonsecret-bool", true, false, false, "true"},
		{"secret-hidden", "sk-longsecretvalue123", true, false, "sk-l***e123"},
		{"secret-revealed", "sk-longsecretvalue123", true, true, "sk-longsecretvalue123"},
		{"nil-nonsecret", nil, false, false, ""},
		{"nil-secret", nil, true, false, "***"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := formatValueForDisplay(tc.v, tc.secret, tc.reveal); got != tc.want {
				t.Errorf("formatValueForDisplay(%v, secret=%v, reveal=%v) = %q, want %q", tc.v, tc.secret, tc.reveal, got, tc.want)
			}
		})
	}
}

// TestExplain_JSONReveal: --reveal + --output json → secret plaintext
// appears in the JSON payload as the value's original Go type shape
// (still a string here, but the code path exercises the reveal branch
// of jsonValue).
func TestExplain_JSONReveal(t *testing.T) {
	h := newExplainHarness(t)
	h.saveProfile("prod", "sk-fullreveal-json-xyz", "https://api.anthropic.com", "opus")
	h.activate("prod")

	explainOutputFlag = "json"
	explainRevealFlag = true
	stdout, stderr, err := runExplainCmd(t, nil)
	if err != nil {
		t.Fatalf("runExplain err = %v", err)
	}
	if !strings.Contains(stdout, "sk-fullreveal-json-xyz") {
		t.Errorf("json --reveal did not surface plaintext:\n%s", stdout)
	}
	if !strings.Contains(stderr, "WARNING: --reveal") {
		t.Errorf("json --reveal did not emit stderr warning; got=%q", stderr)
	}
	// The output must still be valid JSON.
	var doc jsonExplain
	if err := json.Unmarshal([]byte(stdout), &doc); err != nil {
		t.Fatalf("stdout is not valid JSON: %v\n%s", err, stdout)
	}
}

// TestExplain_InvalidOutputFlag: --output yaml is rejected loudly.
func TestExplain_InvalidOutputFlag(t *testing.T) {
	h := newExplainHarness(t)
	h.saveProfile("prod", "sk-longsecretvalue123", "https://api.anthropic.com", "opus")
	h.activate("prod")

	explainOutputFlag = "yaml"
	_, _, err := runExplainCmd(t, nil)
	if err == nil {
		t.Fatal("runExplain err = nil for --output yaml, want error")
	}
	if !strings.Contains(err.Error(), "invalid --output") {
		t.Errorf("error = %v, want mention of 'invalid --output'", err)
	}
}

// TestExplain_UnknownToolInFilterProducesEmptyToolList sanity-checks
// that filtering on an unknown tool ID returns no tool sections without
// erroring.
func TestExplain_UnknownToolInFilterProducesEmptyToolList(t *testing.T) {
	h := newExplainHarness(t)
	h.saveProfile("prod", "sk-longsecretvalue123", "https://api.anthropic.com", "opus")
	h.activate("prod")

	explainToolFlag = "does_not_exist"
	stdout, _, err := runExplainCmd(t, nil)
	if err != nil {
		t.Fatalf("runExplain err = %v", err)
	}
	if strings.Contains(stdout, "Tool:") {
		t.Errorf("stdout should not contain any Tool: sections under unknown filter:\n%s", stdout)
	}
	if !strings.Contains(stdout, "(no tools resolved)") {
		t.Errorf("stdout missing 'no tools resolved' banner:\n%s", stdout)
	}
}

// ---------------------------------------------------------------------------
// Compile-time sanity — surface the packages the tests use so the file
// gets flagged when a downstream refactor removes something we depend on.
// ---------------------------------------------------------------------------

var _ = context.Background // keep imports honest
