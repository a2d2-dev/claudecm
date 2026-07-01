// Package cmd — switch command (Story E6-S2).
//
// cmd/switch is the E6-S2 rewrite of the pre-refactor MVP. Every write
// this command emits routes through the E7 commit orchestrator
// (internal/commit) — direct sequencing of writepath.Apply across files
// is a coding-standards rule 13 (PRD FR-16) violation, so switch never
// calls Apply itself. It only:
//
//  1. Loads the requested profile and enumerates every adapter registered
//     against adapter.DefaultRegistry (optionally filtered by --tool).
//  2. Calls each adapter's Plan(ctx, r, profile) and concatenates the
//     returned WritePlans into a single slice.
//  3. Stages the plans through commit.Committer.Stage — which acquires
//     per-target flocks in canonical order, computes each diff, and
//     writes each backup — then renders a pre-apply diff.
//  4. Respects --dry-run (Stage-only, no Commit) and --yes (skip the
//     interactive confirmation prompt on a TTY; abort on non-TTY without
//     --yes).
//  5. Commits the staged transaction. Updates State.CurrentProfile and
//     records the per-file (path, sha256, appliedAt) tuple via
//     stateio.RecordApplied so `current` / `explain`'s external-drift
//     detector has a fresh anchor.
//
// Diff rendering redacts secrets per NFR-S8 using the shared redactValue
// helper from cmd/explain.go — the same redaction operators see in
// `current` / `explain`. --reveal is intentionally NOT wired: switch's
// diff describes what is about to change, not what a user asked to be
// shown; a plaintext dump belongs in `explain --reveal`, not here.
//
// Exit codes:
//
//	0 — success (commit succeeded; dry-run finished; no-op switch)
//	1 — generic error (bad flag, missing profile, plan/stage error)
//	2 — commit partial failure (some files committed then rolled back
//	    per commit.PartialFailure semantics)
package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"

	"github.com/spf13/cobra"

	"github.com/a2d2-dev/claudecm/internal/adapter"
	// Side-effect imports register the two v1 adapters into
	// adapter.DefaultRegistry from their init() blocks so switch iterates
	// the same tool set as cmd/current and cmd/explain.
	claudecodeadapter "github.com/a2d2-dev/claudecm/internal/adapter/claudecode"
	codexadapter "github.com/a2d2-dev/claudecm/internal/adapter/codex"
	"github.com/a2d2-dev/claudecm/internal/adapter/stateio"
	"github.com/a2d2-dev/claudecm/internal/commit"
	"github.com/a2d2-dev/claudecm/internal/config"
	"github.com/a2d2-dev/claudecm/internal/storage"
	"github.com/a2d2-dev/claudecm/internal/writepath"
)

// switchOutputFormat mirrors the enum shape used by explain/current so
// switch's --output flag speaks the same language operators already know.
type switchOutputFormat string

const (
	switchOutputText switchOutputFormat = "text"
	switchOutputJSON switchOutputFormat = "json"
)

// switchExitPartialFailure is the exit code the CLI wrapper uses when
// commit.PartialFailure surfaces. Kept as a named constant so tests can
// pin the value and readers can grep "exit 2" back to the source.
const switchExitPartialFailure = 2

var (
	switchOutputFlag string
	switchDryRunFlag bool
	switchYesFlag    bool
	switchToolFlag   string
)

