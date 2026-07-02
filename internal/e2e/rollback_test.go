//go:build test

// rollback_test.go — Story E8-S5. Two-phase rollback simulation.
//
// Stages WritePlans for all three v1-owned files (Codex auth.json,
// Codex config.toml, Claude Code settings.json) and injects a post-
// write reparse failure on settings.json AFTER the two Codex files
// have already committed. Asserts:
//
//   - Commit returns *commit.PartialFailure.
//   - Both Codex files' on-disk bytes are rolled back byte-identical
//     to their pre-Stage content (all-or-nothing atomicity, FR-16).
//   - PerFileReport statuses reflect the rollback correctly:
//       auth.json    → StatusRolledBack (committed then rolled back)
//       config.toml  → StatusRolledBack
//       settings.json → StatusFailed (its own AtomicWrite fired but
//                       the post-write reparse rejected it; the
//                       failing file's own rollback restores the
//                       pre-Stage bytes on disk, but the PerFile row
//                       remains StatusFailed to distinguish "the file
//                       whose write failed" from "collateral damage").
//   - The failing file's on-disk bytes ALSO equal its pre-Stage
//     content (rollbackFile ran).
//
// Failure-injection mechanism. Uses the same content-based Parser
// trick E7-S2's TestCommit_PartialFailureRollback does: a Parser
// that rejects when its argument matches newBytes AND the on-disk
// target already equals newBytes — i.e. only on the post-write
// reparse. Stage's parse-new sees newBytes with disk still holding
// seed, so it accepts.
//
// Exit code claim. The story pins exit code 2 on clean rollback. That
// mapping (*commit.PartialFailure → os.Exit(2)) lives in cmd/switch's
// RunE wrapper and is exercised by cmd/switch_test.go. This test
// asserts the *commit.PartialFailure return — the code-2 mapping is
// tested at the CLI boundary elsewhere.

package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"testing"

	"github.com/a2d2-dev/claudecm/internal/adapter"
	codextoml "github.com/a2d2-dev/claudecm/internal/adapter/codex/toml"
	"github.com/a2d2-dev/claudecm/internal/commit"
	"github.com/a2d2-dev/claudecm/internal/writepath"
)

