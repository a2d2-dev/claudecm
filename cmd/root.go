// Package cmd hosts the cobra command tree for the claudecm CLI. This
// file (Story E6-S9) declares the root command plus the four
// process-wide "global" flags — --reveal / --home / --lock-timeout /
// --retention — that every downstream command reads through the
// helpers exposed at the bottom of this file:
//
//	resolverFromGlobals() → *storage.Resolver honoring --home
//	globalRevealActive(local) → bool combining root + per-cmd flags
//	globalLockTimeoutOrDefault() → time.Duration honoring --lock-timeout
//	globalRetentionOrDefault() → int honoring --retention
//
// Design notes:
//
//   - The global flags register as PersistentFlags on rootCmd so any
//     subcommand inherits them without an opt-in wiring per command.
//     Values are captured in unexported package-level bools/strings/
//     ints; per coding-standards rule 12 this is a documented exception
//     that mirrors adapter.DefaultRegistry — set once at flag-parse
//     time, read from thereafter, never mutated at request time.
//
//   - Validation happens in a single PersistentPreRunE (validateGlobalFlags).
//     It refuses zero / negative timeouts and retention counts up front so
//     an operator sees the error before a write path fires. The --home
//     validation delegates to storage.NewResolverWithHome so the NFR-S3
//     invariants (absolute, exists, non-root-owned) apply uniformly.
//     PreRunE errors surface as exit code 1 with the standard cobra
//     error routing; tests bypass cobra by calling validateGlobalFlags
//     directly.
//
//   - --reveal duplicates the per-command --reveal flag on `current` and
//     `explain`. Cobra allows a persistent flag and a local flag to
//     coexist under the same name only if the local one is not
//     re-registered; the local versions are removed in this PR
//     (see cmd/current.go, cmd/explain.go) so root's --reveal is the
//     sole binding. The stderr redaction-notice remains inside each
//     command's Run body (not PersistentPreRunE) so tests that invoke
//     runCurrent / runExplain / runList directly still see the notice
//     without needing to route through cobra's Execute chain.
package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/a2d2-dev/claudecm/internal/storage"
	"github.com/a2d2-dev/claudecm/pkg/version"
)

// Global flag storage. Unexported package-level vars so tests inside
// this package can pin them via t.Cleanup(func(){ ... }) without
// exporting a setter. See package-level doc for the rationale.
var (
	// globalRevealFlag mirrors --reveal at the root. Combined with each
	// command's local knob (currently list only, since current/explain
	// dropped their local --reveal in this PR) via globalRevealActive.
	globalRevealFlag bool

	// globalHomeFlag mirrors --home. Empty string means "use
	// $HOME"; a non-empty value is threaded into
	// storage.NewResolverWithHome via resolverFromGlobals.
	globalHomeFlag string

	// globalLockTimeoutFlag mirrors --lock-timeout. Zero (the default)
	// means "use storage.DefaultLockTimeout"; any positive duration
	// overrides the flock timeout for every commit orchestrator call
	// in this invocation. Negative values are rejected at PreRunE.
	globalLockTimeoutFlag time.Duration

	// globalRetentionFlag mirrors --retention. Zero (the default)
	// means "use storage.DefaultBackupRetention"; any positive integer
	// overrides the per-invocation backup-retention count fed to
	// storage.PruneOptions. Negative / zero values are rejected at
	// PreRunE — passing 0 explicitly would delete every backup, which
	// is precisely the "no fallback writes" foot-gun v1 refuses.
	globalRetentionFlag int
)

// rootCmd represents the base command
var rootCmd = &cobra.Command{
	Use:   "claudecm",
	Short: "Claude Code Environment Manager",
	Long: `claudecm is a CLI tool for managing Claude Code environment configurations.

Easily manage multiple API configurations and switch between them seamlessly.`,
	// Cobra threads PersistentPreRunE through the ancestry chain, so
	// every subcommand implicitly runs the global-flag validator
	// before its own RunE. Tests that call the inner run<Cmd> function
	// bypass this — they invoke validateGlobalFlags directly when they
	// exercise the flag surface.
	PersistentPreRunE: validateGlobalFlags,
}

