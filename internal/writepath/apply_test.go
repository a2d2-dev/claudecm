package writepath

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/a2d2-dev/claudecm/internal/storage"
)

// jsonParser is the standard-library JSON parser wrapped as a
// writepath.Parser. Used across Apply tests to exercise the Parse path
// without pulling in an adapter package that does not yet exist.
var jsonParser = ParserFunc(func(data []byte) (any, error) {
	var v any
	if err := json.Unmarshal(data, &v); err != nil {
		return nil, err
	}
	return v, nil
})

// newTestHome constructs a Resolver bound to a fresh t.TempDir HOME,
// runs Bootstrap so ~/.claudecm/{profiles,backups} exist at 0700, and
// returns the Resolver + the resolved HOME path for convenience.
func newTestHome(t *testing.T) (*storage.Resolver, string) {
	t.Helper()
	home := t.TempDir()
	r, err := storage.NewResolverWithHome(home)
	if err != nil {
		t.Fatalf("NewResolverWithHome err = %v", err)
	}
	if err := storage.Bootstrap(r); err != nil {
		t.Fatalf("Bootstrap err = %v", err)
	}
	return r, home
}

// ensureParent creates the parent directory of p at 0700. Callers
// should use this when the target lives outside the Bootstrap-created
// tree (i.e. anywhere but ~/.claudecm/*). Without a pre-existing
// parent, storage.Acquire's EnsureDir would create it too — but making
// it here keeps tests explicit about intended layout.
func ensureParent(t *testing.T, p string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		t.Fatalf("mkdir parent %q: %v", p, err)
	}
}

func TestApply_HappyFirstWrite(t *testing.T) {
	// AC: given no prior file, Transform runs on empty input, atomic
	// publish creates the target at 0600, PreFingerprint reports
	// exists=false via zero-value, Backup receipt is zero.
	r, home := newTestHome(t)
	target := filepath.Join(home, "tool", "config.json")
	ensureParent(t, target)

	plan := WritePlan{
		Tool:   "tool",
		Target: target,
		Transform: func(cur []byte) ([]byte, error) {
			if len(cur) != 0 {
				t.Fatalf("Transform got non-empty current: %q", cur)
			}
			return []byte(`{"model":"opus"}`), nil
		},
		Parser:    jsonParser,
		OwnedKeys: []string{"model"},
	}
	report, err := Apply(context.Background(), r, plan)
	if err != nil {
		t.Fatalf("Apply err = %v", err)
	}
	if report.Skipped || report.DryRun {
		t.Fatalf("Skipped/DryRun = %v/%v; want false/false", report.Skipped, report.DryRun)
	}
	if (report.PreFingerprint != storage.Fingerprint{}) {
		t.Fatalf("PreFingerprint = %+v; want zero (file did not exist)", report.PreFingerprint)
	}
	if report.PostFingerprint.SHA256 == "" {
		t.Fatalf("PostFingerprint.SHA256 empty; want hash of new bytes")
	}
	if (report.Backup != storage.BackupRecord{}) {
		t.Fatalf("Backup = %+v; want zero (nothing to back up on first write)", report.Backup)
	}
	// File on disk must exist at 0600.
	info, err := os.Lstat(target)
	if err != nil {
		t.Fatalf("Lstat target: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %v; want 0600", info.Mode().Perm())
	}
	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(got) != `{"model":"opus"}` {
		t.Fatalf("bytes = %q; want %q", got, `{"model":"opus"}`)
	}
}

