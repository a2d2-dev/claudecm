package cmd

import (
	"os"

	"github.com/a2d2-dev/claudecm/internal/config"
	"github.com/a2d2-dev/claudecm/internal/storage"
	"github.com/spf13/cobra"
)

// profileNamesCompletion provides completion for profile names.
//
// This helper deliberately does NOT call storage.Bootstrap. Tab completion
// runs on every <TAB> keypress the shell sends, including for users who have
// only typed `claudecm swit<TAB>` on a fresh machine — creating a
// `~/.claudecm/` tree as a side effect of tab completion would be a
// footgun (invisible mkdir + chmod on interactive input). Instead, we
// probe for the profiles directory and return "no completions" if it
// does not exist yet. Real commands still call Bootstrap explicitly.
//
// --home wiring: cobra's completion runtime parses the persistent flag
// set before invoking this callback, so globalHomeFlag is populated by
// the time we run. We therefore consult resolverFromGlobals directly —
// that helper honors --home when set and falls back to storage.Default
// otherwise. If flag parsing was skipped for some reason (older cobra,
// custom shell wiring) globalHomeFlag stays at its "" zero value and
// the helper still degrades to the default resolver.
func profileNamesCompletion(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
	// Don't complete if we already have an argument
	if len(args) > 0 {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}

	resolver, err := resolverFromGlobals()
	if err != nil {
		return nil, cobra.ShellCompDirectiveError
	}
	// Probe rather than create. If the layout is not bootstrapped yet, we
	// have no profiles to complete and we must not touch the filesystem.
	if _, err := os.Stat(resolver.ProfilesDir()); os.IsNotExist(err) {
		return nil, cobra.ShellCompDirectiveNoFileComp
	} else if err != nil {
		return nil, cobra.ShellCompDirectiveError
	}

	store := storage.NewFileStorage(resolver)
	validator := config.NewValidator()
	mgr := config.NewManager(store, validator)

	profiles, err := mgr.ListProfiles()
	if err != nil {
		return nil, cobra.ShellCompDirectiveError
	}

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
