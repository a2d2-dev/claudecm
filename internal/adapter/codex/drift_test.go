package codex_test

// drift_test.go — E5-S4 tests for the Codex adapter's external
// drift-detection surface plus the Apply-side state update.
//
// Codex owns TWO files (auth.json + config.toml) and each is tracked
// independently in State.LastAppliedPerTool[codex][<file>]. The tests
// cover:
//
//   - No prior state → no drift (AC edge case).
//   - Both files match → no drift.
//   - Only one file drifted → ExternalDriftFiles contains only that
//     file, in auth-first order (matches Files() ordering).
//   - Apply updates state for the file it wrote (auth.json and
//     config.toml recorded under separate inner-map entries).
//
// Every test runs against a Bootstrap'd resolver rooted at a per-test
// HOME so state.yaml I/O is isolated.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"testing"
	"time"

	"github.com/a2d2-dev/claudecm/internal/adapter"
	"github.com/a2d2-dev/claudecm/internal/adapter/codex"
	"github.com/a2d2-dev/claudecm/internal/config"
	"github.com/a2d2-dev/claudecm/internal/storage"
)

func hashHex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func mustSaveState(t *testing.T, r *storage.Resolver, s *config.State) {
	t.Helper()
	if err := storage.NewFileStorage(r).SaveState(s); err != nil {
		t.Fatalf("SaveState: %v", err)
	}
}

func mustLoadState(t *testing.T, r *storage.Resolver) *config.State {
	t.Helper()
	s, err := storage.NewFileStorage(r).LoadState()
	if err != nil {
		t.Fatalf("LoadState: %v", err)
	}
	return s
}

// TestProject_NoLastAppliedNoDrift: E5-S4 AC edge case. No prior
// applied state → Project must NOT report drift on either file, even
// when both files exist with arbitrary bytes.
func TestProject_NoLastAppliedNoDrift(t *testing.T) {
	clearCodexEnv(t)
	r := bootstrappedResolver(t)
	a := codex.New()

	writeConfigTOML(t, r, `model = "opus"`+"\n")
	writeAuthJSON(t, r, `{"OPENAI_API_KEY":"sk-1"}`)

	view, err := a.Project(context.Background(), r, config.Profile{})
	if err != nil {
		t.Fatalf("Project: %v", err)
	}
	if view.ExternalDriftDetected {
		t.Errorf("ExternalDriftDetected = true, want false (no prior state)")
	}
	if len(view.ExternalDriftFiles) != 0 {
		t.Errorf("ExternalDriftFiles = %v, want empty", view.ExternalDriftFiles)
	}
}

// TestProject_LastAppliedMatchesNoDrift: state records the current
// SHA256 for both files → no drift.
func TestProject_LastAppliedMatchesNoDrift(t *testing.T) {
	clearCodexEnv(t)
	r := bootstrappedResolver(t)
	a := codex.New()

	tomlBody := `model = "opus"` + "\n"
	authBody := `{"OPENAI_API_KEY":"sk-1"}`
	writeConfigTOML(t, r, tomlBody)
	writeAuthJSON(t, r, authBody)

	s := config.NewState()
	s.RecordApplied(adapter.ToolCodex, codex.ConfigPath(r), hashHex([]byte(tomlBody)), time.Now())
	s.RecordApplied(adapter.ToolCodex, codex.AuthPath(r), hashHex([]byte(authBody)), time.Now())
	mustSaveState(t, r, s)

	view, err := a.Project(context.Background(), r, config.Profile{})
	if err != nil {
		t.Fatalf("Project: %v", err)
	}
	if view.ExternalDriftDetected {
		t.Errorf("ExternalDriftDetected = true, want false (SHAs match)")
	}
	if len(view.ExternalDriftFiles) != 0 {
		t.Errorf("ExternalDriftFiles = %v, want empty", view.ExternalDriftFiles)
	}
}