// switchCmd is the cobra binding. The RunE closure wraps runSwitch so
// commit.PartialFailure can be mapped to exit code 2 without leaking
// os.Exit into the tested inner body — the wrapper lives here at the
// registration site so tests call runSwitch directly and see the
// underlying error, not an os.Exit side effect.
var switchCmd = &cobra.Command{
	Use:   "switch [profile-name]",
	Short: "Switch active profile via the two-phase commit orchestrator",
	Long: `Switch the active profile, atomically writing every owned tool
config through the two-phase commit orchestrator (internal/commit).

For the given profile:
  * enumerate every registered adapter (filtered by --tool),
  * call each adapter's Plan(),
  * Stage all WritePlans through commit.Committer,
  * print the pre-apply diff (secrets redacted per NFR-S8),
  * with --dry-run: abort the staged txn and exit 0,
  * without --yes on a non-TTY: abort with a clear message,
  * on TTY: prompt for confirmation,
  * Commit the transaction (all-or-nothing atomicity),
  * update state.yaml (CurrentProfile + LastAppliedPerTool).

EXAMPLES
  # Interactive switch (prompts on TTY)
  claudecm switch prod

  # Non-interactive switch (used by scripts / CI)
  claudecm switch prod --yes

  # Preview only — no writes, no state update
  claudecm switch prod --dry-run

  # Restrict to a single tool
  claudecm switch prod --tool claude_code --yes

  # Emit machine-readable JSON
  claudecm switch prod --output json --dry-run`,
	Args:              cobra.ExactArgs(1),
	ValidArgsFunction: profileNamesCompletion,
	RunE: func(cmd *cobra.Command, args []string) error {
		err := runSwitch(cmd, args)
		if err == nil {
			return nil
		}
		// commit.PartialFailure → exit code 2. We print the error via
		// cobra's usual channel by silencing its own printing and writing
		// to stderr ourselves, then os.Exit(2). runSwitch's tests call
		// the inner function directly and never hit this path.
		var pf *commit.PartialFailure
		if errors.As(err, &pf) {
			cmd.SilenceErrors = true
			cmd.SilenceUsage = true
			fmt.Fprintln(cmd.ErrOrStderr(), err.Error())
			os.Exit(switchExitPartialFailure)
		}
		return err
	},
}

func init() {
	switchCmd.Flags().StringVarP(&switchOutputFlag, "output", "o", "text", "Output format (text|json)")
	switchCmd.Flags().BoolVar(&switchDryRunFlag, "dry-run", false, "Stage + print diff, then abort without committing")
	switchCmd.Flags().BoolVar(&switchYesFlag, "yes", false, "Skip interactive confirmation")
	switchCmd.Flags().StringVar(&switchToolFlag, "tool", "", "Restrict to a comma-separated list of tool IDs (default all)")
	rootCmd.AddCommand(switchCmd)
}