func TestApply_HappyOverwrite(t *testing.T) {
	// AC: prior file exists → backup captured, new bytes published,
	// PreFingerprint != PostFingerprint, on-disk bytes match new.
	r, home := newTestHome(t)
	target := filepath.Join(home, "tool", "config.json")
	ensureParent(t, target)
	if err := os.WriteFile(target, []byte(`{"model":"sonnet"}`), 0o600); err != nil {
		t.Fatalf("seed target: %v", err)
	}

	plan := WritePlan{
		Tool:   "tool",
		Target: target,
		Transform: func(cur []byte) ([]byte, error) {
			return []byte(`{"model":"opus"}`), nil
		},
		Parser:    jsonParser,
		OwnedKeys: []string{"model"},
	}
	report, err := Apply(context.Background(), r, plan)
	if err != nil {
		t.Fatalf("Apply err = %v", err)
	}
	if report.Backup.BackupPath == "" {
		t.Fatalf("Backup.BackupPath empty; want populated")
	}
	if report.PreFingerprint.SHA256 == report.PostFingerprint.SHA256 {
		t.Fatalf("PreFingerprint.SHA256 == PostFingerprint.SHA256; want different")
	}
	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(got) != `{"model":"opus"}` {
		t.Fatalf("bytes = %q; want %q", got, `{"model":"opus"}`)
	}
}

func TestApply_SkipsIdenticalBytes(t *testing.T) {
	// AC: Transform returns bytes identical to current → Skipped=true,
	// no backup, PostFingerprint == PreFingerprint.
	r, home := newTestHome(t)
	target := filepath.Join(home, "tool", "config.json")
	ensureParent(t, target)
	initial := []byte(`{"model":"opus"}`)
	if err := os.WriteFile(target, initial, 0o600); err != nil {
		t.Fatalf("seed target: %v", err)
	}

	plan := WritePlan{
		Tool:   "tool",
		Target: target,
		Transform: func(cur []byte) ([]byte, error) {
			return append([]byte(nil), cur...), nil // byte-identical
		},
		Parser:    jsonParser,
		OwnedKeys: []string{"model"},
	}
	report, err := Apply(context.Background(), r, plan)
	if err != nil {
		t.Fatalf("Apply err = %v", err)
	}
	if !report.Skipped {
		t.Fatalf("Skipped = false; want true")
	}
	if (report.Backup != storage.BackupRecord{}) {
		t.Fatalf("Backup = %+v; want zero on skip", report.Backup)
	}
	if report.PreFingerprint != report.PostFingerprint {
		t.Fatalf("Pre != Post on skip: %+v vs %+v", report.PreFingerprint, report.PostFingerprint)
	}
	// Confirm the file wasn't rewritten (no backup dir entries).
	assertBackupCount(t, r, "tool", 0)
}

func TestApply_DryRunNoWrite(t *testing.T) {
	// AC: DryRun=true → file on disk is untouched, Diff populated,
	// Backup zero.
	r, home := newTestHome(t)
	target := filepath.Join(home, "tool", "config.json")
	ensureParent(t, target)
	initial := []byte(`{"model":"sonnet"}`)
	if err := os.WriteFile(target, initial, 0o600); err != nil {
		t.Fatalf("seed target: %v", err)
	}

	plan := WritePlan{
		Tool:       "tool",
		Target:     target,
		NewContent: []byte(`{"model":"opus"}`),
		Parser:     jsonParser,
		OwnedKeys:  []string{"model"},
		DryRun:     true,
	}
	report, err := Apply(context.Background(), r, plan)
	if err != nil {
		t.Fatalf("Apply err = %v", err)
	}
	if !report.DryRun {
		t.Fatalf("DryRun = false; want true")
	}
	if (report.Backup != storage.BackupRecord{}) {
		t.Fatalf("Backup populated on dry-run: %+v", report.Backup)
	}
	if len(report.Diff.Changed) == 0 && len(report.Diff.Added) == 0 && len(report.Diff.Removed) == 0 {
		t.Fatalf("Diff empty on dry-run; want populated")
	}
	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if !reflect.DeepEqual(got, initial) {
		t.Fatalf("on-disk changed under dry-run: %q vs %q", got, initial)
	}
	assertBackupCount(t, r, "tool", 0)
}

