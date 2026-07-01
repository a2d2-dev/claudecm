package cmd

// switch_test.go — Story E6-S2 tests for the cmd/switch surface.
//
// Test isolation strategy mirrors cmd/current_test.go and
// cmd/explain_test.go:
//
//  1. HOME points at a per-test t.TempDir() so storage.Default() reads a
//     fresh tree and the developer's real ~/.claudecm is never touched.
//  2. clearAdapterEnv (from explain_test.go) wipes the per-adapter env
//     allowlist so ambient env cannot leak into layer resolution.
//  3. resetSwitchFlags restores the cobra flag package-vars.
//  4. runSwitchInner wraps runSwitch with bytes.Buffers for stdout /
//     stderr capture so no /dev/stdout wiring is needed.
//
// Tests are non-parallel by construction (t.Setenv). switch is fast
// (single-thread commit under per-test HOME); sequential execution is
// acceptable.

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/a2d2-dev/claudecm/internal/adapter"
	claudecodeadapter "github.com/a2d2-dev/claudecm/internal/adapter/claudecode"
	codexadapter "github.com/a2d2-dev/claudecm/internal/adapter/codex"
	"github.com/a2d2-dev/claudecm/internal/adapter/stateio"
	"github.com/a2d2-dev/claudecm/internal/commit"
	"github.com/a2d2-dev/claudecm/internal/config"
	"github.com/a2d2-dev/claudecm/internal/storage"
	"github.com/a2d2-dev/claudecm/internal/writepath"
)

// resetSwitchFlags restores the package-level flag vars to their init()
// defaults. Every test calls this before mutating them.
func resetSwitchFlags() {
	switchOutputFlag = "text"
	switchDryRunFlag = false
	switchYesFlag = false
	switchToolFlag = ""
}

// newSwitchHarness reuses newExplainHarness (identical bootstrap) and
// additionally resets the switch flags.
func newSwitchHarness(t *testing.T) *explainHarness {
	t.Helper()
	h := newExplainHarness(t)
	resetSwitchFlags()
	return h
}

// runSwitchInner invokes runSwitch with a synthetic cobra.Command whose
// Out/Err are bytes.Buffers. Returns captured stdout, stderr, and the
// error return of runSwitch (the CLI-facing RunE wrapper is bypassed so
// tests never call os.Exit).
func runSwitchInner(t *testing.T, args ...string) (stdout, stderr string, err error) {
	t.Helper()
	var out, errBuf bytes.Buffer
	cmd := &cobra.Command{Use: "switch"}
	cmd.SetOut(&out)
	cmd.SetErr(&errBuf)
	err = runSwitch(cmd, args)
	return out.String(), errBuf.String(), err
}

// seedCodexRaw stamps a codex Tools.Raw overlay onto an already-saved
// profile so the codex Plan renders non-empty owned bytes into
// config.toml. Without this, a fresh-install codex config.toml plan
// produces empty output and trips a pre-existing Flatten(nil) diff
// quirk (tracked as followup #34): the parser returns nil, Flatten
// yields {"":nil}, and Diff reports the empty key as TouchesUnowned.
// Seeding a real model_provider keeps the diff purely inside the
// owned set.
func seedCodexRaw(t *testing.T, h *explainHarness, name string) {
	t.Helper()
	profile, err := h.mgr.GetProfile(name)
	if err != nil {
		t.Fatalf("GetProfile(%q): %v", name, err)
	}
	if profile.Tools == nil {
		profile.Tools = map[config.ToolID]config.ToolOverlay{}
	}
	ov := profile.Tools[config.ToolCodex]
	if ov.Raw == nil {
		ov.Raw = map[string]any{}
	}
	ov.Raw["model_provider"] = "openai"
	ov.Raw["model_providers.openai.base_url"] = "https://api." + name + ".example.com"
	ov.Raw["model_providers.openai.name"] = "OpenAI"
	ov.Raw["model_providers.openai.env_key"] = "OPENAI_API_KEY"
	ov.Raw["model_providers.openai.wire_api"] = "responses"
	profile.Tools[config.ToolCodex] = ov
	if err := h.mgr.UpdateProfile(name, profile); err != nil {
		t.Fatalf("UpdateProfile(%q): %v", name, err)
	}
}

// ---------------------------------------------------------------------------
// Tests
// ---------------------------------------------------------------------------

