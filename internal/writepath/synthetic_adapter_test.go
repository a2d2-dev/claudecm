// synthetic_adapter_test.go declares the E2-S5 test-only "synthetic
// adapter" — a caller-shaped helper used exclusively by matrix_test.go
// to drive writepath.Apply end-to-end. It exists so the FR-5 pipeline
// can be exercised from something that looks like a real adapter
// (E3/E4 will land the real Claude Code / Codex adapters) without
// dragging any real Claude Code / Codex parsing into internal/writepath.
//
// Why this file lives inside package writepath and not under
// testsupport/: keeping it in the same package sidesteps a new
// production-visible package (source-tree §… lists exactly what lives
// under internal/writepath — a new dir would need an ADR). The
// _test.go suffix means the compiler never links it into non-test
// binaries, so CODING-STANDARDS rule 12 (no runtime mutable package
// state in production) is preserved.
//
// Contract with matrix_test.go:
//
//   - SyntheticAdapter mirrors the shape of every field WritePlan
//     accepts today. BuildPlan is pure; Apply forwards to
//     writepath.Apply so a matrix row can seed→BuildPlan→Apply→assert
//     from one struct literal.
//
//   - Target is stored relative to the resolver's HOME so a row does
//     not have to know the t.TempDir path at declaration time; BuildPlan
//     joins it against r.Home() at call time.
//
//   - NewJSONParser wraps encoding/json as a writepath.Parser. It is
//     the only parser the matrix rows use; new parsers should be
//     added here rather than duplicated across rows.
//
// This helper is deliberately test-only (file has _test.go suffix).
// Do NOT depend on it from production code. If E3/E4 need an
// adapter-shaped struct they should declare their own in
// internal/adapter/... — the shape here is chosen for test ergonomics,
// not API stability.

package writepath

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/a2d2-dev/claudecm/internal/storage"
)

// SyntheticAdapter is a fixture-shaped stand-in for the real adapters
// that E3/E4 will land. Every field mirrors a WritePlan field except
// Target, which is HOME-relative here and joined against r.Home() by
// BuildPlan. That way a matrix row's struct literal is self-contained
// (no filepath.Join(home, ...) in the row body) and portable across
// t.TempDir instances.
//
// The helper is intentionally dumb: it holds inputs, it forwards to
// Apply. Any "smarts" (parse choice, transform authoring, per-row
// content builders) belong in the matrix row, not here. Adding
// behavior to SyntheticAdapter risks it drifting into a de-facto
// adapter API that future readers might confuse with the real E3/E4
// packages.
type SyntheticAdapter struct {
	Tool         string
	Target       string // HOME-relative; joined against r.Home() by BuildPlan.
	OwnedKeys    []string
	Parser       Parser
	Transform    Transform
	NewContent   []byte
	DryRun       bool
	AllowUnowned bool
	MustNotExist bool
	Reason       string
}

// BuildPlan returns the WritePlan the adapter would hand to
// writepath.Apply. Split out so tests can inspect the plan (e.g. assert
// AllowUnowned on a row that overrides it) without invoking Apply.
func (a *SyntheticAdapter) BuildPlan(r *storage.Resolver) WritePlan {
	return WritePlan{
		Tool:         a.Tool,
		Target:       filepath.Join(r.Home(), a.Target),
		Transform:    a.Transform,
		NewContent:   a.NewContent,
		Parser:       a.Parser,
		OwnedKeys:    a.OwnedKeys,
		Reason:       a.Reason,
		DryRun:       a.DryRun,
		AllowUnowned: a.AllowUnowned,
		MustNotExist: a.MustNotExist,
	}
}

// Apply forwards to writepath.Apply with the plan BuildPlan produces.
// Kept as a one-line wrapper so a matrix row can call
// adapter.Apply(ctx, r) instead of writepath.Apply(ctx, r, adapter.BuildPlan(r)).
func (a *SyntheticAdapter) Apply(ctx context.Context, r *storage.Resolver) (WriteReport, error) {
	return Apply(ctx, r, a.BuildPlan(r))
}

// NewJSONParser returns a Parser that decodes JSON bytes into a Go
// value via encoding/json. Used by every matrix row; a stateful
// variant (statefulJSONParser) lives in matrix_test.go for the E8
// reparse-failure row.
func NewJSONParser() Parser {
	return ParserFunc(func(data []byte) (any, error) {
		var v any
		if err := json.Unmarshal(data, &v); err != nil {
			return nil, err
		}
		return v, nil
	})
}

// seedFile creates parent dirs (0700) and writes bytes at 0600. Used
// to establish the pre-write state for a matrix row that expects an
// existing target. Errors fatally via t.Fatalf — a seed failure is a
// test-harness bug, not a subject-under-test bug.
func seedFile(t *testing.T, path string, contents []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("seedFile mkdir parent %q: %v", path, err)
	}
	if err := os.WriteFile(path, contents, 0o600); err != nil {
		t.Fatalf("seedFile write %q: %v", path, err)
	}
}

// readFileSHA returns the SHA256 of the file at path as a hex string.
// Used by matrix rows that assert "on-disk bytes are unchanged" without
// wanting to compare the raw bytes (some rows care about identity, not
// content). Returns "" when the file does not exist so callers can
// distinguish "target absent" from "target has zero-byte hash".
func readFileSHA(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return ""
		}
		t.Fatalf("readFileSHA %q: %v", path, err)
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}