// TestProject_LastAppliedMismatchesReportsDrift: both files drifted →
// ExternalDriftFiles carries both, auth-first.
func TestProject_LastAppliedMismatchesReportsDrift(t *testing.T) {
	clearCodexEnv(t)
	r := bootstrappedResolver(t)
	a := codex.New()

	writeConfigTOML(t, r, `model = "opus"`+"\n")
	writeAuthJSON(t, r, `{"OPENAI_API_KEY":"sk-1"}`)

	// Seed state with STALE SHAs — pretend both files were edited
	// outside claudecm.
	stale := "0000000000000000000000000000000000000000000000000000000000000000"
	s := config.NewState()
	s.RecordApplied(adapter.ToolCodex, codex.ConfigPath(r), stale, time.Now())
	s.RecordApplied(adapter.ToolCodex, codex.AuthPath(r), stale, time.Now())
	mustSaveState(t, r, s)

	view, err := a.Project(context.Background(), r, config.Profile{})
	if err != nil {
		t.Fatalf("Project: %v", err)
	}
	if !view.ExternalDriftDetected {
		t.Errorf("ExternalDriftDetected = false, want true (both SHAs mismatch)")
	}
	if len(view.ExternalDriftFiles) != 2 {
		t.Fatalf("ExternalDriftFiles len = %d, want 2; got %v", len(view.ExternalDriftFiles), view.ExternalDriftFiles)
	}
	if view.ExternalDriftFiles[0] != codex.AuthPath(r) {
		t.Errorf("ExternalDriftFiles[0] = %q, want %q (auth-first)", view.ExternalDriftFiles[0], codex.AuthPath(r))
	}
	if view.ExternalDriftFiles[1] != codex.ConfigPath(r) {
		t.Errorf("ExternalDriftFiles[1] = %q, want %q", view.ExternalDriftFiles[1], codex.ConfigPath(r))
	}
}

// TestProject_DriftOnPartialFilesForCodex: only auth.json drifted;
// config.toml matches → ExternalDriftFiles contains ONLY auth.json.
// This is the two-file-tracking test called out in the E5-S4 test plan.
func TestProject_DriftOnPartialFilesForCodex(t *testing.T) {
	clearCodexEnv(t)
	r := bootstrappedResolver(t)
	a := codex.New()

	tomlBody := `model = "opus"` + "\n"
	authBody := `{"OPENAI_API_KEY":"sk-current"}`
	writeConfigTOML(t, r, tomlBody)
	writeAuthJSON(t, r, authBody)

	// config.toml SHA matches on-disk; auth.json SHA is stale.
	s := config.NewState()
	s.RecordApplied(adapter.ToolCodex, codex.ConfigPath(r), hashHex([]byte(tomlBody)), time.Now())
	s.RecordApplied(adapter.ToolCodex, codex.AuthPath(r), "0000000000000000000000000000000000000000000000000000000000000000", time.Now())
	mustSaveState(t, r, s)

	view, err := a.Project(context.Background(), r, config.Profile{})
	if err != nil {
		t.Fatalf("Project: %v", err)
	}
	if !view.ExternalDriftDetected {
		t.Errorf("ExternalDriftDetected = false, want true (auth.json mismatch)")
	}
	if len(view.ExternalDriftFiles) != 1 || view.ExternalDriftFiles[0] != codex.AuthPath(r) {
		t.Errorf("ExternalDriftFiles = %v, want [%q]", view.ExternalDriftFiles, codex.AuthPath(r))
	}
}

// TestProject_LastAppliedButFileAbsentSuppressesDrift: state records a
// SHA for a file that has since been deleted from disk. Drift check
// must NOT fire — an absent file is not a drift event (the user may
// have uninstalled the tool, or reset from a backup).
func TestProject_LastAppliedButFileAbsentSuppressesDrift(t *testing.T) {
	clearCodexEnv(t)
	r := bootstrappedResolver(t)
	a := codex.New()

	// Only config.toml present; auth.json absent.
	writeConfigTOML(t, r, `model = "opus"`+"\n")

	s := config.NewState()
	s.RecordApplied(adapter.ToolCodex, codex.AuthPath(r), "aaa", time.Now())
	mustSaveState(t, r, s)

	view, err := a.Project(context.Background(), r, config.Profile{})
	if err != nil {
		t.Fatalf("Project: %v", err)
	}
	if view.ExternalDriftDetected {
		t.Errorf("ExternalDriftDetected = true, want false (file absent, not drifted)")
	}
}

