package codex_test

// apply_test.go — E4-S5. Exercises the Apply surface of the Codex
// adapter end-to-end through writepath.Apply, using a per-test HOME +
// storage.Bootstrap so every case owns its own on-disk tree.
//
// Two-file scope. Unlike claudecode (one file), the Codex adapter's
// Plan returns a slice of two WritePlans (auth.json then config.toml).
// Each row here iterates the returned slice and calls Apply per plan,
// mirroring how the two-phase commit (E7) will drive activation.
// Cross-file ordering / commit orchestration is E7's job — this file
// covers per-file Apply semantics only.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/a2d2-dev/claudecm/internal/adapter"
	"github.com/a2d2-dev/claudecm/internal/adapter/codex"
	codextoml "github.com/a2d2-dev/claudecm/internal/adapter/codex/toml"
	"github.com/a2d2-dev/claudecm/internal/config"
	"github.com/a2d2-dev/claudecm/internal/storage"
	"github.com/a2d2-dev/claudecm/internal/writepath"
)

// bootstrappedResolver builds a Resolver bound to a fresh t.TempDir
// HOME AND runs storage.Bootstrap so ~/.claudecm/{profiles,backups}
// exists at 0700. writepath.Apply needs the backup dir to snapshot the
// pre-write bytes; the lock sidecar needs an existing HOME layout too.
//
// Also ensures ~/.codex exists at 0700 so Plan targets a real dir.
// writepath will EnsureDir it if missing, but making the intended
// layout explicit here keeps tests grep-friendly.
func bootstrappedResolver(t *testing.T) *storage.Resolver {
	t.Helper()
	r, err := storage.NewResolverWithHome(t.TempDir())
	if err != nil {
		t.Fatalf("storage.NewResolverWithHome: %v", err)
	}
	if err := storage.Bootstrap(r); err != nil {
		t.Fatalf("storage.Bootstrap: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(r.Home(), ".codex"), 0o700); err != nil {
		t.Fatalf("mkdir ~/.codex: %v", err)
	}
	return r
}

// plansFor is a shorthand for Plan → []WritePlan with a fatal on error.
// Callers check len(plans) themselves because the auth-elision special
// case makes the slice length semantically meaningful.
func plansFor(t *testing.T, r *storage.Resolver, p config.Profile) []writepath.WritePlan {
	t.Helper()
	plans, err := codex.New().Plan(context.Background(), r, p)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	return plans
}

// planByTarget returns the WritePlan whose Target matches path. Fails
// the test if not found. Mirrors the plan_test.go helper.
func planByTarget(t *testing.T, plans []writepath.WritePlan, path string) writepath.WritePlan {
	t.Helper()
	for _, p := range plans {
		if p.Target == path {
			return p
		}
	}
	t.Fatalf("no WritePlan with Target=%q; got %d plans", path, len(plans))
	return writepath.WritePlan{}
}

// applyAll drives Apply over every plan in auth-first order (the order
// Plan returns them). Returns the collected reports alongside any first
// error so callers can assert failure on either file independently.
func applyAll(ctx context.Context, r *storage.Resolver, plans []writepath.WritePlan) ([]writepath.WriteReport, error) {
	a := codex.New()
	reports := make([]writepath.WriteReport, 0, len(plans))
	for _, p := range plans {
		rep, err := a.Apply(ctx, r, p)
		reports = append(reports, rep)
		if err != nil {
			return reports, err
		}
	}
	return reports, nil
}

// readAuth reads and unmarshals ~/.codex/auth.json. Fatal on error.
func readAuth(t *testing.T, r *storage.Resolver) map[string]any {
	t.Helper()
	data, err := os.ReadFile(codex.AuthPath(r))
	if err != nil {
		t.Fatalf("read auth.json: %v", err)
	}
	var out map[string]any
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatalf("unmarshal auth.json (%q): %v", string(data), err)
	}
	return out
}

// readConfigDoc reads ~/.codex/config.toml and returns a parsed Doc.
// Fatal on error.
func readConfigDoc(t *testing.T, r *storage.Resolver) *codextoml.Doc {
	t.Helper()
	data, err := os.ReadFile(codex.ConfigPath(r))
	if err != nil {
		t.Fatalf("read config.toml: %v", err)
	}
	doc, err := codextoml.Load(data)
	if err != nil {
		t.Fatalf("codextoml.Load: %v", err)
	}
	return doc
}

