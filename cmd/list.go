package cmd

import (
	"fmt"
	"os"
	"text/tabwriter"

	"github.com/imneov/claudecm/internal/config"
	"github.com/imneov/claudecm/internal/storage"
	"github.com/spf13/cobra"
)

var listCmd = &cobra.Command{
	Use:   "list",
	Short: "List all profiles",
	Long:  `List all available Claude Code environment profiles.`,
	RunE:  runList,
}

func init() {
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

	// Create tabwriter for formatted output
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 3, ' ', 0)
	fmt.Fprintln(w, "NAME\tBASE URL\tMODEL\tDESCRIPTION\tACTIVE")
	fmt.Fprintln(w, "----\t--------\t-----\t-----------\t------")

	for _, profile := range profiles {
		active := ""
		if profile.Name == activeName {
			active = "✓"
		}

		model := profile.Model
		if model == "" {
			model = "-"
		}

		description := profile.Description
		if description == "" {
			description = "-"
		}
		// Truncate long descriptions
		if len(description) > 40 {
			description = description[:37] + "..."
		}

		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n",
			profile.Name,
			profile.BaseURL,
			model,
			description,
			active,
		)
	}

	return w.Flush()
}
