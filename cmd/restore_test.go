package cmd

// restore_test.go — Story E6-S7 tests for the cmd/restore surface.
//
// Test isolation strategy:
//   1. HOME points at a per-test t.TempDir() so storage.Default() reads
//      a fresh tree and the developer's real ~/.claudecm is never
//      touched.
//   2. resetRestoreFlags restores the cobra flag package-vars.
//   3. runRestoreInner wraps runRestore with bytes.Buffers.
//   4. Storage.Backup is used directly to seed backups so tests do not
//      depend on a working switch pipeline.
//   5. isTerminalFn is forced to false so the interactive branch never
//      hangs — --yes is passed on the tests that intend to write.

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"github.com/a2d2-dev/claudecm/internal/adapter"
	claudecodeadapter "github.com/a2d2-dev/claudecm/internal/adapter/claudecode"
	codexadapter "github.com/a2d2-dev/claudecm/internal/adapter/codex"
	"github.com/a2d2-dev/claudecm/internal/commit"
	"github.com/a2d2-dev/claudecm/internal/storage"
)

func resetRestoreFlags() {
	restoreToolFlag = ""
	restoreListFlag = false
	restoreLatestFlag = false
	restoreIDFlag = ""
	restoreDryRunFlag = false
	restoreYesFlag = false
	restoreOutputFlag = "text"
}

type restoreHarness struct {
	t     *testing.T
	home  string
	resv  *storage.Resolver
	store *storage.FileStorage
}

func newRestoreHarness(t *testing.T) *restoreHarness {
	t.Helper()
	resetRestoreFlags()

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

	// Non-TTY by default so interactive prompts never hang.
	restoreTTY := SetIsTerminalForTest(func(*os.File) bool { return false })
	t.Cleanup(restoreTTY)

	return &restoreHarness{t: t, home: home, resv: resv, store: store}
}

// seedFile writes body to path (creating parent dirs) with mode 0600.
func (h *restoreHarness) seedFile(path string, body []byte) {
	h.t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		h.t.Fatalf("mkdir %s: %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, body, 0o600); err != nil {
		h.t.Fatalf("write %s: %v", path, err)
	}
}

// backupNow creates a backup of the current bytes at path and returns
// the resulting record.
func (h *restoreHarness) backupNow(tool adapter.ToolID, path string) storage.BackupRecord {
	h.t.Helper()
	rec, err := storage.Backup(h.resv, string(tool), filepath.Base(path), path)
	if err != nil {
		h.t.Fatalf("storage.Backup(%s): %v", path, err)
	}
	return rec
}

func runRestoreInner(t *testing.T) (stdout, stderr string, err error) {
	t.Helper()
	var out, errBuf bytes.Buffer
	cmd := &cobra.Command{Use: "restore"}
	cmd.SetOut(&out)
	cmd.SetErr(&errBuf)
	err = runRestore(cmd, nil)
	return out.String(), errBuf.String(), err
}

// ---------------------------------------------------------------------------
// --list
// ---------------------------------------------------------------------------

func TestRestore_ListShowsBackupsNewestFirst(t *testing.T) {
	h := newRestoreHarness(t)
	settings := claudecodeadapter.SettingsPath(h.resv)
	h.seedFile(settings, []byte(`{"env":{"ANTHROPIC_MODEL":"old"}}`))
	first := h.backupNow(adapter.ToolClaudeCode, settings)
	// Force a monotonic gap so the timestamps sort predictably.
	time.Sleep(2 * time.Millisecond)
	h.seedFile(settings, []byte(`{"env":{"ANTHROPIC_MODEL":"new"}}`))
	second := h.backupNow(adapter.ToolClaudeCode, settings)

	restoreToolFlag = "claude-code"
	restoreListFlag = true
	stdout, _, err := runRestoreInner(t)
	if err != nil {
		t.Fatalf("runRestore --list err=%v", err)
	}
	if !strings.Contains(stdout, first.BackupPath) {
		t.Errorf("stdout missing first backup path: %s", stdout)
	}
	if !strings.Contains(stdout, second.BackupPath) {
		t.Errorf("stdout missing second backup path: %s", stdout)
	}
	// Newest first: second should appear before first.
	if strings.Index(stdout, second.BackupPath) > strings.Index(stdout, first.BackupPath) {
		t.Errorf("backups not newest-first:\n%s", stdout)
	}
}

