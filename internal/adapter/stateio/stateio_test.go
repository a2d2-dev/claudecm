package stateio_test

// stateio_test.go — F1/F6 followup tests for the shared state.yaml
// read-modify-write helper.
//
// F1 was a read-modify-write race: two concurrent adapter.Apply calls
// would each LoadState → mutate → SaveState against ~/.claudecm/state.yaml
// and one of the two writes silently disappeared. The new stateio.RecordApplied
// wraps the critical section in storage.WithLock keyed on state.yaml so
// concurrent writers serialise. TestRecordApplied_HoldsLock hammers the
// helper with 8 goroutines writing distinct (tool, filePath) tuples and
// asserts every entry survives.
//
// F6 was the byte-identical sha256Hex + loadLastApplied + recordAppliedToState
// triplet in claudecode/drift.go and codex/drift.go. Both call sites now
// point at this package, so the tests exercise the SINGLE implementation
// used by both adapters — a regression in either adapter's drift check
// surfaces here first.

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/a2d2-dev/claudecm/internal/adapter"
	"github.com/a2d2-dev/claudecm/internal/adapter/stateio"
	"github.com/a2d2-dev/claudecm/internal/config"
	"github.com/a2d2-dev/claudecm/internal/storage"
)

// writeFile writes data to path, creating parent dirs at 0700 if
// needed. Fatal on error; every caller has already asserted the tree.
func writeFile(t *testing.T, path string, data []byte) error {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o600)
}

// removeFile deletes path, tolerating ENOENT.
func removeFile(t *testing.T, path string) error {
	t.Helper()
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// bootstrappedResolver builds a Resolver on a per-test HOME and runs
// storage.Bootstrap so ~/.claudecm/{profiles,backups} exists. Every
// helper in this package requires the HOME layout to be in place —
// FileStorage.SaveState asserts the bootstrap invariant.
func bootstrappedResolver(t *testing.T) *storage.Resolver {
	t.Helper()
	r, err := storage.NewResolverWithHome(t.TempDir())
	if err != nil {
		t.Fatalf("NewResolverWithHome: %v", err)
	}
	if err := storage.Bootstrap(r); err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	return r
}

// TestRecordApplied_RoundTrip pins the smallest useful round-trip:
// RecordApplied writes an entry; LoadLastApplied reads it back with the
// same (tool, path, SHA, appliedAt).
func TestRecordApplied_RoundTrip(t *testing.T) {
	r := bootstrappedResolver(t)
	path := filepath.Join(r.Home(), ".claude", "settings.json")
	now := time.Now().UTC().Truncate(time.Second)

	if err := stateio.RecordApplied(r, adapter.ToolClaudeCode, path, "sha-1", now); err != nil {
		t.Fatalf("RecordApplied: %v", err)
	}

	got, ok, err := stateio.LoadLastApplied(r, adapter.ToolClaudeCode, path)
	if err != nil {
		t.Fatalf("LoadLastApplied: %v", err)
	}
	if !ok {
		t.Fatalf("LoadLastApplied ok=false, want true")
	}
	if got.FilePath != path {
		t.Errorf("FilePath = %q, want %q", got.FilePath, path)
	}
	if got.SHA256 != "sha-1" {
		t.Errorf("SHA256 = %q, want %q", got.SHA256, "sha-1")
	}
	if !got.AppliedAt.Equal(now) {
		t.Errorf("AppliedAt = %v, want %v", got.AppliedAt, now)
	}
}

// TestRecordApplied_HoldsLock is the F1 race regression: 8 goroutines
// concurrently RecordApplied distinct (tool, filePath) pairs. Without
// the state.yaml flock, a naive LoadState → mutate → SaveState loop
// would drop entries. Every entry must be present at the end.
//
// The tuples span both ToolIDs and 4 file paths each so the assertion
// covers (a) inter-tool interleaving and (b) same-tool multi-path writes
// (the codex two-file case). Also runs under `go test -race`.
func TestRecordApplied_HoldsLock(t *testing.T) {
	r := bootstrappedResolver(t)
	now := time.Now().UTC().Truncate(time.Second)

	type entry struct {
		tool config.ToolID
		path string
		sha  string
	}
	entries := []entry{
		{adapter.ToolClaudeCode, filepath.Join(r.Home(), ".claude", "settings.json"), "cc-1"},
		{adapter.ToolClaudeCode, filepath.Join(r.Home(), ".claude", "settings.json.2"), "cc-2"},
		{adapter.ToolClaudeCode, filepath.Join(r.Home(), ".claude", "settings.json.3"), "cc-3"},
		{adapter.ToolClaudeCode, filepath.Join(r.Home(), ".claude", "settings.json.4"), "cc-4"},
		{adapter.ToolCodex, filepath.Join(r.Home(), ".codex", "auth.json"), "cx-a1"},
		{adapter.ToolCodex, filepath.Join(r.Home(), ".codex", "auth.json.2"), "cx-a2"},
		{adapter.ToolCodex, filepath.Join(r.Home(), ".codex", "config.toml"), "cx-c1"},
		{adapter.ToolCodex, filepath.Join(r.Home(), ".codex", "config.toml.2"), "cx-c2"},
	}

	var wg sync.WaitGroup
	errs := make([]error, len(entries))
	for i, e := range entries {
		wg.Add(1)
		i, e := i, e
		go func() {
			defer wg.Done()
			errs[i] = stateio.RecordApplied(r, e.tool, e.path, e.sha, now)
		}()
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Errorf("RecordApplied[%d]: %v", i, err)
		}
	}

	// Every entry must survive.
	for _, e := range entries {
		got, ok, err := stateio.LoadLastApplied(r, e.tool, e.path)
		if err != nil {
			t.Fatalf("LoadLastApplied (%s, %s): %v", e.tool, e.path, err)
		}
		if !ok {
			t.Errorf("LoadLastApplied (%s, %s) ok=false, want true (entry dropped by race)", e.tool, e.path)
			continue
		}
		if got.SHA256 != e.sha {
			t.Errorf("LoadLastApplied (%s, %s) SHA256 = %q, want %q", e.tool, e.path, got.SHA256, e.sha)
		}
	}
}