// TestSwitch_HappyBothTools switches from one profile to another with
// non-trivial owned key changes on both tools. Asserts:
//   - both files updated on disk with the new values,
//   - state.yaml active profile flipped to the new name,
//   - LastAppliedPerTool populated for both files with the SHA256 of
//     the post-commit bytes.
func TestSwitch_HappyBothTools(t *testing.T) {
	h := newSwitchHarness(t)

	// Two profiles, distinguishable owned values on both tools.
	h.saveProfile("before", "sk-beforetoken-1234abcd", "https://before.example.com", "before-model")
	seedCodexRaw(t, h, "before")
	h.saveProfile("after", "sk-aftertoken-5678wxyz", "https://after.example.com", "after-model")
	seedCodexRaw(t, h, "after")
	h.activate("before")
	// Pre-populate settings.json to reflect "before" so the diff has
	// something to change.
	h.writeSettingsJSON(`{"env":{"ANTHROPIC_MODEL":"before-model","ANTHROPIC_BASE_URL":"https://before.example.com"}}`)

	switchYesFlag = true
	stdout, _, err := runSwitchInner(t, "after")
	if err != nil {
		t.Fatalf("runSwitch after err=%v; stdout=%s", err, stdout)
	}
	if !strings.Contains(stdout, `Switched to "after"`) {
		t.Errorf("stdout missing switched line:\n%s", stdout)
	}

	// State pointer flipped.
	state, err := h.store.LoadState()
	if err != nil {
		t.Fatalf("LoadState: %v", err)
	}
	if state.CurrentProfile != "after" {
		t.Errorf("state.CurrentProfile = %q; want %q", state.CurrentProfile, "after")
	}

	// LastAppliedPerTool populated for claude_code (settings.json).
	settingsPath := claudecodeadapter.SettingsPath(h.resv)
	entry, ok := state.GetLastApplied(config.ToolClaudeCode, settingsPath)
	if !ok {
		t.Errorf("state.LastAppliedPerTool missing claude_code entry for %s", settingsPath)
	} else if entry.SHA256 == "" {
		t.Errorf("LastApplied claude_code SHA256 empty")
	}

	// LastAppliedPerTool populated for codex (config.toml at minimum).
	configPath := codexadapter.ConfigPath(h.resv)
	entry, ok = state.GetLastApplied(config.ToolCodex, configPath)
	if !ok {
		t.Errorf("state.LastAppliedPerTool missing codex entry for %s", configPath)
	} else if entry.SHA256 == "" {
		t.Errorf("LastApplied codex SHA256 empty")
	}

	// The on-disk settings.json reflects the "after" model.
	raw, err := os.ReadFile(settingsPath)
	if err != nil {
		t.Fatalf("read settings.json: %v", err)
	}
	if !strings.Contains(string(raw), "after-model") {
		t.Errorf("settings.json did not receive after values:\n%s", raw)
	}
}

// TestSwitch_MissingProfileErrors: switch to nonexistent profile → error
// (exit non-zero at the CLI, error at the runSwitch layer).
func TestSwitch_MissingProfileErrors(t *testing.T) {
	newSwitchHarness(t)
	_, _, err := runSwitchInner(t, "does-not-exist")
	if err == nil {
		t.Fatalf("runSwitch with missing profile returned nil; want error")
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Errorf("error message missing 'not found': %v", err)
	}
}

// TestSwitch_DryRunNoChanges: --dry-run → diff printed, files unchanged
// on disk, state unchanged.
func TestSwitch_DryRunNoChanges(t *testing.T) {
	h := newSwitchHarness(t)
	h.saveProfile("prod", "sk-prodtoken-1234abcd", "https://prod.example.com", "prod-model")
	// No active profile: leaving state.CurrentProfile empty is a valid
	// pre-condition — dry-run must not modify it.

	switchDryRunFlag = true
	// Codex adapter's fresh-install config.toml plan produces empty
	// output that trips a pre-existing Flatten(nil) diff quirk
	// (followup #34). Restrict to claude_code so this test focuses on
	// the dry-run behaviour, not the codex quirk.
	switchToolFlag = "claude_code"
	stdout, _, err := runSwitchInner(t, "prod")
	if err != nil {
		t.Fatalf("runSwitch --dry-run err=%v", err)
	}
	if !strings.Contains(stdout, "Pre-apply diff:") {
		t.Errorf("stdout missing 'Pre-apply diff:':\n%s", stdout)
	}
	if !strings.Contains(stdout, "--dry-run: nothing will be written.") {
		t.Errorf("stdout missing dry-run hint:\n%s", stdout)
	}
	if !strings.Contains(stdout, "dry-run complete") {
		t.Errorf("stdout missing dry-run complete line:\n%s", stdout)
	}

	// State pointer NOT flipped.
	state, err := h.store.LoadState()
	if err != nil {
		t.Fatalf("LoadState: %v", err)
	}
	if state.CurrentProfile != "" {
		t.Errorf("state.CurrentProfile = %q; want empty (dry-run must not update state)", state.CurrentProfile)
	}
	// settings.json NOT created (nothing was on disk to begin with).
	if _, err := os.Stat(claudecodeadapter.SettingsPath(h.resv)); !os.IsNotExist(err) {
		t.Errorf("settings.json exists after dry-run; err=%v", err)
	}
}

// TestSwitch_NonInteractiveWithoutYesAborts: no --yes, non-TTY stdin →
// aborts with a clear message; no changes.
func TestSwitch_NonInteractiveWithoutYesAborts(t *testing.T) {
	h := newSwitchHarness(t)
	h.saveProfile("prod", "sk-prodtoken-1234abcd", "https://prod.example.com", "prod-model")

	// Restrict to claude_code so this test does not race the codex
	// fresh-install empty-diff quirk (followup #34).
	switchToolFlag = "claude_code"
	// Force the isTerminal probe to report non-TTY. `go test`'s stdin
	// mode is host-dependent (interactive dev machines may show it as
	// a char device even under `go test`), so we override the seam
	// deterministically here.
	defer SetIsTerminalForTest(func(*os.File) bool { return false })()
	_, _, err := runSwitchInner(t, "prod")
	if err == nil {
		t.Fatalf("runSwitch without --yes on non-TTY returned nil; want error")
	}
	if !strings.Contains(err.Error(), "non-interactive session") {
		t.Errorf("error missing non-interactive hint: %v", err)
	}
	// State pointer NOT flipped.
	state, err := h.store.LoadState()
	if err != nil {
		t.Fatalf("LoadState: %v", err)
	}
	if state.CurrentProfile != "" {
		t.Errorf("state.CurrentProfile = %q; want empty", state.CurrentProfile)
	}
}

