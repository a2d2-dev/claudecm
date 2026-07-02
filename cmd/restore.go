// Package cmd — restore command (Story E6-S7).
//
// cmd/restore lists tool-config backups and reverts owned files to a
// chosen snapshot. All writes route through the two-phase commit
// orchestrator (internal/commit) — the same FR-5 pipeline switch uses —
// so multi-file restores (e.g. Codex's auth.json + config.toml pair)
// are atomic: either both files revert or none do, and the pre-restore
// bytes are captured as a fresh backup before any rename fires.
//
// Shape:
//
//	claudecm restore --tool <claude-code|codex>
//	                 [--list | --latest | --id <backup-id>]
//	                 [--dry-run] [--yes] [--output text|json]
//
// Modes:
//
//	--list         : enumerate every backup for the tool's owned files,
//	                 newest first, with timestamp + size + full path.
//	--latest       : restore the newest backup for EACH owned file.
//	--id <id>      : restore that specific backup. The id may be the
//	                 full backup filename or its ".bak.<timestamp>"
//	                 suffix; auto-detects which owned file the backup
//	                 belongs to. Fails if two files share the id.
//
// --dry-run: print the pre-apply summary (owned file, current SHA / size,
// backup SHA / size / timestamp) and exit 0 without writing.
//
// Never DELETES backups; retention lives in storage.PruneBackups.
//
// Exit codes: 0 on success (including --dry-run); 1 on generic errors;
// 2 on commit.PartialFailure (multi-file restore where one file failed
// mid-commit and Committer rolled back the ones already written).
package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/spf13/cobra"

	"github.com/a2d2-dev/claudecm/internal/adapter"
	// Side-effect imports register the v1 adapters into
	// adapter.DefaultRegistry from their init() blocks so restore
	// iterates the same tool set as switch / current / explain.
	_ "github.com/a2d2-dev/claudecm/internal/adapter/claudecode"
	_ "github.com/a2d2-dev/claudecm/internal/adapter/codex"
	"github.com/a2d2-dev/claudecm/internal/adapter/stateio"
	"github.com/a2d2-dev/claudecm/internal/commit"
	"github.com/a2d2-dev/claudecm/internal/config"
	"github.com/a2d2-dev/claudecm/internal/storage"
	"github.com/a2d2-dev/claudecm/internal/writepath"
)

// restoreOutputFormat mirrors the enum shape used by every other
// cmd/*.
type restoreOutputFormat string

const (
	restoreOutputText restoreOutputFormat = "text"
	restoreOutputJSON restoreOutputFormat = "json"
)

// restoreExitPartialFailure is the exit code the CLI wrapper uses when
// commit.PartialFailure surfaces from a multi-file restore. Kept as a
// named constant symmetric with switchExitPartialFailure.
const restoreExitPartialFailure = 2

// restoreToolAlias maps the CLI positional spelling to the corresponding
// adapter.ToolID. Frozen at two entries for v1 (ADR-0001 Decision 1);
// symmetric with importToolAlias in cmd/import.
var restoreToolAlias = map[string]adapter.ToolID{
	"claude-code": adapter.ToolClaudeCode,
	"codex":       adapter.ToolCodex,
}

var (
	restoreToolFlag   string
	restoreListFlag   bool
	restoreLatestFlag bool
	restoreIDFlag     string
	restoreDryRunFlag bool
	restoreYesFlag    bool
	restoreOutputFlag string
)

