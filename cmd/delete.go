package cmd

import (
	"fmt"

	"github.com/AlecAivazis/survey/v2"
	"github.com/a2d2-dev/claudecm/internal/config"
	"github.com/a2d2-dev/claudecm/internal/storage"
	"github.com/spf13/cobra"
)

var deleteCmd = &cobra.Command{
	Use:               "delete [profile-name]",
	Short:             "Delete a profile",
	Long:              `Delete a Claude Code environment profile.`,
	Args:              cobra.MaximumNArgs(1),
	ValidArgsFunction: profileNamesCompletion,
	RunE:              runDelete,
}

func init() {
	rootCmd.AddCommand(deleteCmd)
}

func runDelete(cmd *cobra.Command, args []string) error {
	// Create storage and manager
	store := storage.NewFileStorage()
	validator := config.NewValidator()
	mgr := config.NewManager(store, validator)

	// Get profile name
	var profileName string
	if len(args) > 0 {
		profileName = args[0]
	} else {
		// Interactive selection
		profiles, err := mgr.ListProfiles()
		if err != nil {
			return fmt.Errorf("failed to list profiles: %w", err)
		}

		if len(profiles) == 0 {
			return fmt.Errorf("no profiles found")
		}

		options := make([]string, len(profiles))
		for i, p := range profiles {
			options[i] = p.Name
		}

		var selectedIndex int
		prompt := &survey.Select{
			Message: "Select a profile to delete:",
			Options: options,
		}
		if err := survey.AskOne(prompt, &selectedIndex); err != nil {
			return fmt.Errorf("failed to select profile: %w", err)
		}

		profileName = profiles[selectedIndex].Name
	}

	// Confirm deletion
	var confirm bool
	confirmPrompt := &survey.Confirm{
		Message: fmt.Sprintf("Are you sure you want to delete profile %q?", profileName),
		Default: false,
	}
	if err := survey.AskOne(confirmPrompt, &confirm); err != nil {
		return fmt.Errorf("failed to confirm deletion: %w", err)
	}

	if !confirm {
		fmt.Println("Deletion cancelled")
		return nil
	}

	// Delete profile
	if err := mgr.DeleteProfile(profileName); err != nil {
		return fmt.Errorf("failed to delete profile: %w", err)
	}

	fmt.Printf("✓ Profile %q deleted successfully\n", profileName)

	return nil
}
