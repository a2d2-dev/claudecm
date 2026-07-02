package cmd

// root_test.go — Story E6-S9 tests for the four global flags on
// rootCmd: --reveal, --home, --lock-timeout, --retention.
//
// Isolation strategy: every test resets the global-flag package vars
// with t.Cleanup so leftover state does not leak. Flag validation
// happens through validateGlobalFlags directly (bypassing cobra's
// Execute chain) so the tests never spawn a subprocess.

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"github.com/a2d2-dev/claudecm/internal/config"
	"github.com/a2d2-dev/claudecm/internal/storage"
)

// pinGlobalFlagsForTest wires a defer-restore around the four global
// flag vars so a test can safely mutate them without leaking state.
func pinGlobalFlagsForTest(t *testing.T) {
	t.Helper()
	resetGlobalFlagsForTest()
	t.Cleanup(resetGlobalFlagsForTest)
}

// TestGlobalReveal_PropagatesToList: setting the global --reveal true
// exposes plaintext api_key in list output AND emits the stderr
// warning. Symmetric with TestList_RevealShowsPlaintext but exercises
// the flag propagation path explicitly.
func TestGlobalReveal_PropagatesToList(t *testing.T) {
	pinGlobalFlagsForTest(t)
	resetListFlagsForTest()
	t.Cleanup(resetListFlagsForTest)

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
	if err := mgr.AddProfile(config.NewProfile("prod", "https://api.example.com", "sk-prodverylongkey1234")); err != nil {
		t.Fatalf("AddProfile: %v", err)
	}

	globalRevealFlag = true

	var out, errBuf bytes.Buffer
	cmd := &cobra.Command{Use: "list"}
	cmd.SetOut(&out)
	cmd.SetErr(&errBuf)
	if err := runList(cmd, nil); err != nil {
		t.Fatalf("runList err = %v", err)
	}
	if !strings.Contains(out.String(), "sk-prodverylongkey1234") {
		t.Errorf("global --reveal did not surface plaintext:\n%s", out.String())
	}
	if !strings.Contains(errBuf.String(), "WARNING") {
		t.Errorf("global --reveal missing stderr warning: %q", errBuf.String())
	}
}

// TestGlobalHome_ValidatedAndUsed: --home <existing tmpdir> →
// resolverFromGlobals returns a resolver rooted at that dir, and
// downstream cmds (list) succeed against it.
func TestGlobalHome_ValidatedAndUsed(t *testing.T) {
	pinGlobalFlagsForTest(t)
	resetListFlagsForTest()
	t.Cleanup(resetListFlagsForTest)

	// Point --home at a tmpdir that is bootstrapped ourselves so list
	// finds it clean.
	home := t.TempDir()
	resv, err := storage.NewResolverWithHome(home)
	if err != nil {
		t.Fatalf("NewResolverWithHome (seed): %v", err)
	}
	if err := storage.Bootstrap(resv); err != nil {
		t.Fatalf("Bootstrap (seed): %v", err)
	}

	globalHomeFlag = home

	got, err := resolverFromGlobals()
	if err != nil {
		t.Fatalf("resolverFromGlobals err = %v", err)
	}
	if got.Home() != home {
		t.Errorf("resolverFromGlobals home = %q; want %q", got.Home(), home)
	}

	// End-to-end: list under --home reads the bootstrapped (empty)
	// tree and prints "no profiles" without touching $HOME.
	var out bytes.Buffer
	cmd := &cobra.Command{Use: "list"}
	cmd.SetOut(&out)
	cmd.SetErr(&bytes.Buffer{})
	if err := runList(cmd, nil); err != nil {
		t.Fatalf("runList err = %v", err)
	}
	if !strings.Contains(out.String(), listNoProfilesText) {
		t.Errorf("list under --home missing expected empty-line: %q", out.String())
	}
}

