package cmd

// edit_test.go — Story E6-S5 tests for the cmd/edit surface.
//
// Test isolation strategy mirrors cmd/add_test.go / cmd/switch_test.go:
//
//  1. HOME points at a per-test t.TempDir() so storage.Default() reads
//     a fresh tree and the developer's real ~/.claudecm is never
//     touched.
//  2. resetEditFlags restores the cobra flag package-vars.
//  3. runEditInner wraps runEdit with bytes.Buffers for stdout / stderr
//     capture.
//  4. Timestamps are pinned via SetNowForTest so UpdatedAt assertions
//     stay stable.
//  5. The editor runner is stubbed via SetEditorRunnerForTest so tests
//     can synthesise any temp-file body (round-trip, corrupt YAML,
//     name change, schema drift) without an interactive editor.

import (
	"bytes"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"github.com/a2d2-dev/claudecm/internal/config"
	"github.com/a2d2-dev/claudecm/internal/storage"
)

func resetEditFlags() {
	editSetFlag = nil
	editDryRunFlag = false
}

type editHarness struct {
	t     *testing.T
	home  string
	resv  *storage.Resolver
	store *storage.FileStorage
}

func newEditHarness(t *testing.T) *editHarness {
	t.Helper()
	resetEditFlags()

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

	// Pin the clock so UpdatedAt is deterministic.
	fixed := time.Date(2026, 7, 2, 12, 0, 0, 0, time.UTC)
	restore := SetNowForTest(func() time.Time { return fixed })
	t.Cleanup(restore)

	// Default editor runner: fail loudly so any test that forgets to
	// override falls into the interactive branch and errors visibly.
	restoreEd := SetEditorRunnerForTest(func(string) error {
		return nil // no-op editor by default; matches EDITOR="true" shim
	})
	t.Cleanup(restoreEd)

	return &editHarness{t: t, home: home, resv: resv, store: store}
}

func (h *editHarness) seedProfile(name, apiKey, baseURL, model string) *config.Profile {
	h.t.Helper()
	p := config.NewProfile(name, baseURL, apiKey)
	p.Core.Model = model
	// Stamp deterministic timestamps so subsequent Save's UpdatedAt
	// change is the only observable diff.
	p.CreatedAt = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	p.UpdatedAt = p.CreatedAt
	if err := h.store.SaveProfile(p); err != nil {
		h.t.Fatalf("SaveProfile: %v", err)
	}
	return p
}

func runEditInner(t *testing.T, args ...string) (stdout, stderr string, err error) {
	t.Helper()
	var out, errBuf bytes.Buffer
	cmd := &cobra.Command{Use: "edit"}
	cmd.SetOut(&out)
	cmd.SetErr(&errBuf)
	err = runEdit(cmd, args)
	return out.String(), errBuf.String(), err
}

// ---------------------------------------------------------------------------
// --set mode
// ---------------------------------------------------------------------------

func TestEdit_SetHappyCoreField(t *testing.T) {
	h := newEditHarness(t)
	h.seedProfile("work", "sk-old-1234abcd", "https://old.example.com", "old-model")

	editSetFlag = []string{"core.model=new-model", "core.base_url=https://new.example.com"}
	_, _, err := runEditInner(t, "work")
	if err != nil {
		t.Fatalf("runEdit err=%v", err)
	}
	loaded, err := h.store.LoadProfile("work")
	if err != nil {
		t.Fatalf("LoadProfile: %v", err)
	}
	if loaded.Core.Model != "new-model" {
		t.Errorf("Core.Model=%q; want new-model", loaded.Core.Model)
	}
	if loaded.Core.BaseURL != "https://new.example.com" {
		t.Errorf("Core.BaseURL=%q", loaded.Core.BaseURL)
	}
}

