package codex_test

// Tests live in the _test package so we exercise the adapter through
// its exported surface only — the same shape cmd/* and internal/commit
// will use in E4-S2+ once the stubs are filled in.

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/a2d2-dev/claudecm/internal/adapter"
	"github.com/a2d2-dev/claudecm/internal/adapter/codex"
	"github.com/a2d2-dev/claudecm/internal/config"
	"github.com/a2d2-dev/claudecm/internal/storage"
	"github.com/a2d2-dev/claudecm/internal/writepath"
)

// newResolver builds a Resolver anchored at a per-test HOME. Tests need
// their own HOME so Detect / Files probing the on-disk layout does not
// depend on the developer's real ~/.codex.
func newResolver(t *testing.T) *storage.Resolver {
	t.Helper()
	r, err := storage.NewResolverWithHome(t.TempDir())
	if err != nil {
		t.Fatalf("storage.NewResolverWithHome: %v", err)
	}
	return r
}

// haveCodexBinary reports whether `codex` is on PATH — the PATH probe
// influences Presence.Installed, so several tests need to know whether
// the CI host has it. LookPath returns a non-nil error on miss.
func haveCodexBinary() bool {
	_, err := exec.LookPath("codex")
	return err == nil
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

func TestAdapter_ID(t *testing.T) {
	a := codex.New()
	if got, want := a.ID(), adapter.ToolCodex; got != want {
		t.Fatalf("ID() = %q, want %q", got, want)
	}
}

func TestAdapter_FilesReturnsTwoOwnedFiles(t *testing.T) {
	r := newResolver(t)
	a := codex.New()

	files := a.Files(r)
	if len(files) != 2 {
		t.Fatalf("Files() len = %d, want 2", len(files))
	}

	// auth.json first — architecture §5 auth-first ordering.
	auth := files[0]
	wantAuthPath := filepath.Join(r.Home(), ".codex", "auth.json")
	if auth.Path != wantAuthPath {
		t.Errorf("Files()[0].Path = %q, want %q", auth.Path, wantAuthPath)
	}
	if auth.Format != adapter.FormatJSON {
		t.Errorf("Files()[0].Format = %q, want %q", auth.Format, adapter.FormatJSON)
	}
	if !auth.Optional {
		t.Errorf("Files()[0].Optional = false, want true (auth.json may be absent on a fresh install)")
	}
	if !reflect.DeepEqual(auth.OwnedKeys, codex.OwnedKeysAuthJSON) {
		t.Errorf("Files()[0].OwnedKeys diverges from codex.OwnedKeysAuthJSON")
	}
	if got := codex.AuthPath(r); got != auth.Path {
		t.Errorf("AuthPath = %q, want %q", got, auth.Path)
	}

	// config.toml second.
	cfg := files[1]
	wantCfgPath := filepath.Join(r.Home(), ".codex", "config.toml")
	if cfg.Path != wantCfgPath {
		t.Errorf("Files()[1].Path = %q, want %q", cfg.Path, wantCfgPath)
	}
	if cfg.Format != adapter.FormatTOML {
		t.Errorf("Files()[1].Format = %q, want %q", cfg.Format, adapter.FormatTOML)
	}
	if !cfg.Optional {
		t.Errorf("Files()[1].Optional = false, want true (config.toml may be absent on a fresh install)")
	}
	if !reflect.DeepEqual(cfg.OwnedKeys, codex.OwnedKeysConfigTOML) {
		t.Errorf("Files()[1].OwnedKeys diverges from codex.OwnedKeysConfigTOML")
	}
	if got := codex.ConfigPath(r); got != cfg.Path {
		t.Errorf("ConfigPath = %q, want %q", got, cfg.Path)
	}
}

func TestDetect_FreshHomeReturnsNotInstalled(t *testing.T) {
	// If the CI host has `codex` on PATH the PATH probe would flip
	// Installed to true even in an empty HOME — skip so the test's
	// intent (no config, no binary → not installed) stays honest.
	if haveCodexBinary() {
		t.Skip("host has `codex` on PATH; Detect would report installed via PATH")
	}
	r := newResolver(t)
	a := codex.New()

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

func TestDetect_CodexDirExists(t *testing.T) {
	r := newResolver(t)
	if err := os.MkdirAll(filepath.Join(r.Home(), ".codex"), 0o755); err != nil {
		t.Fatalf("mkdir .codex: %v", err)
	}
	a := codex.New()

	p, err := a.Detect(context.Background(), r)
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	if !p.Detected {
		t.Errorf("Presence.Detected = false, want true (config dir present)")
	}
	if p.ConfigDir != filepath.Join(r.Home(), ".codex") {
		t.Errorf("Presence.ConfigDir = %q, want %q", p.ConfigDir, filepath.Join(r.Home(), ".codex"))
	}
	// Owned files absent → Files must be empty regardless of binary.
	if len(p.Files) != 0 {
		t.Errorf("Presence.Files = %v, want empty (no config.toml / auth.json on disk)", p.Files)
	}
	// Installed may be true iff `codex` is on PATH; do not overconstrain.
	if p.Installed && !haveCodexBinary() {
		t.Errorf("Presence.Installed = true without owned files or codex binary")
	}
}

func TestDetect_ConfigTomlExists(t *testing.T) {
	r := newResolver(t)
	if err := os.MkdirAll(filepath.Join(r.Home(), ".codex"), 0o755); err != nil {
		t.Fatalf("mkdir .codex: %v", err)
	}
	cfg := filepath.Join(r.Home(), ".codex", "config.toml")
	if err := os.WriteFile(cfg, []byte("# empty\n"), 0o600); err != nil {
		t.Fatalf("write config.toml: %v", err)
	}
	a := codex.New()

	p, err := a.Detect(context.Background(), r)
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	if !p.Installed {
		t.Errorf("Presence.Installed = false, want true (config.toml present)")
	}
	if !p.Detected {
		t.Errorf("Presence.Detected = false, want true")
	}
	if want := []string{cfg}; !reflect.DeepEqual(p.Files, want) {
		t.Errorf("Presence.Files = %v, want %v", p.Files, want)
	}
}

func TestDetect_AuthJsonExists(t *testing.T) {
	r := newResolver(t)
	if err := os.MkdirAll(filepath.Join(r.Home(), ".codex"), 0o755); err != nil {
		t.Fatalf("mkdir .codex: %v", err)
	}
	auth := filepath.Join(r.Home(), ".codex", "auth.json")
	if err := os.WriteFile(auth, []byte("{}\n"), 0o600); err != nil {
		t.Fatalf("write auth.json: %v", err)
	}
	a := codex.New()

	p, err := a.Detect(context.Background(), r)
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	if !p.Installed {
		t.Errorf("Presence.Installed = false, want true (auth.json present)")
	}
	if !p.Detected {
		t.Errorf("Presence.Detected = false, want true")
	}
	if want := []string{auth}; !reflect.DeepEqual(p.Files, want) {
		t.Errorf("Presence.Files = %v, want %v", p.Files, want)
	}
}

func TestDetect_BothFilesExist(t *testing.T) {
	r := newResolver(t)
	if err := os.MkdirAll(filepath.Join(r.Home(), ".codex"), 0o755); err != nil {
		t.Fatalf("mkdir .codex: %v", err)
	}
	auth := filepath.Join(r.Home(), ".codex", "auth.json")
	cfg := filepath.Join(r.Home(), ".codex", "config.toml")
	if err := os.WriteFile(auth, []byte("{}\n"), 0o600); err != nil {
		t.Fatalf("write auth.json: %v", err)
	}
	if err := os.WriteFile(cfg, []byte("# empty\n"), 0o600); err != nil {
		t.Fatalf("write config.toml: %v", err)
	}
	a := codex.New()

	p, err := a.Detect(context.Background(), r)
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	if !p.Installed {
		t.Errorf("Presence.Installed = false, want true")
	}
	// auth.json must come first, matching Files() auth-first ordering.
	if want := []string{auth, cfg}; !reflect.DeepEqual(p.Files, want) {
		t.Errorf("Presence.Files = %v, want %v (auth-first order)", p.Files, want)
	}
}

func TestDetect_BinaryOnPath(t *testing.T) {
	if !haveCodexBinary() {
		t.Skip("no `codex` binary on PATH; nothing to assert")
	}
	r := newResolver(t)
	a := codex.New()

	p, err := a.Detect(context.Background(), r)
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	if !p.Installed {
		t.Errorf("Presence.Installed = false, want true (codex binary on PATH)")
	}
	if !p.Detected {
		t.Errorf("Presence.Detected = false, want true")
	}
}

func TestDetect_CodexPathIsNotADirectory(t *testing.T) {
	// Anomalous shape: something occupies ~/.codex but it is a file,
	// not a directory. Detect must not claim the config dir but must
	// leave a note so the operator can investigate.
	r := newResolver(t)
	if err := os.WriteFile(filepath.Join(r.Home(), ".codex"), []byte("not a dir"), 0o600); err != nil {
		t.Fatalf("write ~/.codex as file: %v", err)
	}
	a := codex.New()

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

func TestDetect_AuthJsonIsADirectory(t *testing.T) {
	// Anomalous shape: auth.json exists as a directory. Detect must
	// not admit it to Files and must record a note.
	r := newResolver(t)
	if err := os.MkdirAll(filepath.Join(r.Home(), ".codex", "auth.json"), 0o755); err != nil {
		t.Fatalf("mkdir auth.json as dir: %v", err)
	}
	a := codex.New()

	p, err := a.Detect(context.Background(), r)
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	for _, f := range p.Files {
		if f == filepath.Join(r.Home(), ".codex", "auth.json") {
			t.Errorf("Presence.Files contains %q, want it excluded (dir-shaped)", f)
		}
	}
	if !haveCodexBinary() && !contains(p.Notes, "is a directory") {
		t.Errorf("Presence.Notes = %q, want mention of directory", p.Notes)
	}
}

func TestDetect_SymlinkAuthOutsideHomeOmittedButNoted(t *testing.T) {
	// F3 regression: Detect must not follow symlinks silently. Even
	// when the symlink target lives inside HOME (a "benign" shape),
	// Detect must (a) still report Detected/Installed via the target,
	// (b) leave the symlinked path OUT of Presence.Files, and (c) leave
	// a note mentioning "symlink". Codex has two owned files so we
	// exercise the auth.json branch here — the config.toml branch
	// shares the same code path.
	r := newResolver(t)
	if err := os.MkdirAll(filepath.Join(r.Home(), ".codex"), 0o755); err != nil {
		t.Fatalf("mkdir .codex: %v", err)
	}
	// Real target inside ~/.codex/ — a legitimate in-HOME destination
	// so the write-path's future symlink-aware mode has something
	// plausible to target.
	target := filepath.Join(r.Home(), ".codex", "auth-actual.json")
	if err := os.WriteFile(target, []byte("{}\n"), 0o600); err != nil {
		t.Fatalf("write auth-actual.json: %v", err)
	}
	link := filepath.Join(r.Home(), ".codex", "auth.json")
	if err := os.Symlink(target, link); err != nil {
		t.Fatalf("symlink auth.json -> auth-actual.json: %v", err)
	}

	p, err := codex.New().Detect(context.Background(), r)
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	if !p.Detected {
		t.Errorf("Presence.Detected = false, want true (target file exists)")
	}
	if !p.Installed {
		t.Errorf("Presence.Installed = false, want true (target file exists)")
	}
	for _, f := range p.Files {
		if f == link {
			t.Errorf("Presence.Files contains symlinked auth.json (%q), want excluded", link)
		}
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

	_, err := codex.New().Detect(ctx, r)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Detect on cancelled ctx: err = %v, want context.Canceled", err)
	}
}

func TestRegistrationHappensInInit(t *testing.T) {
	got, ok := adapter.Get(adapter.ToolCodex)
	if !ok {
		t.Fatalf("adapter.Get(ToolCodex) returned !ok; init() failed to register")
	}
	if got == nil {
		t.Fatalf("adapter.Get(ToolCodex) returned nil")
	}
	if id := got.ID(); id != adapter.ToolCodex {
		t.Fatalf("registered adapter.ID() = %q, want %q", id, adapter.ToolCodex)
	}
}

// TestRegistration_DuplicatePanics — F6. The panic-on-duplicate rule
// in adapter.DefaultRegistry is defence-in-depth against two adapter
// packages accidentally claiming the same ToolID. Because init()
// already registered ToolCodex when this test package loaded, a second
// Register call from any goroutine (including a fat-finger during
// refactoring) MUST panic. Guarding it here means a future switch of
// the registry to silent-overwrite semantics fails loudly.
func TestRegistration_DuplicatePanics(t *testing.T) {
	defer func() {
		r := recover()
		if r == nil {
			t.Fatalf("expected panic on duplicate adapter.Register(ToolCodex); got nil")
		}
		msg, ok := r.(string)
		if !ok {
			if err, isErr := r.(error); isErr {
				msg = err.Error()
			} else {
				t.Fatalf("panic value not a string or error: %#v", r)
			}
		}
		if !contains(msg, "duplicate") && !contains(msg, "already registered") {
			t.Fatalf("panic message = %q, want mention of \"duplicate\" or \"already registered\"", msg)
		}
	}()
	adapter.Register(adapter.ToolCodex, codex.New)
}

// TestDetect_ConfigDirIsSymlink covers the ~/.codex-is-a-symlink
// warning branch. The dir is a real directory reached through a
// symlink at the ~/.codex path; Detect must report Detected + note
// the symlink so the operator knows the write-path will need
// symlink-aware semantics before switch.
func TestDetect_ConfigDirIsSymlink(t *testing.T) {
	r := newResolver(t)
	target := filepath.Join(r.Home(), "codex-real")
	if err := os.MkdirAll(target, 0o755); err != nil {
		t.Fatalf("mkdir target: %v", err)
	}
	link := filepath.Join(r.Home(), ".codex")
	if err := os.Symlink(target, link); err != nil {
		t.Fatalf("symlink .codex -> codex-real: %v", err)
	}
	p, err := codex.New().Detect(context.Background(), r)
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	if !p.Detected {
		t.Errorf("Presence.Detected = false, want true (symlinked dir target present)")
	}
	if !contains(p.Notes, "symlink") {
		t.Errorf("Presence.Notes = %q, want mention of symlink warning", p.Notes)
	}
}

// TestDetect_FreshHomeFallbackNoteFires isolates the fallback
// "!p.Detected && p.Notes == ''" branch by clearing PATH so the
// binary probe cannot fire. This makes the test independent of
// whether the CI host has `codex` installed — the earlier
// TestDetect_FreshHomeReturnsNotInstalled skips when the host does.
func TestDetect_FreshHomeFallbackNoteFires(t *testing.T) {
	t.Setenv("PATH", "")
	r := newResolver(t)
	p, err := codex.New().Detect(context.Background(), r)
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	if p.Detected {
		t.Errorf("Presence.Detected = true, want false")
	}
	if !contains(p.Notes, "no .codex directory") {
		t.Errorf("Presence.Notes = %q, want fresh-home fallback message", p.Notes)
	}
}

// TestDetect_NoteAccumulatesAcrossProbes — F4. When one probe surfaces
// a diagnostic (e.g. auth.json is directory-shaped) AND another probe
// then finds a legitimate owned file (config.toml on disk), the
// composite Notes must carry BOTH signals. The pre-F4 code overwrote
// p.Notes on the second probe and hid the shape from the operator.
func TestDetect_NoteAccumulatesAcrossProbes(t *testing.T) {
	r := newResolver(t)
	if err := os.MkdirAll(filepath.Join(r.Home(), ".codex", "auth.json"), 0o755); err != nil {
		t.Fatalf("mkdir auth.json as dir: %v", err)
	}
	cfg := filepath.Join(r.Home(), ".codex", "config.toml")
	if err := os.WriteFile(cfg, []byte("# empty\n"), 0o600); err != nil {
		t.Fatalf("write config.toml: %v", err)
	}

	p, err := codex.New().Detect(context.Background(), r)
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	if !p.Installed {
		t.Errorf("Presence.Installed = false, want true (config.toml present)")
	}
	if !contains(p.Notes, "is a directory") {
		t.Errorf("Presence.Notes = %q, want mention of auth.json directory shape", p.Notes)
	}
	if !contains(p.Notes, "detected via "+cfg) {
		t.Errorf("Presence.Notes = %q, want mention of config.toml detection", p.Notes)
	}
	if !contains(p.Notes, "; ") {
		t.Errorf("Presence.Notes = %q, want \"; \" separator between signals", p.Notes)
	}
}

// TestStubsReturnErrNotImplemented locks in the fact that the
// remaining non-E4-S3 methods are still stubs. Import lifted its
// branch in E4-S3; Plan/Apply/Project follow in E4-S4/S5/S6.
func TestStubsReturnErrNotImplemented(t *testing.T) {
	r := newResolver(t)
	a := codex.New()
	ctx := context.Background()

	t.Run("Plan", func(t *testing.T) {
		if _, err := a.Plan(ctx, r, config.Profile{}); !errors.Is(err, codex.ErrNotImplemented) {
			t.Errorf("Plan err = %v, want ErrNotImplemented", err)
		}
	})
	t.Run("Apply", func(t *testing.T) {
		if _, err := a.Apply(ctx, r, writepath.WritePlan{}); !errors.Is(err, codex.ErrNotImplemented) {
			t.Errorf("Apply err = %v, want ErrNotImplemented", err)
		}
	})
	t.Run("Project", func(t *testing.T) {
		if _, err := a.Project(ctx, r, config.Profile{}); !errors.Is(err, codex.ErrNotImplemented) {
			t.Errorf("Project err = %v, want ErrNotImplemented", err)
		}
	})
}
