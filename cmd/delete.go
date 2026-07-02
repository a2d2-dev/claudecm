// Package cmd — delete command (Story E6-S6).
//
// cmd/delete removes a profile:
//
//	claudecm delete <name> [--yes] [--dry-run]
//
// Semantics:
//  1. Load the profile — refuse if it does not exist.
//  2. --dry-run: print the planned action and exit 0.
//  3. Interactive path: prompt "Delete profile <name>? [y/N]" unless
//     --yes is passed (or stdin is non-interactive with --yes absent,
//     in which case we refuse rather than silently deleting).
//  4. Delete the profile file.
//  5. FR-2: if state.CurrentProfile == name, clear it — the pointer
//     MUST NOT dangle at a missing profile.
//
// delete NEVER touches tool-config files. The last-applied bytes remain
// on disk exactly as they were; a subsequent `switch <other>` re-renders
// them through the two-phase commit pipeline.
//
// Exit codes: 0 on success (including --dry-run); 1 on any error.
package cmd

import (
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/a2d2-dev/claudecm/internal/storage"
)

var (
	deleteYesFlag    bool
	deleteDryRunFlag bool
)

var deleteCmd = &cobra.Command{
	Use:   "delete <name>",
	Short: "Delete a profile",
	Long: `Delete a profile.

Prompts for confirmation on a TTY unless --yes is passed. If the
deleted profile is the active profile, state.CurrentProfile is cleared
(FR-2 — the active pointer never points at a missing profile).

delete NEVER touches tool-config files. The last-applied tool bytes
remain on disk unchanged; a subsequent 'claudecm switch <other>'
re-renders them.`,
	Args:              cobra.ExactArgs(1),
	ValidArgsFunction: profileNamesCompletion,
	RunE:              runDelete,
}

func init() {
	deleteCmd.Flags().BoolVar(&deleteYesFlag, "yes", false, "Skip interactive confirmation")
	deleteCmd.Flags().BoolVar(&deleteDryRunFlag, "dry-run", false, "Print the planned action and exit without deleting")
	rootCmd.AddCommand(deleteCmd)
}

// runDelete is the testable entry point. Returns nil on success
// (including --dry-run), a non-nil error otherwise.
func runDelete(cmd *cobra.Command, args []string) error {
	name := strings.TrimSpace(args[0])
	if name == "" {
		return fmt.Errorf("profile name cannot be empty")
	}

	resv, err := storage.Default()
	if err != nil {
		return fmt.Errorf("failed to resolve HOME: %w", err)
	}
	if err := storage.Bootstrap(resv); err != nil {
		return fmt.Errorf("failed to bootstrap ~/.claudecm layout: %w", err)
	}
	store := storage.NewFileStorage(resv)

	// Existence check via LoadProfile so a parse error on a corrupt
	// file surfaces before we prompt for confirmation. ProfileExists
	// would swallow that signal.
	if _, err := store.LoadProfile(name); err != nil {
		return fmt.Errorf("profile %q not found: %w", name, err)
	}

	if deleteDryRunFlag {
		fmt.Fprintf(cmd.OutOrStdout(), "would delete profile %q\n", name)
		state, sErr := store.LoadState()
		if sErr == nil && state.CurrentProfile == name {
			fmt.Fprintf(cmd.OutOrStdout(), "  (this is the active profile; the pointer would be cleared)\n")
		}
		return nil
	}

	if !deleteYesFlag {
		if !isTerminal(os.Stdin) {
			return fmt.Errorf("non-interactive session: pass --yes to confirm the delete or --dry-run to preview")
		}
		ok, err := promptConfirm(cmd.OutOrStdout(), os.Stdin, fmt.Sprintf("Delete profile %q?", name))
		if err != nil {
			return fmt.Errorf("read confirmation: %w", err)
		}
		if !ok {
			fmt.Fprintln(cmd.OutOrStdout(), "aborted; profile not deleted.")
			return fmt.Errorf("aborted by user")
		}
	}

	if err := store.DeleteProfile(name); err != nil {
		return fmt.Errorf("failed to delete profile %q: %w", name, err)
	}

	// FR-2 pointer clear. Do this AFTER the file is gone so the
	// pointer is never observed dangling in state.yaml.
	cleared, err := maybeClearActivePointer(store, name)
	if err != nil {
		return err
	}

	fmt.Fprintf(cmd.OutOrStdout(), "Deleted %q.\n", name)
	if cleared {
		fmt.Fprintln(cmd.OutOrStdout(), "  (active profile pointer cleared)")
	}
	return nil
}

// maybeClearActivePointer clears state.CurrentProfile if it equals
// name. Returns (true, nil) when the pointer was cleared, (false, nil)
// otherwise. Any I/O error surfaces as-is.
func maybeClearActivePointer(store *storage.FileStorage, name string) (bool, error) {
	state, err := store.LoadState()
	if err != nil {
		return false, fmt.Errorf("load state: %w", err)
	}
	if state.CurrentProfile != name {
		return false, nil
	}
	state.CurrentProfile = ""
	if err := store.SaveState(state); err != nil {
		return false, fmt.Errorf("save state after delete: %w", err)
	}
	return true, nil
}