func TestApply_LockTimeout(t *testing.T) {
	// AC: another goroutine holds the sidecar lock → Apply with a
	// short context deadline returns writepath.ErrLockTimeout.
	r, home := newTestHome(t)
	target := filepath.Join(home, "tool", "config.json")
	ensureParent(t, target)

	rel, err := filepath.Rel(home, target)
	if err != nil {
		t.Fatalf("Rel: %v", err)
	}
	h, err := storage.Acquire(r, rel, storage.LockOptions{Timeout: time.Second})
	if err != nil {
		t.Fatalf("prime Acquire: %v", err)
	}
	defer h.Release()

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	plan := WritePlan{
		Tool:       "tool",
		Target:     target,
		NewContent: []byte(`{"x":1}`),
		Parser:     jsonParser,
		OwnedKeys:  []string{"x"},
	}
	_, err = Apply(ctx, r, plan)
	if !errors.Is(err, ErrLockTimeout) {
		t.Fatalf("err = %v; want wraps ErrLockTimeout", err)
	}
}

func TestApply_ParseFailurePreWrite(t *testing.T) {
	// AC: Parser rejects the current on-disk bytes → ErrParseFailed
	// wraps the parser error, target is untouched, no backup taken.
	r, home := newTestHome(t)
	target := filepath.Join(home, "tool", "config.json")
	ensureParent(t, target)
	orig := []byte(`not valid json {{{`)
	if err := os.WriteFile(target, orig, 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}

	plan := WritePlan{
		Tool:       "tool",
		Target:     target,
		NewContent: []byte(`{"x":1}`),
		Parser:     jsonParser,
		OwnedKeys:  []string{"x"},
	}
	_, err := Apply(context.Background(), r, plan)
	if !errors.Is(err, ErrParseFailed) {
		t.Fatalf("err = %v; want wraps ErrParseFailed", err)
	}
	got, _ := os.ReadFile(target)
	if !reflect.DeepEqual(got, orig) {
		t.Fatalf("file changed under parse-fail: %q vs %q", got, orig)
	}
	assertBackupCount(t, r, "tool", 0)
}

func TestApply_TransformErrorAborts(t *testing.T) {
	// AC: Transform returning an error propagates the error and does
	// NOT fall back to NewContent. File untouched; no backup written.
	r, home := newTestHome(t)
	target := filepath.Join(home, "tool", "config.json")
	ensureParent(t, target)
	orig := []byte(`{"model":"sonnet"}`)
	if err := os.WriteFile(target, orig, 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}

	boom := errors.New("boom")
	plan := WritePlan{
		Tool:       "tool",
		Target:     target,
		NewContent: []byte(`{"model":"opus"}`), // MUST NOT be used
		Transform: func(cur []byte) ([]byte, error) {
			return nil, boom
		},
		Parser:    jsonParser,
		OwnedKeys: []string{"model"},
	}
	_, err := Apply(context.Background(), r, plan)
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v; want wraps boom", err)
	}
	got, _ := os.ReadFile(target)
	if !reflect.DeepEqual(got, orig) {
		t.Fatalf("file changed under transform-fail: %q vs %q", got, orig)
	}
	assertBackupCount(t, r, "tool", 0)
}

func TestApply_UnownedTouchedWithoutOptIn(t *testing.T) {
	// AC: Diff reports TouchesUnowned=true, AllowUnowned=false,
	// DryRun=false → ErrDryRunUnownedTouched, file untouched, no
	// backup written.
	r, home := newTestHome(t)
	target := filepath.Join(home, "tool", "config.json")
	ensureParent(t, target)
	orig := []byte(`{"model":"sonnet"}`)
	if err := os.WriteFile(target, orig, 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}

	plan := WritePlan{
		Tool:       "tool",
		Target:     target,
		NewContent: []byte(`{"model":"opus","extra":"unowned"}`),
		Parser:     jsonParser,
		OwnedKeys:  []string{"model"}, // "extra" is unowned
	}
	_, err := Apply(context.Background(), r, plan)
	if !errors.Is(err, ErrDryRunUnownedTouched) {
		t.Fatalf("err = %v; want wraps ErrDryRunUnownedTouched", err)
	}
	got, _ := os.ReadFile(target)
	if !reflect.DeepEqual(got, orig) {
		t.Fatalf("file changed under unowned-refuse: %q vs %q", got, orig)
	}
	assertBackupCount(t, r, "tool", 0)
}