// runSwitch is the testable entry point. Returns nil on success, a
// non-nil error otherwise. A *commit.PartialFailure return signals the
// CLI wrapper to exit with switchExitPartialFailure (2); every other
// non-nil return maps to cobra's default exit 1.
func runSwitch(cmd *cobra.Command, args []string) error {
	profileName := strings.TrimSpace(args[0])
	if profileName == "" {
		return fmt.Errorf("profile name cannot be empty")
	}

	format, err := parseSwitchOutput(switchOutputFlag)
	if err != nil {
		return err
	}

	if switchDryRunFlag && switchYesFlag {
		// Not fatal — dry-run wins (no writes fire either way) — but
		// warn the operator that --yes is a no-op alongside --dry-run
		// so a script author expecting a commit does not silently get a
		// dry-run.
		fmt.Fprintln(cmd.ErrOrStderr(), "NOTE: --yes is ignored under --dry-run (nothing will be committed).")
	}

	resv, err := storage.Default()
	if err != nil {
		return fmt.Errorf("failed to resolve HOME: %w", err)
	}
	if err := storage.Bootstrap(resv); err != nil {
		return fmt.Errorf("failed to bootstrap ~/.claudecm layout: %w", err)
	}
	store := storage.NewFileStorage(resv)
	mgr := config.NewManager(store, config.NewValidator())

	profile, err := mgr.GetProfile(profileName)
	if err != nil {
		return fmt.Errorf("profile %q not found: %w", profileName, err)
	}

	toolFilter := parseToolFilter(switchToolFlag)
	tools, err := selectSwitchTools(toolFilter)
	if err != nil {
		return err
	}

	ctx := context.Background()
	plans, planErrors, err := collectSwitchPlans(ctx, resv, tools, *profile)
	if err != nil {
		return err
	}

	// Empty plan set: nothing to write. Still update state so the active
	// profile pointer moves — a no-op switch is a legitimate outcome
	// when the profile matches the current on-disk intent.
	if len(plans) == 0 {
		if err := updateStateOnSuccess(resv, store, profileName, nil); err != nil {
			return fmt.Errorf("no plans to commit but state update failed: %w", err)
		}
		return renderNoOp(cmd.OutOrStdout(), format, profileName, planErrors)
	}

	committer := commit.NewCommitter()
	txn, err := committer.Stage(ctx, resv, plans)
	if err != nil {
		return fmt.Errorf("stage: %w", err)
	}

	// Render the pre-apply diff. Text mode prints the diff up front so
	// operators always see what would change even when they answer
	// "no". JSON mode DEFERS diff emission until the final terminal
	// state (dry-run / success / partial failure) so the wire form is
	// exactly one top-level JSON document — critical for shell
	// pipelines that jq the stream.
	if format == switchOutputText {
		if err := renderPreApplyDiffText(cmd.OutOrStdout(), txn, planErrors, switchDryRunFlag); err != nil {
			_ = committer.Abort(txn)
			return fmt.Errorf("render diff: %w", err)
		}
	}

	if switchDryRunFlag {
		if err := committer.Abort(txn); err != nil {
			return fmt.Errorf("abort staged txn: %w", err)
		}
		switch format {
		case switchOutputJSON:
			return writeJSON(cmd.OutOrStdout(), jsonSwitch{
				Profile: profileName,
				Action:  "dry-run",
				Diff:    diffToJSON(txn),
				Skipped: jsonSwitchSkipped(planErrors),
			})
		default:
			fmt.Fprintln(cmd.OutOrStdout(), "dry-run complete; no changes made.")
		}
		return nil
	}

	if !switchYesFlag {
		if !isTerminal(os.Stdin) {
			_ = committer.Abort(txn)
			return fmt.Errorf("non-interactive session: pass --yes to confirm the switch or --dry-run to preview")
		}
		ok, promptErr := promptConfirm(cmd.OutOrStdout(), os.Stdin, "Apply the above changes?")
		if promptErr != nil {
			_ = committer.Abort(txn)
			return fmt.Errorf("read confirmation: %w", promptErr)
		}
		if !ok {
			_ = committer.Abort(txn)
			if format == switchOutputText {
				fmt.Fprintln(cmd.OutOrStdout(), "aborted; no changes made.")
			}
			return fmt.Errorf("aborted by user")
		}
	}

	report, commitErr := committer.Commit(ctx, txn)
	if commitErr != nil {
		// Partial-failure path. Render the per-file status block so the
		// operator can see rolled-back / untouched / failed files
		// without hunting through the wrapped error string, then return
		// the *PartialFailure so the CLI wrapper exits with code 2.
		var pf *commit.PartialFailure
		if errors.As(commitErr, &pf) {
			renderPartialFailure(cmd.ErrOrStderr(), format, profileName, pf)
			return commitErr
		}
		return fmt.Errorf("commit: %w", commitErr)
	}

	if err := updateStateOnSuccess(resv, store, profileName, &report); err != nil {
		return fmt.Errorf("commit succeeded but state update failed: %w", err)
	}

	return renderSuccess(cmd.OutOrStdout(), format, profileName, report)
}

// parseSwitchOutput validates and normalises the --output flag.
// Mirrors parseExplainOutput / parseCurrentOutput for consistency.
func parseSwitchOutput(raw string) (switchOutputFormat, error) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "", "text":
		return switchOutputText, nil
	case "json":
		return switchOutputJSON, nil
	default:
		return "", fmt.Errorf("invalid --output %q (want text|json)", raw)
	}
}

// selectSwitchTools returns the ordered list of Adapter constructors
// switch should invoke. When filter is empty every registered adapter
// runs; otherwise only ToolIDs listed in filter run — an unrecognised
// filter entry is an error so a typo does not silently skip a tool.
func selectSwitchTools(filter []adapter.ToolID) ([]adapter.Adapter, error) {
	registered := adapter.DefaultRegistry.List()
	if len(filter) == 0 {
		out := make([]adapter.Adapter, 0, len(registered))
		for _, id := range registered {
			a, ok := adapter.DefaultRegistry.Get(id)
			if !ok {
				continue
			}
			out = append(out, a)
		}
		return out, nil
	}

	// Build a set of registered IDs for cheap membership tests, then
	// return the filter set in registered order — same iteration order
	// the no-filter branch would use, so `--tool a,b,c` cannot re-order
	// the write plan.
	regSet := map[adapter.ToolID]struct{}{}
	for _, id := range registered {
		regSet[id] = struct{}{}
	}
	wanted := map[adapter.ToolID]struct{}{}
	for _, id := range filter {
		if _, ok := regSet[id]; !ok {
			return nil, fmt.Errorf("--tool %q is not a registered adapter (have: %s)", id, strings.Join(toolIDStrings(registered), ", "))
		}
		wanted[id] = struct{}{}
	}
	out := make([]adapter.Adapter, 0, len(wanted))
	for _, id := range registered {
		if _, ok := wanted[id]; !ok {
			continue
		}
		a, ok := adapter.DefaultRegistry.Get(id)
		if !ok {
			continue
		}
		out = append(out, a)
	}
	return out, nil
}

