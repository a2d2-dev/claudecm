package cmd

// rename_delete_test.go — Story E6-S6 tests for cmd/rename and cmd/delete.
//
// Shared harness pattern mirrors add_test.go: t.TempDir HOME, Bootstrap,
// FileStorage for read-back assertions, deterministic clock via
// SetNowForTest, per-test flag reset.

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

func resetRenameFlags() {
	renameOverwriteFlag = false
	renameDryRunFlag = false
}

func resetDeleteFlags() {
	deleteYesFlag = false
	deleteDryRunFlag = false
}

type renameDeleteHarness struct {
	t     *testing.T
	home  string
	resv  *storage.Resolver
	store *storage.FileStorage
	mgr   *config.Manager
}

func newRenameDeleteHarness(t *testing.T) *renameDeleteHarness {
	t.Helper()
	resetRenameFlags()
	resetDeleteFlags()

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

	fixed := time.Date(2026, 7, 2, 12, 0, 0, 0, time.UTC)
	restore := SetNowForTest(func() time.Time { return fixed })
	t.Cleanup(restore)

	// Non-TTY by default so delete's confirmation prompt does not hang.
	restoreTTY := SetIsTerminalForTest(func(*os.File) bool { return false })
	t.Cleanup(restoreTTY)

	return &renameDeleteHarness{t: t, home: home, resv: resv, store: store, mgr: mgr}
}

func (h *renameDeleteHarness) seed(name string) *config.Profile {
	h.t.Helper()
	p := config.NewProfile(name, "https://api.example.com", "sk-seed-"+name+"-1234")
	p.Core.Model = "opus"
	p.CreatedAt = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	p.UpdatedAt = p.CreatedAt
	if err := h.store.SaveProfile(p); err != nil {
		h.t.Fatalf("SaveProfile(%q): %v", name, err)
	}
	return p
}

func (h *renameDeleteHarness) activate(name string) {
	h.t.Helper()
	if err := h.mgr.SetActive(name); err != nil {
		h.t.Fatalf("SetActive(%q): %v", name, err)
	}
}

func runRenameInner(t *testing.T, args ...string) (stdout, stderr string, err error) {
	t.Helper()
	var out, errBuf bytes.Buffer
	cmd := &cobra.Command{Use: "rename"}
	cmd.SetOut(&out)
	cmd.SetErr(&errBuf)
	err = runRename(cmd, args)
	return out.String(), errBuf.String(), err
}

func runDeleteInner(t *testing.T, args ...string) (stdout, stderr string, err error) {
	t.Helper()
	var out, errBuf bytes.Buffer
	cmd := &cobra.Command{Use: "delete"}
	cmd.SetOut(&out)
	cmd.SetErr(&errBuf)
	err = runDelete(cmd, args)
	return out.String(), errBuf.String(), err
}

// ---------------------------------------------------------------------------
// rename tests
// ---------------------------------------------------------------------------

func TestRename_Happy(t *testing.T) {
	h := newRenameDeleteHarness(t)
	h.seed("old")

	stdout, _, err := runRenameInner(t, "old", "new")
	if err != nil {
		t.Fatalf("runRename err=%v", err)
	}
	if !strings.Contains(stdout, `Renamed "old" -> "new"`) {
		t.Errorf("stdout missing renamed line:\n%s", stdout)
	}
	if _, err := h.store.LoadProfile("new"); err != nil {
		t.Errorf("LoadProfile(new): %v", err)
	}
	if ok, _ := h.store.ProfileExists("old"); ok {
		t.Errorf("old profile file still exists")
	}
	loaded, _ := h.store.LoadProfile("new")
	if loaded.Name != "new" {
		t.Errorf("Name=%q; want new", loaded.Name)
	}
	fixed := time.Date(2026, 7, 2, 12, 0, 0, 0, time.UTC)
	if !loaded.UpdatedAt.Equal(fixed) {
		t.Errorf("UpdatedAt=%v; want %v", loaded.UpdatedAt, fixed)
	}
}

