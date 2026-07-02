//go:build test

// concurrent_edit_test.go — Story E8-S4. Per-owned-file simulation of
// an external mutation firing between writepath's step-3 read (which
// captured PreFingerprint) and the step-9 drift-check Stat.
//
// Mechanism. Uses writepath.SetPostReadHookForTest, the existing seam
// that fires immediately before the drift-check Stat. The hook writes
// distinctive bytes over the target so the fresh Stat sees a different
// SHA256 (or a fresh appears/vanishes signal on the first-write /
// file-vanished paths). Apply then wraps writepath.ErrConcurrentEdit
// and aborts BEFORE the atomic publish.
//
// Assertions per file (auth.json, config.toml, settings.json):
//
//   - writepath.Apply (invoked via the adapter) returns an error that
//     errors.Is against writepath.ErrConcurrentEdit.
//   - The pre-mutation backup file exists on disk under
//     ~/.claudecm/backups/<tool>/. The retention/audit path stays
//     honest: even a drift-aborted Apply leaves the operator-visible
//     backup record.
//
// Additionally TestE2E_FirstWriteConcurrentEdit exercises the
// step-2-said-not-exists + step-9-Stat-said-exists arm: no fixture on
// disk pre-Apply; the hook plants a competing byte stream just before
// the drift check. The abort surfaces the same sentinel and no backup
// exists (nothing to snapshot).
//
// The tests do NOT drive the CLI wrapper — they test the underlying
// Apply contract. cmd/switch's own tests (cmd/switch_test.go) already
// pin the *commit.PartialFailure → exit code 2 mapping.

package e2e

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/a2d2-dev/claudecm/internal/adapter"
	"github.com/a2d2-dev/claudecm/internal/adapter/claudecode"
	"github.com/a2d2-dev/claudecm/internal/adapter/codex"
	"github.com/a2d2-dev/claudecm/internal/config"
	"github.com/a2d2-dev/claudecm/internal/writepath"
)

// TestE2E_ConcurrentEditClaudeSettings exercises the drift check for
// ~/.claude/settings.json. Seed the file with a valid but small JSON
// body, install a hook that mutates the file just before the drift
// check, run apply on the claude_code plan, and assert:
//   - err wraps ErrConcurrentEdit,
//   - backup file exists under ~/.claudecm/backups/claude_code/.
func TestE2E_ConcurrentEditClaudeSettings(t *testing.T) {
	h := newHarness(t)
	profile := seedConcurrentEditProfile(t, h, "ce-claude")
	seedFile(t, h.claudeSettings(), []byte(`{"env":{"ANTHROPIC_MODEL":"seed-model"}}`))

	// Plan → get the settings.json plan.
	claudeAdapter, ok := adapter.DefaultRegistry.Get(adapter.ToolClaudeCode)
	if !ok {
		t.Fatal("claude_code adapter missing from DefaultRegistry")
	}
	plans, err := claudeAdapter.Plan(context.Background(), h.resv, *profile)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	plan := selectPlanForTarget(t, plans, h.claudeSettings())

	// Install hook: overwrite settings.json with different bytes.
	restore := writepath.SetPostReadHookForTest(func() {
		writeRawFile(t, h.claudeSettings(), []byte(`{"env":{"ANTHROPIC_MODEL":"drifted-model"}}`))
	})
	defer restore()

	_, applyErr := claudeAdapter.Apply(context.Background(), h.resv, plan)
	if applyErr == nil {
		t.Fatalf("Apply: expected ErrConcurrentEdit, got nil")
	}
	if !errors.Is(applyErr, writepath.ErrConcurrentEdit) {
		t.Fatalf("Apply err = %v, want ErrConcurrentEdit", applyErr)
	}
	assertBackupExists(t, h, "claude_code", "settings.json")
}