func TestRestore_ListEmpty(t *testing.T) {
	newRestoreHarness(t)
	restoreToolFlag = "claude-code"
	restoreListFlag = true
	stdout, _, err := runRestoreInner(t)
	if err != nil {
		t.Fatalf("runRestore --list err=%v", err)
	}
	if !strings.Contains(stdout, "No backups") {
		t.Errorf("stdout missing empty-list message:\n%s", stdout)
	}
}

func TestRestore_ListJSON(t *testing.T) {
	h := newRestoreHarness(t)
	settings := claudecodeadapter.SettingsPath(h.resv)
	h.seedFile(settings, []byte(`{"env":{"ANTHROPIC_MODEL":"one"}}`))
	h.backupNow(adapter.ToolClaudeCode, settings)

	restoreToolFlag = "claude-code"
	restoreListFlag = true
	restoreOutputFlag = "json"
	stdout, _, err := runRestoreInner(t)
	if err != nil {
		t.Fatalf("runRestore --list json err=%v", err)
	}
	var body jsonRestoreList
	if err := json.Unmarshal([]byte(stdout), &body); err != nil {
		t.Fatalf("json parse: %v\n%s", err, stdout)
	}
	if body.Action != "list" || body.Tool != "claude-code" {
		t.Errorf("wrong action/tool: %+v", body)
	}
	if len(body.Entries) != 1 {
		t.Errorf("Entries len=%d; want 1", len(body.Entries))
	}
}

// ---------------------------------------------------------------------------
// --latest single file
// ---------------------------------------------------------------------------

func TestRestore_LatestSingleFile(t *testing.T) {
	h := newRestoreHarness(t)
	settings := claudecodeadapter.SettingsPath(h.resv)
	first := []byte(`{"env":{"ANTHROPIC_MODEL":"first"}}`)
	h.seedFile(settings, first)
	h.backupNow(adapter.ToolClaudeCode, settings)
	// Simulate switch that overwrote the file.
	h.seedFile(settings, []byte(`{"env":{"ANTHROPIC_MODEL":"current"}}`))

	restoreToolFlag = "claude-code"
	restoreLatestFlag = true
	restoreYesFlag = true
	stdout, _, err := runRestoreInner(t)
	if err != nil {
		t.Fatalf("runRestore --latest err=%v", err)
	}
	if !strings.Contains(stdout, "Restored 1 file") {
		t.Errorf("stdout missing success line:\n%s", stdout)
	}
	got, err := os.ReadFile(settings)
	if err != nil {
		t.Fatalf("read settings: %v", err)
	}
	if !bytes.Equal(got, first) {
		t.Errorf("settings.json not restored:\ngot=%s\nwant=%s", got, first)
	}
}

// ---------------------------------------------------------------------------
// --latest two files (codex)
// ---------------------------------------------------------------------------

func TestRestore_LatestHappyBothFiles(t *testing.T) {
	h := newRestoreHarness(t)
	auth := codexadapter.AuthPath(h.resv)
	config := codexadapter.ConfigPath(h.resv)
	authBefore := []byte(`{"OPENAI_API_KEY":"sk-first-auth-1234"}`)
	configBefore := []byte("model = \"first-model\"\n")
	h.seedFile(auth, authBefore)
	h.seedFile(config, configBefore)
	h.backupNow(adapter.ToolCodex, auth)
	h.backupNow(adapter.ToolCodex, config)

	// Simulate overwrites.
	h.seedFile(auth, []byte(`{"OPENAI_API_KEY":"sk-current-auth-5678"}`))
	h.seedFile(config, []byte("model = \"current-model\"\n"))

	restoreToolFlag = "codex"
	restoreLatestFlag = true
	restoreYesFlag = true
	stdout, _, err := runRestoreInner(t)
	if err != nil {
		t.Fatalf("runRestore --latest err=%v", err)
	}
	if !strings.Contains(stdout, "Restored 2 file") {
		t.Errorf("stdout missing 2-file success line:\n%s", stdout)
	}
	gotAuth, _ := os.ReadFile(auth)
	if !bytes.Equal(gotAuth, authBefore) {
		t.Errorf("auth.json not restored:\ngot=%s\nwant=%s", gotAuth, authBefore)
	}
	gotConfig, _ := os.ReadFile(config)
	if !bytes.Equal(gotConfig, configBefore) {
		t.Errorf("config.toml not restored:\ngot=%s\nwant=%s", gotConfig, configBefore)
	}
}