// TestRecordApplied_NilResolver: nil resolver at the write boundary is
// a programming error we surface loudly rather than silently no-op'ing.
func TestRecordApplied_NilResolver(t *testing.T) {
	err := stateio.RecordApplied(nil, adapter.ToolClaudeCode, "/tmp/x", "sha", time.Now())
	if err == nil {
		t.Fatalf("RecordApplied(nil resolver) err = nil, want non-nil")
	}
}

// TestStateio_LoadLastAppliedMissing: state.yaml absent → (zero, false, nil).
// LoadState returns a fresh NewState() (not nil) on ENOENT so GetLastApplied
// yields false; the wrapper propagates that as (zero, false, nil) — the
// "no prior state" edge case the E5-S4 AC pins.
func TestStateio_LoadLastAppliedMissing(t *testing.T) {
	r := bootstrappedResolver(t)
	path := filepath.Join(r.Home(), ".claude", "settings.json")

	got, ok, err := stateio.LoadLastApplied(r, adapter.ToolClaudeCode, path)
	if err != nil {
		t.Fatalf("LoadLastApplied: %v", err)
	}
	if ok {
		t.Errorf("LoadLastApplied ok=true, want false (no state file)")
	}
	if got != (config.LastApplied{}) {
		t.Errorf("LoadLastApplied entry = %+v, want zero-value", got)
	}
}

// TestStateio_LoadLastAppliedNilResolver: nil resolver on the read
// path is tolerated — the contract is "no state" rather than a panic
// so an early Project call from a partially-constructed environment
// still returns a coherent view.
func TestStateio_LoadLastAppliedNilResolver(t *testing.T) {
	got, ok, err := stateio.LoadLastApplied(nil, adapter.ToolClaudeCode, "/tmp/x")
	if err != nil {
		t.Fatalf("LoadLastApplied: %v", err)
	}
	if ok {
		t.Errorf("ok = true, want false")
	}
	if got != (config.LastApplied{}) {
		t.Errorf("entry = %+v, want zero-value", got)
	}
}