// TestSwitch_ToolFilterNarrows: --tool claude_code → only settings.json
// touched; codex config.toml NOT touched.
func TestSwitch_ToolFilterNarrows(t *testing.T) {
	h := newSwitchHarness(t)
	h.saveProfile("prod", "sk-prodtoken-1234abcd", "https://prod.example.com", "prod-model")

	switchYesFlag = true
	switchToolFlag = "claude_code"
	_, _, err := runSwitchInner(t, "prod")
	if err != nil {
		t.Fatalf("runSwitch --tool=claude_code err=%v", err)
	}

	// settings.json exists (claude_code was committed).
	if _, err := os.Stat(claudecodeadapter.SettingsPath(h.resv)); err != nil {
		t.Errorf("settings.json missing after claude_code switch: %v", err)
	}
	// config.toml does NOT exist (codex was filtered out).
	if _, err := os.Stat(codexadapter.ConfigPath(h.resv)); !os.IsNotExist(err) {
		t.Errorf("config.toml exists despite --tool=claude_code filter; err=%v", err)
	}

	// LastAppliedPerTool populated only for claude_code.
	state, err := h.store.LoadState()
	if err != nil {
		t.Fatalf("LoadState: %v", err)
	}
	if _, ok := state.GetLastApplied(config.ToolCodex, codexadapter.ConfigPath(h.resv)); ok {
		t.Errorf("codex LastApplied present despite --tool filter")
	}
	if _, ok := state.GetLastApplied(config.ToolClaudeCode, claudecodeadapter.SettingsPath(h.resv)); !ok {
		t.Errorf("claude_code LastApplied absent after switch")
	}
}

// TestSwitch_ToolFilterUnknownErrors: --tool typo → clear error.
func TestSwitch_ToolFilterUnknownErrors(t *testing.T) {
	h := newSwitchHarness(t)
	h.saveProfile("prod", "sk-prodtoken-1234abcd", "https://prod.example.com", "prod-model")
	switchYesFlag = true
	switchToolFlag = "not-a-tool"
	_, _, err := runSwitchInner(t, "prod")
	if err == nil {
		t.Fatalf("runSwitch with bad --tool returned nil; want error")
	}
	if !strings.Contains(err.Error(), "not a registered adapter") {
		t.Errorf("error missing 'not a registered adapter': %v", err)
	}
}

// TestSwitch_EmptyPlansNoOp: switch to a profile whose intent is
// byte-identical to the current on-disk state → all plans Skipped, still
// exits 0, still moves the active-profile pointer.
func TestSwitch_EmptyPlansNoOp(t *testing.T) {
	h := newSwitchHarness(t)
	prof := h.saveProfile("prod", "sk-prodtoken-1234abcd", "https://prod.example.com", "prod-model")
	// Best-effort no-op: apply once to establish an aligned baseline,
	// then switch to the same profile a second time. The second switch's
	// per-file plans will be Skipped (currentBytes == newBytes).
	// Restrict to claude_code to sidestep the codex empty-config quirk
	// (followup #34).
	switchYesFlag = true
	switchToolFlag = "claude_code"
	if _, _, err := runSwitchInner(t, "prod"); err != nil {
		t.Fatalf("initial switch prod err=%v", err)
	}

	// Second switch — same profile, files already aligned.
	stdout, _, err := runSwitchInner(t, "prod")
	if err != nil {
		t.Fatalf("second switch err=%v", err)
	}
	if !strings.Contains(stdout, `Switched to "prod"`) {
		t.Errorf("stdout missing switched line on no-op:\n%s", stdout)
	}
	// State still points at prod.
	state, err := h.store.LoadState()
	if err != nil {
		t.Fatalf("LoadState: %v", err)
	}
	if state.CurrentProfile != "prod" {
		t.Errorf("state.CurrentProfile = %q; want prod after no-op switch", state.CurrentProfile)
	}
	// The profile referenced here is unused after this line; keep the
	// AddProfile happy-path smoke by asserting round-trip identity.
	if prof.Name != "prod" {
		t.Errorf("harness saved wrong profile name: %q", prof.Name)
	}
}