// TestApply_UpdatesStateOnSuccess verifies the write-side integration:
// Apply on auth.json records state under (codex, authPath) with the
// matching SHA256; Apply on config.toml records (codex, configPath)
// with its SHA256. Both entries survive because the inner map is keyed
// by file path.
func TestApply_UpdatesStateOnSuccess(t *testing.T) {
	clearCodexEnv(t)
	r := bootstrappedResolver(t)
	a := codex.New()

	profile := config.Profile{
		SchemaVersion: config.CurrentProfileSchemaVersion,
		Name:          "t",
		Core:          config.CoreConfig{APIKey: "sk-1"},
		Tools: map[config.ToolID]config.ToolOverlay{
			adapter.ToolCodex: {Raw: map[string]any{"model": "opus"}},
		},
	}
	plans, err := a.Plan(context.Background(), r, profile)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if len(plans) == 0 {
		t.Fatalf("Plan returned zero plans")
	}

	before := time.Now().Add(-time.Second)
	for _, p := range plans {
		if _, err := a.Apply(context.Background(), r, p); err != nil {
			t.Fatalf("Apply %q: %v", p.Target, err)
		}
	}
	after := time.Now().Add(time.Second)

	state := mustLoadState(t, r)
	// Each plan.Target should now have a LastApplied entry whose
	// SHA256 matches the actual bytes on disk.
	for _, p := range plans {
		onDisk, err := os.ReadFile(p.Target)
		if err != nil {
			t.Fatalf("read %q: %v", p.Target, err)
		}
		entry, ok := state.GetLastApplied(adapter.ToolCodex, p.Target)
		if !ok {
			t.Errorf("state missing LastApplied for (codex, %q)", p.Target)
			continue
		}
		if entry.FilePath != p.Target {
			t.Errorf("FilePath = %q, want %q", entry.FilePath, p.Target)
		}
		if entry.SHA256 != hashHex(onDisk) {
			t.Errorf("SHA256 = %q, want %q (on-disk hash)", entry.SHA256, hashHex(onDisk))
		}
		if entry.AppliedAt.Before(before) || entry.AppliedAt.After(after) {
			t.Errorf("AppliedAt = %v, want within [%v, %v]", entry.AppliedAt, before, after)
		}
	}
}

// TestApply_ThenExternalEditReportsDriftOnAuthOnly: end-to-end signal.
// A full Apply seeds state for both files, then we externally edit
// auth.json only. Project must flag drift on auth.json only.
func TestApply_ThenExternalEditReportsDriftOnAuthOnly(t *testing.T) {
	clearCodexEnv(t)
	r := bootstrappedResolver(t)
	a := codex.New()

	profile := config.Profile{
		SchemaVersion: config.CurrentProfileSchemaVersion,
		Name:          "t",
		Core:          config.CoreConfig{APIKey: "sk-1"},
		Tools: map[config.ToolID]config.ToolOverlay{
			adapter.ToolCodex: {Raw: map[string]any{"model": "opus"}},
		},
	}
	plans, err := a.Plan(context.Background(), r, profile)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	for _, p := range plans {
		if _, err := a.Apply(context.Background(), r, p); err != nil {
			t.Fatalf("Apply %q: %v", p.Target, err)
		}
	}

	// External edit on auth.json only.
	authPath := codex.AuthPath(r)
	if err := os.WriteFile(authPath, []byte(`{"OPENAI_API_KEY":"hand-edited"}`), 0o600); err != nil {
		t.Fatalf("external auth edit: %v", err)
	}

	view, err := a.Project(context.Background(), r, profile)
	if err != nil {
		t.Fatalf("Project: %v", err)
	}
	if !view.ExternalDriftDetected {
		t.Fatalf("ExternalDriftDetected = false after auth.json edit, want true")
	}
	if len(view.ExternalDriftFiles) != 1 || view.ExternalDriftFiles[0] != authPath {
		t.Errorf("ExternalDriftFiles = %v, want [%q]", view.ExternalDriftFiles, authPath)
	}
}