// TestE2E_ConcurrentEditCodexConfig exercises the drift check for
// ~/.codex/config.toml.
func TestE2E_ConcurrentEditCodexConfig(t *testing.T) {
	h := newHarness(t)
	profile := seedConcurrentEditProfileForCodex(t, h, "ce-codex-config")
	seedFile(t, h.codexAuth(), []byte(`{"OPENAI_API_KEY":"sk-seed"}`))
	seedFile(t, h.codexConfig(), []byte("model = \"seed-model\"\nmodel_provider = \"openai\"\n"))

	codexAdapter, ok := adapter.DefaultRegistry.Get(adapter.ToolCodex)
	if !ok {
		t.Fatal("codex adapter missing from DefaultRegistry")
	}
	plans, err := codexAdapter.Plan(context.Background(), h.resv, *profile)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	plan := selectPlanForTarget(t, plans, h.codexConfig())

	restore := writepath.SetPostReadHookForTest(func() {
		writeRawFile(t, h.codexConfig(), []byte("model = \"drifted-model\"\nmodel_provider = \"openai\"\n"))
	})
	defer restore()

	_, applyErr := codexAdapter.Apply(context.Background(), h.resv, plan)
	if applyErr == nil {
		t.Fatalf("Apply: expected ErrConcurrentEdit, got nil")
	}
	if !errors.Is(applyErr, writepath.ErrConcurrentEdit) {
		t.Fatalf("Apply err = %v, want ErrConcurrentEdit", applyErr)
	}
	assertBackupExists(t, h, "codex", "config.toml")
}

// TestE2E_ConcurrentEditCodexAuth exercises the drift check for
// ~/.codex/auth.json.
func TestE2E_ConcurrentEditCodexAuth(t *testing.T) {
	h := newHarness(t)
	profile := seedConcurrentEditProfileForCodex(t, h, "ce-codex-auth")
	seedFile(t, h.codexAuth(), []byte(`{"OPENAI_API_KEY":"sk-seed"}`))
	seedFile(t, h.codexConfig(), []byte("model = \"seed-model\"\nmodel_provider = \"openai\"\n"))

	codexAdapter, _ := adapter.DefaultRegistry.Get(adapter.ToolCodex)
	plans, err := codexAdapter.Plan(context.Background(), h.resv, *profile)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	plan := selectPlanForTarget(t, plans, h.codexAuth())

	restore := writepath.SetPostReadHookForTest(func() {
		writeRawFile(t, h.codexAuth(), []byte(`{"OPENAI_API_KEY":"sk-drifted"}`))
	})
	defer restore()

	_, applyErr := codexAdapter.Apply(context.Background(), h.resv, plan)
	if applyErr == nil {
		t.Fatalf("Apply: expected ErrConcurrentEdit, got nil")
	}
	if !errors.Is(applyErr, writepath.ErrConcurrentEdit) {
		t.Fatalf("Apply err = %v, want ErrConcurrentEdit", applyErr)
	}
	assertBackupExists(t, h, "codex", "auth.json")
}

// TestE2E_FirstWriteConcurrentEdit covers the "file appears between
// read and write" arm of the drift check. Pre-Apply the target does
// not exist; the hook creates it just before the drift check. Apply
// must refuse with ErrConcurrentEdit (see writepath/apply.go step 9
// second bullet). No backup file exists — nothing was there to
// snapshot at Stage time — so the assertion is on the error only.
func TestE2E_FirstWriteConcurrentEdit(t *testing.T) {
	h := newHarness(t)
	profile := seedConcurrentEditProfile(t, h, "ce-firstwrite")

	claudeAdapter, _ := adapter.DefaultRegistry.Get(adapter.ToolClaudeCode)
	plans, err := claudeAdapter.Plan(context.Background(), h.resv, *profile)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	plan := selectPlanForTarget(t, plans, h.claudeSettings())

	// Pre-condition: settings.json does not exist.
	if _, err := os.Stat(h.claudeSettings()); !os.IsNotExist(err) {
		t.Fatalf("pre-condition: settings.json unexpectedly exists (err=%v)", err)
	}

	// Hook plants the file just before Apply's step-9 Stat so drift
	// detection sees prevExists=false && curExists=true.
	restore := writepath.SetPostReadHookForTest(func() {
		writeRawFile(t, h.claudeSettings(), []byte(`{"env":{"ANTHROPIC_MODEL":"appeared"}}`))
	})
	defer restore()

	_, applyErr := claudeAdapter.Apply(context.Background(), h.resv, plan)
	if applyErr == nil {
		t.Fatalf("Apply: expected ErrConcurrentEdit, got nil")
	}
	if !errors.Is(applyErr, writepath.ErrConcurrentEdit) {
		t.Fatalf("Apply err = %v, want ErrConcurrentEdit", applyErr)
	}
	// No backup expected on the first-write path — writepath.Backup
	// returned ErrNothingToBackup at Stage time and the drift branch
	// left the zero-valued BackupRecord in the WriteReport.
	if hasBackup(t, h, "claude_code", "settings.json") {
		t.Errorf("first-write drift should not produce a backup file")
	}
}

