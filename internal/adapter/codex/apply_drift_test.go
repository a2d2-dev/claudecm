//go:build test

// apply_drift_test.go — story E4-S5 AC line 16 ("errors surface as
// wrapped writepath sentinels; errors.Is against ErrConcurrentEdit
// works"). Uses the writepath.SetPostReadHookForTest seam (also under
// the `test` build tag) to inject a mutation between the step-3 read
// (which captured PreFingerprint) and the step-9 drift-check Stat.
//
// Both files are exercised independently. The rest of apply_test.go is
// untagged so `go test ./...` covers the full adapter without a build
// tag. These drift rows live here because the seam is only compiled
// with `-tags=test`; production binaries contain neither the var nor
// the setter (writepath coding-standards rule 12).

package codex_test

import (
	"bytes"
	"context"
	"errors"
	"os"
	"testing"

	"github.com/a2d2-dev/claudecm/internal/adapter/codex"
	"github.com/a2d2-dev/claudecm/internal/config"
	"github.com/a2d2-dev/claudecm/internal/writepath"
)

// TestApply_SurfacesConcurrentEditConfig pins the drift-detection
// wire-up through the codex adapter for the config.toml plan: a
// concurrent mutation between writepath's step-3 read and step-9
// drift-check Stat must abort the Apply with an error that errors.Is
// against writepath.ErrConcurrentEdit — even after the adapter wraps
// the error with its "codex apply %q: %w" prefix.
//
// Also asserts the pre-drift backup file is present on disk after the
// abort — writepath preserves the step-7 backup for audit even when
// the write itself does not land.
func TestApply_SurfacesConcurrentEditConfig(t *testing.T) {
	r := bootstrappedResolver(t)
	target := codex.ConfigPath(r)
	original := []byte(`model = "before"
`)
	if err := os.WriteFile(target, original, 0o600); err != nil {
		t.Fatalf("seed config.toml: %v", err)
	}
	concurrent := []byte(`model = "before"
stolen = "yes"
`)

	restore := writepath.SetPostReadHookForTest(func() {
		if err := os.WriteFile(target, concurrent, 0o600); err != nil {
			t.Fatalf("hook write: %v", err)
		}
	})
	t.Cleanup(restore)

	profile := config.Profile{
		Name: "drift-config",
		Core: config.CoreConfig{APIKey: "sk"},
		Tools: codexOverlay(map[string]any{
			"model": "after",
		}),
	}
	plans := plansFor(t, r, profile)
	configPlan := planByTarget(t, plans, target)

	report, err := codex.New().Apply(context.Background(), r, configPlan)
	if err == nil {
		t.Fatalf("Apply returned nil error; want wrapped ErrConcurrentEdit")
	}
	if !errors.Is(err, writepath.ErrConcurrentEdit) {
		t.Fatalf("Apply err = %v; want errors.Is(err, writepath.ErrConcurrentEdit)", err)
	}

	got, readErr := os.ReadFile(target)
	if readErr != nil {
		t.Fatalf("read config.toml after drift abort: %v", readErr)
	}
	if !bytes.Equal(got, concurrent) {
		t.Fatalf("config.toml bytes = %q; want concurrent %q (must not overwrite on drift)", got, concurrent)
	}

	if report.Backup.BackupPath == "" {
		t.Fatalf("Backup.BackupPath empty; drift must preserve the pre-write backup for audit")
	}
	if _, statErr := os.Stat(report.Backup.BackupPath); statErr != nil {
		t.Fatalf("backup file stat %q: %v; want file present on disk", report.Backup.BackupPath, statErr)
	}
	backupBytes, berr := os.ReadFile(report.Backup.BackupPath)
	if berr != nil {
		t.Fatalf("read backup file: %v", berr)
	}
	if !bytes.Equal(backupBytes, original) {
		t.Fatalf("backup bytes = %q; want %q (pre-drift original)", backupBytes, original)
	}
}

// TestApply_SurfacesConcurrentEditAuth pins the same contract for the
// auth.json plan. Kept as a separate test so a regression that breaks
// drift detection on only one of the two files fails a targeted row.
func TestApply_SurfacesConcurrentEditAuth(t *testing.T) {
	r := bootstrappedResolver(t)
	target := codex.AuthPath(r)
	original := []byte(`{"OPENAI_API_KEY":"before"}`)
	if err := os.WriteFile(target, original, 0o600); err != nil {
		t.Fatalf("seed auth.json: %v", err)
	}
	concurrent := []byte(`{"OPENAI_API_KEY":"before","STOLEN":"yes"}`)

	restore := writepath.SetPostReadHookForTest(func() {
		if err := os.WriteFile(target, concurrent, 0o600); err != nil {
			t.Fatalf("hook write: %v", err)
		}
	})
	t.Cleanup(restore)

	profile := config.Profile{
		Name: "drift-auth",
		Core: config.CoreConfig{APIKey: "after"},
	}
	plans := plansFor(t, r, profile)
	authPlan := planByTarget(t, plans, target)

	report, err := codex.New().Apply(context.Background(), r, authPlan)
	if err == nil {
		t.Fatalf("Apply returned nil error; want wrapped ErrConcurrentEdit")
	}
	if !errors.Is(err, writepath.ErrConcurrentEdit) {
		t.Fatalf("Apply err = %v; want errors.Is(err, writepath.ErrConcurrentEdit)", err)
	}

	got, readErr := os.ReadFile(target)
	if readErr != nil {
		t.Fatalf("read auth.json after drift abort: %v", readErr)
	}
	if !bytes.Equal(got, concurrent) {
		t.Fatalf("auth.json bytes = %q; want concurrent %q (must not overwrite on drift)", got, concurrent)
	}

	if report.Backup.BackupPath == "" {
		t.Fatalf("Backup.BackupPath empty; drift must preserve the pre-write backup for audit")
	}
	backupBytes, berr := os.ReadFile(report.Backup.BackupPath)
	if berr != nil {
		t.Fatalf("read backup file: %v", berr)
	}
	if !bytes.Equal(backupBytes, original) {
		t.Fatalf("backup bytes = %q; want %q (pre-drift original)", backupBytes, original)
	}
}