func TestRename_InvalidNewNameRefused(t *testing.T) {
	h := newRenameDeleteHarness(t)
	h.seed("old")

	_, _, err := runRenameInner(t, "old", "INVALID CAPS")
	if err == nil {
		t.Fatal("expected error for invalid new name")
	}
	// old still exists.
	if ok, _ := h.store.ProfileExists("old"); !ok {
		t.Error("old profile lost on invalid-new-name refusal")
	}
}

func TestRename_MissingSourceErrors(t *testing.T) {
	newRenameDeleteHarness(t)
	_, _, err := runRenameInner(t, "nope", "new")
	if err == nil || !strings.Contains(err.Error(), "not found") {
		t.Errorf("expected not-found error, got %v", err)
	}
}

func TestRename_TargetExistsWithoutOverwrite(t *testing.T) {
	h := newRenameDeleteHarness(t)
	h.seed("a")
	h.seed("b")

	_, _, err := runRenameInner(t, "a", "b")
	if err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Errorf("expected already-exists error, got %v", err)
	}
	// Both still there.
	if ok, _ := h.store.ProfileExists("a"); !ok {
		t.Error("source lost on target-exists refusal")
	}
	if ok, _ := h.store.ProfileExists("b"); !ok {
		t.Error("target lost on target-exists refusal")
	}
}

func TestRename_TargetExistsWithOverwrite(t *testing.T) {
	h := newRenameDeleteHarness(t)
	h.seed("a")
	h.seed("b")

	renameOverwriteFlag = true
	if _, _, err := runRenameInner(t, "a", "b"); err != nil {
		t.Fatalf("runRename --overwrite err=%v", err)
	}
	if ok, _ := h.store.ProfileExists("a"); ok {
		t.Error("source file survived rename")
	}
	loaded, err := h.store.LoadProfile("b")
	if err != nil {
		t.Fatalf("LoadProfile(b) after overwrite: %v", err)
	}
	// b should now carry a's api_key seed value.
	if !strings.Contains(loaded.Core.APIKey, "sk-seed-a-") {
		t.Errorf("Core.APIKey=%q; want a's seed", loaded.Core.APIKey)
	}
}

func TestRename_DryRun(t *testing.T) {
	h := newRenameDeleteHarness(t)
	h.seed("a")

	renameDryRunFlag = true
	stdout, _, err := runRenameInner(t, "a", "b")
	if err != nil {
		t.Fatalf("runRename dry-run err=%v", err)
	}
	if !strings.Contains(stdout, "would rename") {
		t.Errorf("stdout missing 'would rename':\n%s", stdout)
	}
	// Nothing changed on disk.
	if ok, _ := h.store.ProfileExists("a"); !ok {
		t.Error("dry-run removed source")
	}
	if ok, _ := h.store.ProfileExists("b"); ok {
		t.Error("dry-run created target")
	}
}

func TestRename_UpdatesActivePointer(t *testing.T) {
	h := newRenameDeleteHarness(t)
	h.seed("a")
	h.activate("a")

	if _, _, err := runRenameInner(t, "a", "b"); err != nil {
		t.Fatalf("runRename err=%v", err)
	}
	state, _ := h.store.LoadState()
	if state.CurrentProfile != "b" {
		t.Errorf("state.CurrentProfile=%q; want b", state.CurrentProfile)
	}
}

func TestRename_NoOpSameNameRejected(t *testing.T) {
	h := newRenameDeleteHarness(t)
	h.seed("a")
	_, _, err := runRenameInner(t, "a", "a")
	if err == nil || !strings.Contains(err.Error(), "identical") {
		t.Errorf("expected identical-name error, got %v", err)
	}
}

// ---------------------------------------------------------------------------
// delete tests
// ---------------------------------------------------------------------------

func TestDelete_Happy(t *testing.T) {
	h := newRenameDeleteHarness(t)
	h.seed("gone")

	deleteYesFlag = true
	stdout, _, err := runDeleteInner(t, "gone")
	if err != nil {
		t.Fatalf("runDelete err=%v", err)
	}
	if !strings.Contains(stdout, `Deleted "gone"`) {
		t.Errorf("stdout missing deleted line:\n%s", stdout)
	}
	if ok, _ := h.store.ProfileExists("gone"); ok {
		t.Error("profile file still on disk after delete")
	}
}

