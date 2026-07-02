// End-to-end tests for the E7-S2 (Stage) + E7-S3 (Commit) pipeline
// bodies. Uses per-test t.TempDir HOMEs bootstrapped via
// storage.Bootstrap, plus synthetic WritePlans constructed inline so
// the tests do not depend on the real claudecode / codex adapters —
// this keeps the commit package as an isolated pipeline unit.
package commit

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/a2d2-dev/claudecm/internal/adapter"
	"github.com/a2d2-dev/claudecm/internal/storage"
	"github.com/a2d2-dev/claudecm/internal/writepath"
)

// rejectOnNthCallParser is a JSON parser that succeeds for the first
// N-1 calls and rejects the Nth. Used by tests that need to force a
// post-write reparse failure without also failing Stage's parse-for-
// diff step (Stage calls Parser twice per file: once for current, once
// for new bytes; the Commit post-write reparse is the third call).
func rejectOnNthCallParser(n int) writepath.Parser {
	var count atomic.Int64
	return writepath.ParserFunc(func(data []byte) (any, error) {
		c := count.Add(1)
		if int(c) >= n {
			return nil, fmt.Errorf("simulated reparse failure on call %d", c)
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
}

// jsonParser is a std-lib JSON parser wrapped in writepath.ParserFunc.
// Kept local to this test file so we do not depend on any adapter's
// parser implementation.
var jsonParser = writepath.ParserFunc(func(data []byte) (any, error) {
	if len(data) == 0 {
		return map[string]any{}, nil
	}
	var v any
	if err := json.Unmarshal(data, &v); err != nil {
		return nil, err
	}
	return v, nil
})

// newTestHome constructs a Resolver bound to a fresh t.TempDir HOME
// and runs Bootstrap so ~/.claudecm/{profiles,backups} exist at 0700.
func newTestHome(t *testing.T) (*storage.Resolver, string) {
	t.Helper()
	home := t.TempDir()
	r, err := storage.NewResolverWithHome(home)
	if err != nil {
		t.Fatalf("NewResolverWithHome: %v", err)
	}
	if err := storage.Bootstrap(r); err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	return r, home
}

// mkdirParent creates the parent dir of p at 0700 so AtomicWrite can
// land its temp file.
func mkdirParent(t *testing.T, p string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		t.Fatalf("mkdir parent %q: %v", p, err)
	}
}

// codexAuthPlan builds a synthetic codex auth.json write plan.
func codexAuthPlan(home string, body []byte, owned []string) writepath.WritePlan {
	return writepath.WritePlan{
		Tool:       string(adapter.ToolCodex),
		Target:     filepath.Join(home, ".codex", "auth.json"),
		NewContent: body,
		Parser:     jsonParser,
		OwnedKeys:  owned,
	}
}

// claudeSettingsPlan builds a synthetic claude_code settings.json write plan.
func claudeSettingsPlan(home string, body []byte, owned []string) writepath.WritePlan {
	return writepath.WritePlan{
		Tool:       string(adapter.ToolClaudeCode),
		Target:     filepath.Join(home, ".claude", "settings.json"),
		NewContent: body,
		Parser:     jsonParser,
		OwnedKeys:  owned,
	}
}

// -----------------------------------------------------------------------------
// Stage tests
// -----------------------------------------------------------------------------

func TestStage_HappyBothFilesTwoTools(t *testing.T) {
	r, home := newTestHome(t)
	authPath := filepath.Join(home, ".codex", "auth.json")
	settingsPath := filepath.Join(home, ".claude", "settings.json")
	mkdirParent(t, authPath)
	mkdirParent(t, settingsPath)

	plans := []writepath.WritePlan{
		// Order intentionally reversed to prove canonical sort runs.
		claudeSettingsPlan(home, []byte(`{"env":{"K":"V"}}`), []string{"env.K"}),
		codexAuthPlan(home, []byte(`{"OPENAI_API_KEY":"sk"}`), []string{"OPENAI_API_KEY"}),
	}

	c := NewCommitter()
	txn, err := c.Stage(context.Background(), r, plans)
	if err != nil {
		t.Fatalf("Stage: %v", err)
	}
	defer func() { _ = c.Abort(txn) }()

	if len(txn.Prepared) != 2 {
		t.Fatalf("Prepared len = %d, want 2", len(txn.Prepared))
	}
	if len(txn.Locks) != 2 {
		t.Fatalf("Locks len = %d, want 2", len(txn.Locks))
	}
	// Locks are in canonical order — auth.json first.
	if txn.Locks[0].Target != authPath {
		t.Errorf("Locks[0].Target = %q, want %q", txn.Locks[0].Target, authPath)
	}
	if txn.Locks[1].Target != settingsPath {
		t.Errorf("Locks[1].Target = %q, want %q", txn.Locks[1].Target, settingsPath)
	}
	// Prepared[i] corresponds to Plans[i] — caller-order.
	if txn.Prepared[0].Plan.Target != settingsPath {
		t.Errorf("Prepared[0] should mirror Plans[0] (%q), got %q", settingsPath, txn.Prepared[0].Plan.Target)
	}
	// No renames happened yet — target files must not exist on disk.
	if _, err := os.Stat(authPath); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("auth.json exists on disk after Stage; err=%v (want ErrNotExist)", err)
	}
	if _, err := os.Stat(settingsPath); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("settings.json exists on disk after Stage; err=%v (want ErrNotExist)", err)
	}
}

func TestStage_InvalidPlanReturnsError(t *testing.T) {
	r, home := newTestHome(t)
	authPath := filepath.Join(home, ".codex", "auth.json")
	mkdirParent(t, authPath)

	plans := []writepath.WritePlan{
		codexAuthPlan(home, []byte(`{"OPENAI_API_KEY":"sk"}`), []string{"OPENAI_API_KEY"}),
		// Empty Target → ValidatePlan error.
		{Tool: "claude_code", NewContent: []byte("{}"), Parser: jsonParser},
	}

	c := NewCommitter()
	txn, err := c.Stage(context.Background(), r, plans)
	if !errors.Is(err, writepath.ErrPlanInvalid) {
		t.Fatalf("Stage err = %v, want ErrPlanInvalid", err)
	}
	if len(txn.Plans) != 0 || len(txn.Locks) != 0 {
		t.Errorf("Stage returned non-empty txn on ErrPlanInvalid: %+v", txn)
	}
	// No sidecar or lock file should have been created since we
	// bailed before touching disk.
}

