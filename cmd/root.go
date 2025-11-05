package cmd

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
)

var (
	// Version information (will be set by build process)
	Version = "dev"
	Commit  = "none"
	Date    = "unknown"
)

// rootCmd represents the base command
var rootCmd = &cobra.Command{
	Use:   "claudecm",
	Short: "Claude Code Environment Manager",
	Long: `claudecm is a CLI tool for managing Claude Code environment configurations.

Easily manage multiple API configurations and switch between them seamlessly.`,
	Version: Version,
}

// Execute runs the root command
func Execute() {
	if err := rootCmd.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func init() {
	// Global flags can be added here
	rootCmd.SetVersionTemplate(fmt.Sprintf("claudecm version %s (commit: %s, built: %s)\n", Version, Commit, Date))
}
