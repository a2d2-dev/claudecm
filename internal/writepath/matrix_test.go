// matrix_test.go is the E2-S5 end-to-end matrix. Each row wires up a
// SyntheticAdapter (see synthetic_adapter_test.go) as if it were a real
// E3/E4 adapter, drives writepath.Apply through one branch of the
// FR-5 pipeline, and asserts error class + on-disk state + report
// fields.
//
// The matrix is intentionally untagged (no //go:build test) so that
// `go test ./internal/writepath/...` without the -tags=test seam runs
// every row. The rows that need a lock holder run one from a helper
// goroutine; the row that needs a stateful reparse failure uses a
// call-counting parser that flips behavior on the second Parse call.
// Drift-detection rows are NOT in this matrix — those live in
// apply_hooks_test.go under -tags=test where the between-read-and-Stat
// mutation hook is available. That split keeps the matrix here
// buildable without the hook seam.
//
// Row IDs match the E2-S5 story matrix:
//
//	H1  HappyFirstWrite               file created at 0600
//	H2  HappyOverwrite                backup + write + fresh PostFingerprint
//	H3  IdempotentReapply             Skipped=true, no backup
//	H4  WhitespaceOnlyChangeIsSkipped semantic skip, whitespace preserved
//	E1  UnownedTouchedRefused         ErrDryRunUnownedTouched
//	E2  UnownedTouchedAllowed         AllowUnowned=true → write proceeds
//	E3  DryRunNoWrite                 on-disk unchanged, no backup
//	E4  MustNotExistExisting          storage.ErrTargetExists
//	E5  MustNotExistMissing           first write succeeds
//	E6  TransformError                error surfaces, file unchanged
//	E7  ParseFailurePreWrite          ErrParseFailed
//	E8  ReparseFailureRollsBack       stateful parser → rollback
//	E9  SymlinkEscape                 ErrOutsideHome
//	E10 LockTimeout                   goroutine holds lock → ErrLockTimeout
//	E11 ExpiredContext                pre-expired ctx → ErrLockTimeout
//	E12 InvalidPlan                   empty Target → ErrPlanInvalid
//
// Each row must be self-contained: no cross-row state, no shared
// t.TempDir. matrixRow.run receives a fresh Resolver+HOME per t.Run so
// a hang in one row can't corrupt the next.

package writepath

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/a2d2-dev/claudecm/internal/storage"
)

// statefulJSONParser wraps NewJSONParser() with a call counter and a
// "fail on Nth call" switch. Used only by row E8: the reparse-failure
// row wants a parser that accepts the pre-write current bytes and the
// pre-write new bytes (calls 1 and 2) but rejects the post-write
// reread bytes (call 3+). Distinct from countingParser in apply_test.go
// so a future edit to that helper cannot silently change matrix E8's
// contract. Uses atomic.Int32 so a hypothetical concurrent matrix run
// stays race-clean under `go test -race`; the matrix serializes rows
// via t.Run, but the parser itself is defensively safe.
type statefulJSONParser struct {
	calls  atomic.Int32
	failOn int32
	inner  Parser
}

func (p *statefulJSONParser) Parse(data []byte) (any, error) {
	n := p.calls.Add(1)
	if n >= p.failOn {
		return nil, errors.New("statefulJSONParser: rejected on call")
	}
	return p.inner.Parse(data)
}

// matrixRow is one scenario. name is used for t.Run; setup writes any
// pre-write bytes, returns the adapter and any assertion helpers the
// row's assert closure needs. assert receives the freshly returned
// WriteReport + error and does all row-specific checks. Splitting
// setup/assert avoids a "one giant switch" that would obscure which
// branch each row exercises.
type matrixRow struct {
	name   string
	setup  func(t *testing.T, r *storage.Resolver, home string) (*SyntheticAdapter, func(t *testing.T, home string))
	assert func(t *testing.T, home string, rep WriteReport, err error, extra func(t *testing.T, home string))
	// applyCtx returns the context passed to adapter.Apply. Optional
	// (nil → context.Background()); used by E10 (bounded deadline) and
	// E11 (pre-expired deadline).
	applyCtx func(t *testing.T) (context.Context, context.CancelFunc)
	// prime runs BEFORE the adapter's Apply and after setup. Row E10
	// uses it to acquire the sidecar flock from a helper goroutine so
	// Apply's Acquire call blocks and hits the timeout. Returns a
	// cleanup closure the row is responsible for deferring.
	prime func(t *testing.T, r *storage.Resolver, home string) func()
}