// ---------------------------------------------------------------------------
// --dry-run
// ---------------------------------------------------------------------------

func TestRestore_DryRunNoWrite(t *testing.T) {
	h := newRestoreHarness(t)
	settings := claudecodeadapter.SettingsPath(h.resv)
	backupBody := []byte(`{"env":{"ANTHROPIC_MODEL":"backup"}}`)
	h.seedFile(settings, backupBody)
	h.backupNow(adapter.ToolClaudeCode, settings)
	currentBody := []byte(`{"env":{"ANTHROPIC_MODEL":"current"}}`)
	h.seedFile(settings, currentBody)

	restoreToolFlag = "claude-code"
	restoreLatestFlag = true
	restoreDryRunFlag = true
	stdout, _, err := runRestoreInner(t)
	if err != nil {
		t.Fatalf("runRestore --dry-run err=%v", err)
	}
	if !strings.Contains(stdout, "dry-run") {
		t.Errorf("stdout missing dry-run header:\n%s", stdout)
	}
	// File on disk still current, not backup body.
	got, _ := os.ReadFile(settings)
	if !bytes.Equal(got, currentBody) {
		t.Errorf("dry-run mutated file:\ngot=%s", got)
	}
}

// ---------------------------------------------------------------------------
// --id
// ---------------------------------------------------------------------------

func TestRestore_IDHappy(t *testing.T) {
	h := newRestoreHarness(t)
	settings := claudecodeadapter.SettingsPath(h.resv)
	original := []byte(`{"env":{"ANTHROPIC_MODEL":"snapshot"}}`)
	h.seedFile(settings, original)
	rec := h.backupNow(adapter.ToolClaudeCode, settings)

	// Overwrite so restore has bytes to change.
	h.seedFile(settings, []byte(`{"env":{"ANTHROPIC_MODEL":"current"}}`))

	restoreToolFlag = "claude-code"
	restoreIDFlag = filepath.Base(rec.BackupPath)
	restoreYesFlag = true
	stdout, _, err := runRestoreInner(t)
	if err != nil {
		t.Fatalf("runRestore --id err=%v; stdout=%s", err, stdout)
	}
	got, _ := os.ReadFile(settings)
	if !bytes.Equal(got, original) {
		t.Errorf("restore --id did not restore original:\ngot=%s\nwant=%s", got, original)
	}
}

func TestRestore_IDNotFound(t *testing.T) {
	h := newRestoreHarness(t)
	settings := claudecodeadapter.SettingsPath(h.resv)
	h.seedFile(settings, []byte(`{"env":{"ANTHROPIC_MODEL":"x"}}`))
	h.backupNow(adapter.ToolClaudeCode, settings)

	restoreToolFlag = "claude-code"
	restoreIDFlag = "definitely.not.here"
	restoreYesFlag = true
	_, _, err := runRestoreInner(t)
	if err == nil || !strings.Contains(err.Error(), "no backup matching") {
		t.Errorf("expected id-not-found error, got %v", err)
	}
}

// ---------------------------------------------------------------------------
// Error paths
// ---------------------------------------------------------------------------

func TestRestore_UnknownToolErrors(t *testing.T) {
	newRestoreHarness(t)
	restoreToolFlag = "unknown-tool"
	restoreLatestFlag = true
	_, _, err := runRestoreInner(t)
	if err == nil || !strings.Contains(err.Error(), "unknown --tool") {
		t.Errorf("expected unknown-tool error, got %v", err)
	}
}

func TestRestore_NoBackupsErrors(t *testing.T) {
	newRestoreHarness(t)
	restoreToolFlag = "claude-code"
	restoreLatestFlag = true
	restoreYesFlag = true
	_, _, err := runRestoreInner(t)
	if err == nil || !strings.Contains(err.Error(), "no backups") {
		t.Errorf("expected no-backups error, got %v", err)
	}
}

func TestRestore_MissingModeErrors(t *testing.T) {
	newRestoreHarness(t)
	restoreToolFlag = "claude-code"
	_, _, err := runRestoreInner(t)
	if err == nil || !strings.Contains(err.Error(), "one of --list") {
		t.Errorf("expected missing-mode error, got %v", err)
	}
}

func TestRestore_MissingToolErrors(t *testing.T) {
	newRestoreHarness(t)
	restoreListFlag = true
	_, _, err := runRestoreInner(t)
	if err == nil || !strings.Contains(err.Error(), "--tool is required") {
		t.Errorf("expected missing-tool error, got %v", err)
	}
}