// codexOverlay wraps a raw overlay map under ToolCodex for Profile
// literals. Mirrors plan_test.go's withCodexOverlay.
func codexOverlay(raw map[string]any) map[config.ToolID]config.ToolOverlay {
	return map[config.ToolID]config.ToolOverlay{
		adapter.ToolCodex: {Raw: raw},
	}
}

// TestApply_HappyBothFilesFirstWrite covers a fresh install: no
// existing auth.json or config.toml. Plan(profile) → two plans; Apply
// each. Verify both files created at 0600 with expected owned content.
func TestApply_HappyBothFilesFirstWrite(t *testing.T) {
	r := bootstrappedResolver(t)
	profile := config.Profile{
		Name: "first-write",
		Core: config.CoreConfig{APIKey: "sk-first"},
		Tools: codexOverlay(map[string]any{
			"model":          "opus",
			"model_provider": "anthropic",
		}),
	}
	plans := plansFor(t, r, profile)
	if len(plans) != 2 {
		t.Fatalf("Plan len = %d, want 2 (both files)", len(plans))
	}

	reports, err := applyAll(context.Background(), r, plans)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if len(reports) != 2 {
		t.Fatalf("len(reports) = %d, want 2", len(reports))
	}
	for i, rep := range reports {
		if rep.Skipped {
			t.Errorf("reports[%d].Skipped = true, want false (first write must publish)", i)
		}
		if rep.DryRun {
			t.Errorf("reports[%d].DryRun = true, want false", i)
		}
		if rep.RolledBack {
			t.Errorf("reports[%d].RolledBack = true, want false", i)
		}
		if (rep.Backup != storage.BackupRecord{}) {
			t.Errorf("reports[%d].Backup = %+v, want zero-value (first write)", i, rep.Backup)
		}
		if (rep.PreFingerprint != storage.Fingerprint{}) {
			t.Errorf("reports[%d].PreFingerprint = %+v, want zero-value (no prior file)", i, rep.PreFingerprint)
		}
		if rep.PostFingerprint.SHA256 == "" {
			t.Errorf("reports[%d].PostFingerprint.SHA256 empty; want hash of new bytes", i)
		}
	}

	// Mode 0600 on both files.
	for _, path := range []string{codex.AuthPath(r), codex.ConfigPath(r)} {
		info, err := os.Lstat(path)
		if err != nil {
			t.Fatalf("Lstat %q: %v", path, err)
		}
		if info.Mode().Perm() != 0o600 {
			t.Errorf("mode(%q) = %v, want 0600", path, info.Mode().Perm())
		}
	}

	// Content assertions.
	auth := readAuth(t, r)
	if v, ok := auth["OPENAI_API_KEY"]; !ok || v != "sk-first" {
		t.Errorf("OPENAI_API_KEY = %v (ok=%v), want %q", v, ok, "sk-first")
	}
	doc := readConfigDoc(t, r)
	if v, ok := doc.Get("model"); !ok || v != "opus" {
		t.Errorf("model = %v (ok=%v), want opus", v, ok)
	}
	if v, ok := doc.Get("model_provider"); !ok || v != "anthropic" {
		t.Errorf("model_provider = %v (ok=%v), want anthropic", v, ok)
	}
}

