package claudecode_test

// drift_test.go — E5-S4 tests for the Claude Code adapter's external
// drift-detection surface plus the Apply-side state update.
//
// The tests exercise the read path (Project reports drift when state
// records a SHA that no longer matches the on-disk file) and the write
// path (Apply persists the (path, SHA256, appliedAt) tuple into
// state.yaml on success). Every test runs against a Bootstrap'd
// resolver rooted at a per-test HOME so state.yaml I/O is isolated.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"testing"
	"time"

	"github.com/a2d2-dev/claudecm/internal/adapter"
	"github.com/a2d2-dev/claudecm/internal/adapter/claudecode"
	"github.com/a2d2-dev/claudecm/internal/config"
	"github.com/a2d2-dev/claudecm/internal/storage"
	"github.com/a2d2-dev/claudecm/internal/writepath"
)

// hashHex returns the lowercase hex SHA-256 of data. Duplicated from
// the adapter's internal helper on purpose — the tests must not depend
// on the exact same implementation, only on the algorithm.
func hashHex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// mustSaveState writes state via the shared FileStorage so the test
// exercises the same code path Apply uses.
func mustSaveState(t *testing.T, r *storage.Resolver, s *config.State) {
	t.Helper()
	if err := storage.NewFileStorage(r).SaveState(s); err != nil {
		t.Fatalf("SaveState: %v", err)
	}
}

// mustLoadState reads state via the shared FileStorage.
func mustLoadState(t *testing.T, r *storage.Resolver) *config.State {
	t.Helper()
	s, err := storage.NewFileStorage(r).LoadState()
	if err != nil {
		t.Fatalf("LoadState: %v", err)
	}
	return s
}

