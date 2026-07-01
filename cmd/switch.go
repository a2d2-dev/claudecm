package cmd

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/AlecAivazis/survey/v2"
	"github.com/a2d2-dev/claudecm/internal/config"
	"github.com/a2d2-dev/claudecm/internal/export"
	"github.com/a2d2-dev/claudecm/internal/storage"
	"github.com/spf13/cobra"
)

var switchCmd = &cobra.Command{
	Use:   "switch [profile-name]",
	Short: "Switch active profile",
	Long: `Switch to a different Claude Code environment profile.

MODES
  Default mode (lightweight):
    claudecm switch prod
    eval $(claudecm export)

  Shell mode (isolated environment):
    claudecm switch prod --shell
    # New shell starts with profile loaded
    # Type 'exit' to return

  Activation mode (like Python venv):
    source <(claudecm switch prod --init)
    # Current shell modified with prompt indicator
    # Use 'deactivate' to reset

EXAMPLES
  # Quick switch in scripts
  claudecm switch prod
  eval $(claudecm export)

  # Temporary testing in isolated shell
  claudecm switch test --shell

  # Work session with prompt indicator
  source <(claudecm switch prod --init)
  # (claudecm:prod) $ ...
  # (claudecm:prod) $ deactivate`,
	Args:              cobra.MaximumNArgs(1),
	ValidArgsFunction: profileNamesCompletion,
	RunE:              runSwitch,
}

var (
	switchShellFlag bool
	switchInitFlag  bool
)

func init() {
	switchCmd.Flags().BoolVarP(&switchShellFlag, "shell", "s", false, "Start a new shell with profile loaded")
	switchCmd.Flags().BoolVarP(&switchInitFlag, "init", "i", false, "Output activation script for sourcing")
	rootCmd.AddCommand(switchCmd)
}

func runSwitch(cmd *cobra.Command, args []string) error {
	// Create storage and manager
	resolver, err := storage.Default()
	if err != nil {
		return fmt.Errorf("failed to resolve HOME: %w", err)
	}
	store := storage.NewFileStorage(resolver)
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

	// Get active profile name for display
	activeName, _ := mgr.GetActiveName()

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
			// Mark current active profile
			if p.Name == activeName {
				desc += " [CURRENT]"
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

	// Get the profile for shell/init modes
	profile, err := mgr.GetProfile(profileName)
	if err != nil {
		return fmt.Errorf("failed to get profile: %w", err)
	}

	// Handle different modes
	if switchInitFlag {
		return outputInitScript(profile, profileName)
	}

	if switchShellFlag {
		return startShellWithProfile(profile, profileName)
	}

	// Default mode: just switch and show instructions
	fmt.Printf("✓ Switched to profile %q\n", profileName)
	fmt.Println("\n🔄 Load environment variables:")
	fmt.Println("   eval $(claudecm export)")
	fmt.Println("\n💡 TIP: Use --shell flag to start a new shell, or --init for activation mode")

	return nil
}

// startShellWithProfile starts a new shell with the profile environment loaded
func startShellWithProfile(profile *config.Profile, name string) error {
	shellPath := os.Getenv("SHELL")
	if shellPath == "" {
		shellPath = "/bin/bash"
	}

	// Prepare environment variables
	env := os.Environ()

	// Add profile environment variables
	profileEnv := export.ToMap(profile)
	for key, value := range profileEnv {
		env = append(env, fmt.Sprintf("%s=%s", key, value))
	}

	// Modify prompt to show profile name
	shellName := filepath.Base(shellPath)
	switch shellName {
	case "zsh":
		env = append(env, fmt.Sprintf("PS1=(claudecm:%s) %%n@%%m %%~ %%%% ", name))
	case "bash":
		env = append(env, fmt.Sprintf("PS1=(claudecm:%s) \\u@\\h:\\w\\$ ", name))
	case "fish":
		// Fish uses a different mechanism for prompt
		env = append(env, fmt.Sprintf("CLAUDECM_PROFILE=%s", name))
	}

	// Start shell
	fmt.Printf("✓ Switched to profile %q\n", name)
	fmt.Printf("🚀 Starting new shell with environment loaded...\n")
	fmt.Println("   Type 'exit' to return to the previous shell")
	fmt.Println()

	shellCmd := exec.Command(shellPath)
	shellCmd.Env = env
	shellCmd.Stdin = os.Stdin
	shellCmd.Stdout = os.Stdout
	shellCmd.Stderr = os.Stderr

	if err := shellCmd.Run(); err != nil {
		return fmt.Errorf("failed to start shell: %w", err)
	}

	fmt.Println("\n✓ Exited profile shell")
	return nil
}

// outputInitScript outputs a script that can be sourced to activate the profile
func outputInitScript(profile *config.Profile, name string) error {
	shellPath := os.Getenv("SHELL")
	shellName := "sh"
	if shellPath != "" {
		shellName = filepath.Base(shellPath)
	}

	// Generate environment variable exports
	profileEnv := export.ToMap(profile)

	fmt.Println("# claudecm profile activation script")
	fmt.Printf("# Profile: %s\n", name)
	fmt.Println()

	// Export environment variables
	for key, value := range profileEnv {
		fmt.Printf("export %s=%q\n", key, value)
	}

	// Export profile name for tracking
	fmt.Printf("export CLAUDECM_ACTIVE_PROFILE=%q\n", name)
	fmt.Println()

	// Modify prompt based on shell type
	switch shellName {
	case "zsh":
		fmt.Println("# Modify prompt to show active profile")
		fmt.Println("if [ -z \"$CLAUDECM_OLD_PS1\" ]; then")
		fmt.Println("    export CLAUDECM_OLD_PS1=\"$PS1\"")
		fmt.Println("fi")
		fmt.Printf("export PS1=\"(claudecm:%s) $CLAUDECM_OLD_PS1\"\n", name)
		fmt.Println()
	case "bash":
		fmt.Println("# Modify prompt to show active profile")
		fmt.Println("if [ -z \"$CLAUDECM_OLD_PS1\" ]; then")
		fmt.Println("    export CLAUDECM_OLD_PS1=\"$PS1\"")
		fmt.Println("fi")
		fmt.Printf("export PS1=\"(claudecm:%s) $CLAUDECM_OLD_PS1\"\n", name)
		fmt.Println()
	}

	// Define deactivate function
	fmt.Println("# Deactivation function")
	fmt.Println("deactivate() {")
	fmt.Println("    # Unset environment variables")
	for key := range profileEnv {
		fmt.Printf("    unset %s\n", key)
	}
	fmt.Println("    unset CLAUDECM_ACTIVE_PROFILE")
	fmt.Println()
	fmt.Println("    # Restore original prompt")
	fmt.Println("    if [ -n \"$CLAUDECM_OLD_PS1\" ]; then")
	fmt.Println("        export PS1=\"$CLAUDECM_OLD_PS1\"")
	fmt.Println("        unset CLAUDECM_OLD_PS1")
	fmt.Println("    fi")
	fmt.Println()
	fmt.Println("    # Remove deactivate function")
	fmt.Println("    unset -f deactivate")
	fmt.Println("}")

	return nil
}
