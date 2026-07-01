package claudecode_test

// Tests live in the _test package so we exercise the adapter through
// its exported surface only — the same shape cmd/* and internal/commit
// will use in E3-S3+ once the stubs are filled in.

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/a2d2-dev/claudecm/internal/adapter"
	"github.com/a2d2-dev/claudecm/internal/adapter/claudecode"
	"github.com/a2d2-dev/claudecm/internal/storage"
)

// newResolver builds a Resolver anchored at a per-test HOME. Tests need
// their own HOME so Detect/Files probing the on-disk layout does not
// depend on the developer's real ~/.claude.
func newResolver(t *testing.T) *storage.Resolver {
	t.Helper()
	r, err := storage.NewResolverWithHome(t.TempDir())
	if err != nil {
		t.Fatalf("storage.NewResolverWithHome: %v", err)
	}
	return r
}

// haveClaudeBinary reports whether `claude` is on PATH — the PATH probe
// influences Presence.Installed, so several tests need to know whether
// the CI host has it. LookPath returns a non-nil error on miss.
func haveClaudeBinary() bool {
	_, err := exec.LookPath("claude")
	return err == nil
}

func TestAdapter_ID(t *testing.T) {
	a := claudecode.New()
	if got, want := a.ID(), adapter.ToolClaudeCode; got != want {
		t.Fatalf("ID() = %q, want %q", got, want)
	}
}

func TestAdapter_FilesReturnsSettingsJSON(t *testing.T) {
	r := newResolver(t)
	a := claudecode.New()

	files := a.Files(r)
	if len(files) != 1 {
		t.Fatalf("Files() len = %d, want 1", len(files))
	}
	f := files[0]

	wantPath := filepath.Join(r.Home(), ".claude", "settings.json")
	if f.Path != wantPath {
		t.Errorf("Files()[0].Path = %q, want %q", f.Path, wantPath)
	}
	if f.Format != adapter.FormatJSONC {
		t.Errorf("Files()[0].Format = %q, want %q", f.Format, adapter.FormatJSONC)
	}
	if !f.Optional {
		t.Errorf("Files()[0].Optional = false, want true (settings.json may be absent on a fresh install)")
	}
	if !reflect.DeepEqual(f.OwnedKeys, claudecode.OwnedKeysSettingsJSON) {
		t.Errorf("Files()[0].OwnedKeys diverges from claudecode.OwnedKeysSettingsJSON")
	}
	// SettingsPath is exported so cmd/current and cmd/explain can reach
	// the exact path — verify it agrees with Files().
	if got := claudecode.SettingsPath(r); got != f.Path {
		t.Errorf("SettingsPath = %q, want %q", got, f.Path)
	}
}

func TestDetect_FreshHomeReturnsNotInstalled(t *testing.T) {
	// If the CI host has `claude` on PATH the PATH probe would flip
	// Installed to true even in an empty HOME — skip so the test's
	// intent (no config, no binary → not installed) stays honest.
	if haveClaudeBinary() {
		t.Skip("host has `claude` on PATH; Detect would report installed via PATH")
	}
	r := newResolver(t)
	a := claudecode.New()

	p, err := a.Detect(context.Background(), r)
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	if p.Installed {
		t.Errorf("Presence.Installed = true, want false")
	}
	if p.Detected {
		t.Errorf("Presence.Detected = true, want false")
	}
	if p.ConfigDir != "" {
		t.Errorf("Presence.ConfigDir = %q, want empty", p.ConfigDir)
	}
	if len(p.Files) != 0 {
		t.Errorf("Presence.Files = %v, want empty", p.Files)
	}
	if p.Notes == "" {
		t.Errorf("Presence.Notes empty; expected human-readable reason")
	}
}