// TestE2E_TwoPhaseRollbackClaudeSettingsFails stages the three-file
// commit and forces the settings.json reparse to fail. Asserts both
// Codex files rolled back byte-identical and error is *PartialFailure.
func TestE2E_TwoPhaseRollbackClaudeSettingsFails(t *testing.T) {
	h := newHarness(t)

	// Seed pre-Stage state for all three files. Distinct seed values so
	// a rollback failure would be obvious.
	seedAuth := []byte(`{"OPENAI_API_KEY":"old-key"}`)
	seedConfig := []byte("model = \"old-model\"\nmodel_provider = \"openai\"\n")
	seedSettings := []byte(`{"env":{"ANTHROPIC_MODEL":"old-model"}}`)
	seedFile(t, h.codexAuth(), seedAuth)
	seedFile(t, h.codexConfig(), seedConfig)
	seedFile(t, h.claudeSettings(), seedSettings)

	// The three plans. Distinct newBytes so a Commit that succeeded
	// would visibly change the file — makes the rollback assertion
	// meaningful.
	newAuth := []byte(`{"OPENAI_API_KEY":"new-key"}`)
	newConfig := []byte("model = \"new-model\"\nmodel_provider = \"openai\"\n")
	newSettings := []byte(`{"env":{"ANTHROPIC_MODEL":"new-model"}}`)

	authPlan := writepath.WritePlan{
		Tool:       string(adapter.ToolCodex),
		Target:     h.codexAuth(),
		NewContent: newAuth,
		Parser:     jsonParserAny(),
		OwnedKeys:  []string{"OPENAI_API_KEY"},
	}
	// config.toml uses a tolerant TOML parser — any bytes that Load
	// accepts pass, so no rejection on this plan. See tomlParserAny.
	configPlan := writepath.WritePlan{
		Tool:       string(adapter.ToolCodex),
		Target:     h.codexConfig(),
		NewContent: newConfig,
		Parser:     tomlParserAny(),
		OwnedKeys:  []string{"model", "model_provider"},
	}
	// Content-based rejecting parser for settings.json — same shape as
	// the E7-S2 TestCommit_PartialFailureRollback trick. Rejects only
	// on the post-write reparse.
	settingsParser := writepath.ParserFunc(func(data []byte) (any, error) {
		if bytes.Equal(data, newSettings) {
			if onDisk, err := os.ReadFile(h.claudeSettings()); err == nil && bytes.Equal(onDisk, newSettings) {
				return nil, errors.New("reject NEW content on post-write reparse")
			}
		}
		if len(data) == 0 {
			return map[string]any{}, nil
		}
		var v any
		if err := json.Unmarshal(data, &v); err != nil {
			return nil, err
		}
		return v, nil
	})
	settingsPlan := writepath.WritePlan{
		Tool:       string(adapter.ToolClaudeCode),
		Target:     h.claudeSettings(),
		NewContent: newSettings,
		Parser:     settingsParser,
		OwnedKeys:  []string{"env.ANTHROPIC_MODEL"},
	}

	// Stage + Commit.
	c := commit.NewCommitter()
	txn, err := c.Stage(context.Background(), h.resv, []writepath.WritePlan{authPlan, configPlan, settingsPlan})
	if err != nil {
		t.Fatalf("Stage: %v", err)
	}
	report, cerr := c.Commit(context.Background(), txn)
	if cerr == nil {
		t.Fatalf("Commit: expected *PartialFailure, got nil")
	}

	// Error class assertion.
	var pf *commit.PartialFailure
	if !errors.As(cerr, &pf) {
		t.Fatalf("Commit err type = %T; want *commit.PartialFailure", cerr)
	}
	if pf.FailedFile != h.claudeSettings() {
		t.Errorf("FailedFile = %q, want %q", pf.FailedFile, h.claudeSettings())
	}
	if !errors.Is(cerr, writepath.ErrPostWriteReparse) {
		t.Errorf("cause chain does not include ErrPostWriteReparse: %v", cerr)
	}

	// Codex files must equal their pre-Stage bytes on disk.
	assertFileBytes(t, h.codexAuth(), seedAuth)
	assertFileBytes(t, h.codexConfig(), seedConfig)
	// settings.json must equal its pre-Stage bytes too — the failing
	// file's own rollback restored the pre-Stage content (F4:
	// FailingFileRolledBack flag).
	assertFileBytes(t, h.claudeSettings(), seedSettings)

	// PerFile statuses.
	statuses := statusByTarget(report)
	if got := statuses[h.codexAuth()]; got != commit.StatusRolledBack {
		t.Errorf("auth.json status = %q; want %q", got, commit.StatusRolledBack)
	}
	if got := statuses[h.codexConfig()]; got != commit.StatusRolledBack {
		t.Errorf("config.toml status = %q; want %q", got, commit.StatusRolledBack)
	}
	if got := statuses[h.claudeSettings()]; got != commit.StatusFailed {
		t.Errorf("settings.json status = %q; want %q", got, commit.StatusFailed)
	}

	// Aggregate flags.
	if !report.RolledBack {
		t.Errorf("report.RolledBack = false, want true")
	}
	if !report.FailingFileRolledBack {
		t.Errorf("report.FailingFileRolledBack = false, want true (failing file's own rollback should have succeeded)")
	}

	// PartialFailure.RolledBack lists the previously-committed targets
	// that were unwound. Codex order is auth-first; unwind is reverse.
	if len(pf.RolledBack) != 2 {
		t.Errorf("PartialFailure.RolledBack len = %d, want 2 (auth + config)", len(pf.RolledBack))
	}
}

// jsonParserAny returns a Parser that accepts any valid JSON bytes.
func jsonParserAny() writepath.Parser {
	return writepath.ParserFunc(func(data []byte) (any, error) {
		if len(data) == 0 {
			return map[string]any{}, nil
		}
		var v any
		if err := json.Unmarshal(data, &v); err != nil {
			return nil, err
		}
		return v, nil
	})
}

// tomlParserAny returns a Parser that accepts any valid TOML bytes.
// Uses the same doc-model reader the codex adapter uses so shape
// matching stays honest.
func tomlParserAny() writepath.Parser {
	return writepath.ParserFunc(func(data []byte) (any, error) {
		if len(data) == 0 {
			return map[string]any{}, nil
		}
		// Lightweight TOML acceptance via the codex adapter's own
		// reader. We only need "does this parse?"; the returned value
		// need not be a full model, just non-nil so writepath's
		// Flatten step has something to walk.
		return codexTomlLoadForTest(data)
	})
}

// codexTomlLoadForTest wraps the codex adapter's TOML doc-model loader
// into a flat map suitable for writepath.Flatten. Reuses the same
// implementation Stage's tomlParser uses in production.
func codexTomlLoadForTest(data []byte) (any, error) {
	doc, err := codextoml.Load(data)
	if err != nil {
		return nil, err
	}
	out := map[string]any{}
	for _, k := range doc.Keys() {
		if v, ok := doc.Get(k); ok {
			out[k] = v
		}
	}
	return out, nil
}

// assertFileBytes reads path and asserts its content equals want.
func assertFileBytes(t *testing.T, path string, want []byte) {
	t.Helper()
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %q: %v", path, err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("%q content drifted: got %q, want %q", path, got, want)
	}
}

// statusByTarget indexes report.PerFile by Target for easier lookup.
func statusByTarget(report commit.CommitReport) map[string]commit.FileStatus {
	out := map[string]commit.FileStatus{}
	for _, pf := range report.PerFile {
		out[pf.Target] = pf.Status
	}
	return out
}