// toolIDStrings converts a []ToolID to a []string for user-facing
// messages. Kept as its own helper so the message renderer never has to
// know the underlying ToolID type.
func toolIDStrings(ids []adapter.ToolID) []string {
	out := make([]string, len(ids))
	for i, id := range ids {
		out[i] = string(id)
	}
	return out
}

// perToolPlanIssue records a per-adapter Plan error that switch decided
// to surface as a skipped-tool warning rather than a fatal — currently
// used only for the ErrNoConfig branch (fresh install, tool not
// configured yet). Every other Plan error is fatal.
type perToolPlanIssue struct {
	Tool    adapter.ToolID
	Message string
}

// collectSwitchPlans invokes Plan on each adapter and concatenates the
// returned WritePlans in adapter iteration order (which matches
// canonical commit order for v1's two tools). Empty results are
// tolerated. ErrNoConfig from an adapter surfaces as a per-tool warning
// so a partially-configured system still switches the tools that ARE
// configured; every other Plan error is fatal.
func collectSwitchPlans(ctx context.Context, r *storage.Resolver, tools []adapter.Adapter, profile config.Profile) ([]writepath.WritePlan, []perToolPlanIssue, error) {
	var plans []writepath.WritePlan
	var issues []perToolPlanIssue
	for _, a := range tools {
		p, err := a.Plan(ctx, r, profile)
		if err != nil {
			// ErrNoConfig from an adapter's dependencies is treated as
			// a skip (the adapter does not yet have a config it can
			// merge into). No adapter Plan currently returns
			// ErrNoConfig, but the check is here so a future adapter
			// that adds a config-required precondition slots in
			// without a switch-side change.
			if isNoConfigErr(err) {
				issues = append(issues, perToolPlanIssue{Tool: a.ID(), Message: err.Error()})
				continue
			}
			return nil, nil, fmt.Errorf("plan %s: %w", a.ID(), err)
		}
		plans = append(plans, p...)
	}
	return plans, issues, nil
}

// isNoConfigErr returns true when err wraps either adapter package's
// ErrNoConfig. Cheap type-erased check that keeps switch decoupled from
// each adapter's package-private sentinel while still recognising the
// documented "fresh install" case.
func isNoConfigErr(err error) bool {
	return errors.Is(err, claudecodeadapter.ErrNoConfig) || errors.Is(err, codexadapter.ErrNoConfig)
}

// updateStateOnSuccess flips the active-profile pointer and, when
// report != nil, records the (path, sha256, appliedAt) tuple for every
// committed file so external-drift detection has a fresh anchor. On
// the empty-plan path (report == nil) only the pointer moves.
func updateStateOnSuccess(r *storage.Resolver, store *storage.FileStorage, profileName string, report *commit.CommitReport) error {
	state, err := store.LoadState()
	if err != nil {
		return fmt.Errorf("load state: %w", err)
	}
	state.SetCurrentProfile(profileName)
	if err := store.SaveState(state); err != nil {
		return fmt.Errorf("save state: %w", err)
	}
	if report == nil {
		return nil
	}
	for _, pf := range report.PerFile {
		if pf.Status != commit.StatusCommitted {
			continue
		}
		if err := stateio.RecordApplied(
			r,
			config.ToolID(pf.Report.Tool),
			pf.Target,
			pf.Report.PostFingerprint.SHA256,
			pf.Report.AppliedAt,
		); err != nil {
			return fmt.Errorf("record applied for %s: %w", pf.Target, err)
		}
	}
	return nil
}

// promptConfirm prints a y/N question and reads a single line from in.
// Returns true iff the answer is a case-insensitive "y" or "yes".
// Empty input (bare Enter) → false, matching the "[y/N]" convention.
func promptConfirm(w io.Writer, in io.Reader, question string) (bool, error) {
	fmt.Fprintf(w, "%s [y/N]: ", question)
	var line string
	// A tiny scanner-free reader keeps the dependency graph minimal and
	// interacts cleanly with the tests' bytes.Reader stdin injection.
	buf := make([]byte, 1)
	for {
		n, err := in.Read(buf)
		if n > 0 {
			if buf[0] == '\n' {
				break
			}
			line += string(buf[0])
		}
		if err != nil {
			if err == io.EOF {
				break
			}
			return false, err
		}
	}
	line = strings.TrimSpace(strings.ToLower(line))
	return line == "y" || line == "yes", nil
}

