//go:build test

// apply_drift_test.go — story E3-S5 AC line 16 ("errors surface as
// wrapped writepath sentinels; errors.Is against ErrConcurrentEdit
// works"). Uses the writepath.SetPostReadHookForTest seam (also under
// the `test` build tag) to inject a mutation between the step-3 read
// (which captured PreFingerprint) and the step-9 drift-check Stat.
//
// The rest of apply_test.go is untagged so `go test ./...` covers the
// full adapter without a build tag. This drift row lives here because
// the seam is only compiled with `-tags=test`; production binaries
// contain neither the var nor the setter (writepath coding-standards
// rule 12).

package claudecode_test

import (
	"bytes"
	"context"
	"errors"
	"os"
	"testing"

	"github.com/a2d2-dev/claudecm/internal/adapter/claudecode"
	"github.com/a2d2-dev/claudecm/internal/config"
	"github.com/a2d2-dev/claudecm/internal/writepath"
)

// TestApply_SurfacesConcurrentEdit pins the drift-detection wire-up
// through the claudecode adapter: a concurrent mutation to the target
// file between writepath's step-3 read and step-9 drift-check Stat must
// abort the Apply with an error that errors.Is against
// writepath.ErrConcurrentEdit — even after the adapter wraps the error
// with its "claudecode apply %q: %w" prefix. The wrap contract on
// adapter.Apply requires errors.Is to remain transparent; this row is
// what catches a regression that strips the %w or picks a different
// sentinel.
//
// Also asserts the pre-drift backup file is present on disk after the
// abort — writepath preserves the step-7 backup for audit even when
// the write itself does not land.
func TestApply_SurfacesConcurrentEdit(t *testing.T) {
	r := bootstrappedResolver(t)
	target := claudecode.SettingsPath(r)
	original := []byte(`{"env":{"ANTHROPIC_MODEL":"before"}}`)
	if err := os.WriteFile(target, original, 0o600); err != nil {
		t.Fatalf("seed settings.json: %v", err)
	}
	concurrent := []byte(`{"env":{"ANTHROPIC_MODEL":"before","STOLEN":"yes"}}`)

	// Install the drift hook. Fires synchronously between writepath's
	// step-3 read and the step-9 drift-check Stat, so no goroutine
	// coordination is needed. Restore closure runs on cleanup so
	// subsequent tests are not contaminated.
	restore := writepath.SetPostReadHookForTest(func() {
		if err := os.WriteFile(target, concurrent, 0o600); err != nil {
			t.Fatalf("hook write: %v", err)
		}
	})
	t.Cleanup(restore)

	profile := config.Profile{
		Name: "drift",
		Core: config.CoreConfig{
			APIKey: "sk-drift",
			Model:  "claude-opus-4-5",
		},
	}
	plan := planFor(t, r, profile)

	report, err := claudecode.New().Apply(context.Background(), r, plan)
	if err == nil {
		t.Fatalf("Apply returned nil error; want wrapped ErrConcurrentEdit")
	}
	if !errors.Is(err, writepath.ErrConcurrentEdit) {
		t.Fatalf("Apply err = %v; want errors.Is(err, writepath.ErrConcurrentEdit)", err)
	}

	// The concurrent writer's bytes must remain on disk — the adapter
	// (via writepath) must NOT have stomped them with a stale-derived
	// render.
	got, readErr := os.ReadFile(target)
	if readErr != nil {
		t.Fatalf("read settings.json after drift abort: %v", readErr)
	}
	if !bytes.Equal(got, concurrent) {
		t.Fatalf("settings.json bytes = %q; want concurrent %q (must not overwrite on drift)", got, concurrent)
	}

	// Drift preserves the backup for audit. Verify the receipt AND the
	// file are both present — a populated BackupPath that no longer
	// resolves would be a silent audit gap.
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