// TestGlobalHome_InvalidRejected: --home /doesnotexist → PersistentPreRunE
// refuses.
func TestGlobalHome_InvalidRejected(t *testing.T) {
	pinGlobalFlagsForTest(t)

	globalHomeFlag = filepath.Join(os.TempDir(), "claudecm-does-not-exist-xyz-e6s9")

	cmd := &cobra.Command{Use: "x"}
	err := validateGlobalFlags(cmd, nil)
	if err == nil {
		t.Fatalf("validateGlobalFlags expected error on missing --home; got nil")
	}
	if !strings.Contains(err.Error(), "--home invalid") {
		t.Errorf("error message unexpected: %v", err)
	}
}

// TestGlobalLockTimeout_ZeroInvalid: --lock-timeout 0 explicitly set
// → refused. Zero as a default value is fine (unset defaults still
// work); the refusal applies only when the operator typed it.
func TestGlobalLockTimeout_ZeroInvalid(t *testing.T) {
	pinGlobalFlagsForTest(t)

	cmd := &cobra.Command{Use: "x"}
	cmd.PersistentFlags().DurationVar(&globalLockTimeoutFlag, "lock-timeout", 0, "")
	if err := cmd.PersistentFlags().Set("lock-timeout", "0s"); err != nil {
		t.Fatalf("flag Set: %v", err)
	}
	err := validateGlobalFlags(cmd, nil)
	if err == nil {
		t.Fatalf("validateGlobalFlags expected error on --lock-timeout 0; got nil")
	}
	if !strings.Contains(err.Error(), "lock-timeout") {
		t.Errorf("error message unexpected: %v", err)
	}
}

// TestGlobalRetention_ZeroInvalid: --retention 0 explicitly set →
// refused. Same rationale as the lock-timeout case.
func TestGlobalRetention_ZeroInvalid(t *testing.T) {
	pinGlobalFlagsForTest(t)

	cmd := &cobra.Command{Use: "x"}
	cmd.PersistentFlags().IntVar(&globalRetentionFlag, "retention", 0, "")
	if err := cmd.PersistentFlags().Set("retention", "0"); err != nil {
		t.Fatalf("flag Set: %v", err)
	}
	err := validateGlobalFlags(cmd, nil)
	if err == nil {
		t.Fatalf("validateGlobalFlags expected error on --retention 0; got nil")
	}
	if !strings.Contains(err.Error(), "retention") {
		t.Errorf("error message unexpected: %v", err)
	}
}

// TestGlobalLockTimeout_PositiveAccepted: a positive value passes
// validation AND lockTimeoutContext wraps context with the expected
// deadline.
func TestGlobalLockTimeout_PositiveAccepted(t *testing.T) {
	pinGlobalFlagsForTest(t)
	globalLockTimeoutFlag = 500 * time.Millisecond

	// PersistentPreRunE gate.
	cmd := &cobra.Command{Use: "x"}
	cmd.PersistentFlags().DurationVar(&globalLockTimeoutFlag, "lock-timeout", 0, "")
	if err := cmd.PersistentFlags().Set("lock-timeout", "500ms"); err != nil {
		t.Fatalf("flag Set: %v", err)
	}
	if err := validateGlobalFlags(cmd, nil); err != nil {
		t.Errorf("validateGlobalFlags rejected positive --lock-timeout: %v", err)
	}

	// lockTimeoutContext wraps with a deadline near 500ms.
	base, cancel := lockTimeoutContext(context.Background())
	defer cancel()
	deadline, ok := base.Deadline()
	if !ok {
		t.Errorf("lockTimeoutContext did not set a deadline for positive timeout")
	}
	if time.Until(deadline) > 500*time.Millisecond+50*time.Millisecond {
		t.Errorf("deadline too far in the future: %v", deadline)
	}

	// Sanity: globalLockTimeoutOrDefault returns the pinned value.
	if got := globalLockTimeoutOrDefault(); got != 500*time.Millisecond {
		t.Errorf("globalLockTimeoutOrDefault = %v; want 500ms", got)
	}
}

