// Package commit tests (E7-S1 + E7-S2/S3).
//
// The E7-S1 rows here are compile-time shape tests: they pin the
// interface, the method signatures, the FileStatus enum values, the
// PartialFailure error contract, and the canonical commit order.
// The E7-S2/S3 behavioural tests for Stage/Commit/Abort live in
// commit_e2e_test.go so this file remains a stable type-shape gate.
package commit

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/a2d2-dev/claudecm/internal/adapter"
	"github.com/a2d2-dev/claudecm/internal/storage"
	"github.com/a2d2-dev/claudecm/internal/writepath"
)

// Package-level compile-time assertions. Kept at package scope (not
// inside a test) so a future removal of a method or a type-shape
// change breaks the build immediately rather than only failing at
// runtime.
var (
	// *PartialFailure satisfies the error interface.
	_ error = (*PartialFailure)(nil)

	// *committer satisfies the Committer interface.
	_ Committer = (*committer)(nil)
)

// TestCommit_TypesCompile pins the shape of every exported type by
// constructing a value of each. If any type is renamed, removed, or
// changes shape, this test will fail to compile — which is exactly
// the E7-S1 acceptance gate.
func TestCommit_TypesCompile(t *testing.T) {
	// StagedTxn.
	txn := StagedTxn{
		Plans:    []writepath.WritePlan{},
		Locks:    []LockHandle{{Target: "/tmp/x", handle: nil}},
		Prepared: []PreparedFile{},
		StagedAt: time.Now(),
	}
	if txn.StagedAt.IsZero() {
		t.Fatalf("StagedAt was zero")
	}

	// PreparedFile.
	pf := PreparedFile{
		Plan:           writepath.WritePlan{},
		CurrentBytes:   nil,
		NewBytes:       nil,
		PreFingerprint: storage.Fingerprint{},
		Backup:         storage.BackupRecord{},
		Diff:           writepath.DiffResult{},
		Skipped:        true,
	}
	if !pf.Skipped {
		t.Fatalf("Skipped field lost")
	}

	// CommitReport.
	report := CommitReport{
		PerFile: []PerFileReport{{
			Target: "/tmp/x",
			Status: StatusUntouched,
			Backup: storage.BackupRecord{},
			Report: writepath.WriteReport{},
			Error:  "",
		}},
		CommittedAt: time.Now(),
		RolledBack:  false,
	}
	if len(report.PerFile) != 1 {
		t.Fatalf("PerFile length lost")
	}

	// Runtime sanity check that NewCommitter returns non-nil. The
	// compile-time Committer contract is pinned by the package-level
	// var _ Committer = (*stubCommitter)(nil) assertion above.
	if NewCommitter() == nil {
		t.Fatalf("NewCommitter returned nil")
	}
}

// TestCommit_ZeroPlansStageOK exercises the acceptance-criterion
// edge row: zero-plan input → Stage returns an empty transaction and
// Commit against that transaction is a no-op.
func TestCommit_ZeroPlansStageOK(t *testing.T) {
	c := NewCommitter()
	ctx := context.Background()

	txn, err := c.Stage(ctx, nil, nil)
	if err != nil {
		t.Fatalf("Stage(nil plans): unexpected error %v", err)
	}
	if len(txn.Plans) != 0 || len(txn.Prepared) != 0 || len(txn.Locks) != 0 {
		t.Fatalf("Stage(nil plans): expected empty StagedTxn, got %+v", txn)
	}

	// Same shape with an explicit empty slice.
	txn2, err := c.Stage(ctx, nil, []writepath.WritePlan{})
	if err != nil {
		t.Fatalf("Stage([]): unexpected error %v", err)
	}
	if len(txn2.Plans) != 0 || len(txn2.Prepared) != 0 || len(txn2.Locks) != 0 {
		t.Fatalf("Stage([]): expected empty StagedTxn, got %+v", txn2)
	}

	// Commit of an empty StagedTxn is a no-op.
	rep, err := c.Commit(ctx, txn)
	if err != nil {
		t.Fatalf("Commit(empty): unexpected error %v", err)
	}
	if len(rep.PerFile) != 0 || rep.RolledBack {
		t.Fatalf("Commit(empty): expected empty CommitReport, got %+v", rep)
	}

	// Abort of an empty StagedTxn is a no-op.
	if err := c.Abort(txn); err != nil {
		t.Fatalf("Abort(empty): unexpected error %v", err)
	}
}