func TestDetect_ClaudeDirExistsReturnsDetected(t *testing.T) {
	r := newResolver(t)
	if err := os.MkdirAll(filepath.Join(r.Home(), ".claude"), 0o755); err != nil {
		t.Fatalf("mkdir .claude: %v", err)
	}
	a := claudecode.New()

	p, err := a.Detect(context.Background(), r)
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	if !p.Detected {
		t.Errorf("Presence.Detected = false, want true (config dir present)")
	}
	if p.ConfigDir != filepath.Join(r.Home(), ".claude") {
		t.Errorf("Presence.ConfigDir = %q, want %q", p.ConfigDir, filepath.Join(r.Home(), ".claude"))
	}
	// settings.json absent → Files must be empty regardless of binary.
	if len(p.Files) != 0 {
		t.Errorf("Presence.Files = %v, want empty (no settings.json on disk)", p.Files)
	}
	// Installed may be true iff `claude` is on PATH; do not overconstrain.
	if p.Installed && !haveClaudeBinary() {
		t.Errorf("Presence.Installed = true without settings.json or claude binary")
	}
}

func TestDetect_SettingsJsonExistsReturnsInstalled(t *testing.T) {
	r := newResolver(t)
	if err := os.MkdirAll(filepath.Join(r.Home(), ".claude"), 0o755); err != nil {
		t.Fatalf("mkdir .claude: %v", err)
	}
	settings := filepath.Join(r.Home(), ".claude", "settings.json")
	if err := os.WriteFile(settings, []byte("{}\n"), 0o600); err != nil {
		t.Fatalf("write settings.json: %v", err)
	}
	a := claudecode.New()

	p, err := a.Detect(context.Background(), r)
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	if !p.Installed {
		t.Errorf("Presence.Installed = false, want true (settings.json present)")
	}
	if !p.Detected {
		t.Errorf("Presence.Detected = false, want true")
	}
	if p.ConfigDir != filepath.Join(r.Home(), ".claude") {
		t.Errorf("Presence.ConfigDir = %q, want %q", p.ConfigDir, filepath.Join(r.Home(), ".claude"))
	}
	if want := []string{settings}; !reflect.DeepEqual(p.Files, want) {
		t.Errorf("Presence.Files = %v, want %v", p.Files, want)
	}
	if p.Notes == "" {
		t.Errorf("Presence.Notes empty; expected detection reason")
	}
}

func TestDetect_BinaryOnPath(t *testing.T) {
	if !haveClaudeBinary() {
		t.Skip("no `claude` binary on PATH; nothing to assert")
	}
	r := newResolver(t)
	a := claudecode.New()

	p, err := a.Detect(context.Background(), r)
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	if !p.Installed {
		t.Errorf("Presence.Installed = false, want true (claude binary on PATH)")
	}
	if !p.Detected {
		t.Errorf("Presence.Detected = false, want true")
	}
}

func TestDetect_ClaudePathIsNotADirectory(t *testing.T) {
	// Anomalous shape: something occupies ~/.claude but it is a file,
	// not a directory. Detect must not claim the config dir but must
	// leave a note so the operator can investigate.
	r := newResolver(t)
	if err := os.WriteFile(filepath.Join(r.Home(), ".claude"), []byte("not a dir"), 0o600); err != nil {
		t.Fatalf("write ~/.claude as file: %v", err)
	}
	a := claudecode.New()

	p, err := a.Detect(context.Background(), r)
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	if p.ConfigDir != "" {
		t.Errorf("Presence.ConfigDir = %q, want empty (path is not a dir)", p.ConfigDir)
	}
	if p.Notes == "" || !contains(p.Notes, "not a directory") {
		t.Errorf("Presence.Notes = %q, want mention of non-directory", p.Notes)
	}
}