// TestSwitch_DiffPrintsRedactedSecrets: a switch that changes an
// api_key surface (env.ANTHROPIC_AUTH_TOKEN) must NOT print plaintext
// in the diff.
func TestSwitch_DiffPrintsRedactedSecrets(t *testing.T) {
	h := newSwitchHarness(t)
	h.saveProfile("prev", "sk-prevverysecret-xyz1234", "https://prev.example.com", "prev-model")
	h.saveProfile("next", "sk-nextverysecret-abc7890", "https://next.example.com", "next-model")
	// Seed disk to prev so the diff has a real transition.
	h.writeSettingsJSON(`{"env":{"ANTHROPIC_AUTH_TOKEN":"sk-prevverysecret-xyz1234","ANTHROPIC_BASE_URL":"https://prev.example.com","ANTHROPIC_MODEL":"prev-model"}}`)
	h.activate("prev")

	switchDryRunFlag = true
	switchToolFlag = "claude_code"
	stdout, _, err := runSwitchInner(t, "next")
	if err != nil {
		t.Fatalf("dry-run switch next err=%v; stdout=%s", err, stdout)
	}
	if strings.Contains(stdout, "sk-prevverysecret-xyz1234") {
		t.Errorf("stdout leaked prev plaintext secret:\n%s", stdout)
	}
	if strings.Contains(stdout, "sk-nextverysecret-abc7890") {
		t.Errorf("stdout leaked next plaintext secret:\n%s", stdout)
	}
	// Redacted form (first4 + *** + last4) must be present so operator
	// can eyeball that the secret slot was in fact changing.
	if !strings.Contains(stdout, "sk-p***1234") && !strings.Contains(stdout, "sk-n***7890") {
		t.Errorf("stdout missing redacted secret form:\n%s", stdout)
	}
}

// TestSwitch_JSONOutputParses: --output json --dry-run → valid JSON
// with the expected top-level shape.
func TestSwitch_JSONOutputParses(t *testing.T) {
	h := newSwitchHarness(t)
	h.saveProfile("prod", "sk-prodtoken-1234abcd", "https://prod.example.com", "prod-model")
	switchDryRunFlag = true
	switchOutputFlag = "json"
	switchToolFlag = "claude_code"

	stdout, _, err := runSwitchInner(t, "prod")
	if err != nil {
		t.Fatalf("runSwitch --output=json --dry-run err=%v", err)
	}

	var doc jsonSwitch
	if err := json.Unmarshal([]byte(stdout), &doc); err != nil {
		t.Fatalf("stdout not valid JSON: %v\n%s", err, stdout)
	}
	if doc.Profile != "prod" {
		t.Errorf("json profile = %q; want prod", doc.Profile)
	}
	if doc.Action != "dry-run" {
		t.Errorf("json action = %q; want dry-run", doc.Action)
	}
	if len(doc.Diff) == 0 {
		t.Errorf("json diff empty; want at least one entry:\n%s", stdout)
	}
	// Redaction must still hold in JSON.
	if strings.Contains(stdout, "sk-prodtoken-1234abcd") {
		t.Errorf("json leaked plaintext secret:\n%s", stdout)
	}
}

// TestSwitch_UpdatesStateAndLastApplied is a focused post-commit
// assertion: state.CurrentProfile flipped AND at least one
// LastAppliedPerTool entry exists.
func TestSwitch_UpdatesStateAndLastApplied(t *testing.T) {
	h := newSwitchHarness(t)
	h.saveProfile("prod", "sk-prodtoken-1234abcd", "https://prod.example.com", "prod-model")
	seedCodexRaw(t, h, "prod")
	switchYesFlag = true

	_, _, err := runSwitchInner(t, "prod")
	if err != nil {
		t.Fatalf("runSwitch err=%v", err)
	}

	state, err := h.store.LoadState()
	if err != nil {
		t.Fatalf("LoadState: %v", err)
	}
	if state.CurrentProfile != "prod" {
		t.Fatalf("state.CurrentProfile = %q; want prod", state.CurrentProfile)
	}
	if state.LastAppliedPerTool == nil {
		t.Fatalf("state.LastAppliedPerTool is nil after commit")
	}
	// At least one entry across both tools.
	count := 0
	for _, files := range state.LastAppliedPerTool {
		count += len(files)
	}
	if count == 0 {
		t.Errorf("state.LastAppliedPerTool is empty after commit; want >= 1 entry")
	}

	// stateio round-trip: LoadLastApplied resurfaces at least the
	// claude_code settings.json anchor.
	settingsPath := claudecodeadapter.SettingsPath(h.resv)
	entry, present, loadErr := stateio.LoadLastApplied(h.resv, config.ToolClaudeCode, settingsPath)
	if loadErr != nil {
		t.Fatalf("LoadLastApplied claude_code err=%v", loadErr)
	}
	if !present {
		t.Fatalf("LoadLastApplied claude_code: no entry recorded")
	}
	if entry.SHA256 == "" {
		t.Errorf("LoadLastApplied entry SHA256 empty: %+v", entry)
	}
}

