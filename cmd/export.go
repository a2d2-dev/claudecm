package cmd

import (
	"fmt"

	"github.com/imneov/claudecm/internal/config"
	"github.com/imneov/claudecm/internal/export"
	"github.com/imneov/claudecm/internal/storage"
	"github.com/spf13/cobra"
)

var exportCmd = &cobra.Command{
	Use:   "export",
	Short: "Export environment variables",
	Long:  `Export the active profile's environment variables as shell export statements.`,
	RunE:  runExport,
}

func init() {
	rootCmd.AddCommand(exportCmd)
}

func runExport(cmd *cobra.Command, args []string) error {
	// Create storage and manager
	store := storage.NewFileStorage()
	validator := config.NewValidator()
	mgr := config.NewManager(store, validator)

	// Get active profile
	profile, err := mgr.GetActive()
	if err != nil {
		return fmt.Errorf("failed to get active profile: %w", err)
	}

	// Export as shell format
	output := export.ToShell(profile)
	fmt.Println(output)

	return nil
}