// TestApply_HappyOverwriteBothFiles: prior files with unrelated
// content survive; owned keys update; backups captured for both.
func TestApply_HappyOverwriteBothFiles(t *testing.T) {
	r := bootstrappedResolver(t)
	authSeed := []byte(`{"OPENAI_API_KEY":"sk-old","unknown_key":"keep"}`)
	if err := os.WriteFile(codex.AuthPath(r), authSeed, 0o600); err != nil {
		t.Fatalf("seed auth.json: %v", err)
	}
	configSeed := []byte(`model = "stale"
sandbox_mode = "workspace-write"

[history]
max_entries = 100
`)
	if err := os.WriteFile(codex.ConfigPath(r), configSeed, 0o600); err != nil {
		t.Fatalf("seed config.toml: %v", err)
	}

	profile := config.Profile{
		Name: "overwrite",
		Core: config.CoreConfig{APIKey: "sk-new"},
		Tools: codexOverlay(map[string]any{
			"model": "haiku",
		}),
	}
	plans := plansFor(t, r, profile)
	if len(plans) != 2 {
		t.Fatalf("Plan len = %d, want 2", len(plans))
	}
	reports, err := applyAll(context.Background(), r, plans)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}

	// Both reports capture a Backup.
	for i, rep := range reports {
		if rep.Backup.BackupPath == "" {
			t.Errorf("reports[%d].Backup.BackupPath empty; want populated", i)
		}
		if rep.Backup.SourcePath != plans[i].Target {
			t.Errorf("reports[%d].Backup.SourcePath = %q, want %q", i, rep.Backup.SourcePath, plans[i].Target)
		}
		if rep.PreFingerprint.SHA256 == "" {
			t.Errorf("reports[%d].PreFingerprint.SHA256 empty; want populated", i)
		}
		if rep.PostFingerprint.SHA256 == rep.PreFingerprint.SHA256 {
			t.Errorf("reports[%d] PostFingerprint == PreFingerprint; want divergence", i)
		}
	}

	// auth.json: OPENAI_API_KEY updated, unknown_key preserved.
	auth := readAuth(t, r)
	if v, ok := auth["OPENAI_API_KEY"]; !ok || v != "sk-new" {
		t.Errorf("OPENAI_API_KEY = %v, want sk-new", v)
	}
	if v, ok := auth["unknown_key"]; !ok || v != "keep" {
		t.Errorf("unknown_key = %v, want keep (merge-preserve)", v)
	}

	// config.toml: model updated, unknown keys/section preserved.
	configBytes, err := os.ReadFile(codex.ConfigPath(r))
	if err != nil {
		t.Fatalf("read config.toml: %v", err)
	}
	for _, needle := range []string{
		`sandbox_mode = "workspace-write"`,
		"[history]",
		"max_entries = 100",
	} {
		if !bytes.Contains(configBytes, []byte(needle)) {
			t.Errorf("config.toml missing %q after Apply; got:\n%s", needle, string(configBytes))
		}
	}
	doc := readConfigDoc(t, r)
	if v, ok := doc.Get("model"); !ok || v != "haiku" {
		t.Errorf("model = %v, want haiku", v)
	}
}

// TestApply_IdempotentBothFiles: applying the same profile twice yields
// Skipped=true on the second run for BOTH plans.
func TestApply_IdempotentBothFiles(t *testing.T) {
	r := bootstrappedResolver(t)
	profile := config.Profile{
		Name: "same",
		Core: config.CoreConfig{APIKey: "sk-same"},
		Tools: codexOverlay(map[string]any{
			"model": "opus",
		}),
	}
	first := plansFor(t, r, profile)
	if _, err := applyAll(context.Background(), r, first); err != nil {
		t.Fatalf("first Apply: %v", err)
	}

	second := plansFor(t, r, profile)
	reports, err := applyAll(context.Background(), r, second)
	if err != nil {
		t.Fatalf("second Apply: %v", err)
	}
	for i, rep := range reports {
		if !rep.Skipped {
			t.Errorf("reports[%d].Skipped = false, want true (idempotent no-op)", i)
		}
		if (rep.Backup != storage.BackupRecord{}) {
			t.Errorf("reports[%d].Backup = %+v, want zero (skip must not backup)", i, rep.Backup)
		}
		if rep.PreFingerprint != rep.PostFingerprint {
			t.Errorf("reports[%d] Pre != Post on skip; got pre=%+v post=%+v", i, rep.PreFingerprint, rep.PostFingerprint)
		}
	}
}

// TestApply_OverlayAsTruthClearsAuthOwnedKeys: NFR-S6. On-disk auth.json
// has OPENAI_API_KEY set, but the profile carries an empty Core.APIKey.
// Apply must REMOVE the key rather than leaving a stale value.
func TestApply_OverlayAsTruthClearsAuthOwnedKeys(t *testing.T) {
	r := bootstrappedResolver(t)
	authSeed := []byte(`{"OPENAI_API_KEY":"sk-old","unknown_key":"keep"}`)
	if err := os.WriteFile(codex.AuthPath(r), authSeed, 0o600); err != nil {
		t.Fatalf("seed auth.json: %v", err)
	}

	profile := config.Profile{
		Name: "no-auth",
		// Core.APIKey intentionally empty.
		Tools: codexOverlay(map[string]any{
			"model": "opus",
		}),
	}
	plans := plansFor(t, r, profile)
	if _, err := applyAll(context.Background(), r, plans); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	auth := readAuth(t, r)
	if _, ok := auth["OPENAI_API_KEY"]; ok {
		t.Errorf("OPENAI_API_KEY present, want deleted (overlay-as-truth)")
	}
	if v, ok := auth["unknown_key"]; !ok || v != "keep" {
		t.Errorf("unknown_key = %v, want keep (unrelated must survive)", v)
	}
}

