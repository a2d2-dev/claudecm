package cmd

import (
	"fmt"

	"github.com/AlecAivazis/survey/v2"
	"github.com/imneov/claudecm/internal/config"
	"github.com/imneov/claudecm/internal/storage"
	"github.com/spf13/cobra"
)

var switchCmd = &cobra.Command{
	Use:   "switch [profile-name]",
	Short: "Switch active profile",
	Long:  `Switch to a different Claude Code environment profile.`,
	Args:  cobra.MaximumNArgs(1),
	RunE:  runSwitch,
}

func init() {
	rootCmd.AddCommand(switchCmd)
}

func runSwitch(cmd *cobra.Command, args []string) error {
	// Create storage and manager
	store := storage.NewFileStorage()
	validator := config.NewValidator()
	mgr := config.NewManager(store, validator)

	// Get all profiles
	profiles, err := mgr.ListProfiles()
	if err != nil {
		return fmt.Errorf("failed to list profiles: %w", err)
	}

	if len(profiles) == 0 {
		return fmt.Errorf("no profiles found, use 'claudecm add' to create one")
	}

	// Get profile name
	var profileName string
	if len(args) > 0 {
		profileName = args[0]
	} else {
		// Interactive selection
		options := make([]string, len(profiles))
		for i, p := range profiles {
			desc := p.Name
			if p.Description != "" {
				desc = fmt.Sprintf("%s - %s", p.Name, p.Description)
			}
			options[i] = desc
		}

		var selectedIndex int
		prompt := &survey.Select{
			Message: "Select a profile:",
			Options: options,
		}
		if err := survey.AskOne(prompt, &selectedIndex); err != nil {
			return fmt.Errorf("failed to select profile: %w", err)
		}

		profileName = profiles[selectedIndex].Name
	}

	// Set active profile
	if err := mgr.SetActive(profileName); err != nil {
		return fmt.Errorf("failed to switch profile: %w", err)
	}

	fmt.Printf("✓ Switched to profile %q\n", profileName)
	fmt.Println("\nTo load the environment variables, run:")
	fmt.Println("  eval $(claudecm export)")

	return nil
}
