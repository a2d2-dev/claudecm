package cmd

import (
	"fmt"

	"github.com/AlecAivazis/survey/v2"
	"github.com/a2d2-dev/claudecm/internal/config"
	"github.com/a2d2-dev/claudecm/internal/envextract"
	"github.com/a2d2-dev/claudecm/internal/storage"
	"github.com/spf13/cobra"
)

var (
	addAutoConfirm bool
	addProfileName string
)

var addCmd = &cobra.Command{
	Use:   "add",
	Short: "Add a new profile",
	Long:  `Add a new Claude Code environment profile from current environment variables.`,
	RunE:  runAdd,
}

func init() {
	rootCmd.AddCommand(addCmd)
	addCmd.Flags().BoolVarP(&addAutoConfirm, "yes", "y", false, "Auto-confirm and use extracted environment variables without prompting")
	addCmd.Flags().StringVarP(&addProfileName, "name", "n", "", "Profile name (required with --yes flag)")
}

func runAdd(cmd *cobra.Command, args []string) error {
	// Create storage and manager
	store := storage.NewFileStorage()
	validator := config.NewValidator()
	mgr := config.NewManager(store, validator)

	// Step 1: Extract current environment variables
	fmt.Println("Extracting current Claude environment variables...")
	fmt.Println()

	extracted := envextract.ExtractCurrentEnv()

	// Display extracted values
	fmt.Println("Current environment variables:")
	fmt.Printf("  ANTHROPIC_BASE_URL:         %s\n", extracted.BaseURL)
	fmt.Printf("  ANTHROPIC_AUTH_TOKEN:       %s\n", maskToken(extracted.AuthToken))
	fmt.Printf("  ANTHROPIC_MODEL:            %s\n", valueOrEmpty(extracted.Model))
	fmt.Printf("  ANTHROPIC_SMALL_FAST_MODEL: %s\n", valueOrEmpty(extracted.SmallFastModel))
	fmt.Println()

	if !extracted.HasAuthToken() {
		if addAutoConfirm {
			return fmt.Errorf("ANTHROPIC_AUTH_TOKEN is not set in current environment")
		}
		fmt.Println("Warning: ANTHROPIC_AUTH_TOKEN is not set in current environment")
	}

	// Auto-confirm mode: use extracted values directly
	if addAutoConfirm {
		if addProfileName == "" {
			return fmt.Errorf("profile name is required when using --yes flag (use --name/-n)")
		}

		// Check if profile already exists
		exists, err := mgr.ProfileExists(addProfileName)
		if err != nil {
			return fmt.Errorf("failed to check profile existence: %w", err)
		}
		if exists {
			return fmt.Errorf("profile %q already exists", addProfileName)
		}

		// Create profile from extracted values
		profile := extracted.ToProfile(addProfileName)

		// Add profile
		if err := mgr.AddProfile(profile); err != nil {
			return fmt.Errorf("failed to add profile: %w", err)
		}

		fmt.Printf("\n✓ Profile %q created successfully\n", addProfileName)

		// Auto-activate the profile
		if err := mgr.SetActive(addProfileName); err != nil {
			return fmt.Errorf("failed to set active profile: %w", err)
		}
		fmt.Printf("✓ Profile %q is now active\n", addProfileName)

		// Show profile list
		fmt.Println()
		return runList(cmd, nil)
	}

	// Step 2: Allow user to edit values (interactive mode)
	var baseURL, authToken, model, smallFastModel string

	questions := []*survey.Question{
		{
			Name: "baseURL",
			Prompt: &survey.Input{
				Message: "API Base URL:",
				Default: extracted.BaseURL,
			},
			Validate: survey.Required,
		},
		{
			Name: "authToken",
			Prompt: &survey.Input{
				Message: "Authentication Token:",
				Default: extracted.AuthToken,
				Help:    "Press Enter to use extracted token, or input a new one",
			},
			Validate: survey.Required,
		},
		{
			Name: "model",
			Prompt: &survey.Input{
				Message: "Default Model (optional):",
				Default: extracted.Model,
			},
		},
		{
			Name: "smallFastModel",
			Prompt: &survey.Input{
				Message: "Small Fast Model (optional):",
				Default: extracted.SmallFastModel,
			},
		},
	}

	answers := struct {
		BaseURL        string
		AuthToken      string
		Model          string
		SmallFastModel string
	}{}

	if err := survey.Ask(questions, &answers); err != nil {
		return fmt.Errorf("failed to get profile details: %w", err)
	}

	baseURL = answers.BaseURL
	authToken = answers.AuthToken
	model = answers.Model
	smallFastModel = answers.SmallFastModel

	// Step 3: Prompt for profile name
	var profileName string
	namePrompt := &survey.Input{
		Message: "Profile name:",
		Help:    "A unique name for this profile (e.g., anthropic-us, moonshot-dev)",
	}
	if err := survey.AskOne(namePrompt, &profileName, survey.WithValidator(survey.Required)); err != nil {
		return fmt.Errorf("failed to get profile name: %w", err)
	}

	// Check if profile already exists
	exists, err := mgr.ProfileExists(profileName)
	if err != nil {
		return fmt.Errorf("failed to check profile existence: %w", err)
	}
	if exists {
		return fmt.Errorf("profile %q already exists", profileName)
	}

	// Create v1 profile
	profile := config.NewProfile(profileName, baseURL, authToken)
	profile.Core.Model = model
	profile.Core.SmallFastModel = smallFastModel

	// Add profile
	if err := mgr.AddProfile(profile); err != nil {
		return fmt.Errorf("failed to add profile: %w", err)
	}

	fmt.Printf("\n✓ Profile %q created successfully\n", profileName)

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

	// Step 4: Show profile list
	fmt.Println()
	return runList(cmd, nil)
}

// maskToken masks the authentication token for display
func maskToken(token string) string {
	if token == "" {
		return "(not set)"
	}
	if len(token) <= 8 {
		return "****"
	}
	return token[:4] + "..." + token[len(token)-4:]
}

// valueOrEmpty returns the value or "(empty)" if not set
func valueOrEmpty(value string) string {
	if value == "" {
		return "(empty)"
	}
	return value
}