// isTerminalFn is the swap-able TTY probe. Production callers use
// isTerminal; tests may override this seam via SetIsTerminalForTest to
// force interactive/non-interactive behaviour without wiring a real
// PTY. Kept as a package-level var (documented exception to
// coding-standards rule 12, mirroring the DefaultRegistry / envextract
// lookup pattern): written only at init or test-swap time, read
// thereafter.
var isTerminalFn = defaultIsTerminal

// isTerminal reports whether f is a character device (a TTY). Routed
// through isTerminalFn so tests can override deterministically.
func isTerminal(f *os.File) bool { return isTerminalFn(f) }

// defaultIsTerminal is the production TTY probe. Uses os.Stat +
// Mode()&os.ModeCharDevice which is portable across Linux/macOS/Windows
// without a term-package dependency. A stat failure is conservatively
// reported as false so a session with a broken stdin falls through to
// the non-interactive abort branch — safer than prompting a user who is
// not there.
func defaultIsTerminal(f *os.File) bool {
	if f == nil {
		return false
	}
	fi, err := f.Stat()
	if err != nil {
		return false
	}
	return (fi.Mode() & os.ModeCharDevice) != 0
}

// SetIsTerminalForTest overrides the TTY probe. Returns a restore
// closure the caller MUST defer to put the production probe back.
// Test-only helper — production code has no legitimate reason to swap
// the probe. Symmetric with envextract.SetLookupForTest.
func SetIsTerminalForTest(fn func(*os.File) bool) func() {
	prev := isTerminalFn
	isTerminalFn = fn
	return func() { isTerminalFn = prev }
}

// ---------------------------------------------------------------------------
// Rendering
// ---------------------------------------------------------------------------

// renderNoOp emits the "nothing to switch" body. The empty-plan path
// still updates state (active profile pointer moved) so the message
// communicates both: no files changed, but the active profile has been
// set.
func renderNoOp(w io.Writer, format switchOutputFormat, profileName string, issues []perToolPlanIssue) error {
	if format == switchOutputJSON {
		out := jsonSwitch{
			Profile: profileName,
			Action:  "no-op",
			Diff:    []jsonSwitchDiff{},
			Skipped: jsonSwitchSkipped(issues),
		}
		return writeJSON(w, out)
	}
	fmt.Fprintf(w, "Switched to %q (no files needed changes).\n", profileName)
	for _, iss := range issues {
		fmt.Fprintf(w, "  skipped %s: %s\n", iss.Tool, iss.Message)
	}
	return nil
}

// renderPreApplyDiffText prints the per-file diff block to w. Text-only:
// JSON callers defer diff emission to the final render (dry-run /
// success / partial-failure) so the wire stays a single top-level
// document — that decision lives in runSwitch. dryRun toggles the
// trailing hint line — a dry-run tells the operator this is a preview;
// a real run tells them how to confirm.
func renderPreApplyDiffText(w io.Writer, txn commit.StagedTxn, issues []perToolPlanIssue, dryRun bool) error {
	fmt.Fprintln(w, "Pre-apply diff:")
	fmt.Fprintln(w)
	for _, pf := range txn.Prepared {
		renderPreparedFileText(w, pf)
	}
	for _, iss := range issues {
		fmt.Fprintf(w, "skipped %s: %s\n", iss.Tool, iss.Message)
	}
	fmt.Fprintln(w)
	if dryRun {
		fmt.Fprintln(w, "--dry-run: nothing will be written.")
	} else {
		fmt.Fprintln(w, "Would apply the above changes. Confirm with --yes or answer the prompt.")
	}
	return nil
}