var restoreCmd = &cobra.Command{
	Use:   "restore",
	Short: "List backups or revert owned files to a chosen snapshot",
	Long: `List backups or revert a tool's owned files to a chosen snapshot.

Modes:
  --list          Enumerate every backup for the tool's owned files,
                  newest first, with timestamp + size + full path.
  --latest        Restore the newest backup for each owned file of the
                  tool. Multi-file restores are atomic via the two-phase
                  commit pipeline (FR-5 / FR-16).
  --id <id>       Restore a specific backup, identified by its filename
                  or the ".bak.<timestamp>" suffix. The id must uniquely
                  identify one backup among all the tool's owned files.

--dry-run prints the pre-apply summary and exits without writing.
--yes skips the interactive confirmation prompt.

The pre-restore state is captured as a fresh backup before any rename
fires (FR-5). Existing backups are never deleted.`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		err := runRestore(cmd, args)
		if err == nil {
			return nil
		}
		var pf *commit.PartialFailure
		if errors.As(err, &pf) {
			cmd.SilenceErrors = true
			cmd.SilenceUsage = true
			if format, ferr := parseRestoreOutput(restoreOutputFlag); ferr == nil && format != restoreOutputJSON {
				fmt.Fprintln(cmd.ErrOrStderr(), err.Error())
			}
			os.Exit(restoreExitPartialFailure)
		}
		return err
	},
}

func init() {
	restoreCmd.Flags().StringVar(&restoreToolFlag, "tool", "", "Tool to restore (claude-code|codex)")
	restoreCmd.Flags().BoolVar(&restoreListFlag, "list", false, "Enumerate backups and exit (mutually exclusive with --latest/--id)")
	restoreCmd.Flags().BoolVar(&restoreLatestFlag, "latest", false, "Restore the newest backup for each owned file")
	restoreCmd.Flags().StringVar(&restoreIDFlag, "id", "", "Restore a specific backup (filename or .bak.<timestamp> suffix)")
	restoreCmd.Flags().BoolVar(&restoreDryRunFlag, "dry-run", false, "Print the pre-apply summary and exit without writing")
	restoreCmd.Flags().BoolVar(&restoreYesFlag, "yes", false, "Skip interactive confirmation")
	restoreCmd.Flags().StringVarP(&restoreOutputFlag, "output", "o", "text", "Output format (text|json)")
	rootCmd.AddCommand(restoreCmd)
}