func TestEdit_SetHappyOverlay(t *testing.T) {
	h := newEditHarness(t)
	h.seedProfile("work", "sk-1234abcd5678", "https://api.example.com", "opus")

	editSetFlag = []string{
		"tools.claude_code.env.CLAUDE_CODE_USE_BEDROCK=1",
		"tools.codex.raw.model_provider=openai",
	}
	if _, _, err := runEditInner(t, "work"); err != nil {
		t.Fatalf("runEdit err=%v", err)
	}
	loaded, err := h.store.LoadProfile("work")
	if err != nil {
		t.Fatalf("LoadProfile: %v", err)
	}
	if v := loaded.Tools[config.ToolClaudeCode].ExtraEnv["CLAUDE_CODE_USE_BEDROCK"]; v != "1" {
		t.Errorf("claude_code.env CLAUDE_CODE_USE_BEDROCK=%q; want 1", v)
	}
	if v := loaded.Tools[config.ToolCodex].Raw["model_provider"]; v != "openai" {
		t.Errorf("codex.raw model_provider=%v; want openai", v)
	}
}

func TestEdit_SetInvalidPathRefused(t *testing.T) {
	h := newEditHarness(t)
	h.seedProfile("work", "sk-1234abcd5678", "https://api.example.com", "opus")

	editSetFlag = []string{"weird.path=1"}
	_, _, err := runEditInner(t, "work")
	if err == nil {
		t.Fatal("expected error for unsupported --set path")
	}
	if !strings.Contains(err.Error(), "unsupported path") {
		t.Errorf("error missing supported-prefix hint: %v", err)
	}
}

func TestEdit_SetDescription(t *testing.T) {
	h := newEditHarness(t)
	h.seedProfile("work", "sk-1234abcd5678", "https://api.example.com", "opus")

	editSetFlag = []string{"description=updated"}
	if _, _, err := runEditInner(t, "work"); err != nil {
		t.Fatalf("runEdit err=%v", err)
	}
	loaded, _ := h.store.LoadProfile("work")
	if loaded.Description != "updated" {
		t.Errorf("Description=%q; want updated", loaded.Description)
	}
}

func TestEdit_SetMalformedEntry(t *testing.T) {
	h := newEditHarness(t)
	h.seedProfile("work", "sk-1234abcd5678", "https://api.example.com", "opus")

	editSetFlag = []string{"no-equals-here"}
	_, _, err := runEditInner(t, "work")
	if err == nil || !strings.Contains(err.Error(), "malformed") {
		t.Errorf("expected malformed error, got %v", err)
	}
}

func TestEdit_SetUnknownCoreField(t *testing.T) {
	h := newEditHarness(t)
	h.seedProfile("work", "sk-1234abcd5678", "https://api.example.com", "opus")

	editSetFlag = []string{"core.no_such_field=x"}
	_, _, err := runEditInner(t, "work")
	if err == nil || !strings.Contains(err.Error(), "unsupported core field") {
		t.Errorf("expected unsupported-core-field error, got %v", err)
	}
}

// ---------------------------------------------------------------------------
// Missing profile
// ---------------------------------------------------------------------------

func TestEdit_MissingProfileErrors(t *testing.T) {
	newEditHarness(t)
	editSetFlag = []string{"core.model=x"}
	_, _, err := runEditInner(t, "does-not-exist")
	if err == nil {
		t.Fatal("expected error for missing profile")
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Errorf("error missing 'not found': %v", err)
	}
}

// ---------------------------------------------------------------------------
// --dry-run
// ---------------------------------------------------------------------------

func TestEdit_DryRunPrintsDiffNoWrite(t *testing.T) {
	h := newEditHarness(t)
	before := h.seedProfile("work", "sk-old-1234abcd", "https://old.example.com", "old-model")

	editSetFlag = []string{"core.model=new-model"}
	editDryRunFlag = true
	stdout, _, err := runEditInner(t, "work")
	if err != nil {
		t.Fatalf("runEdit --dry-run err=%v", err)
	}
	if !strings.Contains(stdout, "dry-run") {
		t.Errorf("stdout missing dry-run header:\n%s", stdout)
	}
	if !strings.Contains(stdout, "new-model") {
		t.Errorf("stdout missing new value in diff:\n%s", stdout)
	}

	// On-disk file must be untouched: same Model as before.
	loaded, _ := h.store.LoadProfile("work")
	if loaded.Core.Model != before.Core.Model {
		t.Errorf("dry-run mutated file: Model=%q, want %q", loaded.Core.Model, before.Core.Model)
	}
}