func TestStage_TouchesUnownedRefuses(t *testing.T) {
	r, home := newTestHome(t)
	settingsPath := filepath.Join(home, ".claude", "settings.json")
	mkdirParent(t, settingsPath)
	// Pre-seed a settings.json with an unowned key.
	seed := []byte(`{"env":{"UNOWNED":"stale"}}`)
	if err := os.WriteFile(settingsPath, seed, 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}

	// Plan owns nothing but tries to REMOVE the unowned key.
	plans := []writepath.WritePlan{
		claudeSettingsPlan(home, []byte(`{}`), []string{}),
	}

	c := NewCommitter()
	txn, err := c.Stage(context.Background(), r, plans)
	if !errors.Is(err, writepath.ErrDryRunUnownedTouched) {
		t.Fatalf("Stage err = %v, want ErrDryRunUnownedTouched", err)
	}
	if len(txn.Prepared) != 0 {
		t.Errorf("expected empty txn on refuse, got Prepared len=%d", len(txn.Prepared))
	}
	// Locks must have been released — a subsequent Acquire on the
	// same target should succeed immediately.
	rel, _ := filepath.Rel(home, settingsPath)
	h, aerr := storage.Acquire(r, rel, storage.LockOptions{Timeout: 100 * time.Millisecond})
	if aerr != nil {
		t.Fatalf("post-refuse Acquire failed: %v (locks not released?)", aerr)
	}
	_ = h.Release()
}

func TestStage_TouchesUnownedWithAllowUnownedProceeds(t *testing.T) {
	r, home := newTestHome(t)
	settingsPath := filepath.Join(home, ".claude", "settings.json")
	mkdirParent(t, settingsPath)
	seed := []byte(`{"env":{"UNOWNED":"stale"}}`)
	if err := os.WriteFile(settingsPath, seed, 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}

	plan := claudeSettingsPlan(home, []byte(`{}`), []string{})
	plan.AllowUnowned = true
	plans := []writepath.WritePlan{plan}

	c := NewCommitter()
	txn, err := c.Stage(context.Background(), r, plans)
	if err != nil {
		t.Fatalf("Stage: %v", err)
	}
	defer func() { _ = c.Abort(txn) }()
	if len(txn.Prepared) != 1 {
		t.Fatalf("Prepared len = %d, want 1", len(txn.Prepared))
	}
}

func TestStage_DryRunNoBackup(t *testing.T) {
	r, home := newTestHome(t)
	settingsPath := filepath.Join(home, ".claude", "settings.json")
	mkdirParent(t, settingsPath)
	seed := []byte(`{"env":{"K":"stale"}}`)
	if err := os.WriteFile(settingsPath, seed, 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}

	plan := claudeSettingsPlan(home, []byte(`{"env":{"K":"fresh"}}`), []string{"env.K"})
	plan.DryRun = true

	c := NewCommitter()
	txn, err := c.Stage(context.Background(), r, []writepath.WritePlan{plan})
	if err != nil {
		t.Fatalf("Stage: %v", err)
	}
	defer func() { _ = c.Abort(txn) }()
	if len(txn.Prepared) != 1 {
		t.Fatalf("Prepared len = %d, want 1", len(txn.Prepared))
	}
	if !txn.Prepared[0].DryRun {
		t.Errorf("Prepared[0].DryRun = false, want true")
	}
	if !bytes.Equal(txn.Prepared[0].NewBytes, []byte(`{"env":{"K":"fresh"}}`)) {
		t.Errorf("NewBytes = %q, want fresh bytes", string(txn.Prepared[0].NewBytes))
	}
	if txn.Prepared[0].Backup != (storage.BackupRecord{}) {
		t.Errorf("Backup should be zero for DryRun, got %+v", txn.Prepared[0].Backup)
	}
	// Target must still hold the seed bytes.
	got, _ := os.ReadFile(settingsPath)
	if !bytes.Equal(got, seed) {
		t.Errorf("target bytes changed under DryRun: got %q, want %q", got, seed)
	}
}

func TestStage_SkippedBytesEqual(t *testing.T) {
	r, home := newTestHome(t)
	settingsPath := filepath.Join(home, ".claude", "settings.json")
	mkdirParent(t, settingsPath)
	body := []byte(`{"env":{"K":"V"}}`)
	if err := os.WriteFile(settingsPath, body, 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}

	plan := claudeSettingsPlan(home, body, []string{"env.K"})

	c := NewCommitter()
	txn, err := c.Stage(context.Background(), r, []writepath.WritePlan{plan})
	if err != nil {
		t.Fatalf("Stage: %v", err)
	}
	defer func() { _ = c.Abort(txn) }()
	if !txn.Prepared[0].Skipped {
		t.Errorf("Skipped = false, want true (byte-identical)")
	}
	if txn.Prepared[0].Backup != (storage.BackupRecord{}) {
		t.Errorf("Skipped plan produced backup: %+v", txn.Prepared[0].Backup)
	}
}

func TestStage_MalformedCurrentError(t *testing.T) {
	r, home := newTestHome(t)
	settingsPath := filepath.Join(home, ".claude", "settings.json")
	mkdirParent(t, settingsPath)
	// Malformed JSON on disk.
	if err := os.WriteFile(settingsPath, []byte(`{not json`), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}

	plan := claudeSettingsPlan(home, []byte(`{"env":{"K":"V"}}`), []string{"env.K"})

	c := NewCommitter()
	_, err := c.Stage(context.Background(), r, []writepath.WritePlan{plan})
	if !errors.Is(err, writepath.ErrParseFailed) {
		t.Fatalf("Stage err = %v, want ErrParseFailed", err)
	}
}

