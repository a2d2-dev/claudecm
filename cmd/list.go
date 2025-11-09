package cmd

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/imneov/claudecm/internal/config"
	"github.com/imneov/claudecm/internal/storage"
	"github.com/spf13/cobra"
)

var listCmd = &cobra.Command{
	Use:   "list",
	Short: "List all profiles",
	Long: `List all available Claude Code environment profiles.

MODES
  Default (compact):
    claudecm list

  Detailed view:
    claudecm list --details

  JSON output:
    claudecm list --json

EXAMPLES
  # Quick overview
  claudecm list

  # See full configuration
  claudecm list --details

  # Script integration
  claudecm list --json | jq '.[] | select(.name=="prod")'`,
	RunE: runList,
}

var (
	listDetailsFlag bool
	listJSONFlag    bool
)

func init() {
	listCmd.Flags().BoolVarP(&listDetailsFlag, "details", "d", false, "Show detailed information")
	listCmd.Flags().BoolVar(&listJSONFlag, "json", false, "Output as JSON")
	rootCmd.AddCommand(listCmd)
}

func runList(cmd *cobra.Command, args []string) error {
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
		fmt.Println("No profiles found. Use 'claudecm add' to create one.")
		return nil
	}

	// Get active profile name
	activeName, _ := mgr.GetActiveName()

	// JSON output mode
	if listJSONFlag {
		return outputJSON(profiles, activeName)
	}

	// Detailed output mode
	if listDetailsFlag {
		return outputDetailed(profiles, activeName)
	}

	// Default compact mode
	return outputCompact(profiles, activeName)
}

// outputCompact shows a compact list view
func outputCompact(profiles []*config.Profile, activeName string) error {
	fmt.Printf("\nPROFILES (%d total)\n\n", len(profiles))

	for _, profile := range profiles {
		// Name with active indicator
		namePrefix := "  "
		if profile.Name == activeName {
			namePrefix = "✓ "
		}

		// Extract hostname from URL for compact display
		baseURL := profile.BaseURL
		baseURL = strings.TrimPrefix(baseURL, "https://")
		baseURL = strings.TrimPrefix(baseURL, "http://")

		// Model
		model := profile.Model
		if model == "" {
			model = "-"
		}

		fmt.Printf("%s%-20s  %-30s  %s\n", namePrefix, profile.Name, baseURL, model)
	}

	fmt.Println()
	fmt.Println("QUICK ACTIONS")
	fmt.Println("  claudecm switch <name>     Switch profile")
	fmt.Println("  eval $(claudecm export)    Load environment variables")
	fmt.Println()

	return nil
}

// outputDetailed shows detailed card-style view
func outputDetailed(profiles []*config.Profile, activeName string) error {
	fmt.Printf("\nPROFILES (%d total)\n\n", len(profiles))

	for i, profile := range profiles {
		if i > 0 {
			fmt.Println()
		}

		// Header with active indicator
		header := fmt.Sprintf("─ %s ", profile.Name)
		if profile.Name == activeName {
			header = fmt.Sprintf("─ ✓ %s ", profile.Name)
		}

		// Top border
		fmt.Print("┌")
		fmt.Print(header)
		fmt.Print(strings.Repeat("─", max(0, 60-len(header))))
		fmt.Println("┐")

		// Content
		fmt.Printf("│ %-11s %-46s │\n", "Base URL", profile.BaseURL)

		model := profile.Model
		if model == "" {
			model = "(not set)"
		}
		fmt.Printf("│ %-11s %-46s │\n", "Model", model)

		// Small fast model from custom env
		if smallModel, ok := profile.CustomEnv["ANTHROPIC_SMALL_FAST_MODEL"]; ok && smallModel != "" {
			fmt.Printf("│ %-11s %-46s │\n", "Small Model", smallModel)
		}

		// Auth token (masked)
		authToken := maskToken(profile.AuthToken)
		fmt.Printf("│ %-11s %-46s │\n", "Auth Token", authToken)

		// Description
		if profile.Description != "" {
			fmt.Printf("│ %-11s %-46s │\n", "Description", profile.Description)
		}

		// Bottom border
		fmt.Println("└" + strings.Repeat("─", 60) + "┘")
	}

	fmt.Println()
	return nil
}

// outputJSON outputs profiles in JSON format
func outputJSON(profiles []*config.Profile, activeName string) error {
	type profileJSON struct {
		Name        string            `json:"name"`
		BaseURL     string            `json:"base_url"`
		Model       string            `json:"model,omitempty"`
		Description string            `json:"description,omitempty"`
		CustomEnv   map[string]string `json:"custom_env,omitempty"`
		Active      bool              `json:"active"`
	}

	result := make([]profileJSON, len(profiles))
	for i, p := range profiles {
		result[i] = profileJSON{
			Name:        p.Name,
			BaseURL:     p.BaseURL,
			Model:       p.Model,
			Description: p.Description,
			CustomEnv:   p.CustomEnv,
			Active:      p.Name == activeName,
		}
	}

	encoder := json.NewEncoder(os.Stdout)
	encoder.SetIndent("", "  ")
	return encoder.Encode(result)
}

// max returns the maximum of two integers
func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}
