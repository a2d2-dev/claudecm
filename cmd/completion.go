// Package cmd — completion command (Story E6-S9).
//
// `claudecm completion <bash|zsh|fish|powershell>` emits the shell
// completion script for the requested shell on stdout, using cobra's
// built-in generators. The intended use is:
//
//	# bash
//	source <(claudecm completion bash)
//
//	# zsh
//	source <(claudecm completion zsh)
//	# ...or persist under $fpath:
//	claudecm completion zsh > "${fpath[1]}/_claudecm"
//
//	# fish
//	claudecm completion fish | source
//
//	# PowerShell
//	claudecm completion powershell | Out-String | Invoke-Expression
//
// The command carries no side effects — it never writes to disk on the
// operator's behalf, so tab completion configured incorrectly is a
// human-visible failure (an unsourced script) rather than an invisible
// mkdir into a system directory. This is the E6-S9 story shape;
// prior installs-into-fs behaviour is out of scope.
//
// Profile-name tab completion is wired via ValidArgsFunction on
// individual subcommands (see cmd/switch.go, cmd/explain.go, and this
// file's init(), which registers the same completer on cmd/export,
// cmd/delete, cmd/edit, cmd/rename, cmd/restore). The completer lives
// in cmd/completion_utils.go and reuses storage.Default + FileStorage
// to enumerate ~/.claudecm/profiles/.
package cmd

import (
	"fmt"

	"github.com/spf13/cobra"
)

// completionCmd emits a shell completion script. The positional arg
// selects the shell.
var completionCmd = &cobra.Command{
	Use:   "completion [bash|zsh|fish|powershell]",
	Short: "Emit a shell completion script for claudecm",
	Long: `Generate a shell completion script for claudecm.

The script is written to stdout. Redirect or source it as your shell
requires. No files are written on your behalf.

USAGE
  # bash (one-shot for this session)
  source <(claudecm completion bash)

  # zsh (persist under $fpath)
  claudecm completion zsh > "${fpath[1]}/_claudecm"

  # fish
  claudecm completion fish | source

  # PowerShell
  claudecm completion powershell | Out-String | Invoke-Expression`,
	Args:                  cobra.ExactArgs(1),
	ValidArgs:             []string{"bash", "zsh", "fish", "powershell"},
	DisableFlagsInUseLine: true,
	RunE:                  runCompletion,
}

func init() {
	rootCmd.AddCommand(completionCmd)
	// Wire profile-name tab completion at Execute() time via
	// cobra.OnInitialize. init() order within the package is source-file
	// order, so any subcommand whose file sorts after completion.go
	// (delete, edit, rename, restore, export, switch) has not yet
	// registered on rootCmd when this init runs. OnInitialize fires
	// after all inits complete, so every subcommand is present by
	// then. The bare check inside RegisterProfileNameCompletion is
	// defensive: if a name is not found the function no-ops instead of
	// panicking, which keeps a rename of a subcommand from making the
	// binary refuse to start.
	cobra.OnInitialize(RegisterProfileNameCompletion)
}

// RegisterProfileNameCompletion wires profileNamesCompletion onto every
// subcommand whose first positional argument is a profile name so users
// get tab completion consistently across the surface.
//
// switch and explain already register the completer themselves (they
// pre-date E6-S9); we also cover export, delete, edit, rename, and
// restore. Cobra allows re-registration by later assignment, so
// stamping the completer here after AddCommand runs is safe even if a
// command file already set it.
//
// Exported so tests can invoke it deterministically without waiting
// for cobra's Execute() to fire OnInitialize.
func RegisterProfileNameCompletion() {
	// Look each subcommand up by Use prefix so re-labelling a command
	// (e.g. adding flags to Use) does not silently break wiring.
	for _, name := range []string{"switch", "delete", "edit", "rename", "explain", "restore", "export"} {
		if sub := findSubcommandByName(name); sub != nil {
			sub.ValidArgsFunction = profileNamesCompletion
		}
	}
}

// findSubcommandByName is a small helper: cobra's Commands() slice is
// ordered by AddCommand, and each Command's Name() is the first token
// of its Use field. Returning the first hit is fine because we do not
// re-register any subcommand name.
func findSubcommandByName(name string) *cobra.Command {
	for _, c := range rootCmd.Commands() {
		if c.Name() == name {
			return c
		}
	}
	return nil
}

// runCompletion emits the requested shell's script on stdout via the
// matching cobra generator. Unknown shells short-circuit with a
// deterministic error message; cobra's ValidArgs already prevents this
// path in practice, but the switch default is defensive.
func runCompletion(cmd *cobra.Command, args []string) error {
	shell := args[0]
	out := cmd.OutOrStdout()
	switch shell {
	case "bash":
		return rootCmd.GenBashCompletion(out)
	case "zsh":
		return rootCmd.GenZshCompletion(out)
	case "fish":
		return rootCmd.GenFishCompletion(out, true)
	case "powershell":
		return rootCmd.GenPowerShellCompletionWithDesc(out)
	default:
		return fmt.Errorf("unsupported shell %q (want bash|zsh|fish|powershell)", shell)
	}
}