// TestApply_OverlayAsTruthClearsConfigOwnedKeys: NFR-S6. On-disk
// config.toml has model="opus"; profile overlay omits model. Apply must
// DELETE the model key. Unrelated keys survive.
func TestApply_OverlayAsTruthClearsConfigOwnedKeys(t *testing.T) {
	r := bootstrappedResolver(t)
	configSeed := []byte(`model = "opus"
sandbox_mode = "workspace-write"
`)
	if err := os.WriteFile(codex.ConfigPath(r), configSeed, 0o600); err != nil {
		t.Fatalf("seed config.toml: %v", err)
	}
	profile := config.Profile{
		Name: "no-model",
		Core: config.CoreConfig{APIKey: "sk"},
		// Tools omits model on purpose.
	}
	plans := plansFor(t, r, profile)
	if _, err := applyAll(context.Background(), r, plans); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	doc := readConfigDoc(t, r)
	if v, ok := doc.Get("model"); ok {
		t.Errorf("model = %v (present), want deleted", v)
	}
	configBytes, err := os.ReadFile(codex.ConfigPath(r))
	if err != nil {
		t.Fatalf("read config.toml: %v", err)
	}
	if !bytes.Contains(configBytes, []byte(`sandbox_mode = "workspace-write"`)) {
		t.Errorf("sandbox_mode not preserved; got:\n%s", string(configBytes))
	}
}

// TestApply_ContextCanceled: an expired-deadline ctx must abort before
// any write. Apply must surface writepath.ErrLockTimeout — the sentinel
// writepath's zero-deadline short-circuit emits. Adapter wrap must
// preserve errors.Is. Mirrors E3-S5 approach.
func TestApply_ContextCanceled(t *testing.T) {
	r := bootstrappedResolver(t)
	profile := config.Profile{
		Name: "canceled",
		Core: config.CoreConfig{APIKey: "sk-canceled"},
		Tools: codexOverlay(map[string]any{
			"model": "opus",
		}),
	}
	plans := plansFor(t, r, profile)

	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Hour))
	defer cancel()

	// Apply the first plan on the expired-deadline ctx. Both plans should
	// short-circuit; we only need to prove the first does — the loop
	// stops on the first error.
	_, err := codex.New().Apply(ctx, r, plans[0])
	if err == nil {
		t.Fatalf("Apply on canceled ctx returned nil error")
	}
	if !errors.Is(err, writepath.ErrLockTimeout) {
		t.Fatalf("Apply err = %v; want errors.Is(err, writepath.ErrLockTimeout)", err)
	}
	// No file should have landed.
	for _, path := range []string{codex.AuthPath(r), codex.ConfigPath(r)} {
		if _, statErr := os.Lstat(path); !os.IsNotExist(statErr) {
			t.Errorf("%q exists after canceled Apply; want no file written", path)
		}
	}
}

// TestApply_RefusesPlanMismatchTool: WritePlan whose Tool is not
// ToolCodex must be refused with ErrPlanMismatch before writepath sees
// it. Includes empty-string.
func TestApply_RefusesPlanMismatchTool(t *testing.T) {
	r := bootstrappedResolver(t)
	profile := config.Profile{Name: "p", Core: config.CoreConfig{APIKey: "sk"}}
	plans := plansFor(t, r, profile)

	tests := []struct {
		name string
		tool string
	}{
		{"claudecode tool", "claude_code"},
		{"unknown tool", "unknown"},
		{"empty tool", ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			bad := plans[0]
			bad.Tool = tc.tool
			_, err := codex.New().Apply(context.Background(), r, bad)
			if !errors.Is(err, codex.ErrPlanMismatch) {
				t.Fatalf("Apply err = %v, want ErrPlanMismatch", err)
			}
			// Owned files must remain absent.
			for _, path := range []string{codex.AuthPath(r), codex.ConfigPath(r)} {
				if _, statErr := os.Lstat(path); !os.IsNotExist(statErr) {
					t.Errorf("%q exists after refused Apply; want no write", path)
				}
			}
		})
	}
}