func TestDelete_MissingProfileError(t *testing.T) {
	newRenameDeleteHarness(t)
	deleteYesFlag = true
	_, _, err := runDeleteInner(t, "nope")
	if err == nil || !strings.Contains(err.Error(), "not found") {
		t.Errorf("expected not-found error, got %v", err)
	}
}

func TestDelete_YesSkipsPrompt(t *testing.T) {
	h := newRenameDeleteHarness(t)
	h.seed("skip-prompt")

	// TTY on so the prompt WOULD normally fire; --yes bypasses it.
	restoreTTY := SetIsTerminalForTest(func(*os.File) bool { return true })
	t.Cleanup(restoreTTY)

	deleteYesFlag = true
	if _, _, err := runDeleteInner(t, "skip-prompt"); err != nil {
		t.Fatalf("runDelete --yes err=%v", err)
	}
	if ok, _ := h.store.ProfileExists("skip-prompt"); ok {
		t.Error("profile survived --yes delete")
	}
}

func TestDelete_ClearsActivePointerIfActive(t *testing.T) {
	h := newRenameDeleteHarness(t)
	h.seed("active")
	h.activate("active")

	deleteYesFlag = true
	stdout, _, err := runDeleteInner(t, "active")
	if err != nil {
		t.Fatalf("runDelete err=%v", err)
	}
	if !strings.Contains(stdout, "active profile pointer cleared") {
		t.Errorf("stdout missing pointer-cleared note:\n%s", stdout)
	}
	state, _ := h.store.LoadState()
	if state.CurrentProfile != "" {
		t.Errorf("state.CurrentProfile=%q; want empty", state.CurrentProfile)
	}
}

func TestDelete_NonActiveKeepsPointer(t *testing.T) {
	h := newRenameDeleteHarness(t)
	h.seed("keep")
	h.seed("other")
	h.activate("keep")

	deleteYesFlag = true
	if _, _, err := runDeleteInner(t, "other"); err != nil {
		t.Fatalf("runDelete err=%v", err)
	}
	state, _ := h.store.LoadState()
	if state.CurrentProfile != "keep" {
		t.Errorf("state.CurrentProfile=%q; want keep", state.CurrentProfile)
	}
}

func TestDelete_DryRun(t *testing.T) {
	h := newRenameDeleteHarness(t)
	h.seed("preview")

	deleteDryRunFlag = true
	stdout, _, err := runDeleteInner(t, "preview")
	if err != nil {
		t.Fatalf("runDelete --dry-run err=%v", err)
	}
	if !strings.Contains(stdout, "would delete") {
		t.Errorf("stdout missing 'would delete':\n%s", stdout)
	}
	if ok, _ := h.store.ProfileExists("preview"); !ok {
		t.Error("dry-run removed profile")
	}
}

func TestDelete_DryRunActiveNote(t *testing.T) {
	h := newRenameDeleteHarness(t)
	h.seed("preview")
	h.activate("preview")

	deleteDryRunFlag = true
	stdout, _, err := runDeleteInner(t, "preview")
	if err != nil {
		t.Fatalf("runDelete --dry-run err=%v", err)
	}
	if !strings.Contains(stdout, "pointer would be cleared") {
		t.Errorf("stdout missing pointer-would-be-cleared note:\n%s", stdout)
	}
}

func TestDelete_NonInteractiveWithoutYesRefused(t *testing.T) {
	h := newRenameDeleteHarness(t)
	h.seed("noconfirm")

	// TTY off, --yes off → refuse.
	_, _, err := runDeleteInner(t, "noconfirm")
	if err == nil || !strings.Contains(err.Error(), "non-interactive") {
		t.Errorf("expected non-interactive refusal, got %v", err)
	}
	if ok, _ := h.store.ProfileExists("noconfirm"); !ok {
		t.Error("profile deleted despite refusal")
	}
}

