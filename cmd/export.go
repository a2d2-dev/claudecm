package cmd

import (
	"fmt"

	"github.com/a2d2-dev/claudecm/internal/config"
	"github.com/a2d2-dev/claudecm/internal/export"
	"github.com/a2d2-dev/claudecm/internal/storage"
	"github.com/spf13/cobra"
)

var exportCmd = &cobra.Command{
	Use:   "export",
	Short: "Export environment variables",
	Long: `Export the active profile's environment variables as shell export statements.

WHY "eval"?
  Shell commands cannot modify their parent process's environment variables.
  The 'eval' command executes the output in your current shell, making the
  variables available in your session.

USAGE
  eval $(claudecm export)

SHORTCUTS
  Add to your shell config for convenience:

  # Option 1: Alias for quick loading
  alias cmload='eval $(claudecm export)'

  # Option 2: Auto-load active profile on shell start
  if command -v claudecm &> /dev/null; then
    eval $(claudecm export 2>/dev/null)
  fi

  # Option 3: Combined switch and load function
  cmswitch() {
    claudecm switch "$@" && eval $(claudecm export)
  }

EXAMPLES
  # Load active profile
  eval $(claudecm export)

  # Switch and load in one command
  claudecm switch prod && eval $(claudecm export)

  # Using alias (after setup)
  cmload

SEE ALSO
  claudecm switch --shell    Start new shell with profile loaded
  claudecm switch --init     Activation mode (like Python venv)`,
	RunE: runExport,
}

func init() {
	rootCmd.AddCommand(exportCmd)
}

func runExport(cmd *cobra.Command, args []string) error {
	// Create storage and manager
	resolver, err := storage.Default()
	if err != nil {
		return fmt.Errorf("failed to resolve HOME: %w", err)
	}
	store := storage.NewFileStorage(resolver)
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