func TestRestore_MutuallyExclusiveModes(t *testing.T) {
	newRestoreHarness(t)
	restoreToolFlag = "claude-code"
	restoreListFlag = true
	restoreLatestFlag = true
	_, _, err := runRestoreInner(t)
	if err == nil || !strings.Contains(err.Error(), "mutually exclusive") {
		t.Errorf("expected mutex error, got %v", err)
	}
}

func TestRestore_YesSkipsConfirm(t *testing.T) {
	h := newRestoreHarness(t)
	// TTY on so a missing --yes WOULD prompt; --yes bypasses.
	restoreTTY := SetIsTerminalForTest(func(*os.File) bool { return true })
	t.Cleanup(restoreTTY)

	settings := claudecodeadapter.SettingsPath(h.resv)
	original := []byte(`{"env":{"ANTHROPIC_MODEL":"snap"}}`)
	h.seedFile(settings, original)
	h.backupNow(adapter.ToolClaudeCode, settings)
	h.seedFile(settings, []byte(`{"env":{"ANTHROPIC_MODEL":"changed"}}`))

	restoreToolFlag = "claude-code"
	restoreLatestFlag = true
	restoreYesFlag = true
	if _, _, err := runRestoreInner(t); err != nil {
		t.Fatalf("runRestore --yes err=%v", err)
	}
	got, _ := os.ReadFile(settings)
	if !bytes.Equal(got, original) {
		t.Errorf("--yes did not restore file")
	}
}

// TestRestore_NonInteractiveWithoutYesRefused: TTY off, --yes off,
// backups present, --latest set → refuse rather than silently apply.
func TestRestore_NonInteractiveWithoutYesRefused(t *testing.T) {
	h := newRestoreHarness(t)
	settings := claudecodeadapter.SettingsPath(h.resv)
	h.seedFile(settings, []byte(`{"env":{"ANTHROPIC_MODEL":"snap"}}`))
	h.backupNow(adapter.ToolClaudeCode, settings)
	h.seedFile(settings, []byte(`{"env":{"ANTHROPIC_MODEL":"changed"}}`))

	restoreToolFlag = "claude-code"
	restoreLatestFlag = true
	_, _, err := runRestoreInner(t)
	if err == nil || !strings.Contains(err.Error(), "non-interactive") {
		t.Errorf("expected non-interactive refusal, got %v", err)
	}
}

// TestRestore_RoundTrip: seed file A → backup → overwrite with B →
// restore --latest → file matches A byte-for-byte.
func TestRestore_RoundTrip(t *testing.T) {
	h := newRestoreHarness(t)
	settings := claudecodeadapter.SettingsPath(h.resv)
	stateA := []byte(`{"env":{"ANTHROPIC_MODEL":"A"}}`)
	stateB := []byte(`{"env":{"ANTHROPIC_MODEL":"B","ANTHROPIC_BASE_URL":"https://b.example.com"}}`)
	h.seedFile(settings, stateA)
	h.backupNow(adapter.ToolClaudeCode, settings)
	h.seedFile(settings, stateB)

	restoreToolFlag = "claude-code"
	restoreLatestFlag = true
	restoreYesFlag = true
	if _, _, err := runRestoreInner(t); err != nil {
		t.Fatalf("runRestore err=%v", err)
	}
	got, err := os.ReadFile(settings)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !bytes.Equal(got, stateA) {
		t.Errorf("round-trip failed:\ngot=%s\nwant=%s", got, stateA)
	}
	// Verify a pre-restore backup was taken (the switch-from-B state is
	// captured). ListBackups should now show TWO entries.
	recs, err := storage.ListBackups(h.resv, "claude_code", "settings.json")
	if err != nil {
		t.Fatalf("ListBackups: %v", err)
	}
	if len(recs) < 2 {
		t.Errorf("expected >=2 backups after round-trip; got %d", len(recs))
	}
}