func TestStage_LockTimeout(t *testing.T) {
	r, home := newTestHome(t)
	authPath := filepath.Join(home, ".codex", "auth.json")
	settingsPath := filepath.Join(home, ".claude", "settings.json")
	mkdirParent(t, authPath)
	mkdirParent(t, settingsPath)

	// Hold the settings.json lock in a goroutine so Stage's second
	// acquire (canonical order: auth.json then settings.json) times
	// out.
	relSettings, _ := filepath.Rel(home, settingsPath)
	held, err := storage.Acquire(r, relSettings, storage.LockOptions{Timeout: 5 * time.Second})
	if err != nil {
		t.Fatalf("pre-acquire settings.json: %v", err)
	}
	release := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		<-release
		_ = held.Release()
	}()
	defer func() {
		close(release)
		wg.Wait()
	}()

	plans := []writepath.WritePlan{
		codexAuthPlan(home, []byte(`{"OPENAI_API_KEY":"sk"}`), []string{"OPENAI_API_KEY"}),
		claudeSettingsPlan(home, []byte(`{"env":{"K":"V"}}`), []string{"env.K"}),
	}

	// Short deadline so the second Acquire times out fast.
	ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
	defer cancel()

	c := NewCommitter()
	_, serr := c.Stage(ctx, r, plans)
	if !errors.Is(serr, writepath.ErrLockTimeout) {
		t.Fatalf("Stage err = %v, want ErrLockTimeout", serr)
	}

	// The auth.json lock must have been released — another Acquire
	// on it should succeed immediately.
	relAuth, _ := filepath.Rel(home, authPath)
	h, aerr := storage.Acquire(r, relAuth, storage.LockOptions{Timeout: 200 * time.Millisecond})
	if aerr != nil {
		t.Fatalf("post-timeout Acquire on auth.json failed: %v", aerr)
	}
	_ = h.Release()
}

// -----------------------------------------------------------------------------
// Commit tests
// -----------------------------------------------------------------------------

func TestCommit_HappyBothFiles(t *testing.T) {
	r, home := newTestHome(t)
	authPath := filepath.Join(home, ".codex", "auth.json")
	settingsPath := filepath.Join(home, ".claude", "settings.json")
	mkdirParent(t, authPath)
	mkdirParent(t, settingsPath)

	plans := []writepath.WritePlan{
		// Reverse order to prove canonical sort routes correctly.
		claudeSettingsPlan(home, []byte(`{"env":{"K":"V"}}`), []string{"env.K"}),
		codexAuthPlan(home, []byte(`{"OPENAI_API_KEY":"sk"}`), []string{"OPENAI_API_KEY"}),
	}

	c := NewCommitter()
	txn, err := c.Stage(context.Background(), r, plans)
	if err != nil {
		t.Fatalf("Stage: %v", err)
	}
	report, err := c.Commit(context.Background(), txn)
	if err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if len(report.PerFile) != 2 {
		t.Fatalf("PerFile len = %d, want 2", len(report.PerFile))
	}
	for _, pfr := range report.PerFile {
		if pfr.Status != StatusCommitted {
			t.Errorf("target %q status = %q, want %q", pfr.Target, pfr.Status, StatusCommitted)
		}
	}
	// Both files exist on disk.
	if got, err := os.ReadFile(authPath); err != nil || string(got) != `{"OPENAI_API_KEY":"sk"}` {
		t.Errorf("auth.json bytes = %q err=%v", got, err)
	}
	if got, err := os.ReadFile(settingsPath); err != nil || string(got) != `{"env":{"K":"V"}}` {
		t.Errorf("settings.json bytes = %q err=%v", got, err)
	}
}

func TestCommit_AllSkippedIsNoop(t *testing.T) {
	r, home := newTestHome(t)
	authPath := filepath.Join(home, ".codex", "auth.json")
	settingsPath := filepath.Join(home, ".claude", "settings.json")
	mkdirParent(t, authPath)
	mkdirParent(t, settingsPath)

	authBytes := []byte(`{"OPENAI_API_KEY":"sk"}`)
	settingsBytes := []byte(`{"env":{"K":"V"}}`)
	if err := os.WriteFile(authPath, authBytes, 0o600); err != nil {
		t.Fatalf("seed auth: %v", err)
	}
	if err := os.WriteFile(settingsPath, settingsBytes, 0o600); err != nil {
		t.Fatalf("seed settings: %v", err)
	}

	plans := []writepath.WritePlan{
		codexAuthPlan(home, authBytes, []string{"OPENAI_API_KEY"}),
		claudeSettingsPlan(home, settingsBytes, []string{"env.K"}),
	}

	c := NewCommitter()
	txn, err := c.Stage(context.Background(), r, plans)
	if err != nil {
		t.Fatalf("Stage: %v", err)
	}
	report, err := c.Commit(context.Background(), txn)
	if err != nil {
		t.Fatalf("Commit: %v", err)
	}
	for _, pfr := range report.PerFile {
		if pfr.Status != StatusUntouched {
			t.Errorf("target %q status = %q, want %q", pfr.Target, pfr.Status, StatusUntouched)
		}
	}
}

func TestCommit_DryRunNoWrites(t *testing.T) {
	r, home := newTestHome(t)
	settingsPath := filepath.Join(home, ".claude", "settings.json")
	mkdirParent(t, settingsPath)
	seed := []byte(`{"env":{"K":"stale"}}`)
	if err := os.WriteFile(settingsPath, seed, 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}

	plan := claudeSettingsPlan(home, []byte(`{"env":{"K":"fresh"}}`), []string{"env.K"})
	plan.DryRun = true

	c := NewCommitter()
	txn, err := c.Stage(context.Background(), r, []writepath.WritePlan{plan})
	if err != nil {
		t.Fatalf("Stage: %v", err)
	}
	report, err := c.Commit(context.Background(), txn)
	if err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if len(report.PerFile) != 1 || report.PerFile[0].Status != StatusUntouched {
		t.Fatalf("expected single Untouched entry, got %+v", report.PerFile)
	}
	got, _ := os.ReadFile(settingsPath)
	if !bytes.Equal(got, seed) {
		t.Errorf("target bytes changed under DryRun: got %q, want %q", got, seed)
	}
}