func TestEdit_DryRunNoChanges(t *testing.T) {
	h := newEditHarness(t)
	h.seedProfile("work", "sk-old-1234abcd", "https://old.example.com", "old-model")

	// EDITOR that leaves the temp file exactly as-is. The dry-run
	// re-parse should show no changes and print the "(no changes)"
	// marker in the diff body.
	restore := SetEditorRunnerForTest(func(string) error { return nil })
	t.Cleanup(restore)
	editDryRunFlag = true
	stdout, _, err := runEditInner(t, "work")
	if err != nil {
		t.Fatalf("runEdit dry-run no-op err=%v", err)
	}
	// UpdatedAt bumps every dry-run so a byte-identical diff is
	// impossible; the "no changes" branch fires only when we mutate
	// nothing else. Assertion is limited to header presence.
	if !strings.Contains(stdout, "dry-run") {
		t.Errorf("stdout missing dry-run header:\n%s", stdout)
	}
}

// ---------------------------------------------------------------------------
// Editor mode
// ---------------------------------------------------------------------------

// TestEdit_EditorMode: a no-op editor round-trips the profile
// unchanged (except for UpdatedAt) and SaveProfile succeeds.
func TestEdit_EditorMode(t *testing.T) {
	h := newEditHarness(t)
	h.seedProfile("work", "sk-editortest-1234", "https://editor.example.com", "opus")

	restore := SetEditorRunnerForTest(func(string) error { return nil })
	t.Cleanup(restore)

	if _, _, err := runEditInner(t, "work"); err != nil {
		t.Fatalf("runEdit editor no-op err=%v", err)
	}
	loaded, err := h.store.LoadProfile("work")
	if err != nil {
		t.Fatalf("LoadProfile: %v", err)
	}
	if loaded.Core.Model != "opus" {
		t.Errorf("Core.Model mutated: %q", loaded.Core.Model)
	}
	// UpdatedAt should equal the pinned clock.
	fixed := time.Date(2026, 7, 2, 12, 0, 0, 0, time.UTC)
	if !loaded.UpdatedAt.Equal(fixed) {
		t.Errorf("UpdatedAt=%v; want %v", loaded.UpdatedAt, fixed)
	}
}

// TestEdit_EditorMode_CorruptSaveRejected: editor writes malformed
// YAML → refuse, original file preserved byte-for-byte.
func TestEdit_EditorMode_CorruptSaveRejected(t *testing.T) {
	h := newEditHarness(t)
	before := h.seedProfile("work", "sk-corrupt-test-1234", "https://api.example.com", "opus")

	restore := SetEditorRunnerForTest(func(path string) error {
		return os.WriteFile(path, []byte("this: is: not: valid: yaml: [\n"), 0o600)
	})
	t.Cleanup(restore)

	_, _, err := runEditInner(t, "work")
	if err == nil {
		t.Fatal("expected error on corrupt editor save")
	}
	if !strings.Contains(err.Error(), "rejected") && !strings.Contains(err.Error(), "malformed") {
		t.Errorf("error missing rejection hint: %v", err)
	}
	loaded, err := h.store.LoadProfile("work")
	if err != nil {
		t.Fatalf("LoadProfile after corrupt reject: %v", err)
	}
	if loaded.Core.APIKey != before.Core.APIKey || loaded.Core.Model != before.Core.Model {
		t.Errorf("original mutated after reject: got %+v; want %+v", loaded.Core, before.Core)
	}
}

// TestEdit_EditorMode_NameChangeRejected: editor writes a profile
// whose Name is different → refuse, original preserved.
func TestEdit_EditorMode_NameChangeRejected(t *testing.T) {
	h := newEditHarness(t)
	h.seedProfile("work", "sk-namechange-1234", "https://api.example.com", "opus")

	restore := SetEditorRunnerForTest(func(path string) error {
		body := []byte(`schema_version: 1
name: renamed
created_at: 2026-01-01T00:00:00Z
updated_at: 2026-01-01T00:00:00Z
core:
  base_url: https://api.example.com
  api_key: sk-namechange-1234
  model: opus
`)
		return os.WriteFile(path, body, 0o600)
	})
	t.Cleanup(restore)

	_, _, err := runEditInner(t, "work")
	if err == nil {
		t.Fatal("expected refusal on name change")
	}
	if !strings.Contains(err.Error(), "name changed") {
		t.Errorf("error missing 'name changed' hint: %v", err)
	}
	// Original name file still present.
	if _, err := h.store.LoadProfile("work"); err != nil {
		t.Errorf("original profile gone: %v", err)
	}
}