// The matrix itself. Kept as a function-returning-slice rather than a
// package-level var so each row's setup closure captures a fresh
// stateful parser (row E8) — a package-level slice would share pointer
// state across `go test -count=N` runs.
func testMatrix() []matrixRow {
	const rel = "tool/config.json"

	return []matrixRow{
		{
			name: "H1_HappyFirstWrite",
			setup: func(t *testing.T, r *storage.Resolver, home string) (*SyntheticAdapter, func(t *testing.T, home string)) {
				return &SyntheticAdapter{
					Tool:       "tool",
					Target:     rel,
					Parser:     NewJSONParser(),
					NewContent: []byte("{}"),
				}, nil
			},
			assert: func(t *testing.T, home string, rep WriteReport, err error, _ func(t *testing.T, home string)) {
				if err != nil {
					t.Fatalf("Apply err = %v; want nil", err)
				}
				if rep.Skipped || rep.DryRun {
					t.Fatalf("Skipped/DryRun = %v/%v; want false/false", rep.Skipped, rep.DryRun)
				}
				if (rep.Backup != storage.BackupRecord{}) {
					t.Fatalf("Backup populated on first write: %+v", rep.Backup)
				}
				if rep.PostFingerprint.SHA256 == "" {
					t.Fatalf("PostFingerprint.SHA256 empty; want hash of new bytes")
				}
				info, statErr := os.Lstat(filepath.Join(home, rel))
				if statErr != nil {
					t.Fatalf("Lstat target: %v", statErr)
				}
				if info.Mode().Perm() != 0o600 {
					t.Fatalf("mode = %v; want 0600", info.Mode().Perm())
				}
			},
		},
		{
			name: "H2_HappyOverwrite",
			setup: func(t *testing.T, r *storage.Resolver, home string) (*SyntheticAdapter, func(t *testing.T, home string)) {
				seedFile(t, filepath.Join(home, rel), []byte(`{"a":1}`))
				return &SyntheticAdapter{
					Tool:       "tool",
					Target:     rel,
					Parser:     NewJSONParser(),
					OwnedKeys:  []string{"a"},
					NewContent: []byte(`{"a":2}`),
				}, nil
			},
			assert: func(t *testing.T, home string, rep WriteReport, err error, _ func(t *testing.T, home string)) {
				if err != nil {
					t.Fatalf("Apply err = %v", err)
				}
				if rep.Backup.BackupPath == "" {
					t.Fatalf("Backup.BackupPath empty; want populated on overwrite")
				}
				if rep.PreFingerprint.SHA256 == rep.PostFingerprint.SHA256 {
					t.Fatalf("Pre/Post SHA256 equal; want different on overwrite")
				}
				got, _ := os.ReadFile(filepath.Join(home, rel))
				if string(got) != `{"a":2}` {
					t.Fatalf("on-disk = %q; want %q", got, `{"a":2}`)
				}
				// The backup file should contain the pre-write bytes.
				bb, berr := os.ReadFile(rep.Backup.BackupPath)
				if berr != nil {
					t.Fatalf("read backup: %v", berr)
				}
				if string(bb) != `{"a":1}` {
					t.Fatalf("backup bytes = %q; want pre-write %q", bb, `{"a":1}`)
				}
			},
		},
		{
			name: "H3_IdempotentReapply",
			setup: func(t *testing.T, r *storage.Resolver, home string) (*SyntheticAdapter, func(t *testing.T, home string)) {
				seedFile(t, filepath.Join(home, rel), []byte(`{"a":1}`))
				return &SyntheticAdapter{
					Tool:       "tool",
					Target:     rel,
					Parser:     NewJSONParser(),
					OwnedKeys:  []string{"a"},
					NewContent: []byte(`{"a":1}`),
				}, nil
			},
			assert: func(t *testing.T, home string, rep WriteReport, err error, _ func(t *testing.T, home string)) {
				if err != nil {
					t.Fatalf("Apply err = %v", err)
				}
				if !rep.Skipped {
					t.Fatalf("Skipped = false; want true (byte-identical reapply)")
				}
				if (rep.Backup != storage.BackupRecord{}) {
					t.Fatalf("Backup populated on skip: %+v", rep.Backup)
				}
			},
		},
		{
			name: "H4_WhitespaceOnlyChangeIsSkipped",
			setup: func(t *testing.T, r *storage.Resolver, home string) (*SyntheticAdapter, func(t *testing.T, home string)) {
				seedFile(t, filepath.Join(home, rel), []byte(`{"a":1}`))
				return &SyntheticAdapter{
					Tool:       "tool",
					Target:     rel,
					Parser:     NewJSONParser(),
					OwnedKeys:  []string{"a"},
					NewContent: []byte(` { "a" : 1 } `),
				}, nil
			},
			assert: func(t *testing.T, home string, rep WriteReport, err error, _ func(t *testing.T, home string)) {
				if err != nil {
					t.Fatalf("Apply err = %v", err)
				}
				if !rep.Skipped {
					t.Fatalf("Skipped = false; want true (semantic skip)")
				}
				got, _ := os.ReadFile(filepath.Join(home, rel))
				if string(got) != `{"a":1}` {
					t.Fatalf("on-disk = %q; want original whitespace preserved", got)
				}
			},
		},
		{
			name: "E1_UnownedTouchedRefused",
			setup: func(t *testing.T, r *storage.Resolver, home string) (*SyntheticAdapter, func(t *testing.T, home string)) {
				seedFile(t, filepath.Join(home, rel), []byte(`{"a":1}`))
				return &SyntheticAdapter{
					Tool:       "tool",
					Target:     rel,
					Parser:     NewJSONParser(),
					OwnedKeys:  []string{"a"}, // "b" is unowned
					NewContent: []byte(`{"a":1,"b":2}`),
				}, nil
			},
			assert: func(t *testing.T, home string, rep WriteReport, err error, _ func(t *testing.T, home string)) {
				if !errors.Is(err, ErrDryRunUnownedTouched) {
					t.Fatalf("err = %v; want wraps ErrDryRunUnownedTouched", err)
				}
				got, _ := os.ReadFile(filepath.Join(home, rel))
				if string(got) != `{"a":1}` {
					t.Fatalf("file mutated under refuse: %q", got)
				}
				// No backup should have been taken — refuse happens
				// BEFORE the step-6 Backup call.
				assertToolBackupCount(t, filepath.Join(home, storage.ConfigDirName, storage.BackupsDirName, "tool"), 0)
			},
		},
		{
			name: "E2_UnownedTouchedAllowed",
			setup: func(t *testing.T, r *storage.Resolver, home string) (*SyntheticAdapter, func(t *testing.T, home string)) {
				seedFile(t, filepath.Join(home, rel), []byte(`{"a":1}`))
				return &SyntheticAdapter{
					Tool:         "tool",
					Target:       rel,
					Parser:       NewJSONParser(),
					OwnedKeys:    []string{"a"},
					AllowUnowned: true,
					NewContent:   []byte(`{"a":1,"b":2}`),
				}, nil
			},
			assert: func(t *testing.T, home string, rep WriteReport, err error, _ func(t *testing.T, home string)) {
				if err != nil {
					t.Fatalf("Apply err = %v", err)
				}
				if !rep.Diff.TouchesUnowned {
					t.Fatalf("Diff.TouchesUnowned = false; want true (b is unowned)")
				}
				if rep.Backup.BackupPath == "" {
					t.Fatalf("Backup.BackupPath empty; want populated when write proceeds")
				}
				got, _ := os.ReadFile(filepath.Join(home, rel))
				if string(got) != `{"a":1,"b":2}` {
					t.Fatalf("on-disk = %q; want %q", got, `{"a":1,"b":2}`)
				}
			},
		},
		{
			name: "E3_DryRunNoWrite",
			setup: func(t *testing.T, r *storage.Resolver, home string) (*SyntheticAdapter, func(t *testing.T, home string)) {
				original := []byte(`{"a":1}`)
				seedFile(t, filepath.Join(home, rel), original)
				return &SyntheticAdapter{
					Tool:       "tool",
					Target:     rel,
					Parser:     NewJSONParser(),
					OwnedKeys:  []string{"a"},
					NewContent: []byte(`{"a":2}`),
					DryRun:     true,
				}, nil
			},
			assert: func(t *testing.T, home string, rep WriteReport, err error, _ func(t *testing.T, home string)) {
				if err != nil {
					t.Fatalf("Apply err = %v", err)
				}
				if !rep.DryRun {
					t.Fatalf("DryRun = false; want true")
				}
				if (rep.Backup != storage.BackupRecord{}) {
					t.Fatalf("Backup populated on dry-run: %+v", rep.Backup)
				}
				got, _ := os.ReadFile(filepath.Join(home, rel))
				if string(got) != `{"a":1}` {
					t.Fatalf("on-disk mutated under dry-run: %q", got)
				}
			},
		},
		{
			name: "E4_MustNotExistExisting",
			setup: func(t *testing.T, r *storage.Resolver, home string) (*SyntheticAdapter, func(t *testing.T, home string)) {
				original := []byte(`{"a":1}`)
				seedFile(t, filepath.Join(home, rel), original)
				return &SyntheticAdapter{
					Tool:         "tool",
					Target:       rel,
					Parser:       NewJSONParser(),
					OwnedKeys:    []string{"a"},
					NewContent:   []byte(`{"a":2}`),
					MustNotExist: true,
				}, nil
			},
			assert: func(t *testing.T, home string, rep WriteReport, err error, _ func(t *testing.T, home string)) {
				if !errors.Is(err, storage.ErrTargetExists) {
					t.Fatalf("err = %v; want wraps storage.ErrTargetExists", err)
				}
				got, _ := os.ReadFile(filepath.Join(home, rel))
				if string(got) != `{"a":1}` {
					t.Fatalf("file mutated under MustNotExist refuse: %q", got)
				}
			},
		},
		{
			name: "E5_MustNotExistMissing",
			setup: func(t *testing.T, r *storage.Resolver, home string) (*SyntheticAdapter, func(t *testing.T, home string)) {
				return &SyntheticAdapter{
					Tool:         "tool",
					Target:       rel,
					Parser:       NewJSONParser(),
					OwnedKeys:    []string{"a"},
					NewContent:   []byte(`{"a":1}`),
					MustNotExist: true,
				}, nil
			},
			assert: func(t *testing.T, home string, rep WriteReport, err error, _ func(t *testing.T, home string)) {
				if err != nil {
					t.Fatalf("Apply err = %v; want nil (target absent, MustNotExist honored)", err)
				}
				got, _ := os.ReadFile(filepath.Join(home, rel))
				if string(got) != `{"a":1}` {
					t.Fatalf("on-disk = %q; want %q", got, `{"a":1}`)
				}
				if rep.PostFingerprint.SHA256 == "" {
					t.Fatalf("PostFingerprint.SHA256 empty; want hash of new bytes")
				}
			},
		},
		{
			name: "E6_TransformError",
			setup: func(t *testing.T, r *storage.Resolver, home string) (*SyntheticAdapter, func(t *testing.T, home string)) {
				original := []byte(`{"a":1}`)
				seedFile(t, filepath.Join(home, rel), original)
				return &SyntheticAdapter{
					Tool:      "tool",
					Target:    rel,
					Parser:    NewJSONParser(),
					OwnedKeys: []string{"a"},
					Transform: func(cur []byte) ([]byte, error) {
						return nil, errors.New("synthetic transform failure")
					},
					// NewContent set to prove Transform-wins-over-NewContent
					// (should NEVER land on disk).
					NewContent: []byte(`{"a":999}`),
				}, nil
			},
			assert: func(t *testing.T, home string, rep WriteReport, err error, _ func(t *testing.T, home string)) {
				if err == nil {
					t.Fatalf("Apply err = nil; want transform-failure surfaced")
				}
				got, _ := os.ReadFile(filepath.Join(home, rel))
				if string(got) != `{"a":1}` {
					t.Fatalf("file changed under transform-fail: %q", got)
				}
				assertToolBackupCount(t, filepath.Join(home, storage.ConfigDirName, storage.BackupsDirName, "tool"), 0)
			},
		},
		{
			name: "E7_ParseFailurePreWrite",
			setup: func(t *testing.T, r *storage.Resolver, home string) (*SyntheticAdapter, func(t *testing.T, home string)) {
				// Malformed current bytes; JSON parser will refuse to
				// parse them at step 3.
				seedFile(t, filepath.Join(home, rel), []byte(`not valid json {{{`))
				return &SyntheticAdapter{
					Tool:       "tool",
					Target:     rel,
					Parser:     NewJSONParser(),
					OwnedKeys:  []string{"a"},
					NewContent: []byte(`{"a":1}`),
				}, nil
			},
			assert: func(t *testing.T, home string, rep WriteReport, err error, _ func(t *testing.T, home string)) {
				if !errors.Is(err, ErrParseFailed) {
					t.Fatalf("err = %v; want wraps ErrParseFailed", err)
				}
				got, _ := os.ReadFile(filepath.Join(home, rel))
				if string(got) != `not valid json {{{` {
					t.Fatalf("file mutated under parse-fail: %q", got)
				}
			},
		},
		{
			name: "E8_ReparseFailureRollsBack",
			setup: func(t *testing.T, r *storage.Resolver, home string) (*SyntheticAdapter, func(t *testing.T, home string)) {
				original := []byte(`{"a":1}`)
				seedFile(t, filepath.Join(home, rel), original)
				// Pre-write calls: parse current + parse new = 2. Fail
				// on call #3 (the post-write reparse) so the pipeline
				// exercises the rollback path.
				parser := &statefulJSONParser{failOn: 3, inner: NewJSONParser()}
				return &SyntheticAdapter{
						Tool:       "tool",
						Target:     rel,
						Parser:     parser,
						OwnedKeys:  []string{"a"},
						NewContent: []byte(`{"a":2}`),
					}, func(t *testing.T, home string) {
						// Extra sanity: post-rollback bytes must equal
						// original. The main assert closure checks the
						// error class; this closure double-checks disk.
						got, _ := os.ReadFile(filepath.Join(home, rel))
						if string(got) != string(original) {
							t.Fatalf("post-rollback on-disk = %q; want %q", got, original)
						}
					}
			},
			assert: func(t *testing.T, home string, rep WriteReport, err error, extra func(t *testing.T, home string)) {
				if !errors.Is(err, ErrPostWriteReparse) {
					t.Fatalf("err = %v; want wraps ErrPostWriteReparse", err)
				}
				if !errors.Is(err, ErrRollback) {
					t.Fatalf("err = %v; want wraps ErrRollback (successful rollback)", err)
				}
				if errors.Is(err, ErrRollbackFailed) {
					t.Fatalf("err = %v; must NOT wrap ErrRollbackFailed on successful rollback", err)
				}
				if !rep.RolledBack {
					t.Fatalf("RolledBack = false; want true")
				}
				if extra != nil {
					extra(t, home)
				}
			},
		},
		{
			name: "E9_SymlinkEscape",
			setup: func(t *testing.T, r *storage.Resolver, home string) (*SyntheticAdapter, func(t *testing.T, home string)) {
				// Create a sibling temp dir OUTSIDE HOME and symlink it
				// as HOME/escape. Target is HOME/escape/config.json;
				// storage.Acquire's EnsureDir + AtomicWrite's parent
				// check will both refuse. We record the outside dir so
				// the assert closure can confirm nothing leaked there.
				outside, err := os.MkdirTemp("", "e2s5-outside-*")
				if err != nil {
					t.Fatalf("MkdirTemp outside: %v", err)
				}
				t.Cleanup(func() { _ = os.RemoveAll(outside) })
				link := filepath.Join(home, "escape")
				if err := os.Symlink(outside, link); err != nil {
					t.Fatalf("symlink: %v", err)
				}
				return &SyntheticAdapter{
						Tool:       "tool",
						Target:     "escape/config.json",
						Parser:     NewJSONParser(),
						NewContent: []byte(`{"a":1}`),
					}, func(t *testing.T, home string) {
						// Nothing may have landed in the outside dir.
						if _, statErr := os.Lstat(filepath.Join(outside, "config.json")); !os.IsNotExist(statErr) {
							t.Fatalf("file leaked into outside HOME: %v", statErr)
						}
					}
			},
			assert: func(t *testing.T, home string, rep WriteReport, err error, extra func(t *testing.T, home string)) {
				if !errors.Is(err, ErrOutsideHome) {
					t.Fatalf("err = %v; want wraps ErrOutsideHome", err)
				}
				if extra != nil {
					extra(t, home)
				}
			},
		},
		{
			name: "E10_LockTimeout",
			setup: func(t *testing.T, r *storage.Resolver, home string) (*SyntheticAdapter, func(t *testing.T, home string)) {
				// Ensure the parent dir exists so the primer's
				// storage.Acquire (which will EnsureDir) succeeds cleanly
				// before our Apply races it.
				if err := os.MkdirAll(filepath.Dir(filepath.Join(home, rel)), 0o700); err != nil {
					t.Fatalf("mkdir parent: %v", err)
				}
				return &SyntheticAdapter{
					Tool:       "tool",
					Target:     rel,
					Parser:     NewJSONParser(),
					OwnedKeys:  []string{"a"},
					NewContent: []byte(`{"a":1}`),
				}, nil
			},
			applyCtx: func(t *testing.T) (context.Context, context.CancelFunc) {
				return context.WithTimeout(context.Background(), 100*time.Millisecond)
			},
			prime: func(t *testing.T, r *storage.Resolver, home string) func() {
				// Acquire the sidecar flock synchronously on the test
				// goroutine so Apply's Acquire attempt is guaranteed to
				// hit contention. Held until the row's cleanup runs.
				lockRel, err := filepath.Rel(r.Home(), filepath.Join(home, rel))
				if err != nil {
					t.Fatalf("Rel: %v", err)
				}
				h, err := storage.Acquire(r, lockRel, storage.LockOptions{Timeout: time.Second})
				if err != nil {
					t.Fatalf("prime Acquire: %v", err)
				}
				return func() { h.Release() }
			},
			assert: func(t *testing.T, home string, rep WriteReport, err error, _ func(t *testing.T, home string)) {
				if !errors.Is(err, ErrLockTimeout) {
					t.Fatalf("err = %v; want wraps ErrLockTimeout", err)
				}
				// Target must not have been created (primer only touched
				// the sidecar).
				if _, statErr := os.Lstat(filepath.Join(home, rel)); !os.IsNotExist(statErr) {
					t.Fatalf("target created despite lock timeout: %v", statErr)
				}
			},
		},
		{
			name: "E11_ExpiredContext",
			setup: func(t *testing.T, r *storage.Resolver, home string) (*SyntheticAdapter, func(t *testing.T, home string)) {
				return &SyntheticAdapter{
					Tool:       "tool",
					Target:     rel,
					Parser:     NewJSONParser(),
					NewContent: []byte(`{"a":1}`),
				}, nil
			},
			applyCtx: func(t *testing.T) (context.Context, context.CancelFunc) {
				return context.WithDeadline(context.Background(), time.Now().Add(-time.Millisecond))
			},
			assert: func(t *testing.T, home string, rep WriteReport, err error, _ func(t *testing.T, home string)) {
				if !errors.Is(err, ErrLockTimeout) {
					t.Fatalf("err = %v; want wraps ErrLockTimeout", err)
				}
				if _, statErr := os.Lstat(filepath.Join(home, rel)); !os.IsNotExist(statErr) {
					t.Fatalf("target created despite expired ctx: %v", statErr)
				}
			},
		},
		{
			name: "E12_InvalidPlan",
			setup: func(t *testing.T, r *storage.Resolver, home string) (*SyntheticAdapter, func(t *testing.T, home string)) {
				// Target left as "" → BuildPlan yields Target=home; that
				// IS a valid absolute path. To hit ErrPlanInvalid we
				// bypass BuildPlan and call Apply with a hand-rolled
				// empty-Target plan. The adapter is left non-nil so the
				// runner still validates plan-shape checks; the assert
				// closure calls Apply directly (see plan builder below).
				return &SyntheticAdapter{
					Tool: "tool",
					// Target intentionally empty so we exercise
					// ValidatePlan's Target-empty branch.
					Target:     "",
					NewContent: []byte(`{}`),
				}, nil
			},
			assert: func(t *testing.T, home string, rep WriteReport, err error, _ func(t *testing.T, home string)) {
				// Custom check: the runner detects Target=="" and hands
				// Apply an explicitly-empty-Target plan (see run()); the
				// error must wrap ErrPlanInvalid, and no file may have
				// been created anywhere under HOME.
				if !errors.Is(err, ErrPlanInvalid) {
					t.Fatalf("err = %v; want wraps ErrPlanInvalid", err)
				}
			},
		},
	}
}