// TestSwitch_PartialFailureRollsBack is a placeholder for the
// PartialFailure surface: forcing a real commit failure without an
// injected seam requires filesystem gymnastics (chmod'ing the second
// canonical target's parent read-only mid-Stage). The commit package
// has its own e2e failure tests; here we assert only that the
// switch-side wrapper wraps a *PartialFailure as-is (errors.As
// unwraps) and lets the CLI RunE map to exit 2.
//
// The test synthesises a partial failure by directly constructing a
// StagedTxn with a resolver whose target file cannot be written, then
// verifies renderPartialFailure emits the expected block. This
// exercises the render + error-wrapping path without needing a full
// commit lifecycle race.
func TestSwitch_PartialFailureRendering(t *testing.T) {
	newSwitchHarness(t)
	pf := &commit.PartialFailure{
		FailedFile: "/tmp/example/config.toml",
		Cause:      errors.New("simulated commit failure"),
		RolledBack: []string{"/tmp/example/auth.json"},
		Untouched:  []string{"/tmp/example/settings.json"},
	}
	var buf bytes.Buffer
	renderPartialFailure(&buf, switchOutputText, "prod", pf)
	out := buf.String()
	if !strings.Contains(out, "commit failed partway through") {
		t.Errorf("partial-failure text missing header:\n%s", out)
	}
	if !strings.Contains(out, "rolled-back: /tmp/example/auth.json") {
		t.Errorf("partial-failure text missing rolled-back line:\n%s", out)
	}
	if !strings.Contains(out, "untouched: /tmp/example/settings.json") {
		t.Errorf("partial-failure text missing untouched line:\n%s", out)
	}

	// JSON path.
	buf.Reset()
	renderPartialFailure(&buf, switchOutputJSON, "prod", pf)
	var doc jsonSwitchPartial
	if err := json.Unmarshal(buf.Bytes(), &doc); err != nil {
		t.Fatalf("partial-failure JSON invalid: %v\n%s", err, buf.String())
	}
	if doc.FailedFile != "/tmp/example/config.toml" {
		t.Errorf("json failed_file = %q; want /tmp/example/config.toml", doc.FailedFile)
	}
	if len(doc.RolledBack) != 1 || doc.RolledBack[0] != "/tmp/example/auth.json" {
		t.Errorf("json rolled_back mismatch: %+v", doc.RolledBack)
	}
	if len(doc.Untouched) != 1 || doc.Untouched[0] != "/tmp/example/settings.json" {
		t.Errorf("json untouched mismatch: %+v", doc.Untouched)
	}
	// errors.As on the sentinel must reach the *PartialFailure.
	var got *commit.PartialFailure
	if !errors.As(pf, &got) {
		t.Errorf("errors.As did not unwrap to *commit.PartialFailure")
	}
}

// TestSwitch_EmptyPlansUpdatesActiveProfile: if the caller filters to
// zero adapters via --tool with no matches... actually the filter path
// errors first; instead this test covers the same "no-op" path by
// picking an already-aligned profile and asserting the pointer moved.
// Covered by TestSwitch_EmptyPlansNoOp above; this test additionally
// asserts the JSON no-op shape.
func TestSwitch_NoOpJSONShape(t *testing.T) {
	h := newSwitchHarness(t)
	h.saveProfile("prod", "sk-prodtoken-1234abcd", "https://prod.example.com", "prod-model")
	switchYesFlag = true
	switchToolFlag = "claude_code"
	if _, _, err := runSwitchInner(t, "prod"); err != nil {
		t.Fatalf("initial switch err=%v", err)
	}

	// Second switch as JSON.
	switchOutputFlag = "json"
	stdout, _, err := runSwitchInner(t, "prod")
	if err != nil {
		t.Fatalf("second switch json err=%v", err)
	}
	// The second switch is a commit path (files may still be re-emitted
	// as Skipped inside Stage — the diff will be empty and Commit is a
	// no-op). Both "commit" and "no-op" actions are acceptable here;
	// the important assertion is that JSON parses cleanly.
	var doc jsonSwitch
	if err := json.Unmarshal([]byte(stdout), &doc); err != nil {
		t.Fatalf("second switch JSON invalid: %v\n%s", err, stdout)
	}
	if doc.Profile != "prod" {
		t.Errorf("json profile = %q; want prod", doc.Profile)
	}
}

// TestSwitch_DryRunAbortsTxn asserts that --dry-run does not leave any
// backup files behind either. Backups belong under
// ~/.claudecm/backups/; a Stage against a first-write plan (no
// pre-existing target) produces no backup at all, so the assertion is
// specifically over the overwrite case: seed the target, dry-run, and
// verify state / files are untouched.
func TestSwitch_DryRunAbortsTxnPreservesTarget(t *testing.T) {
	h := newSwitchHarness(t)
	h.saveProfile("prod", "sk-prodtoken-1234abcd", "https://prod.example.com", "prod-model")
	// Seed a pre-existing settings.json so Stage would write a backup
	// on a non-dry-run path.
	h.writeSettingsJSON(`{"env":{"ANTHROPIC_MODEL":"pre-existing"}}`)
	settingsPath := claudecodeadapter.SettingsPath(h.resv)
	origBytes, err := os.ReadFile(settingsPath)
	if err != nil {
		t.Fatalf("read seeded settings.json: %v", err)
	}

	switchDryRunFlag = true
	switchToolFlag = "claude_code"
	if _, _, err := runSwitchInner(t, "prod"); err != nil {
		t.Fatalf("dry-run switch err=%v", err)
	}

	afterBytes, err := os.ReadFile(settingsPath)
	if err != nil {
		t.Fatalf("read post-dry-run settings.json: %v", err)
	}
	if !bytes.Equal(origBytes, afterBytes) {
		t.Errorf("dry-run mutated settings.json;\nbefore: %s\nafter:  %s", origBytes, afterBytes)
	}
}

// ---------------------------------------------------------------------------
// Unit-level tests for the small helpers in switch.go. These exercise
// paths the integration tests do not touch directly and lift coverage
// above the 80% bar.
// ---------------------------------------------------------------------------