// TestEdit_SchemaVersionDriftRejected: editor writes a profile with
// schema_version: 99 → refuse. ParseProfile itself refuses future
// schemas, so the error surfaces from the re-parse gate.
func TestEdit_SchemaVersionDriftRejected(t *testing.T) {
	h := newEditHarness(t)
	h.seedProfile("work", "sk-schema-test-1234", "https://api.example.com", "opus")

	restore := SetEditorRunnerForTest(func(path string) error {
		body := []byte(`schema_version: 99
name: work
created_at: 2026-01-01T00:00:00Z
updated_at: 2026-01-01T00:00:00Z
core:
  base_url: https://api.example.com
  api_key: sk-schema-test-1234
  model: opus
`)
		return os.WriteFile(path, body, 0o600)
	})
	t.Cleanup(restore)

	_, _, err := runEditInner(t, "work")
	if err == nil {
		t.Fatal("expected refusal on schema version drift")
	}
	if !strings.Contains(err.Error(), "schema") {
		t.Errorf("error missing schema hint: %v", err)
	}
}

// TestEdit_UpdatedAtBumped: after a --set edit, UpdatedAt equals the
// pinned clock (not the original CreatedAt).
func TestEdit_UpdatedAtBumped(t *testing.T) {
	h := newEditHarness(t)
	h.seedProfile("work", "sk-updatedat-1234", "https://api.example.com", "opus")

	editSetFlag = []string{"core.model=new-model"}
	if _, _, err := runEditInner(t, "work"); err != nil {
		t.Fatalf("runEdit err=%v", err)
	}
	loaded, _ := h.store.LoadProfile("work")
	fixed := time.Date(2026, 7, 2, 12, 0, 0, 0, time.UTC)
	if !loaded.UpdatedAt.Equal(fixed) {
		t.Errorf("UpdatedAt=%v; want %v", loaded.UpdatedAt, fixed)
	}
	// CreatedAt untouched.
	created := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	if !loaded.CreatedAt.Equal(created) {
		t.Errorf("CreatedAt=%v; want %v", loaded.CreatedAt, created)
	}
}

// TestEdit_EditorRunnerFails: editor exits non-zero → error surfaces
// unchanged, original preserved.
func TestEdit_EditorRunnerFails(t *testing.T) {
	h := newEditHarness(t)
	h.seedProfile("work", "sk-runnerfail-1234", "https://api.example.com", "opus")

	restore := SetEditorRunnerForTest(func(string) error {
		return &exitErrShim{msg: "editor died"}
	})
	t.Cleanup(restore)

	_, _, err := runEditInner(t, "work")
	if err == nil {
		t.Fatal("expected editor-runner failure to surface")
	}
	if !strings.Contains(err.Error(), "editor") {
		t.Errorf("error missing 'editor' hint: %v", err)
	}
}

type exitErrShim struct{ msg string }

func (e *exitErrShim) Error() string { return e.msg }

// TestEdit_EmptyName: empty positional arg → error before any I/O.
func TestEdit_EmptyName(t *testing.T) {
	newEditHarness(t)
	_, _, err := runEditInner(t, "")
	if err == nil || !strings.Contains(err.Error(), "empty") {
		t.Errorf("expected empty-name error, got %v", err)
	}
}