func TestDetect_SettingsPathIsADirectory(t *testing.T) {
	// Anomalous shape: settings.json exists as a directory. Detect
	// must not claim Installed and must record a note.
	r := newResolver(t)
	if err := os.MkdirAll(filepath.Join(r.Home(), ".claude", "settings.json"), 0o755); err != nil {
		t.Fatalf("mkdir settings.json as dir: %v", err)
	}
	a := claudecode.New()

	p, err := a.Detect(context.Background(), r)
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	// Installed may be true via PATH; the invariant that matters is
	// that a directory at settings.json is NOT admitted to Files and
	// Notes flags it so the operator can see the anomaly.
	if len(p.Files) != 0 {
		t.Errorf("Presence.Files = %v, want empty (dir at settings.json path)", p.Files)
	}
	if !haveClaudeBinary() && !contains(p.Notes, "is a directory") {
		// When the PATH probe cannot overwrite Notes, the dir-shaped
		// settings.json note is the one we should see.
		t.Errorf("Presence.Notes = %q, want mention of directory", p.Notes)
	}
}

// contains is a tiny substring helper so the anomalous-shape tests
// don't drag in strings just for one call.
func contains(haystack, needle string) bool {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}

func TestDetect_SettingsJsonIsSymlink(t *testing.T) {
	// F3 regression: Detect must not follow symlinks silently. Even
	// when the symlink target lives inside HOME (a "benign" shape),
	// Detect must (a) still report Detected/Installed via the target,
	// (b) leave the symlinked path OUT of Presence.Files — the
	// write-path refuses to write through symlinks and Files must not
	// promise ownership of a path the writer will reject, and (c)
	// leave a note mentioning "symlink" so the operator sees why the
	// file did not appear in the owned list.
	r := newResolver(t)
	if err := os.MkdirAll(filepath.Join(r.Home(), ".claude"), 0o755); err != nil {
		t.Fatalf("mkdir .claude: %v", err)
	}
	// Real target lives inside ~/.claude/ — a legitimate in-HOME
	// destination so the write-path's future symlink-aware mode has
	// something plausible to target.
	target := filepath.Join(r.Home(), ".claude", "settings-actual.json")
	if err := os.WriteFile(target, []byte("{}\n"), 0o600); err != nil {
		t.Fatalf("write settings-actual.json: %v", err)
	}
	link := filepath.Join(r.Home(), ".claude", "settings.json")
	if err := os.Symlink(target, link); err != nil {
		t.Fatalf("symlink settings.json -> settings-actual.json: %v", err)
	}

	p, err := claudecode.New().Detect(context.Background(), r)
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	if !p.Detected {
		t.Errorf("Presence.Detected = false, want true (target file exists)")
	}
	if !p.Installed {
		t.Errorf("Presence.Installed = false, want true (target file exists)")
	}
	if len(p.Files) != 0 {
		t.Errorf("Presence.Files = %v, want empty (symlinked settings.json must not be admitted)", p.Files)
	}
	if !contains(p.Notes, "symlink") {
		t.Errorf("Presence.Notes = %q, want mention of \"symlink\"", p.Notes)
	}
}

func TestDetect_ContextCancelled(t *testing.T) {
	// Detect honours ctx.Err() before filesystem I/O so cmd/* can
	// abort on SIGINT/SIGTERM. Verify the shape.
	r := newResolver(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := claudecode.New().Detect(ctx, r)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Detect on cancelled ctx: err = %v, want context.Canceled", err)
	}
}

func TestRegistrationHappensInInit(t *testing.T) {
	got, ok := adapter.Get(adapter.ToolClaudeCode)
	if !ok {
		t.Fatalf("adapter.Get(ToolClaudeCode) returned !ok; init() failed to register")
	}
	if got == nil {
		t.Fatalf("adapter.Get(ToolClaudeCode) returned nil")
	}
	if id := got.ID(); id != adapter.ToolClaudeCode {
		t.Fatalf("registered adapter.ID() = %q, want %q", id, adapter.ToolClaudeCode)
	}
}

// TestStubsReturnErrNotImplemented was the epic-scoped stub sentinel
// test. All five interface methods have landed in E3-S2..E3-S6 with
// dedicated tests in their own *_test.go files (detect / files: this
// file; import: import_test.go; plan: plan_test.go; apply:
// apply_test.go; project: project_test.go). The stub test itself is
// removed so the file does not carry a dead test body.
