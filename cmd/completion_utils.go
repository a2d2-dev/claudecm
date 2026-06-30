package cmd

import (
	"github.com/imneov/claudecm/internal/config"
	"github.com/imneov/claudecm/internal/storage"
	"github.com/spf13/cobra"
)

// profileNamesCompletion provides completion for profile names
func profileNamesCompletion(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
	// Don't complete if we already have an argument
	if len(args) > 0 {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}

	// Create storage and manager
	store := storage.NewFileStorage()
	validator := config.NewValidator()
	mgr := config.NewManager(store, validator)

	// Get all profiles
	profiles, err := mgr.ListProfiles()
	if err != nil {
		return nil, cobra.ShellCompDirectiveError
	}

	// Return profile names
	names := make([]string, len(profiles))
	for i, p := range profiles {
		if p.Description != "" {
			names[i] = p.Name + "\t" + p.Description
		} else {
			names[i] = p.Name
		}
	}

	return names, cobra.ShellCompDirectiveNoFileComp
}