// TestApply_RefusesPlanMismatchTarget: even with Tool=ToolCodex, a
// Target pointing at some other path must be refused.
func TestApply_RefusesPlanMismatchTarget(t *testing.T) {
	r := bootstrappedResolver(t)
	profile := config.Profile{Name: "p", Core: config.CoreConfig{APIKey: "sk"}}
	plans := plansFor(t, r, profile)

	bad := plans[0]
	bad.Target = filepath.Join(r.Home(), ".codex", "not-owned.json")

	_, err := codex.New().Apply(context.Background(), r, bad)
	if !errors.Is(err, codex.ErrPlanMismatch) {
		t.Fatalf("Apply err = %v, want ErrPlanMismatch", err)
	}
	if _, statErr := os.Lstat(bad.Target); !os.IsNotExist(statErr) {
		t.Errorf("not-owned.json exists after refused Apply; want no write")
	}
	// Owned files also untouched.
	for _, path := range []string{codex.AuthPath(r), codex.ConfigPath(r)} {
		if _, statErr := os.Lstat(path); !os.IsNotExist(statErr) {
			t.Errorf("owned %q exists after refused Apply; want no write", path)
		}
	}
}

// TestApply_RefusesMalformedCurrentConfig: on-disk config.toml is
// malformed. Apply must surface ErrParseFailed and leave the file
// bytes unchanged (no silent rewrite).
func TestApply_RefusesMalformedCurrentConfig(t *testing.T) {
	r := bootstrappedResolver(t)
	malformed := []byte(`model = `)
	if err := os.WriteFile(codex.ConfigPath(r), malformed, 0o600); err != nil {
		t.Fatalf("seed malformed config.toml: %v", err)
	}
	profile := config.Profile{
		Name:  "p",
		Core:  config.CoreConfig{APIKey: "sk"},
		Tools: codexOverlay(map[string]any{"model": "opus"}),
	}
	plans := plansFor(t, r, profile)
	configPlan := planByTarget(t, plans, codex.ConfigPath(r))
	_, err := codex.New().Apply(context.Background(), r, configPlan)
	if !errors.Is(err, writepath.ErrParseFailed) {
		t.Fatalf("Apply err = %v, want ErrParseFailed", err)
	}
	// F2: pin the adapter's wrap prefix so a future refactor that drops
	// the wrap fails loudly rather than silently emitting a bare
	// writepath sentinel.
	if !strings.Contains(err.Error(), "codex apply") {
		t.Errorf("Apply err = %q, want mention of \"codex apply\" wrap prefix", err.Error())
	}
	after, readErr := os.ReadFile(codex.ConfigPath(r))
	if readErr != nil {
		t.Fatalf("read after refused Apply: %v", readErr)
	}
	if !bytes.Equal(after, malformed) {
		t.Errorf("config.toml bytes = %q, want %q (must not be rewritten on parse failure)", after, malformed)
	}
	// F5: the OTHER owned file must not have been created as a
	// side-effect of the refused config write.
	if _, statErr := os.Lstat(codex.AuthPath(r)); !os.IsNotExist(statErr) {
		t.Errorf("auth.json exists after refused config Apply; want no cross-file write (stat=%v)", statErr)
	}
}

// TestApply_RefusesMalformedCurrentAuth: on-disk auth.json is
// malformed. Apply must surface ErrParseFailed, file unchanged.
func TestApply_RefusesMalformedCurrentAuth(t *testing.T) {
	r := bootstrappedResolver(t)
	malformed := []byte(`{"OPENAI_API_KEY":`)
	if err := os.WriteFile(codex.AuthPath(r), malformed, 0o600); err != nil {
		t.Fatalf("seed malformed auth.json: %v", err)
	}
	profile := config.Profile{
		Name: "p",
		Core: config.CoreConfig{APIKey: "sk-new"},
	}
	plans := plansFor(t, r, profile)
	authPlan := planByTarget(t, plans, codex.AuthPath(r))
	_, err := codex.New().Apply(context.Background(), r, authPlan)
	if !errors.Is(err, writepath.ErrParseFailed) {
		t.Fatalf("Apply err = %v, want ErrParseFailed", err)
	}
	after, readErr := os.ReadFile(codex.AuthPath(r))
	if readErr != nil {
		t.Fatalf("read after refused Apply: %v", readErr)
	}
	if !bytes.Equal(after, malformed) {
		t.Errorf("auth.json bytes = %q, want %q (must not be rewritten on parse failure)", after, malformed)
	}
	// F5: the OTHER owned file must not have been created as a
	// side-effect of the refused auth write.
	if _, statErr := os.Lstat(codex.ConfigPath(r)); !os.IsNotExist(statErr) {
		t.Errorf("config.toml exists after refused auth Apply; want no cross-file write (stat=%v)", statErr)
	}
}