// renderPreparedFileText renders one PreparedFile's diff block in the
// human-readable form the story mocks up. Skipped files print a
// "no changes" note so the pre-apply diff still enumerates every plan
// (operator visibility into what the switch touched).
func renderPreparedFileText(w io.Writer, pf commit.PreparedFile) {
	target := pf.Plan.Target
	fmt.Fprintf(w, "%s (%s):\n", pf.Plan.Tool, target)
	if pf.Skipped {
		fmt.Fprintln(w, "  Owned key changes: (none — already at intended state)")
		fmt.Fprintln(w, "  Non-owned keys: preserved verbatim")
		fmt.Fprintln(w)
		return
	}
	changes := 0
	for _, k := range sortedStrings(pf.Diff.Added) {
		fmt.Fprintf(w, "  + %s: %s\n", k, redactedValueDisplay(k, findAddedValue(pf.Diff, k)))
		changes++
	}
	for _, k := range sortedStrings(pf.Diff.Removed) {
		fmt.Fprintf(w, "  - %s\n", k)
		changes++
	}
	for _, kd := range sortedKeyDeltas(pf.Diff.Changed) {
		fmt.Fprintf(w, "  ~ %s: %s -> %s\n",
			kd.Key,
			redactedValueDisplay(kd.Key, kd.OldValue),
			redactedValueDisplay(kd.Key, kd.NewValue),
		)
		changes++
	}
	if changes == 0 {
		fmt.Fprintln(w, "  Owned key changes: (none)")
	}
	fmt.Fprintln(w, "  Non-owned keys: preserved verbatim")
	fmt.Fprintln(w)
}

// sortedStrings returns a defensive copy sorted lexicographically so
// diff output ordering is stable across runs regardless of upstream
// map iteration order.
func sortedStrings(in []string) []string {
	out := make([]string, len(in))
	copy(out, in)
	sort.Strings(out)
	return out
}

// sortedKeyDeltas returns a defensive copy sorted by Key so the changed
// lines render in stable order.
func sortedKeyDeltas(in []writepath.KeyDelta) []writepath.KeyDelta {
	out := make([]writepath.KeyDelta, len(in))
	copy(out, in)
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out
}

// findAddedValue is a placeholder for the missing per-key value on
// Diff.Added — the current DiffResult carries the added key names but
// no value slot for them (Added is a []string). Returning nil keeps
// the renderer honest: "added, value not tracked" prints as the
// redacted-nil sentinel or an empty string, both of which read as
// "value opaque" to the operator. If future work extends DiffResult
// with per-add values, replace this shim.
func findAddedValue(_ writepath.DiffResult, _ string) any { return nil }

// redactedValueDisplay wraps redactValue with the secret-key heuristic
// switch uses in isolation from the resolver: any key whose last
// segment case-insensitively matches _KEY / _TOKEN / _SECRET is
// treated as a secret and redacted. Non-secret values pass through
// fmt.Sprint. Nil renders as the empty string so the diff arrow reads
// cleanly.
//
// This function intentionally does NOT accept a --reveal knob: switch's
// diff is a "what is about to change" summary — plaintext dumps belong
// in `explain --reveal`, not here. Keeping redaction unconditional
// removes the operator-scripted footgun of piping a switch diff to a
// log with --reveal set.
func redactedValueDisplay(key string, v any) string {
	if isSecretKey(key) {
		return redactValue(v)
	}
	if v == nil {
		return ""
	}
	return fmt.Sprint(v)
}

// isSecretKey inspects the last dotted segment of key and reports
// whether it looks like a secret. Matches the ADR-0001 / NFR-S8
// heuristic used elsewhere in the codebase.
func isSecretKey(key string) bool {
	last := key
	if i := strings.LastIndex(key, "."); i >= 0 {
		last = key[i+1:]
	}
	upper := strings.ToUpper(last)
	return strings.HasSuffix(upper, "_KEY") ||
		strings.HasSuffix(upper, "_TOKEN") ||
		strings.HasSuffix(upper, "_SECRET") ||
		upper == "APIKEY" || upper == "AUTHTOKEN"
}

// renderSuccess emits the post-commit summary. Lists every committed
// file plus the backup path storage.Backup created for it so an
// operator inspecting the transaction later can find the pre-write
// bytes.
func renderSuccess(w io.Writer, format switchOutputFormat, profileName string, report commit.CommitReport) error {
	if format == switchOutputJSON {
		out := jsonSwitch{
			Profile: profileName,
			Action:  "commit",
			Report:  reportToJSON(report),
		}
		// Populate Diff from the per-file reports so JSON pipelines
		// see the same shape they would from a dry-run. Text mode
		// already printed the diff before Commit fired; JSON mode
		// delayed it to keep a single top-level document.
		out.Diff = diffFromReport(report)
		return writeJSON(w, out)
	}
	fmt.Fprintf(w, "Switched to %q.\n", profileName)
	for _, pf := range report.PerFile {
		switch pf.Status {
		case commit.StatusCommitted:
			fmt.Fprintf(w, "  committed %s (backup: %s)\n", pf.Target, pf.Backup.BackupPath)
		case commit.StatusUntouched:
			fmt.Fprintf(w, "  untouched %s\n", pf.Target)
		}
	}
	return nil
}