// TestPartialFailure_ErrorImplemented pins the PartialFailure type as
// an error and its Unwrap contract.
func TestPartialFailure_ErrorImplemented(t *testing.T) {
	cause := errors.New("underlying failure")
	pf := &PartialFailure{
		Report:     CommitReport{},
		FailedFile: "/tmp/x",
		Cause:      cause,
		RolledBack: []string{"/tmp/y"},
		Untouched:  []string{"/tmp/z"},
	}

	// Compile-time assertion: *PartialFailure satisfies error. Kept
	// as a package-level var below (var _ error = (*PartialFailure)(nil))
	// so removing the type's Error method breaks the build. Here we
	// exercise the runtime path via errors.Is / errors.Unwrap.

	// errors.Is reaches through Unwrap to Cause.
	if !errors.Is(pf, cause) {
		t.Fatalf("errors.Is(pf, cause): want true, got false")
	}

	// errors.Unwrap returns Cause verbatim.
	if got := errors.Unwrap(pf); got != cause {
		t.Fatalf("Unwrap: want %v, got %v", cause, got)
	}

	// Nil-receiver defensive path.
	var nilPF *PartialFailure
	if got := nilPF.Error(); got == "" {
		t.Fatalf("nil PartialFailure.Error() should not return empty string")
	}
	if got := nilPF.Unwrap(); got != nil {
		t.Fatalf("nil PartialFailure.Unwrap() should return nil, got %v", got)
	}
}

// TestPartialFailure_MessageFormat pins the exact Error() string
// shape. cmd/*'s error-rendering path parses / matches on this
// format; changing it is a wire-visible break.
func TestPartialFailure_MessageFormat(t *testing.T) {
	cause := errors.New("boom")
	pf := &PartialFailure{
		FailedFile: "/home/u/.claude/settings.json",
		Cause:      cause,
		RolledBack: []string{"/a", "/b"},
		Untouched:  []string{"/c"},
	}
	want := `commit: partial failure on "/home/u/.claude/settings.json": boom; rolled back 2, untouched 1`
	if got := pf.Error(); got != want {
		t.Fatalf("Error(): mismatch\n want: %q\n  got: %q", want, got)
	}
}

// TestFileStatus_Values pins the FileStatus enum wire values.
// Renderers grep for these exact strings; changing one is a
// grep-visible break in operator tooling.
func TestFileStatus_Values(t *testing.T) {
	cases := []struct {
		name string
		got  FileStatus
		want string
	}{
		{"StatusCommitted", StatusCommitted, "committed"},
		{"StatusRolledBack", StatusRolledBack, "rolled-back"},
		{"StatusUntouched", StatusUntouched, "untouched"},
		{"StatusFailed", StatusFailed, "failed"},
	}
	for _, tc := range cases {
		if string(tc.got) != tc.want {
			t.Errorf("%s: want %q, got %q", tc.name, tc.want, string(tc.got))
		}
	}
}

// TestCanonicalCommitOrder_HappyBoth verifies the full three-file
// happy-path order: codex auth.json → codex config.toml → claude_code
// settings.json, regardless of caller-supplied order.
func TestCanonicalCommitOrder_HappyBoth(t *testing.T) {
	// Deliberately reversed to prove the sort actually runs.
	plans := []writepath.WritePlan{
		{Tool: "claude_code", Target: "/home/u/.claude/settings.json"},
		{Tool: "codex", Target: "/home/u/.codex/config.toml"},
		{Tool: "codex", Target: "/home/u/.codex/auth.json"},
	}
	got := canonicalCommitOrder(plans)
	wantOrder := []string{
		"/home/u/.codex/auth.json",
		"/home/u/.codex/config.toml",
		"/home/u/.claude/settings.json",
	}
	if len(got) != len(wantOrder) {
		t.Fatalf("canonicalCommitOrder: length mismatch, want %d got %d", len(wantOrder), len(got))
	}
	for i, idx := range got {
		if plans[idx].Target != wantOrder[i] {
			t.Errorf("position %d: want %q, got %q", i, wantOrder[i], plans[idx].Target)
		}
	}
}

// TestCanonicalCommitOrder_ClaudeCodeOnly exercises the single-tool
// case: no Codex plans, one Claude Code plan.
func TestCanonicalCommitOrder_ClaudeCodeOnly(t *testing.T) {
	plans := []writepath.WritePlan{
		{Tool: "claude_code", Target: "/home/u/.claude/settings.json"},
	}
	got := canonicalCommitOrder(plans)
	if len(got) != 1 || got[0] != 0 {
		t.Fatalf("canonicalCommitOrder: single Claude Code plan should stay at index 0, got %v", got)
	}
}