// TestLoadLastApplied_CorruptStateSurfacesError writes garbage to
// state.yaml and asserts LoadLastApplied propagates the parse error
// rather than silently no-op'ing. This is the "state file corrupt"
// signal the DriftForFile convenience swallows, but LoadLastApplied
// surfaces so a future `cmd/state doctor` can act on it.
func TestLoadLastApplied_CorruptStateSurfacesError(t *testing.T) {
	r := bootstrappedResolver(t)
	statePath, err := r.StatePath()
	if err != nil {
		t.Fatalf("StatePath: %v", err)
	}
	if err := os.WriteFile(statePath, []byte("::not valid yaml:::\n\t\x00"), 0o600); err != nil {
		t.Fatalf("corrupt state.yaml: %v", err)
	}
	_, ok, err := stateio.LoadLastApplied(r, adapter.ToolClaudeCode, "/tmp/x")
	if err == nil {
		t.Fatalf("LoadLastApplied err = nil, want non-nil (state corrupt)")
	}
	if ok {
		t.Errorf("LoadLastApplied ok = true, want false on error")
	}
	// DriftForFile must swallow the same error to preserve the
	// informational-only read-side contract.
	if got := stateio.DriftForFile(r, adapter.ToolClaudeCode, "/tmp/x"); got {
		t.Errorf("DriftForFile with corrupt state = true, want false (must swallow load error)")
	}
}

// TestRecordApplied_CorruptStateSurfacesError writes garbage to
// state.yaml and asserts RecordApplied propagates the load error
// (wrapped with "stateio: load state") rather than clobbering the
// corrupt file. Operators must see the load failure as a distinct
// signal — silent overwrite would erase any recoverable prior state
// on next Apply.
func TestRecordApplied_CorruptStateSurfacesError(t *testing.T) {
	r := bootstrappedResolver(t)
	statePath, err := r.StatePath()
	if err != nil {
		t.Fatalf("StatePath: %v", err)
	}
	if err := os.WriteFile(statePath, []byte("::not valid yaml:::\n\t\x00"), 0o600); err != nil {
		t.Fatalf("corrupt state.yaml: %v", err)
	}
	err = stateio.RecordApplied(r, adapter.ToolClaudeCode, "/tmp/x", "sha", time.Now())
	if err == nil {
		t.Fatalf("RecordApplied err = nil, want non-nil (state corrupt)")
	}
}

// TestSha256Hex_KnownVector locks the hash algorithm + encoding so any
// future change (e.g. an accidental switch to base64) surfaces here.
// Vector: sha256("") = e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855.
func TestSha256Hex_KnownVector(t *testing.T) {
	got := stateio.Sha256Hex(nil)
	want := "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
	if got != want {
		t.Errorf("Sha256Hex(nil) = %q, want %q", got, want)
	}
	got2 := stateio.Sha256Hex([]byte("abc"))
	want2 := "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad"
	if got2 != want2 {
		t.Errorf("Sha256Hex(\"abc\") = %q, want %q", got2, want2)
	}
}

// TestDriftForFile_TableCovers pins the DriftForFile behaviour matrix:
//
//   - no state + file present         → false (no prior anchor)
//   - state matches on-disk SHA       → false
//   - state mismatches on-disk SHA    → true
//   - state present, file absent      → false (read error swallowed)
func TestDriftForFile_TableCovers(t *testing.T) {
	r := bootstrappedResolver(t)
	path := filepath.Join(r.Home(), ".claude", "owned.txt")

	// No state, no file → false.
	if stateio.DriftForFile(r, adapter.ToolClaudeCode, path) {
		t.Errorf("DriftForFile with no state = true, want false")
	}

	// Write the file; state still absent → false.
	body := []byte("hello")
	if err := writeFile(t, path, body); err != nil {
		t.Fatal(err)
	}
	if stateio.DriftForFile(r, adapter.ToolClaudeCode, path) {
		t.Errorf("DriftForFile with file but no state = true, want false")
	}

	// Record matching SHA → false.
	if err := stateio.RecordApplied(r, adapter.ToolClaudeCode, path, stateio.Sha256Hex(body), time.Now()); err != nil {
		t.Fatalf("RecordApplied: %v", err)
	}
	if stateio.DriftForFile(r, adapter.ToolClaudeCode, path) {
		t.Errorf("DriftForFile with matching SHA = true, want false")
	}

	// Record STALE SHA → true.
	if err := stateio.RecordApplied(r, adapter.ToolClaudeCode, path, "0000000000000000000000000000000000000000000000000000000000000000", time.Now()); err != nil {
		t.Fatalf("RecordApplied stale: %v", err)
	}
	if !stateio.DriftForFile(r, adapter.ToolClaudeCode, path) {
		t.Errorf("DriftForFile with stale SHA = false, want true")
	}

	// Delete the file → false (absent file must not surface as drift).
	if err := removeFile(t, path); err != nil {
		t.Fatal(err)
	}
	if stateio.DriftForFile(r, adapter.ToolClaudeCode, path) {
		t.Errorf("DriftForFile with absent file = true, want false")
	}
}