func TestApply_UnownedTouchedWithOptIn(t *testing.T) {
	// AC: same diff, AllowUnowned=true → write proceeds normally.
	r, home := newTestHome(t)
	target := filepath.Join(home, "tool", "config.json")
	ensureParent(t, target)
	orig := []byte(`{"model":"sonnet"}`)
	if err := os.WriteFile(target, orig, 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}

	newBytes := []byte(`{"model":"opus","extra":"unowned"}`)
	plan := WritePlan{
		Tool:         "tool",
		Target:       target,
		NewContent:   newBytes,
		Parser:       jsonParser,
		OwnedKeys:    []string{"model"},
		AllowUnowned: true,
	}
	report, err := Apply(context.Background(), r, plan)
	if err != nil {
		t.Fatalf("Apply err = %v", err)
	}
	if !report.Diff.TouchesUnowned {
		t.Fatalf("Diff.TouchesUnowned = false; want true")
	}
	got, _ := os.ReadFile(target)
	if !reflect.DeepEqual(got, newBytes) {
		t.Fatalf("bytes = %q; want %q", got, newBytes)
	}
	assertBackupCount(t, r, "tool", 1)
}

func TestApply_SymlinkEscape(t *testing.T) {
	// AC: target's parent is a symlink pointing outside HOME →
	// ErrOutsideHome. Target inside HOME must NOT be created.
	r, home := newTestHome(t)
	outside := t.TempDir()
	link := filepath.Join(home, "escape")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	target := filepath.Join(link, "config.json")

	plan := WritePlan{
		Tool:       "tool",
		Target:     target,
		NewContent: []byte(`{"x":1}`),
		Parser:     jsonParser,
	}
	_, err := Apply(context.Background(), r, plan)
	if !errors.Is(err, ErrOutsideHome) {
		t.Fatalf("err = %v; want wraps ErrOutsideHome", err)
	}
	// Nothing landed inside HOME under the (still-symlink) escape dir.
	if _, statErr := os.Lstat(filepath.Join(outside, "config.json")); !os.IsNotExist(statErr) {
		t.Fatalf("file leaked into outside HOME: %v", statErr)
	}
}

func TestApply_MustNotExistExisting(t *testing.T) {
	// AC: MustNotExist=true against a pre-existing target → error
	// surfacing storage.ErrTargetExists via errors.Is; file unchanged.
	r, home := newTestHome(t)
	target := filepath.Join(home, "tool", "config.json")
	ensureParent(t, target)
	orig := []byte(`{"a":1}`)
	if err := os.WriteFile(target, orig, 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}

	plan := WritePlan{
		Tool:         "tool",
		Target:       target,
		NewContent:   []byte(`{"a":2}`),
		Parser:       jsonParser,
		OwnedKeys:    []string{"a"},
		MustNotExist: true,
	}
	_, err := Apply(context.Background(), r, plan)
	if !errors.Is(err, storage.ErrTargetExists) {
		t.Fatalf("err = %v; want wraps storage.ErrTargetExists", err)
	}
	got, _ := os.ReadFile(target)
	if !reflect.DeepEqual(got, orig) {
		t.Fatalf("file mutated under MustNotExist refuse: %q vs %q", got, orig)
	}
}

func TestApply_ValidatesPlan(t *testing.T) {
	// AC: invalid plan (empty Target) → ErrPlanInvalid, no I/O.
	// "No I/O" is asserted by using a nil resolver deliberately: if
	// validation ran first, we never reach the resolver. If we did
	// reach the resolver, the panic on nil-dereference in downstream
	// storage calls would fail the test loudly instead of silently.
	_, err := Apply(context.Background(), nil, WritePlan{Tool: "tool" /* Target: "" */})
	if !errors.Is(err, ErrPlanInvalid) {
		t.Fatalf("err = %v; want wraps ErrPlanInvalid", err)
	}
}