// runRestore is the testable entry point. Returns nil on success
// (including --dry-run), a non-nil error otherwise. A *commit.PartialFailure
// return signals the CLI wrapper to exit with restoreExitPartialFailure (2);
// every other non-nil return maps to cobra's default exit 1.
func runRestore(cmd *cobra.Command, args []string) error {
	format, err := parseRestoreOutput(restoreOutputFlag)
	if err != nil {
		return err
	}

	toolArg := strings.TrimSpace(restoreToolFlag)
	if toolArg == "" {
		return fmt.Errorf("--tool is required (want one of: %s)", strings.Join(sortedRestoreToolAliases(), ", "))
	}
	toolID, ok := restoreToolAlias[toolArg]
	if !ok {
		return fmt.Errorf("unknown --tool %q (want one of: %s)", toolArg, strings.Join(sortedRestoreToolAliases(), ", "))
	}

	// Mode selection: exactly one of --list / --latest / --id must fire.
	modeCount := 0
	if restoreListFlag {
		modeCount++
	}
	if restoreLatestFlag {
		modeCount++
	}
	if restoreIDFlag != "" {
		modeCount++
	}
	if modeCount == 0 {
		return fmt.Errorf("one of --list, --latest, or --id is required")
	}
	if modeCount > 1 {
		return fmt.Errorf("--list, --latest, and --id are mutually exclusive")
	}

	resv, err := resolverFromGlobals()
	if err != nil {
		return fmt.Errorf("failed to resolve HOME: %w", err)
	}
	if err := storage.Bootstrap(resv); err != nil {
		return fmt.Errorf("failed to bootstrap ~/.claudecm layout: %w", err)
	}

	a, ok := adapter.DefaultRegistry.Get(toolID)
	if !ok {
		return fmt.Errorf("no adapter registered for tool %q", toolArg)
	}
	owned := a.Files(resv)
	if len(owned) == 0 {
		return fmt.Errorf("adapter %q reported no owned files", toolArg)
	}

	if restoreListFlag {
		return runRestoreList(cmd.OutOrStdout(), format, resv, toolID, toolArg, owned)
	}

	// Restore path (either --latest or --id).
	plans, sources, skipped, err := planRestore(resv, toolID, toolArg, owned)
	if err != nil {
		return err
	}
	if len(plans) == 0 {
		return fmt.Errorf("no backups available to restore for tool %q", toolArg)
	}

	if restoreDryRunFlag {
		return renderRestoreDryRun(cmd.OutOrStdout(), format, toolArg, sources, skipped)
	}

	// Interactive confirm unless --yes. Non-interactive session without
	// --yes is a fatal error rather than a silent hang.
	if !restoreYesFlag {
		if !isTerminal(os.Stdin) {
			return fmt.Errorf("non-interactive session: pass --yes to confirm the restore or --dry-run to preview")
		}
		renderRestoreConfirmSummary(cmd.OutOrStdout(), toolArg, sources, skipped)
		ok, promptErr := promptConfirm(cmd.OutOrStdout(), os.Stdin, "Apply the above restore?")
		if promptErr != nil {
			return fmt.Errorf("read confirmation: %w", promptErr)
		}
		if !ok {
			if format == restoreOutputText {
				fmt.Fprintln(cmd.OutOrStdout(), "aborted; no changes made.")
			}
			return fmt.Errorf("aborted by user")
		}
	}

	// Honor --lock-timeout: wrap the base context with a deadline so
	// commit.Committer picks it up. See cmd/switch.go for the symmetric
	// wiring rationale.
	ctx, cancel := lockTimeoutContext(context.Background())
	defer cancel()
	committer := commit.NewCommitter()
	txn, err := committer.Stage(ctx, resv, plans)
	if err != nil {
		return fmt.Errorf("stage: %w", err)
	}
	report, commitErr := committer.Commit(ctx, txn)
	if commitErr != nil {
		// PartialFailure propagates untouched so the CLI wrapper exits
		// with restoreExitPartialFailure. Wrapping would break
		// errors.As at the RunE boundary.
		var pf *commit.PartialFailure
		if errors.As(commitErr, &pf) {
			renderRestorePartialFailure(cmd.ErrOrStderr(), format, toolArg, pf)
			return commitErr
		}
		return fmt.Errorf("commit: %w", commitErr)
	}

	// Update state.LastAppliedPerTool so external-drift detection has
	// a fresh anchor per file. Mirrors updateStateOnSuccess in switch.go
	// but without moving the active-profile pointer — restore does not
	// change which profile is active, only the on-disk tool bytes.
	for _, pf := range report.PerFile {
		if pf.Status != commit.StatusCommitted {
			continue
		}
		if err := stateio.RecordApplied(
			resv,
			config.ToolID(pf.Report.Tool),
			pf.Target,
			pf.Report.PostFingerprint.SHA256,
			pf.Report.AppliedAt,
		); err != nil {
			return fmt.Errorf("record applied for %s: %w", pf.Target, err)
		}
	}

	// NFR-R1: enforce backup retention after every successful restore.
	// storage.PruneAll is a no-op when the (tool, basename) backup
	// count is at or below the retention target; a broken audit log
	// surfaces here as a "restore worked but housekeeping failed"
	// condition an operator sees rather than a silent swallow.
	if err := pruneAllAfterWrite(resv); err != nil {
		return fmt.Errorf("restore succeeded but backup pruning failed: %w", err)
	}

	return renderRestoreSuccess(cmd.OutOrStdout(), format, toolArg, report, sources, skipped)
}

// parseRestoreOutput validates and normalises the --output flag.
func parseRestoreOutput(raw string) (restoreOutputFormat, error) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "", "text":
		return restoreOutputText, nil
	case "json":
		return restoreOutputJSON, nil
	default:
		return "", fmt.Errorf("invalid --output %q (want text|json)", raw)
	}
}

