// Package cmd — version command (Story E6-S9).
//
// `claudecm version` prints the semantic version + git commit + build
// date the binary was linked with. Values are sourced from pkg/version
// so a `go build -ldflags "-X ..."` in the release script pins them
// without touching cobra plumbing.
//
// Two output modes:
//
//	--output text (default): three-line human-readable form.
//	--output json          : {"version": "...", "commit": "...", "build_date": "..."}
//
// The command is intentionally minimal — it does NOT bootstrap the
// ~/.claudecm layout, does NOT construct a Resolver, and takes zero
// positional args. That keeps `claudecm version` fast and usable inside
// a container image before any state exists on disk.
package cmd

import (
	"fmt"
	"io"

	"github.com/spf13/cobra"

	"github.com/a2d2-dev/claudecm/pkg/version"
)

// versionOutputFormat mirrors the enum shape used by every other cmd/*.
type versionOutputFormat string

const (
	versionOutputText versionOutputFormat = "text"
	versionOutputJSON versionOutputFormat = "json"
)

// versionOutputFlag stores --output. Package-level per the same
// exception documented in cmd/root.go: written only at flag-parse time,
// read from thereafter.
var versionOutputFlag string

var versionCmd = &cobra.Command{
	Use:   "version",
	Short: "Print the claudecm version, commit, and build date",
	Long: `Print the version, commit hash, and build date this binary
was linked with. Populated by -ldflags at release time; unlabeled
builds display "dev" / "none" / "unknown".

EXAMPLES
  # Human-readable form
  claudecm version

  # Machine-readable JSON
  claudecm version --output json`,
	Args: cobra.NoArgs,
	RunE: runVersion,
}

func init() {
	versionCmd.Flags().StringVarP(&versionOutputFlag, "output", "o", "text", "Output format (text|json)")
	rootCmd.AddCommand(versionCmd)
}

// runVersion is the testable entry point.
func runVersion(cmd *cobra.Command, args []string) error {
	format, err := parseVersionOutput(versionOutputFlag)
	if err != nil {
		return err
	}
	switch format {
	case versionOutputJSON:
		return renderVersionJSON(cmd.OutOrStdout())
	default:
		return renderVersionText(cmd.OutOrStdout())
	}
}

// parseVersionOutput validates --output.
func parseVersionOutput(raw string) (versionOutputFormat, error) {
	switch trimAndLower(raw) {
	case "", "text":
		return versionOutputText, nil
	case "json":
		return versionOutputJSON, nil
	default:
		return "", fmt.Errorf("invalid --output %q (want text|json)", raw)
	}
}

// renderVersionText prints the three-line human form.
func renderVersionText(w io.Writer) error {
	if _, err := fmt.Fprintf(w, "claudecm %s\n", version.Version); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "commit: %s\n", version.Commit); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "built: %s\n", version.Date); err != nil {
		return err
	}
	return nil
}

// versionJSON is the wire form for --output json.
type versionJSON struct {
	Version   string `json:"version"`
	Commit    string `json:"commit"`
	BuildDate string `json:"build_date"`
}

// renderVersionJSON emits the JSON body.
func renderVersionJSON(w io.Writer) error {
	return encodeIndentedJSON(w, versionJSON{
		Version:   version.Version,
		Commit:    version.Commit,
		BuildDate: version.Date,
	})
}