// TestRestore_InteractiveConfirmYes: TTY on, stdin fed "y\n"; restore
// applies. Exercises renderRestoreConfirmSummary + promptConfirm.
func TestRestore_InteractiveConfirmYes(t *testing.T) {
	h := newRestoreHarness(t)
	restoreTTY := SetIsTerminalForTest(func(*os.File) bool { return true })
	t.Cleanup(restoreTTY)

	settings := claudecodeadapter.SettingsPath(h.resv)
	orig := []byte(`{"env":{"ANTHROPIC_MODEL":"orig"}}`)
	h.seedFile(settings, orig)
	h.backupNow(adapter.ToolClaudeCode, settings)
	h.seedFile(settings, []byte(`{"env":{"ANTHROPIC_MODEL":"changed"}}`))

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("Pipe: %v", err)
	}
	origStdin := os.Stdin
	os.Stdin = r
	t.Cleanup(func() { os.Stdin = origStdin })
	if _, err := w.WriteString("y\n"); err != nil {
		t.Fatalf("write: %v", err)
	}
	_ = w.Close()

	restoreToolFlag = "claude-code"
	restoreLatestFlag = true
	if _, _, err := runRestoreInner(t); err != nil {
		t.Fatalf("runRestore interactive-yes err=%v", err)
	}
	got, _ := os.ReadFile(settings)
	if !bytes.Equal(got, orig) {
		t.Errorf("interactive-yes did not restore original:\ngot=%s", got)
	}
}

// TestRestore_InteractiveConfirmNo: TTY on, stdin fed "n\n"; user
// aborts, no writes.
func TestRestore_InteractiveConfirmNo(t *testing.T) {
	h := newRestoreHarness(t)
	restoreTTY := SetIsTerminalForTest(func(*os.File) bool { return true })
	t.Cleanup(restoreTTY)

	settings := claudecodeadapter.SettingsPath(h.resv)
	h.seedFile(settings, []byte(`{"env":{"ANTHROPIC_MODEL":"orig"}}`))
	h.backupNow(adapter.ToolClaudeCode, settings)
	changed := []byte(`{"env":{"ANTHROPIC_MODEL":"changed"}}`)
	h.seedFile(settings, changed)

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("Pipe: %v", err)
	}
	origStdin := os.Stdin
	os.Stdin = r
	t.Cleanup(func() { os.Stdin = origStdin })
	if _, err := w.WriteString("n\n"); err != nil {
		t.Fatalf("write: %v", err)
	}
	_ = w.Close()

	restoreToolFlag = "claude-code"
	restoreLatestFlag = true
	_, _, err = runRestoreInner(t)
	if err == nil || !strings.Contains(err.Error(), "aborted") {
		t.Errorf("expected abort error, got %v", err)
	}
	got, _ := os.ReadFile(settings)
	if !bytes.Equal(got, changed) {
		t.Errorf("no-confirm mutated file:\ngot=%s", got)
	}
}

// TestRestore_IDCodexAuth: --id lets the operator pick a specific
// codex owned file even when the other file has backups too. Restore
// exercises the buildRestorePlan path with a single-file result.
func TestRestore_IDCodexAuth(t *testing.T) {
	h := newRestoreHarness(t)
	auth := codexadapter.AuthPath(h.resv)
	config := codexadapter.ConfigPath(h.resv)
	authOrig := []byte(`{"OPENAI_API_KEY":"sk-orig-auth-1234"}`)
	h.seedFile(auth, authOrig)
	h.seedFile(config, []byte("model = \"orig\"\n"))
	authRec := h.backupNow(adapter.ToolCodex, auth)
	h.backupNow(adapter.ToolCodex, config)

	// Overwrite only auth so restore has bytes to change.
	h.seedFile(auth, []byte(`{"OPENAI_API_KEY":"sk-changed-9999"}`))

	restoreToolFlag = "codex"
	restoreIDFlag = filepath.Base(authRec.BackupPath)
	restoreYesFlag = true
	if _, _, err := runRestoreInner(t); err != nil {
		t.Fatalf("runRestore --id auth err=%v", err)
	}
	got, _ := os.ReadFile(auth)
	if !bytes.Equal(got, authOrig) {
		t.Errorf("--id did not restore auth.json:\ngot=%s", got)
	}
}

// TestRestore_DryRunJSON: --dry-run --output json emits the structured
// document with action=dry-run and one source.
func TestRestore_DryRunJSON(t *testing.T) {
	h := newRestoreHarness(t)
	settings := claudecodeadapter.SettingsPath(h.resv)
	h.seedFile(settings, []byte(`{"env":{"ANTHROPIC_MODEL":"orig"}}`))
	h.backupNow(adapter.ToolClaudeCode, settings)

	restoreToolFlag = "claude-code"
	restoreLatestFlag = true
	restoreDryRunFlag = true
	restoreOutputFlag = "json"
	stdout, _, err := runRestoreInner(t)
	if err != nil {
		t.Fatalf("runRestore dry-run json err=%v", err)
	}
	var body jsonRestoreDryRun
	if err := json.Unmarshal([]byte(stdout), &body); err != nil {
		t.Fatalf("json parse: %v\n%s", err, stdout)
	}
	if body.Action != "dry-run" {
		t.Errorf("Action=%q; want dry-run", body.Action)
	}
	if len(body.Sources) != 1 {
		t.Errorf("Sources len=%d; want 1", len(body.Sources))
	}
}