// TestGlobalRetention_PositiveAccepted: a positive value passes
// validation AND pruneOptionsFromGlobals reflects it.
func TestGlobalRetention_PositiveAccepted(t *testing.T) {
	pinGlobalFlagsForTest(t)
	globalRetentionFlag = 3

	cmd := &cobra.Command{Use: "x"}
	cmd.PersistentFlags().IntVar(&globalRetentionFlag, "retention", 0, "")
	if err := cmd.PersistentFlags().Set("retention", "3"); err != nil {
		t.Fatalf("flag Set: %v", err)
	}
	if err := validateGlobalFlags(cmd, nil); err != nil {
		t.Errorf("validateGlobalFlags rejected positive --retention: %v", err)
	}
	if got := pruneOptionsFromGlobals().Keep; got != 3 {
		t.Errorf("pruneOptionsFromGlobals.Keep = %d; want 3", got)
	}
	if got := globalRetentionOrDefault(); got != 3 {
		t.Errorf("globalRetentionOrDefault = %d; want 3", got)
	}
}

// TestGlobalRetention_DefaultsToTen: unset --retention returns
// storage.DefaultBackupRetention (10 per NFR-R1).
func TestGlobalRetention_DefaultsToTen(t *testing.T) {
	pinGlobalFlagsForTest(t)
	if got := globalRetentionOrDefault(); got != storage.DefaultBackupRetention {
		t.Errorf("default retention = %d; want %d", got, storage.DefaultBackupRetention)
	}
}

// TestGlobalLockTimeout_DefaultsToStorageDefault: unset → storage
// DefaultLockTimeout.
func TestGlobalLockTimeout_DefaultsToStorageDefault(t *testing.T) {
	pinGlobalFlagsForTest(t)
	if got := globalLockTimeoutOrDefault(); got != storage.DefaultLockTimeout {
		t.Errorf("default lock-timeout = %v; want %v", got, storage.DefaultLockTimeout)
	}
	// lockTimeoutContext with unset flag returns the original context
	// (no deadline).
	base, cancel := lockTimeoutContext(context.Background())
	defer cancel()
	if _, ok := base.Deadline(); ok {
		t.Errorf("lockTimeoutContext set a deadline when --lock-timeout unset")
	}
}