// sortedRestoreToolAliases returns the CLI-visible tool spellings sorted
// for a stable error message.
func sortedRestoreToolAliases() []string {
	out := make([]string, 0, len(restoreToolAlias))
	for k := range restoreToolAlias {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// ---------------------------------------------------------------------------
// --list mode
// ---------------------------------------------------------------------------

// runRestoreList enumerates every backup for the tool's owned files
// and prints them newest first. A missing backup dir is treated as
// "no backups yet" rather than an error — a fresh install has neither
// backed up nor restored anything.
func runRestoreList(w io.Writer, format restoreOutputFormat, resv *storage.Resolver, toolID adapter.ToolID, toolArg string, owned adapter.OwnedFiles) error {
	entries := []restoreListEntry{}
	for _, of := range owned {
		basename := filepath.Base(of.Path)
		recs, err := storage.ListBackups(resv, string(toolID), basename)
		if err != nil {
			return fmt.Errorf("list backups for %s: %w", of.Path, err)
		}
		for _, rec := range recs {
			entries = append(entries, restoreListEntry{
				Tool:       string(toolID),
				Owned:      of.Path,
				Basename:   basename,
				BackupPath: rec.BackupPath,
				Timestamp:  rec.Timestamp.UTC().Format("2006-01-02T15:04:05.000000000Z07:00"),
				Size:       rec.Fingerprint.Size,
				SHA256:     rec.Fingerprint.SHA256,
			})
		}
	}
	// Newest first across ALL owned files. Sort by the ISO-8601
	// Timestamp string — it is fixed-width so lexicographic and
	// chronological order agree, and the timestamp field is independent
	// of the basename prefix. Sorting by BackupPath would let the
	// basename dominate the ordering (e.g. "auth.json.bak.*" always
	// sorting after "config.toml.bak.*"), which is the F1 review
	// finding.
	sort.Slice(entries, func(i, j int) bool { return entries[i].Timestamp > entries[j].Timestamp })

	if format == restoreOutputJSON {
		return writeRestoreJSON(w, jsonRestoreList{
			Action:  "list",
			Tool:    toolArg,
			Entries: entries,
		})
	}
	if len(entries) == 0 {
		fmt.Fprintf(w, "No backups found for tool %q.\n", toolArg)
		return nil
	}
	fmt.Fprintf(w, "Backups for tool %q (newest first):\n", toolArg)
	for _, e := range entries {
		fmt.Fprintf(w, "  %s  size=%d  %s\n    -> %s\n", e.Timestamp, e.Size, e.Basename, e.BackupPath)
	}
	return nil
}

type restoreListEntry struct {
	Tool       string `json:"tool"`
	Owned      string `json:"owned_file"`
	Basename   string `json:"basename"`
	BackupPath string `json:"backup_path"`
	Timestamp  string `json:"timestamp"`
	Size       int64  `json:"size"`
	SHA256     string `json:"sha256"`
}

// ---------------------------------------------------------------------------
// Restore planning
// ---------------------------------------------------------------------------

// restoreSource is the resolved (owned file → chosen backup) pair we
// intend to restore. Populated by planRestore and used by every
// downstream renderer.
type restoreSource struct {
	Tool       adapter.ToolID
	OwnedFile  string
	Basename   string
	Backup     storage.BackupRecord
	Current    storage.Fingerprint
	CurrentHas bool
}

// restoreSkipped records an owned file that --latest could not restore
// because no backups exist for it. Surfaced to text output as a warning
// line and to JSON output under a "skipped" array so operators do not
// misread partial restores as full ones (F5 review finding).
type restoreSkipped struct {
	OwnedFile string `json:"file"`
	Reason    string `json:"reason"`
}

// planRestore resolves the restore intent into a slice of WritePlans
// + a parallel slice of restoreSource metadata used by the renderers.
// For --latest: one WritePlan per owned file that has at least one
// backup. For --id: exactly one WritePlan, matching whichever owned
// file's backup dir contains the id.
func planRestore(resv *storage.Resolver, toolID adapter.ToolID, toolArg string, owned adapter.OwnedFiles) ([]writepath.WritePlan, []restoreSource, []restoreSkipped, error) {
	var plans []writepath.WritePlan
	var sources []restoreSource
	var skipped []restoreSkipped

	if restoreLatestFlag {
		for _, of := range owned {
			basename := filepath.Base(of.Path)
			recs, err := storage.ListBackups(resv, string(toolID), basename)
			if err != nil {
				return nil, nil, nil, fmt.Errorf("list backups for %s: %w", of.Path, err)
			}
			if len(recs) == 0 {
				// A tool with multiple owned files (Codex) may have
				// backups for only one. Record the skip so runRestore
				// can surface a warning line to the operator — silent
				// skips let partial restores masquerade as full ones
				// (F5 review finding).
				skipped = append(skipped, restoreSkipped{OwnedFile: of.Path, Reason: "no backups"})
				continue
			}
			latest := recs[0]
			plan, src, err := buildRestorePlan(resv, toolID, of, latest)
			if err != nil {
				return nil, nil, nil, err
			}
			plans = append(plans, plan)
			sources = append(sources, src)
		}
		return plans, sources, skipped, nil
	}

	// --id path. Iterate owned files, look for a backup filename that
	// equals the id or ends with it (so ".bak.<ts>" suffix is a legal
	// shorthand). Refuse if two owned files' backups match — the id is
	// ambiguous.
	id := strings.TrimSpace(restoreIDFlag)
	if id == "" {
		return nil, nil, nil, fmt.Errorf("--id is empty")
	}
	type match struct {
		of  adapter.OwnedFile
		rec storage.BackupRecord
	}
	var matches []match
	for _, of := range owned {
		basename := filepath.Base(of.Path)
		recs, err := storage.ListBackups(resv, string(toolID), basename)
		if err != nil {
			return nil, nil, nil, fmt.Errorf("list backups for %s: %w", of.Path, err)
		}
		for _, rec := range recs {
			bn := filepath.Base(rec.BackupPath)
			if bn == id || strings.HasSuffix(bn, id) {
				matches = append(matches, match{of: of, rec: rec})
			}
		}
	}
	switch len(matches) {
	case 0:
		return nil, nil, nil, fmt.Errorf("no backup matching --id %q found for tool %q", id, toolArg)
	case 1:
		plan, src, err := buildRestorePlan(resv, toolID, matches[0].of, matches[0].rec)
		if err != nil {
			return nil, nil, nil, err
		}
		return []writepath.WritePlan{plan}, []restoreSource{src}, nil, nil
	default:
		names := make([]string, 0, len(matches))
		for _, m := range matches {
			names = append(names, filepath.Base(m.rec.BackupPath))
		}
		sort.Strings(names)
		return nil, nil, nil, fmt.Errorf("--id %q matches multiple backups (%s); pass the full filename to disambiguate", id, strings.Join(names, ", "))
	}
}

// buildRestorePlan reads a backup file and builds the corresponding
// WritePlan. NewContent is the backup bytes verbatim; Transform is nil
// so commit.Committer takes them as-is. Parser is nil — the backup was
// produced by a previous claudecm write against the same allowlist, so
// re-parsing on read would only fire on backups that are already
// corrupt, and the copy-of-copy failure is not a check restore should
// gate on. Storage.Backup captures the pre-restore state before we
// rename, so the "safety net" is intact even without a parser.
func buildRestorePlan(resv *storage.Resolver, toolID adapter.ToolID, of adapter.OwnedFile, backup storage.BackupRecord) (writepath.WritePlan, restoreSource, error) {
	body, err := os.ReadFile(backup.BackupPath)
	if err != nil {
		return writepath.WritePlan{}, restoreSource{}, fmt.Errorf("read backup %q: %w", backup.BackupPath, err)
	}
	curFP, curExists, err := storage.Stat(of.Path)
	if err != nil {
		return writepath.WritePlan{}, restoreSource{}, fmt.Errorf("stat current %q: %w", of.Path, err)
	}
	// Backup record's SourcePath was empty at ListBackups time; fill it
	// in from the owned-file path so the source metadata reads cleanly.
	backup.SourcePath = of.Path
	plan := writepath.WritePlan{
		Tool:         string(toolID),
		Target:       of.Path,
		NewContent:   body,
		OwnedKeys:    of.OwnedKeys,
		Reason:       fmt.Sprintf("restore from %s", filepath.Base(backup.BackupPath)),
		AllowUnowned: true, // restore is byte-for-byte replay of a previous state — the owned/unowned distinction does not apply.
	}
	src := restoreSource{
		Tool:       toolID,
		OwnedFile:  of.Path,
		Basename:   filepath.Base(of.Path),
		Backup:     backup,
		Current:    curFP,
		CurrentHas: curExists,
	}
	return plan, src, nil
}

// ---------------------------------------------------------------------------
// Rendering
// ---------------------------------------------------------------------------

// renderRestoreDryRun writes the pre-apply summary to w. Byte-level
// diffs are intentionally NOT rendered here: the underlying files
// carry API keys and provider config, and a raw unified diff would
// leak plaintext secrets into stdout / a shell log. Instead we print
// the size + SHA256 fingerprint of both current and backup states so
// the operator can confirm which snapshot is about to land without
// exposing the payload.
func renderRestoreDryRun(w io.Writer, format restoreOutputFormat, toolArg string, sources []restoreSource, skipped []restoreSkipped) error {
	if format == restoreOutputJSON {
		return writeRestoreJSON(w, jsonRestoreDryRun{
			Action:  "dry-run",
			Tool:    toolArg,
			Sources: sourcesToJSON(sources),
			Skipped: skipped,
		})
	}
	fmt.Fprintf(w, "--- dry-run: restore tool %q (not written) ---\n", toolArg)
	for _, s := range sources {
		renderRestoreSourceText(w, s)
	}
	renderRestoreSkippedText(w, skipped)
	fmt.Fprintln(w, "--dry-run: nothing will be written.")
	return nil
}

// renderRestoreConfirmSummary prints the human-readable pre-apply
// summary before the y/N prompt fires. Same body as the dry-run
// renderer but without the "--dry-run" framing.
func renderRestoreConfirmSummary(w io.Writer, toolArg string, sources []restoreSource, skipped []restoreSkipped) {
	fmt.Fprintf(w, "Restore plan for tool %q:\n", toolArg)
	for _, s := range sources {
		renderRestoreSourceText(w, s)
	}
	renderRestoreSkippedText(w, skipped)
}

// renderRestoreSkippedText prints one warning line per skipped owned
// file so operators see the whole picture — a --latest against Codex
// with backups for only one of the two owned files is a legitimate
// partial restore, but silence would let the operator misread it as
// a full one.
func renderRestoreSkippedText(w io.Writer, skipped []restoreSkipped) {
	for _, s := range skipped {
		fmt.Fprintf(w, "Warning: no backups available for %s — skipped\n", s.OwnedFile)
	}
}

func renderRestoreSourceText(w io.Writer, s restoreSource) {
	fmt.Fprintf(w, "  %s\n", s.OwnedFile)
	if s.CurrentHas {
		fmt.Fprintf(w, "    current: size=%d sha256=%s\n", s.Current.Size, s.Current.SHA256)
	} else {
		fmt.Fprintln(w, "    current: (absent)")
	}
	fmt.Fprintf(w, "    backup : size=%d sha256=%s\n", s.Backup.Fingerprint.Size, s.Backup.Fingerprint.SHA256)
	fmt.Fprintf(w, "    from   : %s (taken %s)\n",
		s.Backup.BackupPath, s.Backup.Timestamp.UTC().Format("2006-01-02T15:04:05.000000000Z07:00"))
}

// renderRestoreSuccess prints the post-commit summary. Backup paths of
// the pre-restore state (FR-5) are surfaced so the operator can undo
// the restore by feeding those back to a subsequent `restore --id`.
func renderRestoreSuccess(w io.Writer, format restoreOutputFormat, toolArg string, report commit.CommitReport, sources []restoreSource, skipped []restoreSkipped) error {
	committed := 0
	newBackups := []string{}
	for _, pf := range report.PerFile {
		if pf.Status == commit.StatusCommitted {
			committed++
			if pf.Backup.BackupPath != "" {
				newBackups = append(newBackups, pf.Backup.BackupPath)
			}
		}
	}
	if format == restoreOutputJSON {
		return writeRestoreJSON(w, jsonRestoreSuccess{
			Action:     "restored",
			Tool:       toolArg,
			Committed:  committed,
			NewBackups: newBackups,
			Sources:    sourcesToJSON(sources),
			Skipped:    skipped,
		})
	}
	fmt.Fprintf(w, "Restored %d file(s) for tool %q.\n", committed, toolArg)
	if len(newBackups) > 0 {
		fmt.Fprintln(w, "Pre-restore state captured at:")
		for _, p := range newBackups {
			fmt.Fprintf(w, "  %s\n", p)
		}
	}
	// Surface untouched (skipped) files so operators see the whole
	// picture — the restore had no bytes to change for these.
	for _, pf := range report.PerFile {
		if pf.Status == commit.StatusUntouched {
			fmt.Fprintf(w, "  untouched %s (already at backup bytes)\n", pf.Target)
		}
	}
	// Warn about owned files with no backups to restore from — these
	// were silently skipped before F5.
	renderRestoreSkippedText(w, skipped)
	return nil
}

// renderRestorePartialFailure emits the partial-failure body. Text
// goes to stderr (human-facing); JSON goes to stderr too because the
// PartialFailure return will cascade to os.Exit(2), and callers
// scripting the command expect the failure signal on the same stream
// their normal error output goes to.
func renderRestorePartialFailure(w io.Writer, format restoreOutputFormat, toolArg string, pf *commit.PartialFailure) {
	if format == restoreOutputJSON {
		_ = writeRestoreJSON(w, jsonRestorePartial{
			Tool:       toolArg,
			FailedFile: pf.FailedFile,
			Cause:      pf.Cause.Error(),
			RolledBack: pf.RolledBack,
			Untouched:  pf.Untouched,
		})
		return
	}
	fmt.Fprintln(w, "restore failed partway through:")
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

type jsonRestoreList struct {
	Action  string             `json:"action"`
	Tool    string             `json:"tool"`
	Entries []restoreListEntry `json:"entries"`
}

type jsonRestoreSource struct {
	OwnedFile string `json:"owned_file"`
	Basename  string `json:"basename"`
	Current   struct {
		Exists bool   `json:"exists"`
		Size   int64  `json:"size,omitempty"`
		SHA256 string `json:"sha256,omitempty"`
	} `json:"current"`
	Backup struct {
		Path      string `json:"path"`
		Size      int64  `json:"size"`
		SHA256    string `json:"sha256"`
		Timestamp string `json:"timestamp"`
	} `json:"backup"`
}

func sourcesToJSON(sources []restoreSource) []jsonRestoreSource {
	out := make([]jsonRestoreSource, 0, len(sources))
	for _, s := range sources {
		var entry jsonRestoreSource
		entry.OwnedFile = s.OwnedFile
		entry.Basename = s.Basename
		entry.Current.Exists = s.CurrentHas
		if s.CurrentHas {
			entry.Current.Size = s.Current.Size
			entry.Current.SHA256 = s.Current.SHA256
		}
		entry.Backup.Path = s.Backup.BackupPath
		entry.Backup.Size = s.Backup.Fingerprint.Size
		entry.Backup.SHA256 = s.Backup.Fingerprint.SHA256
		entry.Backup.Timestamp = s.Backup.Timestamp.UTC().Format("2006-01-02T15:04:05.000000000Z07:00")
		out = append(out, entry)
	}
	return out
}

type jsonRestoreDryRun struct {
	Action  string              `json:"action"`
	Tool    string              `json:"tool"`
	Sources []jsonRestoreSource `json:"sources"`
	Skipped []restoreSkipped    `json:"skipped,omitempty"`
}

type jsonRestoreSuccess struct {
	Action     string              `json:"action"`
	Tool       string              `json:"tool"`
	Committed  int                 `json:"committed"`
	NewBackups []string            `json:"new_backups,omitempty"`
	Sources    []jsonRestoreSource `json:"sources"`
	Skipped    []restoreSkipped    `json:"skipped,omitempty"`
}

type jsonRestorePartial struct {
	Tool       string   `json:"tool"`
	FailedFile string   `json:"failed_file"`
	Cause      string   `json:"cause"`
	RolledBack []string `json:"rolled_back,omitempty"`
	Untouched  []string `json:"untouched,omitempty"`
}

func writeRestoreJSON(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}