// TestRestore_InvalidOutputFlag: bad --output value → error.
func TestRestore_InvalidOutputFlag(t *testing.T) {
	newRestoreHarness(t)
	restoreToolFlag = "claude-code"
	restoreListFlag = true
	restoreOutputFlag = "xml"
	_, _, err := runRestoreInner(t)
	if err == nil || !strings.Contains(err.Error(), "invalid --output") {
		t.Errorf("expected invalid-output error, got %v", err)
	}
}

// TestRestore_RenderPartialFailure_Text exercises the partial-failure
// text renderer directly with a synthetic PartialFailure so cmd/restore
// covers its error rendering without needing to force a real commit
// mid-run failure.
func TestRestore_RenderPartialFailure_Text(t *testing.T) {
	var buf bytes.Buffer
	pf := &commit.PartialFailure{
		FailedFile: "/tmp/failed",
		Cause:      &exitErrShim{msg: "boom"},
		RolledBack: []string{"/tmp/rb1"},
		Untouched:  []string{"/tmp/ut1"},
	}
	renderRestorePartialFailure(&buf, restoreOutputText, "codex", pf)
	got := buf.String()
	for _, want := range []string{"failed", "/tmp/failed", "boom", "rolled-back", "/tmp/rb1", "untouched", "/tmp/ut1"} {
		if !strings.Contains(got, want) {
			t.Errorf("output missing %q:\n%s", want, got)
		}
	}
}

// TestRestore_RenderPartialFailure_JSON exercises the JSON branch.
func TestRestore_RenderPartialFailure_JSON(t *testing.T) {
	var buf bytes.Buffer
	pf := &commit.PartialFailure{
		FailedFile: "/tmp/failed",
		Cause:      &exitErrShim{msg: "boom"},
		RolledBack: []string{"/tmp/rb1"},
		Untouched:  []string{"/tmp/ut1"},
	}
	renderRestorePartialFailure(&buf, restoreOutputJSON, "codex", pf)
	var body jsonRestorePartial
	if err := json.Unmarshal(buf.Bytes(), &body); err != nil {
		t.Fatalf("json parse: %v\n%s", err, buf.String())
	}
	if body.Tool != "codex" || body.FailedFile != "/tmp/failed" || body.Cause != "boom" {
		t.Errorf("wrong body: %+v", body)
	}
}

// TestRestore_CurrentAbsent: run --latest on a tool whose owned file
// backup exists but the owned file itself is absent. Exercises the
// CurrentHas=false branch in restoreSource rendering and buildRestorePlan.
func TestRestore_CurrentAbsent(t *testing.T) {
	h := newRestoreHarness(t)
	settings := claudecodeadapter.SettingsPath(h.resv)
	body := []byte(`{"env":{"ANTHROPIC_MODEL":"seed"}}`)
	h.seedFile(settings, body)
	h.backupNow(adapter.ToolClaudeCode, settings)
	// Remove the current file so CurrentHas=false in the source.
	if err := os.Remove(settings); err != nil {
		t.Fatalf("remove settings: %v", err)
	}

	restoreToolFlag = "claude-code"
	restoreLatestFlag = true
	restoreDryRunFlag = true
	stdout, _, err := runRestoreInner(t)
	if err != nil {
		t.Fatalf("runRestore dry-run absent-current err=%v", err)
	}
	if !strings.Contains(stdout, "current: (absent)") {
		t.Errorf("stdout missing absent-current marker:\n%s", stdout)
	}
}