// Execute runs the root command
func Execute() {
	if err := rootCmd.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func init() {
	rootCmd.SetVersionTemplate(fmt.Sprintf("claudecm version %s (commit: %s, built: %s)\n",
		version.Version, version.Commit, version.Date))
	rootCmd.Version = version.Version

	// Persistent flags. Each subcommand inherits these; helpers below
	// (resolverFromGlobals / globalLockTimeoutOrDefault / etc) are the
	// intended read gateways so command code does not duplicate the
	// zero-value fallback logic.
	rootCmd.PersistentFlags().BoolVar(&globalRevealFlag, "reveal", false,
		"Reveal secret values in plaintext output (prints stderr warning)")
	rootCmd.PersistentFlags().StringVar(&globalHomeFlag, "home", "",
		"Override $HOME for path resolution (must be an absolute, existing directory)")
	rootCmd.PersistentFlags().DurationVar(&globalLockTimeoutFlag, "lock-timeout", 0,
		"Override the default flock acquisition timeout for write commands (default 5s)")
	rootCmd.PersistentFlags().IntVar(&globalRetentionFlag, "retention", 0,
		"Override backup retention (keep last N) for write commands (default 10)")
}

// resetGlobalFlagsForTest wipes the package-level flag storage back to
// the init() defaults. Test-only helper — the package's test files call
// this in their harness setup so leftover state from a prior test never
// leaks across.
func resetGlobalFlagsForTest() {
	globalRevealFlag = false
	globalHomeFlag = ""
	globalLockTimeoutFlag = 0
	globalRetentionFlag = 0
}

// validateGlobalFlags is the PersistentPreRunE for rootCmd. It refuses
// nonsensical values (zero-only --lock-timeout / --retention were not
// explicitly rejected because zero means "use default" per flag docs;
// negative values are rejected). --home is passed through
// storage.NewResolverWithHome so the NFR-S3 invariants (absolute,
// exists, not root-owned when non-root, not "/") apply uniformly.
//
// Story AC (E6-S9): "--home /nonexistent → refused; --lock-timeout 0 →
// invalid; --retention 0 → invalid". The story explicitly labels the
// zero-value inputs as invalid — that trumps the "zero means default"
// convention: the operator who typed `--lock-timeout 0` on the command
// line meant "no timeout", not "use the default", and refusing here
// makes the intent unambiguous.
func validateGlobalFlags(cmd *cobra.Command, args []string) error {
	if flagWasChanged(cmd, "lock-timeout") {
		if globalLockTimeoutFlag <= 0 {
			return fmt.Errorf("--lock-timeout must be a positive duration; got %s", globalLockTimeoutFlag)
		}
	}
	if flagWasChanged(cmd, "retention") {
		if globalRetentionFlag <= 0 {
			return fmt.Errorf("--retention must be a positive integer; got %d", globalRetentionFlag)
		}
	}
	if globalHomeFlag != "" {
		if _, err := storage.NewResolverWithHome(globalHomeFlag); err != nil {
			return fmt.Errorf("--home invalid: %w", err)
		}
	}
	return nil
}

// flagWasChanged inspects both the local and the persistent flag set
// for a --name that had .Set() invoked on it (either at flag-parse
// time or in a test via FlagSet.Set). Cobra normally merges persistent
// flags into the local set via mergePersistentFlags at Execute time —
// tests that bypass Execute (unit tests calling validateGlobalFlags
// directly) do not benefit from that merge, so we probe both.
func flagWasChanged(cmd *cobra.Command, name string) bool {
	if cmd.Flags().Changed(name) {
		return true
	}
	if cmd.PersistentFlags().Changed(name) {
		return true
	}
	return false
}

// resolverFromGlobals returns the storage.Resolver every command should
// use. Honors --home when set; otherwise delegates to storage.Default
// which reads $HOME through os.UserHomeDir. Kept in this file (not a
// helpers.go) so the flag → resolver contract is next to the flag
// definition.
func resolverFromGlobals() (*storage.Resolver, error) {
	if globalHomeFlag != "" {
		return storage.NewResolverWithHome(globalHomeFlag)
	}
	return storage.Default()
}

// globalRevealActive combines the global --reveal with an optional
// per-command --reveal flag value. Any true wins — so `--reveal` at
// root OR `claudecm list --reveal` both flip redaction off.
func globalRevealActive(localReveal bool) bool {
	return globalRevealFlag || localReveal
}

// globalLockTimeoutOrDefault returns the effective flock timeout: the
// global --lock-timeout when non-zero, otherwise
// storage.DefaultLockTimeout. Callers that want to build a context
// with a deadline should use lockTimeoutContext; this getter is for
// diagnostic surfaces (e.g. `claudecm version --output json`).
func globalLockTimeoutOrDefault() time.Duration {
	if globalLockTimeoutFlag > 0 {
		return globalLockTimeoutFlag
	}
	return storage.DefaultLockTimeout
}

// lockTimeoutContext returns ctx wrapped with the global --lock-timeout
// as a deadline when set; otherwise returns ctx and a no-op cancel.
// The commit orchestrator (internal/commit/commit.go) honors
// ctx.Deadline for its flock acquisition, so this is the sole wiring
// point needed to make --lock-timeout effective across every write
// command.
func lockTimeoutContext(ctx context.Context) (context.Context, context.CancelFunc) {
	if globalLockTimeoutFlag > 0 {
		return context.WithTimeout(ctx, globalLockTimeoutFlag)
	}
	return ctx, func() {}
}

// globalRetentionOrDefault returns the effective backup-retention
// count: the global --retention when non-zero, otherwise
// storage.DefaultBackupRetention.
func globalRetentionOrDefault() int {
	if globalRetentionFlag > 0 {
		return globalRetentionFlag
	}
	return storage.DefaultBackupRetention
}

// pruneOptionsFromGlobals wraps globalRetentionOrDefault into the
// storage.PruneOptions struct the retention primitive consumes.
func pruneOptionsFromGlobals() storage.PruneOptions {
	return storage.PruneOptions{Keep: globalRetentionOrDefault()}
}

// pruneAllAfterWrite fires storage.PruneAll under the current global
// retention so a switch / restore that just wrote new backups keeps the
// per-(tool, file) count bounded to NFR-R1. Errors are surfaced back to
// the caller (never swallowed) so a broken audit log does not silently
// hide behind a successful commit.
//
// Kept as a helper in root.go so both switch and restore call the same
// wiring point — retention policy is a global concern, not
// per-command state.
func pruneAllAfterWrite(r *storage.Resolver) error {
	_, err := storage.PruneAll(r, pruneOptionsFromGlobals())
	return err
}

// emitRevealNoticeIfNeeded writes the NFR-S8 --reveal warning to w when
// effective is true. Kept as a shared helper so list / current / explain
// print the identical banner and a future edit does not have to update
// three call sites. No-op when effective is false.
func emitRevealNoticeIfNeeded(w io.Writer, effective bool) {
	if !effective {
		return
	}
	fmt.Fprintln(w, "WARNING: --reveal exposes secret values on your terminal and in scrollback.")
}

// encodeIndentedJSON is a shared "encode and flush" helper for JSON
// renderers in this file (version) and the sibling files (list, export,
// completion). Kept here to avoid a per-file dupe of the same three
// lines.
func encodeIndentedJSON(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

// trimAndLower is a common preprocessor used across output-format flag
// validators (parseVersionOutput, parseListOutput, parseExportFormat).
// Kept as a package-level helper because every command's --output flag
// wants "TEXT" / "  text " / "" all mapped to the same canonical form.
func trimAndLower(s string) string {
	return strings.ToLower(strings.TrimSpace(s))
}