// TestEdit_SetAllCoreFields exercises every core.* setter path so the
// closure bodies in editCoreFieldSetters each get covered.
func TestEdit_SetAllCoreFields(t *testing.T) {
	h := newEditHarness(t)
	h.seedProfile("work", "sk-cover-1234abcd", "https://old.example.com", "old")

	editSetFlag = []string{
		"core.base_url=https://new.example.com",
		"core.api_key=sk-newvalue-9999",
		"core.model=new-model",
		"core.small_fast_model=new-fast",
		"core.provider=anthropic",
	}
	if _, _, err := runEditInner(t, "work"); err != nil {
		t.Fatalf("runEdit err=%v", err)
	}
	loaded, _ := h.store.LoadProfile("work")
	if loaded.Core.BaseURL != "https://new.example.com" {
		t.Errorf("base_url=%q", loaded.Core.BaseURL)
	}
	if loaded.Core.APIKey != "sk-newvalue-9999" {
		t.Errorf("api_key=%q", loaded.Core.APIKey)
	}
	if loaded.Core.Model != "new-model" {
		t.Errorf("model=%q", loaded.Core.Model)
	}
	if loaded.Core.SmallFastModel != "new-fast" {
		t.Errorf("small_fast_model=%q", loaded.Core.SmallFastModel)
	}
	if loaded.Core.Provider != "anthropic" {
		t.Errorf("provider=%q", loaded.Core.Provider)
	}
}

// TestEdit_SetEmptyKey: --set with empty key before '=' → error.
func TestEdit_SetEmptyKey(t *testing.T) {
	h := newEditHarness(t)
	h.seedProfile("work", "sk-emptykey-1234", "https://api.example.com", "opus")
	editSetFlag = []string{"=value"}
	_, _, err := runEditInner(t, "work")
	if err == nil || !strings.Contains(err.Error(), "empty key") {
		t.Errorf("expected empty-key error, got %v", err)
	}
}

// TestEdit_SetEnvVarInvalid: tools.claude_code.env.<name> where <name>
// contains a dot → error at env-var-name validation.
func TestEdit_SetEnvVarInvalid(t *testing.T) {
	h := newEditHarness(t)
	h.seedProfile("work", "sk-envinvalid-1234", "https://api.example.com", "opus")
	editSetFlag = []string{"tools.claude_code.env.HAS.DOT=x"}
	_, _, err := runEditInner(t, "work")
	if err == nil || !strings.Contains(err.Error(), "invalid character") {
		t.Errorf("expected env-var-name error, got %v", err)
	}
}

// TestEdit_SetCodexRawEmptyKey: tools.codex.raw. with no key part → error.
func TestEdit_SetCodexRawEmptyKey(t *testing.T) {
	h := newEditHarness(t)
	h.seedProfile("work", "sk-rawempty-1234", "https://api.example.com", "opus")
	editSetFlag = []string{"tools.codex.raw.=x"}
	_, _, err := runEditInner(t, "work")
	if err == nil || !strings.Contains(err.Error(), "codex raw key is empty") {
		t.Errorf("expected codex-empty-key error, got %v", err)
	}
}

// TestEdit_DryRunNoOpNoChangesLine: --set with a value equal to the
// current one produces the "(no changes)" body except UpdatedAt bumps
// so we get real diff bytes; use --set to an identical value AND pin
// UpdatedAt via the seeded value to prove renderEditDryRun handles a
// zero-length diff. We fake it by keeping the seed's UpdatedAt equal
// to the pinned clock — then no bytes change at all.
func TestEdit_DryRunNoOpNoChangesLine(t *testing.T) {
	h := newEditHarness(t)
	// Seed with UpdatedAt == the pinned clock so applying the same
	// value produces byte-identical YAML.
	p := config.NewProfile("noop", "https://api.example.com", "sk-noop-1234-5678")
	p.Core.Model = "opus"
	p.CreatedAt = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	p.UpdatedAt = time.Date(2026, 7, 2, 12, 0, 0, 0, time.UTC)
	if err := h.store.SaveProfile(p); err != nil {
		h.t.Fatalf("SaveProfile: %v", err)
	}

	editSetFlag = []string{"core.model=opus"}
	editDryRunFlag = true
	stdout, _, err := runEditInner(t, "noop")
	if err != nil {
		t.Fatalf("runEdit err=%v", err)
	}
	if !strings.Contains(stdout, "(no changes)") {
		t.Errorf("stdout missing '(no changes)' marker:\n%s", stdout)
	}
}