// -----------------------------------------------------------------------------
// Helpers
// -----------------------------------------------------------------------------

// seedConcurrentEditProfile creates a Claude Code-only profile with
// non-trivial owned values. The concurrent-edit tests only need one
// tool active per test; a claude_code-only profile leaves the codex
// adapter's Plan a no-op which is fine.
func seedConcurrentEditProfile(t *testing.T, h *harness, name string) *config.Profile {
	t.Helper()
	p := config.NewProfile(name, "https://api.anthropic.com", "sk-longsecret-testing")
	p.Core.Model = "claude-opus-4-5"
	if err := h.mgr.AddProfile(p); err != nil {
		t.Fatalf("AddProfile(%q): %v", name, err)
	}
	return p
}

// seedConcurrentEditProfileForCodex additionally stamps a codex
// overlay so the codex adapter's Plan produces non-trivial owned
// bytes.
func seedConcurrentEditProfileForCodex(t *testing.T, h *harness, name string) *config.Profile {
	t.Helper()
	p := seedConcurrentEditProfile(t, h, name)
	if p.Tools == nil {
		p.Tools = map[config.ToolID]config.ToolOverlay{}
	}
	ov := p.Tools[config.ToolCodex]
	if ov.Raw == nil {
		ov.Raw = map[string]any{}
	}
	ov.Raw["model"] = "gpt-fresh"
	ov.Raw["model_provider"] = "openai"
	ov.Raw["model_providers.openai.name"] = "OpenAI"
	ov.Raw["model_providers.openai.base_url"] = "https://relay.example.com"
	ov.Raw["model_providers.openai.env_key"] = "OPENAI_API_KEY"
	ov.Raw["model_providers.openai.wire_api"] = "responses"
	p.Tools[config.ToolCodex] = ov
	if err := h.mgr.UpdateProfile(name, p); err != nil {
		t.Fatalf("UpdateProfile(%q): %v", name, err)
	}
	return p
}

// selectPlanForTarget returns the plan in the slice whose Target ==
// want. Fails the test if none match — the caller has a bad
// expectation about the adapter's plan set.
func selectPlanForTarget(t *testing.T, plans []writepath.WritePlan, want string) writepath.WritePlan {
	t.Helper()
	for _, p := range plans {
		if p.Target == want {
			return p
		}
	}
	t.Fatalf("no plan for target %q in %d plans", want, len(plans))
	return writepath.WritePlan{}
}

// writeRawFile writes bytes to path at 0600. Used by the drift hooks —
// they cannot go through storage.AtomicWrite because we are already
// inside a writepath.Apply flock.
func writeRawFile(t *testing.T, path string, body []byte) {
	t.Helper()
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatalf("hook write %q: %v", path, err)
	}
}

// assertBackupExists walks ~/.claudecm/backups/<tool>/ and asserts at
// least one file with the given basename prefix exists. Backup names
// are `<basename>.<timestamp>` per storage.Backup — a prefix match on
// the basename is stable across clock jitter.
func assertBackupExists(t *testing.T, h *harness, tool, basename string) {
	t.Helper()
	if !hasBackup(t, h, tool, basename) {
		t.Errorf("expected backup for %s/%s to exist under %s", tool, basename, backupDir(h, tool))
	}
}

// hasBackup returns true when a backup file with the given basename
// prefix exists under ~/.claudecm/backups/<tool>/.
func hasBackup(t *testing.T, h *harness, tool, basename string) bool {
	t.Helper()
	dir := backupDir(h, tool)
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return false
		}
		t.Fatalf("read backup dir %q: %v", dir, err)
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if strings.HasPrefix(e.Name(), basename+".") {
			return true
		}
	}
	return false
}

// backupDir returns the on-disk path where storage.Backup writes
// backups for the given tool.
func backupDir(h *harness, tool string) string {
	return filepath.Join(h.home, ".claudecm", "backups", tool)
}

// unused imports guard — the two adapter packages are referenced only
// to keep the plan path deterministic; they can drop as _-imports if
// the direct references above disappear during a refactor.
var (
	_ = claudecode.SettingsPath
	_ = codex.AuthPath
)