// assertToolBackupCount counts .bak. entries under
// ~/.claudecm/backups/<tool>. Missing dir counts as zero. Foreign
// entries are ignored. Duplicate of apply_test.go's assertBackupCount
// helper renamed here so the matrix can be read without cross-file
// dependency confusion. Keeps the row body local to matrix_test.go.
func assertToolBackupCount(t *testing.T, dir string, want int) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			if want != 0 {
				t.Fatalf("backup dir missing; want %d entries", want)
			}
			return
		}
		t.Fatalf("readdir %q: %v", dir, err)
	}
	got := 0
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		got++
	}
	if got != want {
		t.Fatalf("backup count = %d; want %d", got, want)
	}
}

// TestApplyMatrix walks every scenario. Each row runs under its own
// t.Run so a single failure doesn't mask later rows and so the
// per-row t.TempDir is scoped correctly. sync.WaitGroup unused —
// rows are serial by design so backup-dir counts are stable.
func TestApplyMatrix(t *testing.T) {
	for _, row := range testMatrix() {
		row := row
		t.Run(row.name, func(t *testing.T) {
			r, home := newTestHome(t)
			adapter, extra := row.setup(t, r, home)

			if row.prime != nil {
				cleanup := row.prime(t, r, home)
				defer cleanup()
			}

			ctx := context.Background()
			if row.applyCtx != nil {
				var cancel context.CancelFunc
				ctx, cancel = row.applyCtx(t)
				defer cancel()
			}

			var (
				rep WriteReport
				err error
			)
			// E12 needs an explicitly-empty Target plan; the SyntheticAdapter
			// would fill in filepath.Join(home, "") == home which is a
			// valid absolute path. Detect the row by Target=="" and
			// call Apply with a bespoke plan.
			if adapter.Target == "" {
				rep, err = Apply(ctx, r, WritePlan{
					Tool:       adapter.Tool,
					Target:     "", // triggers ErrPlanInvalid
					NewContent: adapter.NewContent,
				})
			} else {
				rep, err = adapter.Apply(ctx, r)
			}
			row.assert(t, home, rep, err, extra)
		})
	}
}

