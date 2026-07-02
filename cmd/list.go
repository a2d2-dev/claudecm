// Package cmd — list command (Story E6-S10).
//
// `claudecm list` enumerates every profile under
// ~/.claudecm/profiles/ and prints a compact row for each: name,
// provider, model, and an active marker against the profile pointed at
// by state.yaml. Read-only over profiles and state; the only write
// this command performs is the ~/.claudecm layout Bootstrap that every
// cmd/* entry point calls up front.
//
// Design notes:
//
//   - Refuses on malformed profile (NFR-S1). The pre-E6-S10 MVP used
//     FileStorage.LoadAllProfiles, which "loud-skips" broken files
//     (writes a stderr warning and continues). That masks corruption
//     from JSON pipelines that only inspect stdout, so this command
//     enumerates the directory itself and calls config.ParseProfile
//     on each file — the first parse error aborts the whole listing
//     with the offending file's path, no partial results emitted.
//
//   - Default redaction (NFR-S8). Every api_key value is redacted via
//     redactValue (shared with cmd/explain / cmd/current) so a text
//     table or JSON output never leaks a plaintext token. --reveal —
//     either the global root persistent flag or the local flag on this
//     command — flips redaction off and triggers the stderr warning.
//
//   - --output json emits `[]` (empty array) when no profiles exist so
//     shell consumers never have to distinguish stdout-empty from
//     stdout-`[]`. Text mode prints "no profiles" for the same case,
//     matching the story AC.
package cmd

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/a2d2-dev/claudecm/internal/config"
	"github.com/a2d2-dev/claudecm/internal/storage"
)

// listOutputFormat mirrors the enum shape used across cmd/*.
type listOutputFormat string

const (
	listOutputText listOutputFormat = "text"
	listOutputJSON listOutputFormat = "json"
)

// listNoProfilesText is the single-line body the text renderer emits
// when the profiles dir is empty. Named so tests grep against a
// constant, not a stringy literal.
const listNoProfilesText = "no profiles"

// listActiveMarker is the leading marker for the active row in text
// output. Kept as a constant so a future style change is a one-line
// edit and every test assertion agrees on the exact byte.
const listActiveMarker = "*"

var (
	listOutputFlag string
	// listRevealFlag is a test-only seam surviving the E6-S9 promotion
	// of --reveal to a persistent flag on rootCmd. Registering a local
	// --reveal here would panic under pflag's "flag redefined" check
	// because rootCmd already binds the same name; the var is kept as
	// a per-command override tests can pin, then OR'd into the effective
	// reveal via globalRevealActive.
	listRevealFlag bool
)

var listCmd = &cobra.Command{
	Use:   "list",
	Short: "List every profile with the active one marked",
	Long: `List every profile stored in ~/.claudecm/profiles/.

Each row shows the profile name, provider, model, and (in text mode) an
active marker on the row referenced by state.yaml. api_key values are
redacted by default; pass --reveal (or the global --reveal at root) to
render them plaintext — a stderr warning is emitted whenever redaction
is bypassed.

Any profile file that fails schema-version parsing (missing schema_version,
newer schema, or malformed YAML) refuses the whole listing with a
non-zero exit code and names the offending file. No partial results are
emitted (NFR-S1).

EXAMPLES
  # Text table (default)
  claudecm list

  # JSON array for shell pipelines
  claudecm list --output json

  # Reveal secrets (with warning)
  claudecm list --reveal`,
	Args: cobra.NoArgs,
	RunE: runList,
}

func init() {
	listCmd.Flags().StringVarP(&listOutputFlag, "output", "o", "text", "Output format (text|json)")
	// --reveal is inherited from rootCmd (persistent flag). Do not
	// register a local one here — pflag would panic on the duplicate
	// name. See package doc / cmd/root.go for the promotion rationale.
	rootCmd.AddCommand(listCmd)
}

// runList is the testable entry point.
func runList(cmd *cobra.Command, args []string) error {
	format, err := parseListOutput(listOutputFlag)
	if err != nil {
		return err
	}
	effectiveReveal := globalRevealActive(listRevealFlag)
	emitRevealNoticeIfNeeded(cmd.ErrOrStderr(), effectiveReveal)

	resv, err := resolverFromGlobals()
	if err != nil {
		return fmt.Errorf("failed to resolve HOME: %w", err)
	}
	if err := storage.Bootstrap(resv); err != nil {
		return fmt.Errorf("failed to bootstrap ~/.claudecm layout: %w", err)
	}

	profiles, err := loadAllProfilesStrict(resv)
	if err != nil {
		return err
	}

	activeName, err := readActiveName(resv)
	if err != nil {
		return fmt.Errorf("failed to read active profile: %w", err)
	}

	switch format {
	case listOutputJSON:
		return renderListJSON(cmd.OutOrStdout(), profiles, activeName, effectiveReveal)
	default:
		return renderListText(cmd.OutOrStdout(), profiles, activeName, effectiveReveal)
	}
}

// parseListOutput validates --output.
func parseListOutput(raw string) (listOutputFormat, error) {
	switch trimAndLower(raw) {
	case "", "text":
		return listOutputText, nil
	case "json":
		return listOutputJSON, nil
	default:
		return "", fmt.Errorf("invalid --output %q (want text|json)", raw)
	}
}