func TestDelete_InteractiveConfirmYes(t *testing.T) {
	h := newRenameDeleteHarness(t)
	h.seed("confirm-yes")

	restoreTTY := SetIsTerminalForTest(func(*os.File) bool { return true })
	t.Cleanup(restoreTTY)

	// Feed "y\n" via stdin redirect: promptConfirm reads from os.Stdin.
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("Pipe: %v", err)
	}
	orig := os.Stdin
	os.Stdin = r
	t.Cleanup(func() { os.Stdin = orig })
	if _, err := w.WriteString("y\n"); err != nil {
		t.Fatalf("write pipe: %v", err)
	}
	_ = w.Close()

	if _, _, err := runDeleteInner(t, "confirm-yes"); err != nil {
		t.Fatalf("runDelete confirm=y err=%v", err)
	}
	if ok, _ := h.store.ProfileExists("confirm-yes"); ok {
		t.Error("profile survived interactive-yes delete")
	}
}

func TestDelete_InteractiveConfirmNo(t *testing.T) {
	h := newRenameDeleteHarness(t)
	h.seed("confirm-no")

	restoreTTY := SetIsTerminalForTest(func(*os.File) bool { return true })
	t.Cleanup(restoreTTY)

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("Pipe: %v", err)
	}
	orig := os.Stdin
	os.Stdin = r
	t.Cleanup(func() { os.Stdin = orig })
	if _, err := w.WriteString("n\n"); err != nil {
		t.Fatalf("write pipe: %v", err)
	}
	_ = w.Close()

	_, _, err = runDeleteInner(t, "confirm-no")
	if err == nil || !strings.Contains(err.Error(), "aborted") {
		t.Errorf("expected abort error, got %v", err)
	}
	if ok, _ := h.store.ProfileExists("confirm-no"); !ok {
		t.Error("profile deleted despite user-no")
	}
}

func TestDelete_EmptyName(t *testing.T) {
	newRenameDeleteHarness(t)
	deleteYesFlag = true
	_, _, err := runDeleteInner(t, "")
	if err == nil || !strings.Contains(err.Error(), "empty") {
		t.Errorf("expected empty-name error, got %v", err)
	}
}

// TestRename_EmptyOldName: empty old positional → error before I/O.
func TestRename_EmptyOldName(t *testing.T) {
	newRenameDeleteHarness(t)
	_, _, err := runRenameInner(t, "", "new")
	if err == nil || !strings.Contains(err.Error(), "empty") {
		t.Errorf("expected empty-name error, got %v", err)
	}
}

// TestRename_DryRunWithOverwrite: --dry-run over an existing target
// annotates that overwrite would replace it, still no writes.
func TestRename_DryRunWithOverwrite(t *testing.T) {
	h := newRenameDeleteHarness(t)
	h.seed("a")
	h.seed("b")

	renameDryRunFlag = true
	renameOverwriteFlag = true
	stdout, _, err := runRenameInner(t, "a", "b")
	if err != nil {
		t.Fatalf("runRename dry-run+overwrite err=%v", err)
	}
	if !strings.Contains(stdout, "target exists") {
		t.Errorf("stdout missing 'target exists' note:\n%s", stdout)
	}
	// Neither file changed.
	if ok, _ := h.store.ProfileExists("a"); !ok {
		t.Error("dry-run mutated source")
	}
	if ok, _ := h.store.ProfileExists("b"); !ok {
		t.Error("dry-run removed target")
	}
}

// TestRename_CompletionOnlyFirstArg: the ValidArgsFunction for rename
// returns the profile-name completion only for the first argument.
func TestRename_CompletionOnlyFirstArg(t *testing.T) {
	newRenameDeleteHarness(t)
	// Second-arg completion returns nil directive-NoFileComp.
	names, dir := renameCmd.ValidArgsFunction(renameCmd, []string{"a"}, "")
	if names != nil {
		t.Errorf("second-arg completion returned %v; want nil", names)
	}
	if dir != cobra.ShellCompDirectiveNoFileComp {
		t.Errorf("second-arg directive = %v; want NoFileComp", dir)
	}
	// First-arg completion delegates to profileNamesCompletion, which
	// on a fresh harness returns nil (no profiles).
	names, _ = renameCmd.ValidArgsFunction(renameCmd, nil, "")
	if len(names) != 0 {
		t.Errorf("first-arg completion on empty tree returned %v", names)
	}
}