func TestApply_BackupThenAtomicWrite(t *testing.T) {
	// AC: after a successful overwrite, the backups dir contains one
	// entry whose bytes match the pre-write state.
	r, home := newTestHome(t)
	target := filepath.Join(home, "tool", "config.json")
	ensureParent(t, target)
	orig := []byte(`{"model":"sonnet"}`)
	if err := os.WriteFile(target, orig, 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}

	plan := WritePlan{
		Tool:       "tool",
		Target:     target,
		NewContent: []byte(`{"model":"opus"}`),
		Parser:     jsonParser,
		OwnedKeys:  []string{"model"},
	}
	report, err := Apply(context.Background(), r, plan)
	if err != nil {
		t.Fatalf("Apply err = %v", err)
	}
	if report.Backup.BackupPath == "" {
		t.Fatalf("Backup.BackupPath empty; want populated")
	}
	backupBytes, err := os.ReadFile(report.Backup.BackupPath)
	if err != nil {
		t.Fatalf("read backup: %v", err)
	}
	if !reflect.DeepEqual(backupBytes, orig) {
		t.Fatalf("backup bytes = %q; want %q", backupBytes, orig)
	}
	assertBackupCount(t, r, "tool", 1)
}

func TestApply_NilContextDefaultsToBackground(t *testing.T) {
	// Nil ctx is coerced to context.Background so downstream deadline
	// checks don't nil-deref. Included for coverage.
	r, home := newTestHome(t)
	target := filepath.Join(home, "tool", "config.json")
	ensureParent(t, target)

	plan := WritePlan{
		Tool:       "tool",
		Target:     target,
		NewContent: []byte(`{"x":1}`),
		Parser:     jsonParser,
		OwnedKeys:  []string{"x"},
	}
	//nolint:staticcheck // deliberately passing a nil ctx to exercise the guard.
	if _, err := Apply(nil, r, plan); err != nil {
		t.Fatalf("Apply(nil ctx) err = %v", err)
	}
}

func TestApply_NoParserSkipsDiffAndSkipsOnByteIdentity(t *testing.T) {
	// When Parser is nil, Diff is empty and the only skip trigger is
	// byte identity. Two cases in one to cover both branches:
	//  (a) bytes differ → write proceeds (Skipped=false).
	//  (b) bytes identical → Skipped=true.
	r, home := newTestHome(t)
	target := filepath.Join(home, "tool", "config.txt")
	ensureParent(t, target)
	if err := os.WriteFile(target, []byte("alpha"), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}

	// (a) bytes differ.
	rep, err := Apply(context.Background(), r, WritePlan{
		Tool:       "tool",
		Target:     target,
		NewContent: []byte("beta"),
	})
	if err != nil {
		t.Fatalf("Apply differ err = %v", err)
	}
	if rep.Skipped {
		t.Fatalf("Skipped = true on byte-differ; want false")
	}
	// (b) bytes identical to what we just wrote.
	rep2, err := Apply(context.Background(), r, WritePlan{
		Tool:       "tool",
		Target:     target,
		NewContent: []byte("beta"),
	})
	if err != nil {
		t.Fatalf("Apply identical err = %v", err)
	}
	if !rep2.Skipped {
		t.Fatalf("Skipped = false on byte-identical; want true")
	}
}