// TestGlobalHome_AllCommandsHonored verifies that every command
// touching storage honors the --home override rather than reaching
// through storage.Default() to $HOME. The pre-F1 code path silently
// ignored --home in add/current/edit/delete/rename/explain/import,
// so a `HOME=/tmp/real claudecm --home /tmp/sandbox add p1 ...` call
// would write into the real HOME. This test exercises add / list /
// switch under a --home pointed at a sandbox tree that is DIFFERENT
// from $HOME, then asserts:
//
//  1. The profile file lands under sandbox/.claudecm/profiles/, and
//  2. Nothing was written under the real HOME's .claudecm tree.
//
// The pattern (real HOME + separate sandbox --home) is exactly the
// smoke-test invocation the reviewer called out on the PR.
func TestGlobalHome_AllCommandsHonored(t *testing.T) {
	pinGlobalFlagsForTest(t)
	resetAddFlags()
	resetListFlagsForTest()
	t.Cleanup(func() {
		resetAddFlags()
		resetListFlagsForTest()
	})

	realHome := t.TempDir()
	sandbox := t.TempDir()

	// Rewire $HOME to a real tempdir so that a leaked storage.Default()
	// call writes there — the assertion below catches that leak by
	// noticing the sandbox stayed empty.
	t.Setenv("HOME", realHome)

	// Bootstrap the sandbox layout ourselves (mirrors what a real
	// invocation would do the first time --home is used).
	sandboxResv, err := storage.NewResolverWithHome(sandbox)
	if err != nil {
		t.Fatalf("NewResolverWithHome(sandbox): %v", err)
	}
	if err := storage.Bootstrap(sandboxResv); err != nil {
		t.Fatalf("Bootstrap(sandbox): %v", err)
	}

	globalHomeFlag = sandbox

	// ---- add ------------------------------------------------------
	addBaseURLFlag = "https://api.example.com"
	addAPIKeyFlag = "sk-globalhomehonored-1234"
	addModelFlag = "opus"

	var addOut, addErr bytes.Buffer
	addCmd := &cobra.Command{Use: "add"}
	addCmd.SetOut(&addOut)
	addCmd.SetErr(&addErr)
	if err := runAdd(addCmd, []string{"honor"}); err != nil {
		t.Fatalf("runAdd err = %v; stderr=%s", err, addErr.String())
	}
	sandboxProfilePath := filepath.Join(sandbox, ".claudecm", "profiles", "honor.yaml")
	if _, err := os.Stat(sandboxProfilePath); err != nil {
		t.Fatalf("add: sandbox profile missing at %q: %v", sandboxProfilePath, err)
	}
	realProfilePath := filepath.Join(realHome, ".claudecm", "profiles", "honor.yaml")
	if _, err := os.Stat(realProfilePath); err == nil {
		t.Fatalf("add: profile leaked into real HOME at %q — --home ignored", realProfilePath)
	} else if !os.IsNotExist(err) {
		t.Fatalf("add: unexpected error stating real-HOME profile: %v", err)
	}

	// The real HOME's .claudecm subtree must not have been created by
	// runAdd. storage.Default() would have called Bootstrap on it —
	// that would leave a directory behind. Its absence proves nothing
	// in the write path reached storage.Default().
	if _, err := os.Stat(filepath.Join(realHome, ".claudecm")); err == nil {
		t.Errorf("add: bootstrapped real HOME .claudecm tree despite --home")
	}

	// ---- list -----------------------------------------------------
	var listOut, listErr bytes.Buffer
	listCmdTest := &cobra.Command{Use: "list"}
	listCmdTest.SetOut(&listOut)
	listCmdTest.SetErr(&listErr)
	if err := runList(listCmdTest, nil); err != nil {
		t.Fatalf("runList err = %v; stderr=%s", err, listErr.String())
	}
	if !strings.Contains(listOut.String(), "honor") {
		t.Errorf("list: expected the sandbox-only profile 'honor'; got:\n%s", listOut.String())
	}

	// ---- switch ---------------------------------------------------
	// runSwitch flips state.yaml under --home; verify the state file
	// lands in sandbox, not real HOME.
	switchCmd := &cobra.Command{Use: "switch"}
	var switchOut, switchErr bytes.Buffer
	switchCmd.SetOut(&switchOut)
	switchCmd.SetErr(&switchErr)
	// switch honors --yes to skip the confirmation prompt in test.
	switchYesFlag = true
	t.Cleanup(func() { switchYesFlag = false })
	if err := runSwitch(switchCmd, []string{"honor"}); err != nil {
		t.Fatalf("runSwitch err = %v; stderr=%s", err, switchErr.String())
	}
	sandboxStatePath := filepath.Join(sandbox, ".claudecm", "state.yaml")
	if _, err := os.Stat(sandboxStatePath); err != nil {
		t.Fatalf("switch: sandbox state.yaml missing at %q: %v", sandboxStatePath, err)
	}
	realStatePath := filepath.Join(realHome, ".claudecm", "state.yaml")
	if _, err := os.Stat(realStatePath); err == nil {
		t.Fatalf("switch: state.yaml leaked into real HOME at %q — --home ignored", realStatePath)
	}
}

// TestGlobalRevealActive_ORLogic asserts globalRevealActive's OR
// semantics: local || global wins.
func TestGlobalRevealActive_ORLogic(t *testing.T) {
	pinGlobalFlagsForTest(t)

	if globalRevealActive(false) {
		t.Errorf("both false → active must be false")
	}
	if !globalRevealActive(true) {
		t.Errorf("local true → active must be true")
	}
	globalRevealFlag = true
	if !globalRevealActive(false) {
		t.Errorf("global true, local false → active must be true")
	}
}