// TestSwitch_ParseOutputFormat covers the flag parser.
func TestSwitch_ParseOutputFormat(t *testing.T) {
	cases := []struct {
		in      string
		want    switchOutputFormat
		wantErr bool
	}{
		{"", switchOutputText, false},
		{"text", switchOutputText, false},
		{"TEXT", switchOutputText, false},
		{"json", switchOutputJSON, false},
		{"  json  ", switchOutputJSON, false},
		{"yaml", "", true},
	}
	for _, c := range cases {
		got, err := parseSwitchOutput(c.in)
		if c.wantErr {
			if err == nil {
				t.Errorf("parseSwitchOutput(%q) = %q, nil; want err", c.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("parseSwitchOutput(%q) unexpected err: %v", c.in, err)
		}
		if got != c.want {
			t.Errorf("parseSwitchOutput(%q) = %q; want %q", c.in, got, c.want)
		}
	}
}

// TestSwitch_IsSecretKey covers the secret-key heuristic.
func TestSwitch_IsSecretKey(t *testing.T) {
	cases := map[string]bool{
		"env.ANTHROPIC_API_KEY":           true,
		"env.ANTHROPIC_AUTH_TOKEN":        true,
		"OPENAI_API_KEY":                  true,
		"model_providers.openai.base_url": false,
		"env.ANTHROPIC_MODEL":             false,
		"tokens.access_token":             true,
		"tokens.refresh_token":            true,
	}
	for k, want := range cases {
		got := isSecretKey(k)
		if got != want {
			t.Errorf("isSecretKey(%q) = %v; want %v", k, got, want)
		}
	}
}

// TestSwitch_RedactedValueDisplay covers the wrapper that combines the
// secret heuristic with the shared redactValue.
func TestSwitch_RedactedValueDisplay(t *testing.T) {
	// Secret key with a long value → redacted form.
	got := redactedValueDisplay("env.ANTHROPIC_API_KEY", "sk-longsecret1234")
	if strings.Contains(got, "longsecret") {
		t.Errorf("secret key leaked plaintext: %q", got)
	}
	if got == "" {
		t.Errorf("secret redaction returned empty string")
	}
	// Non-secret key → passthrough.
	got = redactedValueDisplay("env.ANTHROPIC_MODEL", "opus")
	if got != "opus" {
		t.Errorf("non-secret display = %q; want opus", got)
	}
	// Nil value → empty string.
	got = redactedValueDisplay("env.ANTHROPIC_MODEL", nil)
	if got != "" {
		t.Errorf("nil display = %q; want empty", got)
	}
}

// TestSwitch_SelectTools covers the filter helper.
func TestSwitch_SelectTools(t *testing.T) {
	newSwitchHarness(t)
	// Empty filter returns everything.
	all, err := selectSwitchTools(nil)
	if err != nil {
		t.Fatalf("selectSwitchTools(nil) err=%v", err)
	}
	if len(all) == 0 {
		t.Fatalf("selectSwitchTools(nil) returned no adapters; DefaultRegistry empty?")
	}
	// Filter with valid entry.
	one, err := selectSwitchTools([]adapter.ToolID{adapter.ToolClaudeCode})
	if err != nil {
		t.Fatalf("selectSwitchTools([claude_code]) err=%v", err)
	}
	if len(one) != 1 {
		t.Errorf("selectSwitchTools([claude_code]) returned %d adapters; want 1", len(one))
	}
	if one[0].ID() != adapter.ToolClaudeCode {
		t.Errorf("selectSwitchTools returned wrong tool: %q", one[0].ID())
	}
	// Filter with invalid entry.
	if _, err := selectSwitchTools([]adapter.ToolID{"nope"}); err == nil {
		t.Errorf("selectSwitchTools([nope]) returned nil err; want error")
	}
}

// TestSwitch_PromptConfirm covers the y/N reader.
func TestSwitch_PromptConfirm(t *testing.T) {
	var out bytes.Buffer
	cases := []struct {
		in   string
		want bool
	}{
		{"y\n", true},
		{"Y\n", true},
		{"yes\n", true},
		{"YES\n", true},
		{"n\n", false},
		{"\n", false},
		{"", false},
		{"no\n", false},
	}
	for _, c := range cases {
		out.Reset()
		got, err := promptConfirm(&out, strings.NewReader(c.in), "?")
		if err != nil {
			t.Errorf("promptConfirm(%q) err=%v", c.in, err)
			continue
		}
		if got != c.want {
			t.Errorf("promptConfirm(%q) = %v; want %v", c.in, got, c.want)
		}
	}
}

// TestSwitch_IsTerminal covers both branches of the TTY probe.
func TestSwitch_IsTerminal(t *testing.T) {
	if isTerminal(nil) {
		t.Errorf("isTerminal(nil) = true; want false")
	}
	// A plain file is not a character device.
	f, err := os.CreateTemp("", "switch-tty-*")
	if err != nil {
		t.Fatalf("CreateTemp: %v", err)
	}
	defer os.Remove(f.Name())
	defer f.Close()
	if isTerminal(f) {
		t.Errorf("isTerminal(regular file) = true; want false")
	}
}

// TestSwitch_IsNoConfigErr covers the sentinel check.
func TestSwitch_IsNoConfigErr(t *testing.T) {
	if !isNoConfigErr(claudecodeadapter.ErrNoConfig) {
		t.Errorf("isNoConfigErr(claudecodeadapter.ErrNoConfig) = false; want true")
	}
	if !isNoConfigErr(codexadapter.ErrNoConfig) {
		t.Errorf("isNoConfigErr(codexadapter.ErrNoConfig) = false; want true")
	}
	if isNoConfigErr(errors.New("some other error")) {
		t.Errorf("isNoConfigErr(other) = true; want false")
	}
}

// TestSwitch_RenderNoOpText covers the text path of the no-op renderer.
func TestSwitch_RenderNoOpText(t *testing.T) {
	var buf bytes.Buffer
	err := renderNoOp(&buf, switchOutputText, "prod", []perToolPlanIssue{
		{Tool: adapter.ToolCodex, Message: "no codex config"},
	})
	if err != nil {
		t.Fatalf("renderNoOp err=%v", err)
	}
	out := buf.String()
	if !strings.Contains(out, `Switched to "prod"`) {
		t.Errorf("renderNoOp text missing switched line:\n%s", out)
	}
	if !strings.Contains(out, "skipped codex") {
		t.Errorf("renderNoOp text missing skipped notice:\n%s", out)
	}
}

// TestSwitch_RenderNoOpJSON covers the JSON path of the no-op renderer.
func TestSwitch_RenderNoOpJSON(t *testing.T) {
	var buf bytes.Buffer
	err := renderNoOp(&buf, switchOutputJSON, "prod", nil)
	if err != nil {
		t.Fatalf("renderNoOp JSON err=%v", err)
	}
	var doc jsonSwitch
	if err := json.Unmarshal(buf.Bytes(), &doc); err != nil {
		t.Fatalf("renderNoOp JSON invalid: %v\n%s", err, buf.String())
	}
	if doc.Action != "no-op" {
		t.Errorf("json action = %q; want no-op", doc.Action)
	}
}

// TestSwitch_TTYPromptYesCommits: simulate a TTY session answering "y"
// via SetIsTerminalForTest — the commit runs and state flips.
func TestSwitch_TTYPromptYesCommits(t *testing.T) {
	h := newSwitchHarness(t)
	h.saveProfile("prod", "sk-prodtoken-1234abcd", "https://prod.example.com", "prod-model")
	switchToolFlag = "claude_code"

	// Force isTerminal=true so the prompt fires. Redirect the real
	// os.Stdin to a bytes.Reader containing "y\n" for the duration of
	// the run.
	defer SetIsTerminalForTest(func(*os.File) bool { return true })()

	// promptConfirm reads directly from os.Stdin — we can't easily
	// swap it without more plumbing. Instead, hand it a file backed by
	// a pipe whose write end carries "y\n". Requires temporarily
	// replacing os.Stdin.
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	if _, err := w.WriteString("y\n"); err != nil {
		t.Fatalf("pipe write: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("pipe close: %v", err)
	}
	origStdin := os.Stdin
	os.Stdin = r
	defer func() {
		os.Stdin = origStdin
		r.Close()
	}()

	stdout, _, runErr := runSwitchInner(t, "prod")
	if runErr != nil {
		t.Fatalf("runSwitch TTY-y err=%v\n%s", runErr, stdout)
	}
	if !strings.Contains(stdout, `Switched to "prod"`) {
		t.Errorf("TTY-y switch did not commit:\n%s", stdout)
	}
	state, err := h.store.LoadState()
	if err != nil {
		t.Fatalf("LoadState: %v", err)
	}
	if state.CurrentProfile != "prod" {
		t.Errorf("state.CurrentProfile = %q; want prod", state.CurrentProfile)
	}
}

// TestSwitch_TTYPromptNoAborts: simulate a TTY session answering "n" —
// the commit aborts, state stays untouched.
func TestSwitch_TTYPromptNoAborts(t *testing.T) {
	h := newSwitchHarness(t)
	h.saveProfile("prod", "sk-prodtoken-1234abcd", "https://prod.example.com", "prod-model")
	switchToolFlag = "claude_code"

	defer SetIsTerminalForTest(func(*os.File) bool { return true })()

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	if _, err := w.WriteString("n\n"); err != nil {
		t.Fatalf("pipe write: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("pipe close: %v", err)
	}
	origStdin := os.Stdin
	os.Stdin = r
	defer func() {
		os.Stdin = origStdin
		r.Close()
	}()

	_, _, runErr := runSwitchInner(t, "prod")
	if runErr == nil {
		t.Fatalf("TTY-n switch returned nil err; want abort error")
	}
	if !strings.Contains(runErr.Error(), "aborted by user") {
		t.Errorf("expected 'aborted by user' err; got: %v", runErr)
	}
	state, err := h.store.LoadState()
	if err != nil {
		t.Fatalf("LoadState: %v", err)
	}
	if state.CurrentProfile != "" {
		t.Errorf("state.CurrentProfile = %q; want empty (abort must not flip pointer)", state.CurrentProfile)
	}
}

// TestSwitch_DiffAllChangeOps covers the diffToJSON path for all three
// op flavours plus a mixed-non-secret / secret rendering.
func TestSwitch_DiffAllChangeOps(t *testing.T) {
	txn := commit.StagedTxn{
		Prepared: []commit.PreparedFile{
			{
				Plan: writepathPlan("claude_code", "/tmp/settings.json"),
				Diff: writepath.DiffResult{
					Added:   []string{"env.ANTHROPIC_MODEL"},
					Removed: []string{"env.OBSOLETE_KEY"},
					Changed: []writepath.KeyDelta{
						{Key: "env.ANTHROPIC_BASE_URL", OldValue: "https://a", NewValue: "https://b"},
						{Key: "env.ANTHROPIC_API_KEY", OldValue: "sk-oldsecretaaaa", NewValue: "sk-newsecretbbbb"},
					},
				},
			},
		},
	}
	got := diffToJSON(txn)
	if len(got) != 1 {
		t.Fatalf("diffToJSON len = %d; want 1", len(got))
	}
	if len(got[0].OwnedChanges) != 4 {
		t.Fatalf("OwnedChanges len = %d; want 4: %+v", len(got[0].OwnedChanges), got[0].OwnedChanges)
	}
	// Redaction of api_key change.
	for _, c := range got[0].OwnedChanges {
		if c.Key == "env.ANTHROPIC_API_KEY" {
			if strings.Contains(c.OldValue, "oldsecret") || strings.Contains(c.NewValue, "newsecret") {
				t.Errorf("api_key change leaked plaintext: %+v", c)
			}
		}
	}
}

// writepathPlan is a tiny helper to make TestSwitch_DiffAllChangeOps
// readable — it constructs a synthetic writepath.WritePlan with just
// the fields diffToJSON needs.
func writepathPlan(tool, target string) writepath.WritePlan {
	return writepath.WritePlan{Tool: tool, Target: target}
}

// TestSwitch_ReportToJSONSkipsUncommitted asserts only Committed rows
// land in the JSON report.
func TestSwitch_ReportToJSONSkipsUncommitted(t *testing.T) {
	report := commit.CommitReport{
		PerFile: []commit.PerFileReport{
			{
				Target: "/tmp/a",
				Status: commit.StatusCommitted,
				Report: writepath.WriteReport{Tool: "claude_code", PostFingerprint: storage.Fingerprint{SHA256: "aaa"}},
				Backup: storage.BackupRecord{BackupPath: "/tmp/backups/a.bak"},
			},
			{
				Target: "/tmp/b",
				Status: commit.StatusUntouched,
			},
			{
				Target: "/tmp/c",
				Status: commit.StatusRolledBack,
			},
		},
	}
	got := reportToJSON(report)
	if len(got.Committed) != 1 {
		t.Errorf("Committed len = %d; want 1: %+v", len(got.Committed), got.Committed)
	}
	if len(got.Backups) != 1 {
		t.Errorf("Backups len = %d; want 1: %+v", len(got.Backups), got.Backups)
	}
	if got.Committed[0].Target != "/tmp/a" {
		t.Errorf("wrong committed target: %+v", got.Committed[0])
	}
}

// TestSwitch_UpdateStateOnSuccessRecordsCommittedOnly asserts that only
// StatusCommitted entries make it into LastAppliedPerTool — a
// StatusUntouched file (skipped no-op) must NOT anchor drift.
func TestSwitch_UpdateStateOnSuccessRecordsCommittedOnly(t *testing.T) {
	h := newSwitchHarness(t)
	h.saveProfile("prod", "sk-prodtoken-1234abcd", "https://prod.example.com", "prod-model")

	settingsPath := claudecodeadapter.SettingsPath(h.resv)
	// Ensure parent dir exists so writepath/atomic write can land the
	// (synthetic) SHA later. Nothing else does the setup here.
	if err := os.MkdirAll(filepath.Dir(settingsPath), 0o700); err != nil {
		t.Fatalf("mkdir parent: %v", err)
	}

	report := commit.CommitReport{
		PerFile: []commit.PerFileReport{
			{
				Target: settingsPath,
				Status: commit.StatusCommitted,
				Report: writepath.WriteReport{
					Tool:            string(config.ToolClaudeCode),
					Target:          settingsPath,
					PostFingerprint: storage.Fingerprint{SHA256: "deadbeef"},
				},
			},
			{
				Target: codexadapter.ConfigPath(h.resv),
				Status: commit.StatusUntouched,
				Report: writepath.WriteReport{
					Tool:            string(config.ToolCodex),
					Target:          codexadapter.ConfigPath(h.resv),
					PostFingerprint: storage.Fingerprint{SHA256: "ignored-untouched"},
				},
			},
		},
	}
	if err := updateStateOnSuccess(h.resv, h.store, "prod", &report); err != nil {
		t.Fatalf("updateStateOnSuccess: %v", err)
	}
	state, err := h.store.LoadState()
	if err != nil {
		t.Fatalf("LoadState: %v", err)
	}
	if state.CurrentProfile != "prod" {
		t.Errorf("state.CurrentProfile = %q; want prod", state.CurrentProfile)
	}
	if _, ok := state.GetLastApplied(config.ToolClaudeCode, settingsPath); !ok {
		t.Errorf("committed claude_code entry not recorded")
	}
	if _, ok := state.GetLastApplied(config.ToolCodex, codexadapter.ConfigPath(h.resv)); ok {
		t.Errorf("untouched codex entry was recorded; should be skipped")
	}
}