// TestApply_ContextAlreadyExpired pins F1: an already-expired context
// deadline must short-circuit with ErrLockTimeout before ever calling
// storage.Acquire — no ~5s DefaultLockTimeout fall-back. Timing bound is
// intentionally generous (100ms) to stay stable on shared CI runners.
func TestApply_ContextAlreadyExpired(t *testing.T) {
	r, home := newTestHome(t)
	target := filepath.Join(home, "tool", "config.json")
	ensureParent(t, target)

	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Millisecond))
	defer cancel()

	plan := WritePlan{
		Tool:       "tool",
		Target:     target,
		NewContent: []byte(`{"x":1}`),
		Parser:     jsonParser,
		OwnedKeys:  []string{"x"},
	}
	start := time.Now()
	_, err := Apply(ctx, r, plan)
	elapsed := time.Since(start)
	if !errors.Is(err, ErrLockTimeout) {
		t.Fatalf("err = %v; want wraps ErrLockTimeout", err)
	}
	if !errors.Is(err, ctx.Err()) {
		t.Fatalf("err = %v; want also wraps ctx.Err() = %v", err, ctx.Err())
	}
	if elapsed > 100*time.Millisecond {
		t.Fatalf("Apply took %v for expired ctx; want <100ms (no DefaultLockTimeout fallback)", elapsed)
	}
	// File must not have been created.
	if _, statErr := os.Lstat(target); !os.IsNotExist(statErr) {
		t.Fatalf("target created despite expired ctx: %v", statErr)
	}
}

// TestApply_FirstWriteEmptyDocPublishesFile pins F2: a first write of an
// empty document ({}) with a Parser MUST still publish the file at 0600
// even though the parsed diff against the "nothing" side computes as
// empty (curFlat starts empty; Flatten({}) is also empty).
func TestApply_FirstWriteEmptyDocPublishesFile(t *testing.T) {
	r, home := newTestHome(t)
	target := filepath.Join(home, "tool", "config.json")
	ensureParent(t, target)

	plan := WritePlan{
		Tool:       "tool",
		Target:     target,
		NewContent: []byte("{}"),
		Parser:     jsonParser,
		// OwnedKeys deliberately empty — nothing touched inside the doc.
	}
	report, err := Apply(context.Background(), r, plan)
	if err != nil {
		t.Fatalf("Apply err = %v", err)
	}
	if report.Skipped {
		t.Fatalf("Skipped = true on first write of empty doc; want false")
	}
	info, err := os.Lstat(target)
	if err != nil {
		t.Fatalf("Lstat target: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %v; want 0600", info.Mode().Perm())
	}
	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(got) != "{}" {
		t.Fatalf("bytes = %q; want %q", got, "{}")
	}
}

// TestApply_FirstWriteWithParserAndNewContentPublishes is the F4
// companion to F2 — the exact shape F2 fixed. Would have failed against
// the pre-F2 skip guard (bytesEqual=false, diffEmpty=true, no exists
// gate). Kept alongside the F2 test as a redundant tripwire.
func TestApply_FirstWriteWithParserAndNewContentPublishes(t *testing.T) {
	r, home := newTestHome(t)
	target := filepath.Join(home, "tool", "settings.json")
	ensureParent(t, target)

	plan := WritePlan{
		Tool:       "tool",
		Target:     target,
		NewContent: []byte("{}"),
		Parser:     jsonParser,
	}
	report, err := Apply(context.Background(), r, plan)
	if err != nil {
		t.Fatalf("Apply err = %v", err)
	}
	if report.Skipped {
		t.Fatalf("Skipped = true; want false (first write must publish)")
	}
	if report.PostFingerprint.SHA256 == "" {
		t.Fatalf("PostFingerprint.SHA256 empty; want hash of new bytes")
	}
	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(got) != "{}" {
		t.Fatalf("bytes = %q; want %q", got, "{}")
	}
}

// TestApply_ParsedIdenticalDespiteWhitespaceIsSkipped pins the semantic-
// skip claim in the header: parser-equal values skip the write even
// when the raw bytes differ. Whitespace on disk must remain untouched.
func TestApply_ParsedIdenticalDespiteWhitespaceIsSkipped(t *testing.T) {
	r, home := newTestHome(t)
	target := filepath.Join(home, "tool", "config.json")
	ensureParent(t, target)
	orig := []byte(`{"a": 1}`)
	if err := os.WriteFile(target, orig, 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}

	plan := WritePlan{
		Tool:       "tool",
		Target:     target,
		NewContent: []byte(`{ "a" : 1 }`), // extra whitespace, same semantic
		Parser:     jsonParser,
		OwnedKeys:  []string{"a"},
	}
	report, err := Apply(context.Background(), r, plan)
	if err != nil {
		t.Fatalf("Apply err = %v", err)
	}
	if !report.Skipped {
		t.Fatalf("Skipped = false; want true (parsed values are equal)")
	}
	if (report.Backup != storage.BackupRecord{}) {
		t.Fatalf("Backup = %+v; want zero on semantic skip", report.Backup)
	}
	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if !reflect.DeepEqual(got, orig) {
		t.Fatalf("on-disk changed under semantic skip: %q vs %q", got, orig)
	}
	assertBackupCount(t, r, "tool", 0)
}