// TestCanonicalCommitOrder_UnknownToolLast verifies that a plan for an
// unrecognized tool sorts into bucket 3 (last) while preserving stable
// ordering relative to any other bucket-3 plans.
func TestCanonicalCommitOrder_UnknownToolLast(t *testing.T) {
	plans := []writepath.WritePlan{
		{Tool: "mystery", Target: "/home/u/.mystery/config.yaml"},
		{Tool: "codex", Target: "/home/u/.codex/auth.json"},
		{Tool: "claude_code", Target: "/home/u/.claude/settings.json"},
		{Tool: "codex", Target: "/home/u/.codex/config.toml"},
	}
	got := canonicalCommitOrder(plans)
	// Expected commit order:
	//   codex auth.json (idx 1)
	//   codex config.toml (idx 3)
	//   claude_code settings.json (idx 2)
	//   mystery (idx 0) — bucket 3, last
	want := []int{1, 3, 2, 0}
	if len(got) != len(want) {
		t.Fatalf("length mismatch, want %d got %d", len(want), len(got))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("position %d: want plan[%d] (%q), got plan[%d] (%q)",
				i, want[i], plans[want[i]].Target, got[i], plans[got[i]].Target)
		}
	}
}

// TestCanonicalCommitOrder_EmptyInput verifies the trivial edge case
// does not panic and returns an empty slice — this matches how Stage
// treats zero-plan input.
func TestCanonicalCommitOrder_EmptyInput(t *testing.T) {
	got := canonicalCommitOrder(nil)
	if len(got) != 0 {
		t.Fatalf("expected empty slice for nil input, got %v", got)
	}
	got = canonicalCommitOrder([]writepath.WritePlan{})
	if len(got) != 0 {
		t.Fatalf("expected empty slice for empty input, got %v", got)
	}
}

// TestCommitPriority_TracksAdapterConstants pins that the commit-order
// routing keys off the typed adapter.ToolCodex / adapter.ToolClaudeCode
// constants rather than raw string literals. If either constant's
// underlying string value is renamed in a future adapter refactor,
// commitPriority would silently reroute Codex or Claude Code writes
// into the "unknown" bucket 3, breaking the auth-first ordering. This
// test uses the constants directly so a drift in either literal fails
// here at test time (and, in most refactor patterns, at compile time
// via the adapter constant rename itself).
func TestCommitPriority_TracksAdapterConstants(t *testing.T) {
	auth := writepath.WritePlan{
		Tool:   string(adapter.ToolCodex),
		Target: "/home/u/.codex/auth.json",
	}
	if got := commitPriority(auth); got != 0 {
		t.Errorf("codex auth.json: want priority 0, got %d — adapter.ToolCodex value drifted?", got)
	}

	config := writepath.WritePlan{
		Tool:   string(adapter.ToolCodex),
		Target: "/home/u/.codex/config.toml",
	}
	if got := commitPriority(config); got != 1 {
		t.Errorf("codex config.toml: want priority 1, got %d — adapter.ToolCodex value drifted?", got)
	}

	settings := writepath.WritePlan{
		Tool:   string(adapter.ToolClaudeCode),
		Target: "/home/u/.claude/settings.json",
	}
	if got := commitPriority(settings); got != 2 {
		t.Errorf("claude_code settings.json: want priority 2, got %d — adapter.ToolClaudeCode value drifted?", got)
	}

	// Sanity: an explicitly unknown ToolID lands in bucket 3.
	unknown := writepath.WritePlan{
		Tool:   "not-a-real-tool",
		Target: "/home/u/.mystery/config.yaml",
	}
	if got := commitPriority(unknown); got != 3 {
		t.Errorf("unknown tool: want priority 3, got %d", got)
	}
}

// TestCanonicalCommitOrder_UnknownBasenameSameTool verifies that a
// codex plan whose basename is neither auth.json nor config.toml
// (defensive edge — should not happen in v1) falls into bucket 3.
func TestCanonicalCommitOrder_UnknownBasenameSameTool(t *testing.T) {
	plans := []writepath.WritePlan{
		{Tool: "codex", Target: "/home/u/.codex/mystery.yaml"},
		{Tool: "codex", Target: "/home/u/.codex/auth.json"},
	}
	got := canonicalCommitOrder(plans)
	want := []int{1, 0}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("position %d: want %d, got %d", i, want[i], got[i])
		}
	}
}