// diffFromReport reconstructs a minimal JSON diff shape from a
// CommitReport's per-file WriteReport slice. The per-file WriteReport
// carries the same DiffResult that Stage computed, so JSON consumers
// see the same key set as a dry-run against the same profile — no
// re-parsing required.
func diffFromReport(report commit.CommitReport) []jsonSwitchDiff {
	out := make([]jsonSwitchDiff, 0, len(report.PerFile))
	for _, pf := range report.PerFile {
		entry := jsonSwitchDiff{
			Tool:         pf.Report.Tool,
			Target:       pf.Target,
			Skipped:      pf.Status == commit.StatusUntouched,
			OwnedChanges: []jsonSwitchChange{},
		}
		diff := pf.Report.Diff
		for _, k := range sortedStrings(diff.Added) {
			entry.OwnedChanges = append(entry.OwnedChanges, jsonSwitchChange{
				Op:       "added",
				Key:      k,
				NewValue: redactedValueDisplay(k, findAddedValue(diff, k)),
			})
		}
		for _, k := range sortedStrings(diff.Removed) {
			entry.OwnedChanges = append(entry.OwnedChanges, jsonSwitchChange{
				Op:  "removed",
				Key: k,
			})
		}
		for _, kd := range sortedKeyDeltas(diff.Changed) {
			entry.OwnedChanges = append(entry.OwnedChanges, jsonSwitchChange{
				Op:       "changed",
				Key:      kd.Key,
				OldValue: redactedValueDisplay(kd.Key, kd.OldValue),
				NewValue: redactedValueDisplay(kd.Key, kd.NewValue),
			})
		}
		out = append(out, entry)
	}
	return out
}

// renderPartialFailure writes the partial-failure block to stderr so
// a JSON stdout pipeline is not polluted. The block enumerates
// rolled-back / untouched / failed files with the specific cause the
// commit orchestrator returned.
func renderPartialFailure(w io.Writer, format switchOutputFormat, profileName string, pf *commit.PartialFailure) {
	if format == switchOutputJSON {
		out := jsonSwitchPartial{
			Profile:    profileName,
			FailedFile: pf.FailedFile,
			Cause:      pf.Cause.Error(),
			RolledBack: pf.RolledBack,
			Untouched:  pf.Untouched,
		}
		_ = writeJSON(w, out)
		return
	}
	fmt.Fprintln(w, "commit failed partway through:")
	fmt.Fprintf(w, "  failed: %s\n    cause: %v\n", pf.FailedFile, pf.Cause)
	for _, p := range pf.RolledBack {
		fmt.Fprintf(w, "  rolled-back: %s\n", p)
	}
	for _, p := range pf.Untouched {
		fmt.Fprintf(w, "  untouched: %s\n", p)
	}
}

// ---------------------------------------------------------------------------
// JSON wire types
// ---------------------------------------------------------------------------

// jsonSwitch is the top-level JSON document. Fields present under
// dry-run: Profile, Action, Diff, Skipped. Fields present under a
// successful commit: Profile, Action, Report. The two shapes are
// kept in one struct with omitempty tags rather than split into
// separate types so a downstream JSON consumer can decode both with
// one schema and inspect Action to route.
type jsonSwitch struct {
	Profile string            `json:"profile"`
	Action  string            `json:"action"`
	Diff    []jsonSwitchDiff  `json:"diff,omitempty"`
	Skipped []jsonSwitchSkip  `json:"skipped,omitempty"`
	Report  *jsonSwitchReport `json:"report,omitempty"`
}

// jsonSwitchDiff is one file's diff block on the wire.
type jsonSwitchDiff struct {
	Tool         string             `json:"tool"`
	Target       string             `json:"target"`
	Skipped      bool               `json:"skipped"`
	OwnedChanges []jsonSwitchChange `json:"owned_changes"`
}