// TestRestore_UntouchedWhenBackupMatchesCurrent: restore where current
// file already matches the backup bytes → commit marks Untouched, the
// success line reports 0 committed + an untouched note.
func TestRestore_UntouchedWhenBackupMatchesCurrent(t *testing.T) {
	h := newRestoreHarness(t)
	settings := claudecodeadapter.SettingsPath(h.resv)
	body := []byte(`{"env":{"ANTHROPIC_MODEL":"same"}}`)
	h.seedFile(settings, body)
	h.backupNow(adapter.ToolClaudeCode, settings)
	// Do NOT overwrite: current == backup. Restore should be a no-op.

	restoreToolFlag = "claude-code"
	restoreLatestFlag = true
	restoreYesFlag = true
	stdout, _, err := runRestoreInner(t)
	if err != nil {
		t.Fatalf("runRestore untouched err=%v", err)
	}
	if !strings.Contains(stdout, "Restored 0 file") {
		t.Errorf("stdout missing 0-file line:\n%s", stdout)
	}
	if !strings.Contains(stdout, "untouched") {
		t.Errorf("stdout missing untouched note:\n%s", stdout)
	}
}

// TestRestore_ListNewestFirstAcrossFiles exercises the F1 review
// finding: when --list enumerates backups spanning two different owned
// files (Codex's auth.json + config.toml), the ordering must be by
// TIMESTAMP not by BackupPath — otherwise the basename dominates the
// sort and one file's older backups render as if they were the newer
// ones. Seed alternating timestamps and assert strict chronological
// (newest-first) order across both files.
func TestRestore_ListNewestFirstAcrossFiles(t *testing.T) {
	h := newRestoreHarness(t)
	auth := codexadapter.AuthPath(h.resv)
	config := codexadapter.ConfigPath(h.resv)

	// Alternating writes so each backup captures a distinct timestamp.
	// The sleep gaps guarantee the backup timestamp suffixes differ at
	// nanosecond resolution — good enough for the fixed-width lexicographic
	// sort to produce a well-defined order.
	h.seedFile(auth, []byte(`{"OPENAI_API_KEY":"sk-auth-01"}`))
	authFirst := h.backupNow(adapter.ToolCodex, auth)
	time.Sleep(2 * time.Millisecond)

	h.seedFile(config, []byte("model = \"config-01\"\n"))
	configFirst := h.backupNow(adapter.ToolCodex, config)
	time.Sleep(2 * time.Millisecond)

	h.seedFile(auth, []byte(`{"OPENAI_API_KEY":"sk-auth-02"}`))
	authSecond := h.backupNow(adapter.ToolCodex, auth)
	time.Sleep(2 * time.Millisecond)

	h.seedFile(config, []byte("model = \"config-02\"\n"))
	configSecond := h.backupNow(adapter.ToolCodex, config)

	restoreToolFlag = "codex"
	restoreListFlag = true
	stdout, _, err := runRestoreInner(t)
	if err != nil {
		t.Fatalf("runRestore --list err=%v", err)
	}

	// All four backup paths must appear.
	for _, p := range []string{authFirst.BackupPath, authSecond.BackupPath, configFirst.BackupPath, configSecond.BackupPath} {
		if !strings.Contains(stdout, p) {
			t.Errorf("stdout missing backup path %q:\n%s", p, stdout)
		}
	}

	// Chronological order (newest first): configSecond > authSecond >
	// configFirst > authFirst.
	order := []string{configSecond.BackupPath, authSecond.BackupPath, configFirst.BackupPath, authFirst.BackupPath}
	prev := -1
	for _, p := range order {
		idx := strings.Index(stdout, p)
		if idx < 0 {
			t.Fatalf("path %q not present in stdout:\n%s", p, stdout)
		}
		if idx <= prev {
			t.Errorf("backups not newest-first: %q appeared at index %d, expected after previous index %d\nfull stdout:\n%s",
				p, idx, prev, stdout)
		}
		prev = idx
	}
}

