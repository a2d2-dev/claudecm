// Package cmd — rename command (Story E6-S6).
//
// cmd/rename renames a profile:
//
//	claudecm rename <old> <new> [--overwrite] [--dry-run]
//
// Semantics:
//  1. Validate the new name via storage.ValidateProfileName so
//     NFR-S5's regex + reserved-name checks fire at exactly one gate.
//  2. Load the old profile — refuse if it does not exist.
//  3. Refuse if the target name already exists unless --overwrite.
//  4. --dry-run: print the planned action and exit 0.
//  5. Otherwise save the profile at the new name (with Name field
//     updated and UpdatedAt bumped) and delete the old file.
//  6. If state.CurrentProfile == old, update it to new — FR-2 pointer
//     cannot dangle.
//
// rename NEVER touches tool-config files.
//
// Exit codes: 0 on success (including --dry-run); 1 on any error.
package cmd

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/a2d2-dev/claudecm/internal/storage"
)

var (
	renameOverwriteFlag bool
	renameDryRunFlag    bool
)

var renameCmd = &cobra.Command{
	Use:   "rename <old> <new>",
	Short: "Rename a profile",
	Long: `Rename a profile.

The new name is validated against the profile-name regex (NFR-S5).
Refuses when the target name already exists unless --overwrite is
passed. If the renamed profile was the active profile, state.yaml is
updated so the pointer stays valid (FR-2).

rename NEVER touches tool-config files. Re-render happens on the next
'claudecm switch'.`,
	Args: cobra.ExactArgs(2),
	// Only complete the first positional (the source name). The target
	// name is a new label the user is inventing — completion would be
	// misleading.
	ValidArgsFunction: func(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
		if len(args) == 0 {
			return profileNamesCompletion(cmd, args, toComplete)
		}
		return nil, cobra.ShellCompDirectiveNoFileComp
	},
	RunE: runRename,
}

func init() {
	renameCmd.Flags().BoolVar(&renameOverwriteFlag, "overwrite", false, "Allow replacing an existing profile with the target name")
	renameCmd.Flags().BoolVar(&renameDryRunFlag, "dry-run", false, "Print the planned action and exit without writing")
	rootCmd.AddCommand(renameCmd)
}

// runRename is the testable entry point. Returns nil on success
// (including --dry-run), a non-nil error otherwise.
func runRename(cmd *cobra.Command, args []string) error {
	oldName := strings.TrimSpace(args[0])
	newName := strings.TrimSpace(args[1])
	if oldName == "" {
		return fmt.Errorf("old profile name cannot be empty")
	}
	if err := storage.ValidateProfileName(newName); err != nil {
		return err
	}
	if oldName == newName {
		return fmt.Errorf("old and new names are identical (%q); nothing to do", oldName)
	}

	resv, err := resolverFromGlobals()
	if err != nil {
		return fmt.Errorf("failed to resolve HOME: %w", err)
	}
	if err := storage.Bootstrap(resv); err != nil {
		return fmt.Errorf("failed to bootstrap ~/.claudecm layout: %w", err)
	}
	store := storage.NewFileStorage(resv)

	profile, err := store.LoadProfile(oldName)
	if err != nil {
		return fmt.Errorf("profile %q not found: %w", oldName, err)
	}

	newExists, err := store.ProfileExists(newName)
	if err != nil {
		return fmt.Errorf("failed to check whether profile %q exists: %w", newName, err)
	}
	if newExists && !renameOverwriteFlag {
		return fmt.Errorf("profile %q already exists; pass --overwrite to replace it", newName)
	}

	if renameDryRunFlag {
		fmt.Fprintf(cmd.OutOrStdout(), "would rename %q -> %q\n", oldName, newName)
		if newExists {
			fmt.Fprintf(cmd.OutOrStdout(), "  (target exists; --overwrite would replace it)\n")
		}
		return nil
	}

	// Save the profile at the new name FIRST, then delete the old file.
	// Ordering matters: if SaveProfile fails we still have the old
	// file, and the operator can retry without data loss. If we deleted
	// first, a save failure would leave the operator profile-less. The
	// tradeoff is that a mid-flight process kill between the two calls
	// leaves two files on disk — but both are readable, and the
	// operator can decide manually which to keep.
	profile.Name = newName
	profile.UpdatedAt = nowFn().UTC()
	if err := store.SaveProfile(profile); err != nil {
		return fmt.Errorf("failed to write new profile %q: %w", newName, err)
	}
	if err := store.DeleteProfile(oldName); err != nil {
		return fmt.Errorf("failed to remove old profile file %q: %w", oldName, err)
	}

	// FR-2: if the active pointer was pointing at the old name, move
	// it. Do this AFTER the file rename succeeded so a pointer that
	// gets updated never briefly points at a name that has not yet
	// been persisted under the new label.
	if err := maybeUpdateStateAfterRename(store, oldName, newName); err != nil {
		return err
	}

	fmt.Fprintf(cmd.OutOrStdout(), "Renamed %q -> %q.\n", oldName, newName)
	return nil
}

// maybeUpdateStateAfterRename flips state.CurrentProfile from oldName
// to newName if it was pointing at oldName. Returns nil for the "not
// active" branch — no state I/O has to fire when the pointer already
// pointed elsewhere.
func maybeUpdateStateAfterRename(store *storage.FileStorage, oldName, newName string) error {
	state, err := store.LoadState()
	if err != nil {
		return fmt.Errorf("load state: %w", err)
	}
	if state.CurrentProfile != oldName {
		return nil
	}
	state.SetCurrentProfile(newName)
	if err := store.SaveState(state); err != nil {
		return fmt.Errorf("save state after rename: %w", err)
	}
	return nil
}