// jsonSwitchChange is one added/removed/changed key. Op is one of
// "added" / "removed" / "changed". OldValue / NewValue are populated
// per-op; secrets are redacted via redactedValueDisplay.
type jsonSwitchChange struct {
	Op       string `json:"op"`
	Key      string `json:"key"`
	OldValue string `json:"old_value,omitempty"`
	NewValue string `json:"new_value,omitempty"`
}

// jsonSwitchSkip is one adapter that was skipped (fresh install, etc).
type jsonSwitchSkip struct {
	Tool    string `json:"tool"`
	Message string `json:"message"`
}

// jsonSwitchReport is the post-commit summary.
type jsonSwitchReport struct {
	Committed []jsonSwitchCommitted `json:"committed"`
	Backups   []string              `json:"backups"`
}

// jsonSwitchCommitted describes one committed file.
type jsonSwitchCommitted struct {
	Tool   string `json:"tool"`
	Target string `json:"target"`
	SHA256 string `json:"sha256"`
	Backup string `json:"backup,omitempty"`
}

// jsonSwitchPartial is the partial-failure JSON body.
type jsonSwitchPartial struct {
	Profile    string   `json:"profile"`
	FailedFile string   `json:"failed_file"`
	Cause      string   `json:"cause"`
	RolledBack []string `json:"rolled_back,omitempty"`
	Untouched  []string `json:"untouched,omitempty"`
}

// diffToJSON converts a StagedTxn's Prepared slice into JSON-friendly
// entries. Preserves adapter-emitted order (Plans order) so the JSON
// output matches the text output line-for-line.
func diffToJSON(txn commit.StagedTxn) []jsonSwitchDiff {
	out := make([]jsonSwitchDiff, 0, len(txn.Prepared))
	for _, pf := range txn.Prepared {
		entry := jsonSwitchDiff{
			Tool:         pf.Plan.Tool,
			Target:       pf.Plan.Target,
			Skipped:      pf.Skipped,
			OwnedChanges: []jsonSwitchChange{},
		}
		for _, k := range sortedStrings(pf.Diff.Added) {
			entry.OwnedChanges = append(entry.OwnedChanges, jsonSwitchChange{
				Op:       "added",
				Key:      k,
				NewValue: redactedValueDisplay(k, findAddedValue(pf.Diff, k)),
			})
		}
		for _, k := range sortedStrings(pf.Diff.Removed) {
			entry.OwnedChanges = append(entry.OwnedChanges, jsonSwitchChange{
				Op:  "removed",
				Key: k,
			})
		}
		for _, kd := range sortedKeyDeltas(pf.Diff.Changed) {
			entry.OwnedChanges = append(entry.OwnedChanges, jsonSwitchChange{
				Op:       "changed",
				Key:      kd.Key,
				OldValue: redactedValueDisplay(kd.Key, kd.OldValue),
				NewValue: redactedValueDisplay(kd.Key, kd.NewValue),
			})
		}
		out = append(out, entry)
	}
	return out
}

// reportToJSON converts a commit.CommitReport into the JSON success
// body. Only StatusCommitted rows land in the committed slice; skipped
// / untouched files are noise for a successful-commit summary.
func reportToJSON(report commit.CommitReport) *jsonSwitchReport {
	out := &jsonSwitchReport{
		Committed: []jsonSwitchCommitted{},
		Backups:   []string{},
	}
	for _, pf := range report.PerFile {
		if pf.Status != commit.StatusCommitted {
			continue
		}
		out.Committed = append(out.Committed, jsonSwitchCommitted{
			Tool:   pf.Report.Tool,
			Target: pf.Target,
			SHA256: pf.Report.PostFingerprint.SHA256,
			Backup: pf.Backup.BackupPath,
		})
		if pf.Backup.BackupPath != "" {
			out.Backups = append(out.Backups, pf.Backup.BackupPath)
		}
	}
	return out
}

// jsonSwitchSkipped converts perToolPlanIssue slice to its wire shape.
func jsonSwitchSkipped(issues []perToolPlanIssue) []jsonSwitchSkip {
	out := make([]jsonSwitchSkip, 0, len(issues))
	for _, iss := range issues {
		out = append(out, jsonSwitchSkip{Tool: string(iss.Tool), Message: iss.Message})
	}
	return out
}

// writeJSON is the shared "encode and flush" helper used by every JSON
// renderer. Kept as a one-liner so all switch JSON output uses the
// same indent + newline discipline.
func writeJSON(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}