// TestRestore_LatestReportsSkippedFilesWithoutBackups exercises the F5
// review finding: --latest on a multi-file tool must surface a warning
// line for each owned file that has no backups to restore from, rather
// than silently dropping them. Seed backups for auth.json only, then
// verify the text output warns about config.toml and the file itself
// remains unwritten.
func TestRestore_LatestReportsSkippedFilesWithoutBackups(t *testing.T) {
	h := newRestoreHarness(t)
	auth := codexadapter.AuthPath(h.resv)
	config := codexadapter.ConfigPath(h.resv)

	authOriginal := []byte(`{"OPENAI_API_KEY":"sk-auth-orig"}`)
	h.seedFile(auth, authOriginal)
	h.backupNow(adapter.ToolCodex, auth)
	// Seed config on disk but NEVER back it up.
	configCurrent := []byte("model = \"config-current\"\n")
	h.seedFile(config, configCurrent)
	// Overwrite auth so restore has bytes to change.
	h.seedFile(auth, []byte(`{"OPENAI_API_KEY":"sk-auth-changed"}`))

	restoreToolFlag = "codex"
	restoreLatestFlag = true
	restoreYesFlag = true
	stdout, _, err := runRestoreInner(t)
	if err != nil {
		t.Fatalf("runRestore --latest err=%v", err)
	}
	if !strings.Contains(stdout, "Restored 1 file") {
		t.Errorf("stdout missing 1-file success line:\n%s", stdout)
	}
	if !strings.Contains(stdout, "Warning: no backups available for "+config) {
		t.Errorf("stdout missing skipped-file warning:\n%s", stdout)
	}
	if !strings.Contains(stdout, "skipped") {
		t.Errorf("stdout missing 'skipped' marker:\n%s", stdout)
	}
	// auth.json restored, config.toml untouched.
	gotAuth, _ := os.ReadFile(auth)
	if !bytes.Equal(gotAuth, authOriginal) {
		t.Errorf("auth.json not restored:\ngot=%s\nwant=%s", gotAuth, authOriginal)
	}
	gotConfig, _ := os.ReadFile(config)
	if !bytes.Equal(gotConfig, configCurrent) {
		t.Errorf("config.toml unexpectedly mutated:\ngot=%s\nwant=%s", gotConfig, configCurrent)
	}
}

// TestRestore_LatestReportsSkippedJSON verifies the JSON view of the F5
// warning: a "skipped" array carrying the owned-file path and the
// "no backups" reason so machine consumers can distinguish partial
// from full restores.
func TestRestore_LatestReportsSkippedJSON(t *testing.T) {
	h := newRestoreHarness(t)
	auth := codexadapter.AuthPath(h.resv)
	config := codexadapter.ConfigPath(h.resv)
	h.seedFile(auth, []byte(`{"OPENAI_API_KEY":"sk-auth-json"}`))
	h.backupNow(adapter.ToolCodex, auth)
	h.seedFile(config, []byte("model = \"c\"\n"))
	h.seedFile(auth, []byte(`{"OPENAI_API_KEY":"sk-auth-json-changed"}`))

	restoreToolFlag = "codex"
	restoreLatestFlag = true
	restoreYesFlag = true
	restoreOutputFlag = "json"
	stdout, _, err := runRestoreInner(t)
	if err != nil {
		t.Fatalf("runRestore --latest json err=%v", err)
	}
	var body jsonRestoreSuccess
	if err := json.Unmarshal([]byte(stdout), &body); err != nil {
		t.Fatalf("json parse: %v\n%s", err, stdout)
	}
	if body.Committed != 1 {
		t.Errorf("Committed=%d; want 1", body.Committed)
	}
	if len(body.Skipped) != 1 {
		t.Fatalf("Skipped len=%d; want 1: %+v", len(body.Skipped), body.Skipped)
	}
	if body.Skipped[0].OwnedFile != config {
		t.Errorf("Skipped[0].OwnedFile=%q; want %q", body.Skipped[0].OwnedFile, config)
	}
	if body.Skipped[0].Reason != "no backups" {
		t.Errorf("Skipped[0].Reason=%q; want 'no backups'", body.Skipped[0].Reason)
	}
}

func TestRestore_SuccessJSON(t *testing.T) {
	h := newRestoreHarness(t)
	settings := claudecodeadapter.SettingsPath(h.resv)
	h.seedFile(settings, []byte(`{"env":{"ANTHROPIC_MODEL":"snap"}}`))
	h.backupNow(adapter.ToolClaudeCode, settings)
	h.seedFile(settings, []byte(`{"env":{"ANTHROPIC_MODEL":"changed"}}`))

	restoreToolFlag = "claude-code"
	restoreLatestFlag = true
	restoreYesFlag = true
	restoreOutputFlag = "json"
	stdout, _, err := runRestoreInner(t)
	if err != nil {
		t.Fatalf("runRestore json err=%v", err)
	}
	var body jsonRestoreSuccess
	if err := json.Unmarshal([]byte(stdout), &body); err != nil {
		t.Fatalf("json parse: %v\n%s", err, stdout)
	}
	if body.Action != "restored" || body.Committed != 1 {
		t.Errorf("wrong body: %+v", body)
	}
	if len(body.NewBackups) != 1 {
		t.Errorf("expected 1 new backup; got %+v", body.NewBackups)
	}
}
