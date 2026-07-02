//go:build test

// Package e2e is Story E8-S3/S4/S5: end-to-end integration tests that
// drive the compiled command surface (import → switch → export →
// explain) through the cmd/ package's test-only helpers.
//
// The package is gated on `-tags=test` for two reasons:
//
//  1. It calls into cmd.RunSwitchForTest / RunImportForTest / etc which
//     are themselves compiled only under -tags=test — the CLI wrapper
//     that maps *commit.PartialFailure to os.Exit is bypassed by these
//     helpers so tests can inspect the returned error directly.
//  2. It calls internal/writepath.SetPostReadHookForTest, another
//     test-tag-only seam, to inject concurrent-edit mutations.
//
// No production code path imports internal/e2e. Regular `go test ./...`
// skips this package entirely; `go test -tags=test ./internal/e2e/...`
// runs the suite.
package e2e

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/a2d2-dev/claudecm/cmd"
	"github.com/a2d2-dev/claudecm/internal/adapter/claudecode"
	"github.com/a2d2-dev/claudecm/internal/adapter/codex"
	"github.com/a2d2-dev/claudecm/internal/config"
	"github.com/a2d2-dev/claudecm/internal/storage"
)

// harness bundles the per-test HOME tree, resolver, and manager. Every
// e2e test starts by constructing one via newHarness; the returned
// value's methods carry the setup helpers the tests reach for.
type harness struct {
	t     *testing.T
	home  string
	resv  *storage.Resolver
	store *storage.FileStorage
	mgr   *config.Manager
}

// newHarness wires per-test HOME + resolver + Bootstrap. Mirrors
// cmd/explain_test.go's newExplainHarness but lives outside the cmd
// package so internal/e2e can use it without importing cmd's _test.go
// files.
//
// t.Setenv("HOME", home) makes storage.Default() (used inside every
// runXxx path via cmd.resolverFromGlobals) resolve to the temp dir
// without needing the --home global flag.
func newHarness(t *testing.T) *harness {
	t.Helper()
	// Isolate every env var either adapter treats as an EnvOverride
	// source so ambient CI/dev env cannot spoof owned-key values into
	// the round-trip.
	clearAdapterEnv(t)

	home := t.TempDir()
	t.Setenv("HOME", home)
	resv, err := storage.NewResolverWithHome(home)
	if err != nil {
		t.Fatalf("NewResolverWithHome: %v", err)
	}
	if err := storage.Bootstrap(resv); err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	store := storage.NewFileStorage(resv)
	mgr := config.NewManager(store, config.NewValidator())
	return &harness{t: t, home: home, resv: resv, store: store, mgr: mgr}
}

// adapterEnvVarNames enumerates every env var either the claudecode or
// codex adapter treats as an EnvOverride source. Cleared in newHarness
// so ambient env cannot leak into an e2e round-trip. Kept in sync with
// cmd/explain_test.go's list — a future adapter that adds an env
// override key must be listed here too.
var adapterEnvVarNames = []string{
	// Claude Code
	"ANTHROPIC_API_KEY",
	"ANTHROPIC_AUTH_TOKEN",
	"ANTHROPIC_BASE_URL",
	"ANTHROPIC_MODEL",
	"ANTHROPIC_SMALL_FAST_MODEL",
	"CLAUDE_CODE_USE_BEDROCK",
	"CLAUDE_CODE_USE_VERTEX",
	// Codex
	"OPENAI_API_KEY",
	"CODEX_MODEL",
	"CODEX_MODEL_PROVIDER",
}

// clearAdapterEnv wipes every adapter-visible env var for the duration
// of the test. Uses t.Setenv so the process env is restored on Cleanup.
func clearAdapterEnv(t *testing.T) {
	t.Helper()
	for _, name := range adapterEnvVarNames {
		t.Setenv(name, "")
	}
}

// seedFile writes body to path, creating the parent dir at 0700. Used
// to plant fixture bytes into ~/.claude or ~/.codex before an import.
func seedFile(t *testing.T, path string, body []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("mkdir parent %q: %v", path, err)
	}
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatalf("write %q: %v", path, err)
	}
}

// mustReadFile is a shorthand for os.ReadFile with a t.Fatalf on error.
func mustReadFile(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %q: %v", path, err)
	}
	return b
}

// claudeSettings returns the path of ~/.claude/settings.json for this
// harness's HOME. Convenience so tests do not need to know the layout.
func (h *harness) claudeSettings() string { return claudecode.SettingsPath(h.resv) }

// codexAuth returns the path of ~/.codex/auth.json.
func (h *harness) codexAuth() string { return codex.AuthPath(h.resv) }

// codexConfig returns the path of ~/.codex/config.toml.
func (h *harness) codexConfig() string { return codex.ConfigPath(h.resv) }

// runImport is a per-harness helper that resets the cmd flag package
// vars, invokes cmd.RunImportForTest, and defers the flag restore.
// Returns whatever the underlying runImport returned.
func (h *harness) runImport(args []string, name string, yes, overwrite bool) (stdout, stderr string, err error) {
	h.t.Helper()
	restore := cmd.SetImportFlagsForTest(name, yes, overwrite, false, "", "text")
	defer restore()
	return cmd.RunImportForTest(args)
}

// runSwitch wraps cmd.RunSwitchForTest with the standard --yes flag
// pattern used across every round-trip test. output is one of "text"
// or "json".
func (h *harness) runSwitch(args []string, output string, yes, dryRun bool) (stdout, stderr string, err error) {
	h.t.Helper()
	restore := cmd.SetSwitchFlagsForTest(output, dryRun, yes, "")
	defer restore()
	return cmd.RunSwitchForTest(args)
}

// runExport wraps cmd.RunExportForTest with the standard shell/yaml
// format toggle.
func (h *harness) runExport(args []string, format string, redact bool) (stdout, stderr string, err error) {
	h.t.Helper()
	restore := cmd.SetExportFlagsForTest(format, redact)
	defer restore()
	return cmd.RunExportForTest(args)
}

// runExplain wraps cmd.RunExplainForTest with the standard output
// toggle.
func (h *harness) runExplain(args []string, output string, reveal bool) (stdout, stderr string, err error) {
	h.t.Helper()
	restore := cmd.SetExplainFlagsForTest(output, reveal, false, "")
	defer restore()
	return cmd.RunExplainForTest(args)
}

// timeit runs fn and reports its wall-clock duration through t.Logf.
// Used by round-trip tests to enforce the SM-1 <5s ceiling per case.
func timeit(t *testing.T, label string, budget time.Duration, fn func()) {
	t.Helper()
	start := time.Now()
	fn()
	elapsed := time.Since(start)
	t.Logf("%s took %s", label, elapsed)
	if elapsed > budget {
		t.Errorf("%s exceeded budget %s: %s", label, budget, elapsed)
	}
}