func TestCommit_PartialFailureRollback(t *testing.T) {
	// Setup two plans: codex auth.json (commits first, first-write)
	// + claude settings.json (wired to fail post-write reparse via a
	// content-based Parser that rejects specifically when it sees the
	// plan's new bytes actually landed on disk — i.e. after
	// AtomicWrite finishes. This avoids the earlier fragile call-count
	// parser: reparse rejection now keys off deterministic content,
	// not "third invocation" (F5).
	r, home := newTestHome(t)
	authPath := filepath.Join(home, ".codex", "auth.json")
	settingsPath := filepath.Join(home, ".claude", "settings.json")
	mkdirParent(t, authPath)
	mkdirParent(t, settingsPath)
	// Seed settings.json so rollback restores from a real backup.
	seedSettings := []byte(`{"env":{"K":"stale"}}`)
	if err := os.WriteFile(settingsPath, seedSettings, 0o600); err != nil {
		t.Fatalf("seed settings: %v", err)
	}

	newSettings := []byte(`{"env":{"K":"fresh"}}`)
	// Content-based Parser: rejects only when the argument matches
	// newSettings AND the on-disk target already equals newSettings.
	// Stage's parse-new sees newSettings but disk still holds the
	// seed → accept. Commit AtomicWrite lands newSettings on disk;
	// the post-write reparse re-reads the target (= newSettings) and
	// re-Parses it — now the disk-check matches, so the Parser
	// rejects, forcing rollback. Deterministic and independent of
	// how many times Stage happens to call the Parser.
	settingsParser := writepath.ParserFunc(func(data []byte) (any, error) {
		if bytes.Equal(data, newSettings) {
			if onDisk, err := os.ReadFile(settingsPath); err == nil && bytes.Equal(onDisk, newSettings) {
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

	authPlan := codexAuthPlan(home, []byte(`{"OPENAI_API_KEY":"sk"}`), []string{"OPENAI_API_KEY"})
	settingsPlan := claudeSettingsPlan(home, newSettings, []string{"env.K"})
	settingsPlan.Parser = settingsParser

	c := NewCommitter()
	txn, err := c.Stage(context.Background(), r, []writepath.WritePlan{authPlan, settingsPlan})
	if err != nil {
		t.Fatalf("Stage: %v", err)
	}
	report, cerr := c.Commit(context.Background(), txn)
	if cerr == nil {
		t.Fatalf("Commit: expected *PartialFailure, got nil")
	}
	var pf *PartialFailure
	if !errors.As(cerr, &pf) {
		t.Fatalf("Commit err type = %T; want *PartialFailure", cerr)
	}
	if pf.FailedFile != settingsPath {
		t.Errorf("FailedFile = %q, want %q", pf.FailedFile, settingsPath)
	}
	if !report.RolledBack {
		t.Errorf("report.RolledBack = false, want true")
	}
	// F4: the failing file was AtomicWritten and then rolled back
	// from CurrentBytes — FailingFileRolledBack must be true.
	if !report.FailingFileRolledBack {
		t.Errorf("report.FailingFileRolledBack = false, want true (post-write reparse rollback succeeded)")
	}
	// auth.json was committed first → rollback restored it (first-
	// write case: file removed).
	if _, err := os.Stat(authPath); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("auth.json exists after rollback (should have been removed); err=%v", err)
	}
	// settings.json was staged but rejected → still holds seed bytes.
	got, _ := os.ReadFile(settingsPath)
	if !bytes.Equal(got, seedSettings) {
		t.Errorf("settings.json bytes = %q, want seed %q", got, seedSettings)
	}
	// Per-file statuses.
	authRow, settingsRow := report.PerFile[0], report.PerFile[1]
	if authRow.Status != StatusRolledBack {
		t.Errorf("auth row status = %q, want RolledBack", authRow.Status)
	}
	if settingsRow.Status != StatusFailed {
		t.Errorf("settings row status = %q, want Failed", settingsRow.Status)
	}
}

// TestStage_DuplicateTargetPlansCauseCommitFailure pins the documented
// behavior for callers who pass multiple plans against the same target
// (F3). Stage accepts them; Commit trips concurrent-edit drift on the
// second plan because the first plan's write invalidated the second
// plan's PreFingerprint.
func TestStage_DuplicateTargetPlansCauseCommitFailure(t *testing.T) {
	r, home := newTestHome(t)
	settingsPath := filepath.Join(home, ".claude", "settings.json")
	mkdirParent(t, settingsPath)
	if err := os.WriteFile(settingsPath, []byte(`{"env":{"K":"seed"}}`), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}

	// Two plans on the SAME target with different NewContent — a
	// caller who forgot to dedupe.
	plan1 := claudeSettingsPlan(home, []byte(`{"env":{"K":"first"}}`), []string{"env.K"})
	plan2 := claudeSettingsPlan(home, []byte(`{"env":{"K":"second"}}`), []string{"env.K"})

	c := NewCommitter()
	txn, err := c.Stage(context.Background(), r, []writepath.WritePlan{plan1, plan2})
	if err != nil {
		t.Fatalf("Stage(dup targets): unexpected error %v", err)
	}
	report, cerr := c.Commit(context.Background(), txn)
	if cerr == nil {
		t.Fatalf("Commit(dup targets): expected *PartialFailure, got nil")
	}
	var pf *PartialFailure
	if !errors.As(cerr, &pf) {
		t.Fatalf("Commit err type = %T; want *PartialFailure", cerr)
	}
	// Cause must be ErrConcurrentEdit — the first plan's write drove
	// the second plan's PreFingerprint out of sync.
	if !errors.Is(cerr, writepath.ErrConcurrentEdit) {
		t.Errorf("cause = %v, want ErrConcurrentEdit", cerr)
	}
	// The SECOND plan's target is the failing file (the first plan
	// committed successfully before drift was detected on the second
	// pass against the same target). Both share the same Target so
	// FailedFile == settingsPath.
	if pf.FailedFile != settingsPath {
		t.Errorf("FailedFile = %q, want %q", pf.FailedFile, settingsPath)
	}
	// The FIRST plan (index 0) was committed then rolled back.
	if report.PerFile[0].Status != StatusRolledBack {
		t.Errorf("first plan status = %q, want RolledBack", report.PerFile[0].Status)
	}
	// The SECOND plan (index 1) is the one that failed.
	if report.PerFile[1].Status != StatusFailed {
		t.Errorf("second plan status = %q, want Failed", report.PerFile[1].Status)
	}
}

// TestCommit_SkippedFileDriftDetected verifies F8: a Skipped file
// whose target is mutated externally between Stage and Commit is
// promoted to StatusFailed rather than silently reported as Untouched.
func TestCommit_SkippedFileDriftDetected(t *testing.T) {
	r, home := newTestHome(t)
	settingsPath := filepath.Join(home, ".claude", "settings.json")
	mkdirParent(t, settingsPath)
	body := []byte(`{"env":{"K":"seed"}}`)
	if err := os.WriteFile(settingsPath, body, 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}

	// Stage a plan whose NewContent equals the seed bytes → Skipped.
	plan := claudeSettingsPlan(home, body, []string{"env.K"})
	c := NewCommitter()
	txn, err := c.Stage(context.Background(), r, []writepath.WritePlan{plan})
	if err != nil {
		t.Fatalf("Stage: %v", err)
	}
	if !txn.Prepared[0].Skipped {
		t.Fatalf("expected Prepared[0].Skipped = true (byte-identical)")
	}

	// Simulate a non-claudecm actor mutating the target (bypasses the
	// advisory flock). Sleep briefly so ModTime advances measurably
	// even on filesystems with coarse mtime resolution.
	time.Sleep(20 * time.Millisecond)
	external := []byte(`{"env":{"K":"external"}}`)
	if err := os.WriteFile(settingsPath, external, 0o600); err != nil {
		t.Fatalf("external mutate: %v", err)
	}

	report, cerr := c.Commit(context.Background(), txn)
	if cerr == nil {
		t.Fatalf("Commit: expected *PartialFailure on skipped-file drift, got nil")
	}
	if !errors.Is(cerr, writepath.ErrConcurrentEdit) {
		t.Errorf("cause = %v, want ErrConcurrentEdit", cerr)
	}
	if report.PerFile[0].Status != StatusFailed {
		t.Errorf("skipped-drift status = %q, want Failed", report.PerFile[0].Status)
	}
	if !bytes.Contains([]byte(report.PerFile[0].Error), []byte("external drift on skipped file")) {
		t.Errorf("Error = %q, want to contain %q", report.PerFile[0].Error, "external drift on skipped file")
	}
	// Skipped-drift failure did not write anything, so no rollback
	// of the failing file — FailingFileRolledBack must be false.
	if report.FailingFileRolledBack {
		t.Errorf("FailingFileRolledBack = true, want false (skipped file was never written)")
	}
	// The external bytes remain on disk — commit did not touch them.
	got, _ := os.ReadFile(settingsPath)
	if !bytes.Equal(got, external) {
		t.Errorf("target bytes = %q, want external %q (Skipped path must not write)", got, external)
	}
}

func TestCommit_AllOrNothing(t *testing.T) {
	// Verify: after a mid-commit failure, both files are in either
	// their pre-Stage state or their post-Commit state — no in-
	// between. Uses the PartialFailureRollback scenario and asserts
	// on file contents.
	r, home := newTestHome(t)
	authPath := filepath.Join(home, ".codex", "auth.json")
	settingsPath := filepath.Join(home, ".claude", "settings.json")
	mkdirParent(t, authPath)
	mkdirParent(t, settingsPath)
	seedAuth := []byte(`{"OPENAI_API_KEY":"old"}`)
	seedSettings := []byte(`{"env":{"K":"old"}}`)
	if err := os.WriteFile(authPath, seedAuth, 0o600); err != nil {
		t.Fatalf("seed auth: %v", err)
	}
	if err := os.WriteFile(settingsPath, seedSettings, 0o600); err != nil {
		t.Fatalf("seed settings: %v", err)
	}
	settingsParser := rejectOnNthCallParser(3)
	authPlan := codexAuthPlan(home, []byte(`{"OPENAI_API_KEY":"new"}`), []string{"OPENAI_API_KEY"})
	settingsPlan := claudeSettingsPlan(home, []byte(`{"env":{"K":"fresh"}}`), []string{"env.K"})
	settingsPlan.Parser = settingsParser

	c := NewCommitter()
	txn, err := c.Stage(context.Background(), r, []writepath.WritePlan{authPlan, settingsPlan})
	if err != nil {
		t.Fatalf("Stage: %v", err)
	}
	_, cerr := c.Commit(context.Background(), txn)
	if cerr == nil {
		t.Fatalf("expected PartialFailure, got nil")
	}
	// Both files must equal their pre-Stage bytes (all-or-nothing).
	if got, _ := os.ReadFile(authPath); !bytes.Equal(got, seedAuth) {
		t.Errorf("auth.json = %q, want pre-Stage %q (all-or-nothing violated)", got, seedAuth)
	}
	if got, _ := os.ReadFile(settingsPath); !bytes.Equal(got, seedSettings) {
		t.Errorf("settings.json = %q, want pre-Stage %q (all-or-nothing violated)", got, seedSettings)
	}
}

func TestCommit_ReleasesLocks(t *testing.T) {
	r, home := newTestHome(t)
	settingsPath := filepath.Join(home, ".claude", "settings.json")
	mkdirParent(t, settingsPath)

	plan := claudeSettingsPlan(home, []byte(`{"env":{"K":"V"}}`), []string{"env.K"})
	c := NewCommitter()
	txn, err := c.Stage(context.Background(), r, []writepath.WritePlan{plan})
	if err != nil {
		t.Fatalf("Stage: %v", err)
	}
	if _, err := c.Commit(context.Background(), txn); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	rel, _ := filepath.Rel(home, settingsPath)
	h, aerr := storage.Acquire(r, rel, storage.LockOptions{Timeout: 200 * time.Millisecond})
	if aerr != nil {
		t.Fatalf("post-Commit Acquire failed: %v (locks not released?)", aerr)
	}
	_ = h.Release()
}

// -----------------------------------------------------------------------------
// Abort tests
// -----------------------------------------------------------------------------

func TestAbort_ReleasesLocks(t *testing.T) {
	r, home := newTestHome(t)
	settingsPath := filepath.Join(home, ".claude", "settings.json")
	mkdirParent(t, settingsPath)
	// Seed so a backup fires during Stage.
	if err := os.WriteFile(settingsPath, []byte(`{"env":{"K":"old"}}`), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}

	plan := claudeSettingsPlan(home, []byte(`{"env":{"K":"new"}}`), []string{"env.K"})
	c := NewCommitter()
	txn, err := c.Stage(context.Background(), r, []writepath.WritePlan{plan})
	if err != nil {
		t.Fatalf("Stage: %v", err)
	}
	if err := c.Abort(txn); err != nil {
		t.Fatalf("Abort: %v", err)
	}
	// File must still hold seed bytes.
	got, _ := os.ReadFile(settingsPath)
	if string(got) != `{"env":{"K":"old"}}` {
		t.Errorf("target bytes changed under Abort: got %q", got)
	}
	// Backup file MAY exist on disk — Abort does not delete it.
	// Retention (NFR-R1) prunes eventually. This is the documented
	// behaviour: we keep the pre-write snapshot for audit.
	rel, _ := filepath.Rel(home, settingsPath)
	h, aerr := storage.Acquire(r, rel, storage.LockOptions{Timeout: 200 * time.Millisecond})
	if aerr != nil {
		t.Fatalf("post-Abort Acquire failed: %v (locks not released?)", aerr)
	}
	_ = h.Release()
}

func TestAbort_EmptyTxnNoop(t *testing.T) {
	c := NewCommitter()
	if err := c.Abort(StagedTxn{}); err != nil {
		t.Errorf("Abort(empty): %v", err)
	}
}

// -----------------------------------------------------------------------------
// Contract / auxiliary tests
// -----------------------------------------------------------------------------

func TestStage_ContextCanceledEarly(t *testing.T) {
	r, home := newTestHome(t)
	settingsPath := filepath.Join(home, ".claude", "settings.json")
	mkdirParent(t, settingsPath)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel BEFORE Stage runs.

	plan := claudeSettingsPlan(home, []byte(`{"env":{"K":"V"}}`), []string{"env.K"})
	c := NewCommitter()
	_, err := c.Stage(ctx, r, []writepath.WritePlan{plan})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Stage err = %v, want context.Canceled", err)
	}
}

func TestCommit_ContextCanceledReleasesLocks(t *testing.T) {
	r, home := newTestHome(t)
	settingsPath := filepath.Join(home, ".claude", "settings.json")
	mkdirParent(t, settingsPath)

	plan := claudeSettingsPlan(home, []byte(`{"env":{"K":"V"}}`), []string{"env.K"})
	c := NewCommitter()
	txn, err := c.Stage(context.Background(), r, []writepath.WritePlan{plan})
	if err != nil {
		t.Fatalf("Stage: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, cerr := c.Commit(ctx, txn)
	if !errors.Is(cerr, context.Canceled) {
		t.Fatalf("Commit err = %v, want context.Canceled", cerr)
	}
	// Post-cancel: locks released.
	rel, _ := filepath.Rel(home, settingsPath)
	h, aerr := storage.Acquire(r, rel, storage.LockOptions{Timeout: 200 * time.Millisecond})
	if aerr != nil {
		t.Fatalf("post-cancel Acquire failed: %v", aerr)
	}
	_ = h.Release()
}

// TestWithClock exercises the WithClock option so the option-plumbing
// path is covered. Production paths default to time.Now.
func TestWithClock(t *testing.T) {
	fixed := time.Date(2026, 6, 30, 12, 0, 0, 0, time.UTC)
	c := NewCommitter(WithClock(func() time.Time { return fixed }))
	// Empty-plan path exercises the clock through StagedAt.
	txn, err := c.Stage(context.Background(), nil, nil)
	if err != nil {
		t.Fatalf("Stage: %v", err)
	}
	if !txn.StagedAt.Equal(fixed) {
		t.Errorf("StagedAt = %v, want %v", txn.StagedAt, fixed)
	}
	// Nil clock via WithClock should still be safe (defaults to time.Now).
	c2 := NewCommitter(WithClock(nil))
	if c2 == nil {
		t.Fatal("NewCommitter returned nil under WithClock(nil)")
	}
}

// TestStage_TransformError verifies a Transform failure aborts Stage
// without falling back to NewContent (no-fallback rule).
func TestStage_TransformError(t *testing.T) {
	r, home := newTestHome(t)
	settingsPath := filepath.Join(home, ".claude", "settings.json")
	mkdirParent(t, settingsPath)

	plan := writepath.WritePlan{
		Tool:   string(adapter.ToolClaudeCode),
		Target: settingsPath,
		Transform: func(_ []byte) ([]byte, error) {
			return nil, errors.New("simulated transform failure")
		},
		NewContent: []byte(`{"env":{"K":"unused"}}`), // must be ignored on Transform error
		Parser:     jsonParser,
		OwnedKeys:  []string{"env.K"},
	}

	c := NewCommitter()
	_, err := c.Stage(context.Background(), r, []writepath.WritePlan{plan})
	if err == nil || !bytes.Contains([]byte(err.Error()), []byte("transform")) {
		t.Fatalf("Stage err = %v, want transform failure", err)
	}
	// Target must not exist.
	if _, err := os.Stat(settingsPath); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("target created despite transform error: err=%v", err)
	}
}

// TestStage_MalformedNewBytesRefused covers the parse-new failure path.
func TestStage_MalformedNewBytesRefused(t *testing.T) {
	r, home := newTestHome(t)
	settingsPath := filepath.Join(home, ".claude", "settings.json")
	mkdirParent(t, settingsPath)

	plan := writepath.WritePlan{
		Tool:       string(adapter.ToolClaudeCode),
		Target:     settingsPath,
		NewContent: []byte(`{malformed`),
		Parser:     jsonParser,
		OwnedKeys:  []string{},
	}
	c := NewCommitter()
	_, err := c.Stage(context.Background(), r, []writepath.WritePlan{plan})
	if !errors.Is(err, writepath.ErrParseFailed) {
		t.Fatalf("Stage err = %v, want ErrParseFailed", err)
	}
}

// TestStage_DedupSameTargetLocks verifies two plans pointing at the
// same target result in a single lock, not two.
func TestStage_DedupSameTargetLocks(t *testing.T) {
	r, home := newTestHome(t)
	settingsPath := filepath.Join(home, ".claude", "settings.json")
	mkdirParent(t, settingsPath)

	body := []byte(`{"env":{"K":"V"}}`)
	if err := os.WriteFile(settingsPath, body, 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}
	plan1 := claudeSettingsPlan(home, body, []string{"env.K"})
	plan2 := claudeSettingsPlan(home, body, []string{"env.K"})

	c := NewCommitter()
	txn, err := c.Stage(context.Background(), r, []writepath.WritePlan{plan1, plan2})
	if err != nil {
		t.Fatalf("Stage: %v", err)
	}
	defer func() { _ = c.Abort(txn) }()
	if len(txn.Locks) != 1 {
		t.Errorf("Locks len = %d, want 1 (deduped)", len(txn.Locks))
	}
}

// TestCommit_ConcurrentEditAborts induces external drift between
// Stage and Commit and asserts Commit returns *PartialFailure with
// ErrConcurrentEdit.
func TestCommit_ConcurrentEditAborts(t *testing.T) {
	r, home := newTestHome(t)
	settingsPath := filepath.Join(home, ".claude", "settings.json")
	mkdirParent(t, settingsPath)
	if err := os.WriteFile(settingsPath, []byte(`{"env":{"K":"old"}}`), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}

	plan := claudeSettingsPlan(home, []byte(`{"env":{"K":"new"}}`), []string{"env.K"})
	c := NewCommitter()
	txn, err := c.Stage(context.Background(), r, []writepath.WritePlan{plan})
	if err != nil {
		t.Fatalf("Stage: %v", err)
	}

	// Simulate a non-claudecm actor mutating the file between Stage
	// and Commit. Bump the ModTime by writing new bytes.
	time.Sleep(20 * time.Millisecond)
	if err := os.WriteFile(settingsPath, []byte(`{"env":{"K":"external"}}`), 0o600); err != nil {
		t.Fatalf("external mutate: %v", err)
	}

	report, cerr := c.Commit(context.Background(), txn)
	if cerr == nil {
		t.Fatalf("Commit: want *PartialFailure, got nil")
	}
	if !errors.Is(cerr, writepath.ErrConcurrentEdit) {
		t.Fatalf("Commit err = %v, want ErrConcurrentEdit", cerr)
	}
	// F4: concurrent-edit is detected before AtomicWrite, so no
	// rollback of the failing file was needed — FailingFileRolledBack
	// stays false. Distinguishes this case from the post-write-reparse
	// path where the failing file gets AtomicWritten then restored.
	if report.FailingFileRolledBack {
		t.Errorf("report.FailingFileRolledBack = true, want false (nothing was written)")
	}
}

// TestStage_NilResolverRefused covers the r==nil branch.
func TestStage_NilResolverRefused(t *testing.T) {
	c := NewCommitter()
	plans := []writepath.WritePlan{{
		Tool:       "claude_code",
		Target:     "/home/u/.claude/settings.json",
		NewContent: []byte(`{}`),
	}}
	_, err := c.Stage(context.Background(), nil, plans)
	if !errors.Is(err, writepath.ErrPlanInvalid) {
		t.Fatalf("Stage err = %v, want ErrPlanInvalid (nil resolver)", err)
	}
}

// TestCommit_PartialFailureWithUntouched covers the "not yet visited"
// case: three plans, second one fails, third stays untouched.
func TestCommit_PartialFailureWithUntouched(t *testing.T) {
	r, home := newTestHome(t)
	authPath := filepath.Join(home, ".codex", "auth.json")
	configPath := filepath.Join(home, ".codex", "config.toml")
	settingsPath := filepath.Join(home, ".claude", "settings.json")
	mkdirParent(t, authPath)
	mkdirParent(t, configPath)
	mkdirParent(t, settingsPath)

	// Seed all three so backups + rollbacks are meaningful.
	if err := os.WriteFile(authPath, []byte(`{"OPENAI_API_KEY":"old"}`), 0o600); err != nil {
		t.Fatalf("seed auth: %v", err)
	}
	if err := os.WriteFile(configPath, []byte(`{"model":"old"}`), 0o600); err != nil {
		t.Fatalf("seed config: %v", err)
	}
	if err := os.WriteFile(settingsPath, []byte(`{"env":{"K":"old"}}`), 0o600); err != nil {
		t.Fatalf("seed settings: %v", err)
	}

	// Config plan uses the reject-on-N parser so post-write reparse
	// fails at commit time; settings.json is never visited.
	authPlan := codexAuthPlan(home, []byte(`{"OPENAI_API_KEY":"new"}`), []string{"OPENAI_API_KEY"})
	configPlan := writepath.WritePlan{
		Tool:       string(adapter.ToolCodex),
		Target:     configPath,
		NewContent: []byte(`{"model":"new"}`),
		Parser:     rejectOnNthCallParser(3),
		OwnedKeys:  []string{"model"},
	}
	settingsPlan := claudeSettingsPlan(home, []byte(`{"env":{"K":"new"}}`), []string{"env.K"})

	c := NewCommitter()
	txn, err := c.Stage(context.Background(), r, []writepath.WritePlan{authPlan, configPlan, settingsPlan})
	if err != nil {
		t.Fatalf("Stage: %v", err)
	}
	report, cerr := c.Commit(context.Background(), txn)
	if cerr == nil {
		t.Fatalf("Commit: want PartialFailure, got nil")
	}
	var pf *PartialFailure
	if !errors.As(cerr, &pf) {
		t.Fatalf("Commit err = %T, want *PartialFailure", cerr)
	}
	if len(pf.Untouched) != 1 || pf.Untouched[0] != settingsPath {
		t.Errorf("Untouched = %v, want [%q]", pf.Untouched, settingsPath)
	}
	if len(pf.RolledBack) != 1 || pf.RolledBack[0] != authPath {
		t.Errorf("RolledBack = %v, want [%q]", pf.RolledBack, authPath)
	}
	// F4: config.toml's own post-write reparse rollback ran → true.
	if !report.FailingFileRolledBack {
		t.Errorf("report.FailingFileRolledBack = false, want true")
	}
	// PerFile lookup (Plans-order): [0]=auth, [1]=config, [2]=settings.
	if report.PerFile[0].Status != StatusRolledBack {
		t.Errorf("auth status = %q, want RolledBack", report.PerFile[0].Status)
	}
	if report.PerFile[1].Status != StatusFailed {
		t.Errorf("config status = %q, want Failed", report.PerFile[1].Status)
	}
	if report.PerFile[2].Status != StatusUntouched {
		t.Errorf("settings status = %q, want Untouched", report.PerFile[2].Status)
	}
	// On-disk: all three files must be at their pre-Stage bytes.
	if got, _ := os.ReadFile(authPath); string(got) != `{"OPENAI_API_KEY":"old"}` {
		t.Errorf("auth = %q, want old", got)
	}
	if got, _ := os.ReadFile(configPath); string(got) != `{"model":"old"}` {
		t.Errorf("config = %q, want old", got)
	}
	if got, _ := os.ReadFile(settingsPath); string(got) != `{"env":{"K":"old"}}` {
		t.Errorf("settings = %q, want old", got)
	}
}

func TestCommit_NilResolverRefused(t *testing.T) {
	// A caller that hand-builds a StagedTxn without going through
	// Stage cannot Commit — Commit refuses loudly.
	c := NewCommitter()
	txn := StagedTxn{
		Plans:    []writepath.WritePlan{{Tool: "t", Target: "/tmp/x", NewContent: []byte("x")}},
		Prepared: []PreparedFile{{Plan: writepath.WritePlan{Tool: "t", Target: "/tmp/x"}}},
	}
	_, err := c.Commit(context.Background(), txn)
	if err == nil {
		t.Fatalf("Commit: want error on nil resolver, got nil")
	}
}

// nilOnEmptyParser mirrors codex tomlParser's empty-input policy: it
// returns (nil, nil) for zero-byte input and delegates to jsonParser
// otherwise. Used to reproduce the cmd/switch fresh-install scenario
// where the parser hands writepath a nil parsed value.
var nilOnEmptyParser = writepath.ParserFunc(func(data []byte) (any, error) {
	if len(data) == 0 {
		return nil, nil
	}
	return jsonParser.Parse(data)
})

// TestStage_NilCurrentBytesNoDrift is the commit-level regression for
// E2-FOLLOWUP-flatten-nil. Scenario mirrors cmd/switch on a fresh
// install with the codex TOML parser:
//
//   - Target file exists but is zero bytes (or whitespace-only).
//   - Parser.Parse(current) returns (nil, nil).
//   - New bytes render an owned key.
//
// Before the fix, writepath.Flatten(nil) surfaced a phantom "" key on
// the current side; Diff reported TouchesUnowned=true because "" is
// not in any OwnedKeys allowlist; Stage refused with
// ErrDryRunUnownedTouched. That refusal was what broke cmd/switch
// (PR #45) on fresh installs.
//
// After the fix, Flatten(nil) → empty map; Diff sees Added=["a"] with
// "a" in OwnedKeys; TouchesUnowned=false; Stage proceeds cleanly and
// the plan is queued for Commit.
func TestStage_NilCurrentBytesNoDrift(t *testing.T) {
	r, home := newTestHome(t)
	settingsPath := filepath.Join(home, ".claude", "settings.json")
	mkdirParent(t, settingsPath)
	// Seed the target as an existing but zero-byte file so the Stage
	// pipeline takes the exists=true branch AND the parser returns
	// (nil, nil) — the exact shape codex tomlParser produces for an
	// empty ~/.codex/config.toml on a fresh install.
	if err := os.WriteFile(settingsPath, []byte{}, 0o600); err != nil {
		t.Fatalf("seed zero-byte file: %v", err)
	}

	plan := writepath.WritePlan{
		Tool:       string(adapter.ToolClaudeCode),
		Target:     settingsPath,
		NewContent: []byte(`{"a":1}`),
		Parser:     nilOnEmptyParser,
		OwnedKeys:  []string{"a"},
	}

	c := NewCommitter()
	txn, err := c.Stage(context.Background(), r, []writepath.WritePlan{plan})
	if err != nil {
		t.Fatalf("Stage: %v (want clean stage; a Flatten(nil) regression would surface ErrDryRunUnownedTouched here)", err)
	}
	defer func() { _ = c.Abort(txn) }()

	if len(txn.Prepared) != 1 {
		t.Fatalf("Prepared len = %d, want 1", len(txn.Prepared))
	}
	pf := txn.Prepared[0]
	if pf.Skipped {
		t.Errorf("Skipped = true, want false (bytes differ from zero-byte current)")
	}
	if pf.Diff.TouchesUnowned {
		t.Errorf("Diff.TouchesUnowned = true; want false (a is in OwnedKeys, no phantom '' key on current)")
	}
	if _, hasEmpty := indexOf(pf.Diff.Added, ""); hasEmpty {
		t.Errorf("Diff.Added contains empty-string key: %v (Flatten(nil) regression)", pf.Diff.Added)
	}
	wantAdded := []string{"a"}
	if len(pf.Diff.Added) != 1 || pf.Diff.Added[0] != wantAdded[0] {
		t.Errorf("Diff.Added = %v; want %v", pf.Diff.Added, wantAdded)
	}
}

// indexOf reports whether s contains needle. Local helper so the
// regression test above stays self-contained.
func indexOf(s []string, needle string) (int, bool) {
	for i, v := range s {
		if v == needle {
			return i, true
		}
	}
	return -1, false
}