// TestApply_LinksToWritepathReport: the WriteReport that flows back
// from writepath carries Tool/Target matching the plan and populated
// diff + fingerprint fields.
func TestApply_LinksToWritepathReport(t *testing.T) {
	r := bootstrappedResolver(t)
	// Seed both files so Diff shows meaningful changes.
	if err := os.WriteFile(codex.AuthPath(r), []byte(`{"OPENAI_API_KEY":"sk-old"}`), 0o600); err != nil {
		t.Fatalf("seed auth.json: %v", err)
	}
	if err := os.WriteFile(codex.ConfigPath(r), []byte(`model = "stale"
`), 0o600); err != nil {
		t.Fatalf("seed config.toml: %v", err)
	}
	profile := config.Profile{
		Name: "linkcheck",
		Core: config.CoreConfig{APIKey: "sk-link"},
		Tools: codexOverlay(map[string]any{
			"model": "haiku",
		}),
	}
	plans := plansFor(t, r, profile)
	// F3: pin the wire value of ToolCodex. If someone renames the
	// constant without also updating the string literal on-disk state
	// & audit-log tooling depend on, this positive check breaks loudly.
	if plans[0].Tool != "codex" {
		t.Fatalf("plans[0].Tool = %q, want %q (wire value of ToolCodex)", plans[0].Tool, "codex")
	}
	reports, err := applyAll(context.Background(), r, plans)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if len(reports) != 2 {
		t.Fatalf("len(reports) = %d, want 2", len(reports))
	}
	for i, rep := range reports {
		if rep.Tool != string(adapter.ToolCodex) {
			t.Errorf("reports[%d].Tool = %q, want %q", i, rep.Tool, adapter.ToolCodex)
		}
		if rep.Target != plans[i].Target {
			t.Errorf("reports[%d].Target = %q, want %q", i, rep.Target, plans[i].Target)
		}
		if rep.PreFingerprint.SHA256 == "" {
			t.Errorf("reports[%d].PreFingerprint.SHA256 empty; want populated (prior file existed)", i)
		}
		if rep.PostFingerprint.SHA256 == "" {
			t.Errorf("reports[%d].PostFingerprint.SHA256 empty; want populated", i)
		}
		if len(rep.Diff.Added) == 0 && len(rep.Diff.Changed) == 0 && len(rep.Diff.Removed) == 0 {
			t.Errorf("reports[%d].Diff empty; want non-empty", i)
		}
	}
}

