package cmd

import (
	"fmt"

	"github.com/AlecAivazis/survey/v2"
	"github.com/imneov/claudecm/internal/config"
	"github.com/imneov/claudecm/internal/storage"
	"github.com/spf13/cobra"
)

var addCmd = &cobra.Command{
	Use:   "add [name]",
	Short: "Add a new profile",
	Long:  `Add a new Claude Code environment profile with interactive prompts.`,
	Args:  cobra.MaximumNArgs(1),
	RunE:  runAdd,
}

func init() {
	rootCmd.AddCommand(addCmd)
}

func runAdd(cmd *cobra.Command, args []string) error {
	// Create storage and manager
	store := storage.NewFileStorage()
	validator := config.NewValidator()
	mgr := config.NewManager(store, validator)

	// Get profile name
	var profileName string
	if len(args) > 0 {
		profileName = args[0]
	} else {
		prompt := &survey.Input{
			Message: "Profile name:",
			Help:    "A unique name for this profile (e.g., anthropic-us, moonshot-dev)",
		}
		if err := survey.AskOne(prompt, &profileName, survey.WithValidator(survey.Required)); err != nil {
			return fmt.Errorf("failed to get profile name: %w", err)
		}
	}

	// Check if profile already exists
	exists, err := mgr.ProfileExists(profileName)
	if err != nil {
		return fmt.Errorf("failed to check profile existence: %w", err)
	}
	if exists {
		return fmt.Errorf("profile %q already exists", profileName)
	}

	// Get base URL
	var baseURL string
	urlPrompt := &survey.Input{
		Message: "API Base URL:",
		Default: "https://api.anthropic.com",
		Help:    "The API endpoint URL",
	}
	if err := survey.AskOne(urlPrompt, &baseURL, survey.WithValidator(survey.Required)); err != nil {
		return fmt.Errorf("failed to get base URL: %w", err)
	}

	// Get auth token
	var authToken string
	tokenPrompt := &survey.Password{
		Message: "Authentication Token:",
		Help:    "Your API key or token",
	}
	if err := survey.AskOne(tokenPrompt, &authToken, survey.WithValidator(survey.Required)); err != nil {
		return fmt.Errorf("failed to get auth token: %w", err)
	}

	// Get model (optional)
	var model string
	modelPrompt := &survey.Input{
		Message: "Default Model (optional):",
		Default: "claude-sonnet-4",
		Help:    "The default model to use with this profile",
	}
	if err := survey.AskOne(modelPrompt, &model); err != nil {
		return fmt.Errorf("failed to get model: %w", err)
	}

	// Get description (optional)
	var description string
	descPrompt := &survey.Input{
		Message: "Description (optional):",
		Help:    "A human-readable description of this profile",
	}
	if err := survey.AskOne(descPrompt, &description); err != nil {
		return fmt.Errorf("failed to get description: %w", err)
	}

	// Create profile
	profile := config.NewProfile(profileName, baseURL, authToken)
	profile.Model = model
	profile.Description = description

	// Add profile
	if err := mgr.AddProfile(profile); err != nil {
		return fmt.Errorf("failed to add profile: %w", err)
	}

	fmt.Printf("✓ Profile %q created successfully\n", profileName)

	// Ask if user wants to activate this profile
	var activate bool
	activatePrompt := &survey.Confirm{
		Message: "Set this as the active profile?",
		Default: true,
	}
	if err := survey.AskOne(activatePrompt, &activate); err != nil {
		return nil // Don't fail if this step fails
	}

	if activate {
		if err := mgr.SetActive(profileName); err != nil {
			return fmt.Errorf("failed to set active profile: %w", err)
		}
		fmt.Printf("✓ Profile %q is now active\n", profileName)
	}

	return nil
}
