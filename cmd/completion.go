package cmd

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"

	"github.com/spf13/cobra"
)

var completionCmd = &cobra.Command{
	Use:   "completion",
	Short: "Install shell completion",
	Long: `Install shell completion for claudecm.

This command will:
  1. Detect your current shell (zsh/bash/fish)
  2. Generate the completion script
  3. Install it to the appropriate location
  4. Show you what to add to your shell config

EXAMPLES
  # Quick install (auto-detect shell)
  claudecm completion

  # Install for specific shell
  claudecm completion --shell zsh

  # Output script with usage instructions
  claudecm completion --print

  # Output script for specific shell
  claudecm completion --print --shell bash

WHAT IS SHELL COMPLETION?
  Shell completion allows you to press TAB to auto-complete claudecm
  commands, subcommands, and profile names.`,
	RunE: runCompletion,
}

var (
	completionShellFlag string
	completionPrintFlag bool
)

func init() {
	completionCmd.Flags().StringVar(&completionShellFlag, "shell", "", "Target shell: zsh, bash, fish, powershell (default: auto-detect)")
	completionCmd.Flags().BoolVar(&completionPrintFlag, "print", false, "Output completion script with usage instructions")
	rootCmd.AddCommand(completionCmd)
}

func runCompletion(cmd *cobra.Command, args []string) error {
	// Determine shell type
	shellName := completionShellFlag
	if shellName == "" {
		// Auto-detect from $SHELL
		shell := os.Getenv("SHELL")
		if shell == "" {
			return fmt.Errorf("unable to detect shell from $SHELL environment variable\n\n💡 Specify manually: claudecm completion --shell zsh")
		}
		shellName = filepath.Base(shell)
	}

	// Validate shell type
	validShells := map[string]bool{
		"bash":       true,
		"zsh":        true,
		"fish":       true,
		"powershell": true,
	}
	if !validShells[shellName] {
		return fmt.Errorf("unsupported shell: %s\n\n💡 Supported shells: zsh, bash, fish, powershell", shellName)
	}

	// Handle print mode
	if completionPrintFlag {
		return printCompletionScript(shellName)
	}

	// Default: install mode
	return installCompletion(shellName)
}

func installCompletion(shellName string) error {
	fmt.Printf("🔍 Target shell: %s\n\n", shellName)

	switch shellName {
	case "zsh":
		return installZshCompletion()
	case "bash":
		return installBashCompletion()
	case "fish":
		return installFishCompletion()
	case "powershell":
		return installPowerShellCompletion()
	default:
		return fmt.Errorf("unsupported shell: %s", shellName)
	}
}

func printCompletionScript(shellName string) error {
	// Print header with instructions
	fmt.Printf("# ============================================\n")
	fmt.Printf("# claudecm completion script for %s\n", shellName)
	fmt.Printf("# ============================================\n")
	fmt.Println("#")

	switch shellName {
	case "zsh":
		fmt.Println("# INSTALLATION:")
		fmt.Println("#   1. Save this script:")
		fmt.Println("#      claudecm completion --print --shell zsh > ~/.zsh/completions/_claudecm")
		fmt.Println("#")
		fmt.Println("#   2. Add to your ~/.zshrc:")
		fmt.Println("#      fpath=(~/.zsh/completions $fpath)")
		fmt.Println("#      autoload -U compinit; compinit")
		fmt.Println("#")
		fmt.Println("#   3. Reload shell:")
		fmt.Println("#      source ~/.zshrc")
		fmt.Println("#")
		fmt.Printf("# ============================================\n\n")
		return rootCmd.GenZshCompletion(os.Stdout)

	case "bash":
		fmt.Println("# INSTALLATION:")
		if runtime.GOOS == "darwin" {
			fmt.Println("#   1. Save this script:")
			fmt.Println("#      claudecm completion --print --shell bash > $(brew --prefix)/etc/bash_completion.d/claudecm")
		} else {
			fmt.Println("#   1. Save this script:")
			fmt.Println("#      claudecm completion --print --shell bash > ~/.bash_completion.d/claudecm")
		}
		fmt.Println("#")
		fmt.Println("#   2. Reload shell:")
		fmt.Println("#      source ~/.bashrc")
		fmt.Println("#")
		fmt.Printf("# ============================================\n\n")
		return rootCmd.GenBashCompletion(os.Stdout)

	case "fish":
		fmt.Println("# INSTALLATION:")
		fmt.Println("#   1. Save this script:")
		fmt.Println("#      claudecm completion --print --shell fish > ~/.config/fish/completions/claudecm.fish")
		fmt.Println("#")
		fmt.Println("#   2. Completion will work in new fish sessions")
		fmt.Println("#")
		fmt.Printf("# ============================================\n\n")
		return rootCmd.GenFishCompletion(os.Stdout, true)

	case "powershell":
		fmt.Println("# INSTALLATION:")
		fmt.Println("#   1. Save this script:")
		fmt.Println("#      claudecm completion --print --shell powershell > claudecm.ps1")
		fmt.Println("#")
		fmt.Println("#   2. Add to your PowerShell profile:")
		fmt.Println("#      . /path/to/claudecm.ps1")
		fmt.Println("#")
		fmt.Printf("# ============================================\n\n")
		return rootCmd.GenPowerShellCompletionWithDesc(os.Stdout)

	default:
		return fmt.Errorf("unsupported shell: %s", shellName)
	}
}