// TestProject_NoLastAppliedNoDrift exercises the E5-S4 AC edge case:
// no prior applied state → Project must NOT report drift, even when
// the on-disk file exists with arbitrary bytes.
func TestProject_NoLastAppliedNoDrift(t *testing.T) {
	clearClaudeEnv(t)
	r := bootstrappedResolver(t)
	a := claudecode.New()

	writeSettings(t, r, `{"env":{"ANTHROPIC_MODEL":"opus"}}`)

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
// on-disk SHA256 → no drift.
func TestProject_LastAppliedMatchesNoDrift(t *testing.T) {
	clearClaudeEnv(t)
	r := bootstrappedResolver(t)
	a := claudecode.New()

	body := `{"env":{"ANTHROPIC_MODEL":"opus"}}`
	writeSettings(t, r, body)

	// Seed state with the SHA256 of the exact bytes on disk.
	s := config.NewState()
	s.RecordApplied(adapter.ToolClaudeCode, claudecode.SettingsPath(r), hashHex([]byte(body)), time.Now())
	mustSaveState(t, r, s)

	view, err := a.Project(context.Background(), r, config.Profile{})
	if err != nil {
		t.Fatalf("Project: %v", err)
	}
	if view.ExternalDriftDetected {
		t.Errorf("ExternalDriftDetected = true, want false (SHA matches)")
	}
	if len(view.ExternalDriftFiles) != 0 {
		t.Errorf("ExternalDriftFiles = %v, want empty", view.ExternalDriftFiles)
	}
}

// TestProject_LastAppliedMismatchesReportsDrift: state records a
// different SHA256 → drift reported on the owned file path.
func TestProject_LastAppliedMismatchesReportsDrift(t *testing.T) {
	clearClaudeEnv(t)
	r := bootstrappedResolver(t)
	a := claudecode.New()

	writeSettings(t, r, `{"env":{"ANTHROPIC_MODEL":"opus"}}`)

	// Seed state with an OLD SHA — pretend we applied a different body
	// earlier and the user hand-edited settings.json outside claudecm.
	s := config.NewState()
	s.RecordApplied(adapter.ToolClaudeCode, claudecode.SettingsPath(r), "0000000000000000000000000000000000000000000000000000000000000000", time.Now())
	mustSaveState(t, r, s)

	view, err := a.Project(context.Background(), r, config.Profile{})
	if err != nil {
		t.Fatalf("Project: %v", err)
	}
	if !view.ExternalDriftDetected {
		t.Errorf("ExternalDriftDetected = false, want true (SHA mismatch)")
	}
	wantPath := claudecode.SettingsPath(r)
	if len(view.ExternalDriftFiles) != 1 || view.ExternalDriftFiles[0] != wantPath {
		t.Errorf("ExternalDriftFiles = %v, want [%q]", view.ExternalDriftFiles, wantPath)
	}
}

// TestProject_LastAppliedForDifferentFileIgnored: state records an
// entry for a different file path (e.g. an old ~/.codex/... path in a
// state.yaml that survived a HOME move). Project must NOT report drift
// because the recorded FilePath doesn't match the file it's projecting.
func TestProject_LastAppliedForDifferentFileIgnored(t *testing.T) {
	clearClaudeEnv(t)
	r := bootstrappedResolver(t)
	a := claudecode.New()

	writeSettings(t, r, `{"env":{"ANTHROPIC_MODEL":"opus"}}`)

	// The recorded FilePath does NOT match the current settings.json
	// path. GetLastApplied by (tool, path) returns false and drift is
	// suppressed.
	s := config.NewState()
	s.RecordApplied(adapter.ToolClaudeCode, "/somewhere/else/settings.json", "aaa", time.Now())
	mustSaveState(t, r, s)

	view, err := a.Project(context.Background(), r, config.Profile{})
	if err != nil {
		t.Fatalf("Project: %v", err)
	}
	if view.ExternalDriftDetected {
		t.Errorf("ExternalDriftDetected = true, want false (wrong file recorded)")
	}
}

// TestApply_UpdatesStateOnSuccess verifies the write-side integration:
// after Apply, state.LastAppliedPerTool[claude_code][path] carries
// FilePath, SHA256 (matching the on-disk bytes), and an AppliedAt
// within the last second.
func TestApply_UpdatesStateOnSuccess(t *testing.T) {
	clearClaudeEnv(t)
	r := bootstrappedResolver(t)
	a := claudecode.New()

	profile := config.Profile{
		SchemaVersion: config.CurrentProfileSchemaVersion,
		Name:          "t",
		Core: config.CoreConfig{
			BaseURL: "https://api.example.com",
			Model:   "opus",
		},
	}
	plans, err := a.Plan(context.Background(), r, profile)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if len(plans) != 1 {
		t.Fatalf("Plan returned %d plans, want 1", len(plans))
	}

	before := time.Now().Add(-time.Second)
	report, err := a.Apply(context.Background(), r, plans[0])
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	after := time.Now().Add(time.Second)

	settingsPath := claudecode.SettingsPath(r)
	onDisk, err := os.ReadFile(settingsPath)
	if err != nil {
		t.Fatalf("read settings.json: %v", err)
	}
	wantSHA := hashHex(onDisk)

	state := mustLoadState(t, r)
	entry, ok := state.GetLastApplied(adapter.ToolClaudeCode, settingsPath)
	if !ok {
		t.Fatalf("state has no LastApplied entry for claude_code + %q", settingsPath)
	}
	if entry.FilePath != settingsPath {
		t.Errorf("FilePath = %q, want %q", entry.FilePath, settingsPath)
	}
	if entry.SHA256 != wantSHA {
		t.Errorf("SHA256 = %q, want %q", entry.SHA256, wantSHA)
	}
	if entry.SHA256 != report.PostFingerprint.SHA256 {
		t.Errorf("state SHA256 = %q, report PostFingerprint.SHA256 = %q; want equal", entry.SHA256, report.PostFingerprint.SHA256)
	}
	if entry.AppliedAt.Before(before) || entry.AppliedAt.After(after) {
		t.Errorf("AppliedAt = %v, want within [%v, %v]", entry.AppliedAt, before, after)
	}
}

// TestApply_ThenProjectReportsNoDrift is the round-trip anchor: a
// successful Apply immediately followed by Project must NOT report
// drift. It pins the guarantee that Apply's state-write records the
// same SHA the next Project's re-hash of the on-disk file computes.
func TestApply_ThenProjectReportsNoDrift(t *testing.T) {
	clearClaudeEnv(t)
	r := bootstrappedResolver(t)
	a := claudecode.New()

	profile := config.Profile{
		SchemaVersion: config.CurrentProfileSchemaVersion,
		Name:          "t",
		Core:          config.CoreConfig{BaseURL: "https://api.example.com"},
	}
	plans, err := a.Plan(context.Background(), r, profile)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if _, err := a.Apply(context.Background(), r, plans[0]); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	view, err := a.Project(context.Background(), r, profile)
	if err != nil {
		t.Fatalf("Project: %v", err)
	}
	if view.ExternalDriftDetected {
		t.Errorf("ExternalDriftDetected = true after immediate Apply, want false")
	}
}

// TestApply_ThenExternalEditReportsDrift is the E5-S4 happy-path drift
// signal end-to-end. Apply seeds state, we then hand-edit settings.json
// out from under claudecm, and the next Project must flag drift on the
// owned file.
func TestApply_ThenExternalEditReportsDrift(t *testing.T) {
	clearClaudeEnv(t)
	r := bootstrappedResolver(t)
	a := claudecode.New()

	profile := config.Profile{
		SchemaVersion: config.CurrentProfileSchemaVersion,
		Name:          "t",
		Core:          config.CoreConfig{BaseURL: "https://api.example.com"},
	}
	plans, err := a.Plan(context.Background(), r, profile)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if _, err := a.Apply(context.Background(), r, plans[0]); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	// Externally edit settings.json — this simulates the operator
	// running `vim ~/.claude/settings.json` after a switch.
	settingsPath := claudecode.SettingsPath(r)
	if err := os.WriteFile(settingsPath, []byte(`{"env":{"ANTHROPIC_MODEL":"externally-edited"}}`), 0o600); err != nil {
		t.Fatalf("external edit: %v", err)
	}

	view, err := a.Project(context.Background(), r, profile)
	if err != nil {
		t.Fatalf("Project: %v", err)
	}
	if !view.ExternalDriftDetected {
		t.Fatalf("ExternalDriftDetected = false after external edit, want true")
	}
	if len(view.ExternalDriftFiles) != 1 || view.ExternalDriftFiles[0] != settingsPath {
		t.Errorf("ExternalDriftFiles = %v, want [%q]", view.ExternalDriftFiles, settingsPath)
	}
}

// TestApply_DoesNotUpdateStateOnFailure verifies that if writepath.Apply
// returns an error (here: a plan-mismatch on the Tool field), state is
// NOT updated — no LastApplied entry is written, so a subsequent
// Project has no stale SHA to compare against.
func TestApply_DoesNotUpdateStateOnFailure(t *testing.T) {
	clearClaudeEnv(t)
	r := bootstrappedResolver(t)
	a := claudecode.New()

	// Hand-forge a plan with the wrong Tool so the adapter's
	// ErrPlanMismatch fires before writepath.Apply is even called.
	badPlan := writepath.WritePlan{
		Tool:   "codex", // deliberate mismatch
		Target: claudecode.SettingsPath(r),
	}
	if _, err := a.Apply(context.Background(), r, badPlan); err == nil {
		t.Fatalf("Apply on mismatched plan: err = nil, want ErrPlanMismatch")
	}
	state := mustLoadState(t, r)
	if _, ok := state.GetLastApplied(adapter.ToolClaudeCode, claudecode.SettingsPath(r)); ok {
		t.Errorf("failed Apply left a LastApplied entry, want none")
	}
}

// TestApply_DryRunDoesNotUpdateState pins the DryRun contract: a
// dry-run Apply must NOT persist a LastApplied entry, because nothing
// on disk actually changed. A false LastApplied would produce a
// perpetual drift alarm against a file the user never asked us to
// write.
func TestApply_DryRunDoesNotUpdateState(t *testing.T) {
	clearClaudeEnv(t)
	r := bootstrappedResolver(t)
	a := claudecode.New()

	profile := config.Profile{
		SchemaVersion: config.CurrentProfileSchemaVersion,
		Name:          "t",
		Core:          config.CoreConfig{BaseURL: "https://api.example.com"},
	}
	plans, err := a.Plan(context.Background(), r, profile)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	plans[0].DryRun = true
	if _, err := a.Apply(context.Background(), r, plans[0]); err != nil {
		t.Fatalf("Apply(dry-run): %v", err)
	}
	state := mustLoadState(t, r)
	if _, ok := state.GetLastApplied(adapter.ToolClaudeCode, claudecode.SettingsPath(r)); ok {
		t.Errorf("dry-run Apply left a LastApplied entry, want none")
	}
}