// TestApply_SymlinkEscapeConfig: config.toml sits inside a symlinked
// parent pointing outside HOME. writepath's containment guard fires
// and Apply surfaces ErrOutsideHome. Mirrors the claudecode row: we
// plant the symlink at the PARENT (~/.codex) because writepath's guard
// resolves parent components, and leaf-level symlink escape is not the
// invariant currently enforced.
func TestApply_SymlinkEscapeConfig(t *testing.T) {
	home, err := os.MkdirTemp("", "codex-apply-home-*")
	if err != nil {
		t.Fatalf("mkdtemp home: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(home) })
	r, err := storage.NewResolverWithHome(home)
	if err != nil {
		t.Fatalf("NewResolverWithHome: %v", err)
	}
	if err := storage.Bootstrap(r); err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	outside, err := os.MkdirTemp("", "codex-apply-outside-*")
	if err != nil {
		t.Fatalf("mkdtemp outside: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(outside) })
	if err := os.Symlink(outside, filepath.Join(home, ".codex")); err != nil {
		t.Fatalf("symlink ~/.codex -> %s: %v", outside, err)
	}

	profile := config.Profile{
		Name:  "escape-config",
		Core:  config.CoreConfig{APIKey: "sk"},
		Tools: codexOverlay(map[string]any{"model": "opus"}),
	}
	plans := plansFor(t, r, profile)
	configPlan := planByTarget(t, plans, codex.ConfigPath(r))
	_, err = codex.New().Apply(context.Background(), r, configPlan)
	if !errors.Is(err, writepath.ErrOutsideHome) {
		t.Fatalf("Apply on out-of-HOME parent symlink: err = %v, want ErrOutsideHome", err)
	}
	if _, statErr := os.Lstat(filepath.Join(outside, "config.toml")); !os.IsNotExist(statErr) {
		t.Errorf("config.toml leaked to outside HOME (%s): stat=%v", outside, statErr)
	}
}

// TestApply_SymlinkEscapeAuth: same as above but for auth.json.
func TestApply_SymlinkEscapeAuth(t *testing.T) {
	home, err := os.MkdirTemp("", "codex-apply-home-*")
	if err != nil {
		t.Fatalf("mkdtemp home: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(home) })
	r, err := storage.NewResolverWithHome(home)
	if err != nil {
		t.Fatalf("NewResolverWithHome: %v", err)
	}
	if err := storage.Bootstrap(r); err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	outside, err := os.MkdirTemp("", "codex-apply-outside-*")
	if err != nil {
		t.Fatalf("mkdtemp outside: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(outside) })
	if err := os.Symlink(outside, filepath.Join(home, ".codex")); err != nil {
		t.Fatalf("symlink ~/.codex -> %s: %v", outside, err)
	}

	profile := config.Profile{
		Name: "escape-auth",
		Core: config.CoreConfig{APIKey: "sk-escape"},
	}
	plans := plansFor(t, r, profile)
	authPlan := planByTarget(t, plans, codex.AuthPath(r))
	_, err = codex.New().Apply(context.Background(), r, authPlan)
	if !errors.Is(err, writepath.ErrOutsideHome) {
		t.Fatalf("Apply on out-of-HOME parent symlink: err = %v, want ErrOutsideHome", err)
	}
	if _, statErr := os.Lstat(filepath.Join(outside, "auth.json")); !os.IsNotExist(statErr) {
		t.Errorf("auth.json leaked to outside HOME (%s): stat=%v", outside, statErr)
	}
}

// TestApply_BackupCapturedForBoth: guards the FR-16 "backup before
// write" invariant for BOTH files. Each Backup receipt SourcePath
// matches its plan.Target and the backup file on disk contains the
// pre-write bytes verbatim.
func TestApply_BackupCapturedForBoth(t *testing.T) {
	r := bootstrappedResolver(t)
	authSeed := []byte(`{"OPENAI_API_KEY":"before","unowned":42}`)
	if err := os.WriteFile(codex.AuthPath(r), authSeed, 0o600); err != nil {
		t.Fatalf("seed auth.json: %v", err)
	}
	configSeed := []byte(`model = "before"
unowned = "keep"
`)
	if err := os.WriteFile(codex.ConfigPath(r), configSeed, 0o600); err != nil {
		t.Fatalf("seed config.toml: %v", err)
	}

	profile := config.Profile{
		Name: "backup",
		Core: config.CoreConfig{APIKey: "after"},
		Tools: codexOverlay(map[string]any{
			"model": "after",
		}),
	}
	plans := plansFor(t, r, profile)
	reports, err := applyAll(context.Background(), r, plans)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}

	// Map plan.Target -> pre-write seed bytes so the assertion works
	// regardless of Plan ordering (auth-first is separately covered).
	seeds := map[string][]byte{
		codex.AuthPath(r):   authSeed,
		codex.ConfigPath(r): configSeed,
	}
	for i, rep := range reports {
		if rep.Backup.SourcePath != plans[i].Target {
			t.Errorf("reports[%d].Backup.SourcePath = %q, want %q", i, rep.Backup.SourcePath, plans[i].Target)
		}
		if rep.Backup.BackupPath == "" {
			t.Fatalf("reports[%d].Backup.BackupPath empty; want populated", i)
		}
		backupBytes, berr := os.ReadFile(rep.Backup.BackupPath)
		if berr != nil {
			t.Fatalf("read backup for %q: %v", rep.Target, berr)
		}
		want := seeds[rep.Target]
		if !bytes.Equal(backupBytes, want) {
			t.Errorf("backup bytes for %q = %q, want %q", rep.Target, backupBytes, want)
		}
	}
}