// loadAllProfilesStrict enumerates ~/.claudecm/profiles/, parses every
// *.yaml through config.ParseProfile, and returns the profile slice in
// name-sorted order. The first parse error aborts with the offending
// path — no partial results (NFR-S1). Missing directory is treated as
// empty (Bootstrap already ran at the caller, so a missing dir is a
// filesystem race, not a user-visible error).
func loadAllProfilesStrict(resv *storage.Resolver) ([]*config.Profile, error) {
	dir := resv.ProfilesDir()
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read profiles dir %q: %w", dir, err)
	}

	profiles := make([]*config.Profile, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), storage.ProfileFileExt) {
			continue
		}
		full := filepath.Join(dir, entry.Name())
		body, err := os.ReadFile(full)
		if err != nil {
			return nil, fmt.Errorf("read profile file %q: %w", full, err)
		}
		p, err := config.ParseProfile(body)
		if err != nil {
			// NFR-S1: refuse on malformed profile. Include the file
			// path so an operator can grep straight to the offending
			// file without having to guess which one broke.
			return nil, fmt.Errorf("profile %q failed to parse: %w", full, err)
		}
		// The parsed profile's Name field may or may not agree with the
		// filename (older shapes stored the name only in the body).
		// Prefer the filename-derived name when the parse produced an
		// empty Name — the file is the canonical identifier.
		if p.Name == "" {
			p.Name = strings.TrimSuffix(entry.Name(), storage.ProfileFileExt)
		}
		profiles = append(profiles, p)
	}
	sort.Slice(profiles, func(i, j int) bool { return profiles[i].Name < profiles[j].Name })
	return profiles, nil
}

// readActiveName pulls state.yaml's CurrentProfile without going through
// config.Manager (Manager treats "no state yet" as an error). Missing
// state is a legitimate "fresh install" case: the list command must
// still succeed against a bootstrapped-but-empty tree, so a
// not-found error is normalized to ("", nil) here. Every other error
// (permission denied, malformed YAML, etc.) still propagates so a
// real problem does not hide behind the fresh-install path.
func readActiveName(resv *storage.Resolver) (string, error) {
	store := storage.NewFileStorage(resv)
	state, err := store.LoadState()
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) || os.IsNotExist(err) {
			return "", nil
		}
		return "", err
	}
	return state.CurrentProfile, nil
}

// listRow is the display-ready row shape used by both renderers. Values
// are already redaction-applied so the renderers can concentrate on
// output formatting.
type listRow struct {
	Name     string
	Provider string
	Model    string
	APIKey   string
	Active   bool
}

// buildListRows applies redaction and active-marker computation once so
// both renderers walk the same input.
func buildListRows(profiles []*config.Profile, active string, reveal bool) []listRow {
	rows := make([]listRow, 0, len(profiles))
	for _, p := range profiles {
		rows = append(rows, listRow{
			Name:     p.Name,
			Provider: p.Core.Provider,
			Model:    p.Core.Model,
			APIKey:   apiKeyForListDisplay(p.Core.APIKey, reveal),
			Active:   p.Name == active,
		})
	}
	return rows
}

// apiKeyForListDisplay honors --reveal: return the raw key when reveal
// is on, otherwise the shared redactValue rule (first4***last4 for
// len>=8, "***" for shorter).
func apiKeyForListDisplay(key string, reveal bool) string {
	if reveal {
		return key
	}
	if key == "" {
		return ""
	}
	return redactValue(key)
}

// renderListText writes the tab-aligned table. Empty input produces the
// single "no profiles" line the AC calls for.
func renderListText(w io.Writer, profiles []*config.Profile, active string, reveal bool) error {
	if len(profiles) == 0 {
		_, err := fmt.Fprintln(w, listNoProfilesText)
		return err
	}
	rows := buildListRows(profiles, active, reveal)

	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	if _, err := fmt.Fprintln(tw, "  NAME\tPROVIDER\tMODEL\tAPI_KEY"); err != nil {
		return err
	}
	for _, r := range rows {
		marker := " "
		if r.Active {
			marker = listActiveMarker
		}
		if _, err := fmt.Fprintf(tw, "%s %s\t%s\t%s\t%s\n",
			marker,
			orListDash(r.Name),
			orListDash(r.Provider),
			orListDash(r.Model),
			orListDash(r.APIKey),
		); err != nil {
			return err
		}
	}
	return tw.Flush()
}

// orListDash renders "-" for an empty string so columns never collapse.
func orListDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// listJSON is the wire shape for --output json. Empty profiles list
// emits `[]` (empty JSON array), matching the story AC.
type listJSON struct {
	Name     string `json:"name"`
	Provider string `json:"provider"`
	Model    string `json:"model"`
	Active   bool   `json:"active"`
	APIKey   string `json:"api_key"`
}

func renderListJSON(w io.Writer, profiles []*config.Profile, active string, reveal bool) error {
	rows := buildListRows(profiles, active, reveal)
	out := make([]listJSON, 0, len(rows))
	for _, r := range rows {
		out = append(out, listJSON{
			Name:     r.Name,
			Provider: r.Provider,
			Model:    r.Model,
			Active:   r.Active,
			APIKey:   r.APIKey,
		})
	}
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(out)
}
