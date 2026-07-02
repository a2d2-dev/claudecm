package cmd

// completion_test.go — Story E6-S9 tests for the cmd/completion
// surface and the ValidArgsFunction wiring.

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/a2d2-dev/claudecm/internal/config"
	"github.com/a2d2-dev/claudecm/internal/storage"
)

// runCompletionInner invokes runCompletion with a synthetic
// cobra.Command whose Out/Err are bytes.Buffers.
func runCompletionInner(t *testing.T, shell string) (stdout, stderr string, err error) {
	t.Helper()
	var out, errBuf bytes.Buffer
	cmd := &cobra.Command{Use: "completion"}
	cmd.SetOut(&out)
	cmd.SetErr(&errBuf)
	err = runCompletion(cmd, []string{shell})
	return out.String(), errBuf.String(), err
}

// TestCompletion_BashScriptEmits asserts the bash generator produces
// something recognizably bash — `complete -F` shows up in every
// cobra-generated bash completion script.
func TestCompletion_BashScriptEmits(t *testing.T) {
	stdout, _, err := runCompletionInner(t, "bash")
	if err != nil {
		t.Fatalf("runCompletion(bash) err = %v", err)
	}
	if !strings.Contains(stdout, "complete -F") && !strings.Contains(stdout, "_claudecm") {
		t.Errorf("bash completion missing expected markers:\n%s", stdout[:min(len(stdout), 200)])
	}
}

// TestCompletion_ZshScriptEmits: zsh output contains `compdef` or
// `_claudecm`.
func TestCompletion_ZshScriptEmits(t *testing.T) {
	stdout, _, err := runCompletionInner(t, "zsh")
	if err != nil {
		t.Fatalf("runCompletion(zsh) err = %v", err)
	}
	if !strings.Contains(stdout, "compdef") && !strings.Contains(stdout, "_claudecm") {
		t.Errorf("zsh completion missing expected markers:\n%s", stdout[:min(len(stdout), 200)])
	}
}

// TestCompletion_FishScriptEmits: fish output contains `complete`.
func TestCompletion_FishScriptEmits(t *testing.T) {
	stdout, _, err := runCompletionInner(t, "fish")
	if err != nil {
		t.Fatalf("runCompletion(fish) err = %v", err)
	}
	if !strings.Contains(stdout, "complete") {
		t.Errorf("fish completion missing expected marker:\n%s", stdout[:min(len(stdout), 200)])
	}
}

// TestCompletion_PowerShellScriptEmits: PowerShell output contains
// `Register-ArgumentCompleter`.
func TestCompletion_PowerShellScriptEmits(t *testing.T) {
	stdout, _, err := runCompletionInner(t, "powershell")
	if err != nil {
		t.Fatalf("runCompletion(powershell) err = %v", err)
	}
	if !strings.Contains(stdout, "Register-ArgumentCompleter") {
		t.Errorf("powershell completion missing expected marker:\n%s", stdout[:min(len(stdout), 200)])
	}
}

// TestCompletion_UnsupportedShellRefused: unknown positional → error.
func TestCompletion_UnsupportedShellRefused(t *testing.T) {
	_, _, err := runCompletionInner(t, "csh")
	if err == nil {
		t.Fatalf("runCompletion(csh) expected error; got nil")
	}
	if !strings.Contains(err.Error(), "unsupported shell") {
		t.Errorf("error message unexpected: %v", err)
	}
}

// TestCompletion_ProfileNamesTabComplete seeds two profiles under a
// per-test HOME and asserts profileNamesCompletion enumerates both.
// The completer is what `claudecm completion` shell scripts invoke at
// runtime via `claudecm __complete ...` — this test exercises the
// same code path without going through cobra's __complete pipeline.
func TestCompletion_ProfileNamesTabComplete(t *testing.T) {
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

	for _, name := range []string{"alpha", "beta"} {
		p := config.NewProfile(name, "https://api.example.com", "sk-longtestkey-"+name)
		if err := mgr.AddProfile(p); err != nil {
			t.Fatalf("AddProfile(%s): %v", name, err)
		}
	}

	names, directive := profileNamesCompletion(&cobra.Command{}, nil, "")
	if directive == cobra.ShellCompDirectiveError {
		t.Fatalf("profileNamesCompletion returned error directive")
	}
	// Cobra completion strings may carry "\tDescription" suffixes; check
	// leading token match.
	want := map[string]bool{"alpha": true, "beta": true}
	for _, entry := range names {
		head := entry
		if i := strings.Index(entry, "\t"); i >= 0 {
			head = entry[:i]
		}
		delete(want, head)
	}
	if len(want) > 0 {
		t.Errorf("profileNamesCompletion missing expected names: %+v; got %+v", want, names)
	}
}

// TestCompletion_ProfileNamesEmptyDir: on a fresh HOME (no
// bootstrap), completion returns no entries and does NOT create the
// profiles dir. This is important because tab completion fires on
// every <TAB> keypress; an mkdir side effect would silently spam the
// operator's filesystem.
func TestCompletion_ProfileNamesEmptyDir(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	// Do NOT bootstrap; the completer must gracefully handle the
	// missing profiles dir.
	names, directive := profileNamesCompletion(&cobra.Command{}, nil, "")
	if len(names) != 0 {
		t.Errorf("expected no completions; got %+v", names)
	}
	if directive == cobra.ShellCompDirectiveError {
		t.Errorf("expected no error directive on empty tree; got %v", directive)
	}
	// Assert no ~/.claudecm/profiles/ was created.
	if _, err := os.Stat(filepath.Join(home, ".claudecm", "profiles")); !os.IsNotExist(err) {
		t.Errorf("tab completion should NOT create ~/.claudecm/profiles; err=%v", err)
	}
}

// TestCompletion_RegisterProfileNameCompletionWiring asserts the
// central wiring stamps the same ValidArgsFunction on the subcommands
// listed in the story AC (switch, delete, edit, rename, explain,
// restore, export).
func TestCompletion_RegisterProfileNameCompletionWiring(t *testing.T) {
	// The wiring runs from cobra.OnInitialize during rootCmd.Execute();
	// under `go test` we invoke it directly so the assertion is
	// deterministic.
	RegisterProfileNameCompletion()

	for _, name := range []string{"switch", "delete", "edit", "rename", "explain", "restore", "export"} {
		sub := findSubcommandByName(name)
		if sub == nil {
			t.Errorf("subcommand %q not registered under rootCmd", name)
			continue
		}
		if sub.ValidArgsFunction == nil {
			t.Errorf("subcommand %q missing ValidArgsFunction after register", name)
		}
	}
}

// min is a tiny helper so the test file does not pull in a stdlib
// dependency just for two call sites. Kept unexported.
func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