// TestApply_TransformAndNewContentBothSet_TransformWins pins the
// documented precedence: when both fields are set, Transform's return
// value is written and NewContent is ignored. No fallback to NewContent
// under any condition.
func TestApply_TransformAndNewContentBothSet_TransformWins(t *testing.T) {
	r, home := newTestHome(t)
	target := filepath.Join(home, "tool", "config.txt")
	ensureParent(t, target)

	plan := WritePlan{
		Tool:       "tool",
		Target:     target,
		NewContent: []byte("B"),
		Transform: func(cur []byte) ([]byte, error) {
			return []byte("A"), nil
		},
		// No Parser: byte-identity is the only skip trigger, and "A" !=
		// nothing so the write proceeds.
	}
	report, err := Apply(context.Background(), r, plan)
	if err != nil {
		t.Fatalf("Apply err = %v", err)
	}
	if report.Skipped {
		t.Fatalf("Skipped = true; want false")
	}
	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(got) != "A" {
		t.Fatalf("bytes = %q; want %q (Transform must win over NewContent)", got, "A")
	}
}

// assertBackupCount counts .bak. entries in ~/.claudecm/backups/<tool>.
// Missing dir counts as zero. Foreign entries are ignored.
func assertBackupCount(t *testing.T, r *storage.Resolver, tool string, want int) {
	t.Helper()
	dir := filepath.Join(r.Home(), storage.ConfigDirName, storage.BackupsDirName, tool)
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
		if strings.Contains(e.Name(), ".bak.") {
			got++
		}
	}
	if got != want {
		t.Fatalf("backup count = %d; want %d", got, want)
	}
}

// TestApply_ConcurrentSerialization pins that WithLock serializes two
// Apply calls against the same target: both succeed, the winner's
// bytes are on disk, and exactly one backup exists (the second call
// sees the first call's output as its "current" and backs it up).
func TestApply_ConcurrentSerialization(t *testing.T) {
	r, home := newTestHome(t)
	target := filepath.Join(home, "tool", "config.json")
	ensureParent(t, target)

	planA := WritePlan{
		Tool:       "tool",
		Target:     target,
		NewContent: []byte(`{"who":"A"}`),
		Parser:     jsonParser,
		OwnedKeys:  []string{"who"},
	}
	planB := WritePlan{
		Tool:       "tool",
		Target:     target,
		NewContent: []byte(`{"who":"B"}`),
		Parser:     jsonParser,
		OwnedKeys:  []string{"who"},
	}
	var wg sync.WaitGroup
	errs := make([]error, 2)
	wg.Add(2)
	go func() { defer wg.Done(); _, errs[0] = Apply(context.Background(), r, planA) }()
	go func() { defer wg.Done(); _, errs[1] = Apply(context.Background(), r, planB) }()
	wg.Wait()
	for i, e := range errs {
		if e != nil {
			t.Fatalf("Apply[%d] err = %v", i, e)
		}
	}
	got, _ := os.ReadFile(target)
	if s := string(got); s != `{"who":"A"}` && s != `{"who":"B"}` {
		t.Fatalf("final bytes = %q; want one of A/B JSON", got)
	}
	// One writer sees an empty target (no backup) and the other sees
	// the first writer's bytes (one backup). Exactly one backup total.
	assertBackupCount(t, r, "tool", 1)
}