func installZshCompletion() error {
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("failed to get home directory: %w", err)
	}

	completionDir := filepath.Join(homeDir, ".zsh", "completions")
	fmt.Println("📦 Installing completion...")

	if err := os.MkdirAll(completionDir, 0755); err != nil {
		return fmt.Errorf("failed to create completion directory: %w", err)
	}
	fmt.Printf("   ✓ Created directory: %s\n", completionDir)

	completionFile := filepath.Join(completionDir, "_claudecm")
	f, err := os.Create(completionFile)
	if err != nil {
		return fmt.Errorf("failed to create completion file: %w", err)
	}
	defer f.Close()

	if err := rootCmd.GenZshCompletion(f); err != nil {
		return fmt.Errorf("failed to generate completion: %w", err)
	}
	fmt.Println("   ✓ Generated completion script")
	fmt.Printf("   ✓ Installed to: %s\n", completionFile)

	fmt.Println()
	fmt.Println("📝 Next steps:")
	fmt.Println("   Add these lines to your ~/.zshrc (if not already present):")
	fmt.Println()
	fmt.Println("   # Enable zsh completion")
	fmt.Println("   autoload -U compinit; compinit")
	fmt.Printf("   fpath=(%s $fpath)\n", completionDir)
	fmt.Println()
	fmt.Println("   Then reload: source ~/.zshrc")
	fmt.Println()
	fmt.Println("✅ Done! You'll have tab completion for claudecm commands.")

	return nil
}

func installBashCompletion() error {
	var completionDir string

	fmt.Println("📦 Installing completion...")

	if runtime.GOOS == "darwin" {
		// macOS - try Homebrew first
		brewPrefix, err := exec.Command("brew", "--prefix").Output()
		if err == nil {
			completionDir = filepath.Join(string(brewPrefix[:len(brewPrefix)-1]), "etc", "bash_completion.d")
		} else {
			homeDir, _ := os.UserHomeDir()
			completionDir = filepath.Join(homeDir, ".bash_completion.d")
		}
	} else {
		// Linux
		if _, err := os.Stat("/etc/bash_completion.d"); err == nil {
			completionDir = "/etc/bash_completion.d"
		} else {
			homeDir, _ := os.UserHomeDir()
			completionDir = filepath.Join(homeDir, ".bash_completion.d")
		}
	}

	if err := os.MkdirAll(completionDir, 0755); err != nil {
		return fmt.Errorf("failed to create completion directory: %w", err)
	}
	fmt.Printf("   ✓ Created directory: %s\n", completionDir)

	completionFile := filepath.Join(completionDir, "claudecm")
	f, err := os.Create(completionFile)
	if err != nil {
		return fmt.Errorf("failed to create completion file: %w", err)
	}
	defer f.Close()

	if err := rootCmd.GenBashCompletion(f); err != nil {
		return fmt.Errorf("failed to generate completion: %w", err)
	}
	fmt.Println("   ✓ Generated completion script")
	fmt.Printf("   ✓ Installed to: %s\n", completionFile)

	fmt.Println()
	fmt.Println("📝 Next steps:")
	fmt.Println("   Reload your shell: source ~/.bashrc")
	fmt.Println()
	fmt.Println("✅ Done! You'll have tab completion for claudecm commands.")

	return nil
}

func installFishCompletion() error {
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("failed to get home directory: %w", err)
	}

	fmt.Println("📦 Installing completion...")

	completionDir := filepath.Join(homeDir, ".config", "fish", "completions")
	if err := os.MkdirAll(completionDir, 0755); err != nil {
		return fmt.Errorf("failed to create completion directory: %w", err)
	}
	fmt.Printf("   ✓ Created directory: %s\n", completionDir)

	completionFile := filepath.Join(completionDir, "claudecm.fish")
	f, err := os.Create(completionFile)
	if err != nil {
		return fmt.Errorf("failed to create completion file: %w", err)
	}
	defer f.Close()

	if err := rootCmd.GenFishCompletion(f, true); err != nil {
		return fmt.Errorf("failed to generate completion: %w", err)
	}
	fmt.Println("   ✓ Generated completion script")
	fmt.Printf("   ✓ Installed to: %s\n", completionFile)

	fmt.Println()
	fmt.Println("✅ Done! Completion will be available in new fish sessions.")

	return nil
}

func installPowerShellCompletion() error {
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("failed to get home directory: %w", err)
	}

	fmt.Println("📦 Installing completion...")

	completionFile := filepath.Join(homeDir, "claudecm-completion.ps1")
	f, err := os.Create(completionFile)
	if err != nil {
		return fmt.Errorf("failed to create completion file: %w", err)
	}
	defer f.Close()

	if err := rootCmd.GenPowerShellCompletionWithDesc(f); err != nil {
		return fmt.Errorf("failed to generate completion: %w", err)
	}
	fmt.Println("   ✓ Generated completion script")
	fmt.Printf("   ✓ Saved to: %s\n", completionFile)

	fmt.Println()
	fmt.Println("📝 Next steps:")
	fmt.Println("   Add this line to your PowerShell profile:")
	fmt.Println()
	fmt.Printf("   . %s\n", completionFile)
	fmt.Println()
	fmt.Println("   Find your profile location with: $PROFILE")
	fmt.Println()
	fmt.Println("✅ Done! Restart PowerShell for completion to work.")

	return nil
}
