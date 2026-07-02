// Package cmd — edit command (Story E6-S5).
//
// cmd/edit updates an existing profile in place. Two modes:
//
//  1. Interactive editor mode (default): the current profile is
//     marshalled to YAML into a temp file under ~/.claudecm/tmp/,
//     $EDITOR (fallback vi) is launched on it, and on save the bytes
//     are re-parsed via config.ParseProfile. Refuses on parse error,
//     schema_version drift, or Name field change.
//
//  2. --set key=value mode (repeatable, non-interactive): applies each
//     key=value using the same parser as cmd/add for the tools.*
//     overlay paths, plus core.<field> and description shortcuts.
//
// --dry-run prints a unified diff of the original vs. edited YAML and
// exits 0 without writing.
//
// cmd/edit NEVER touches tool-config files. Applying a value that would
// affect tool-config rendering requires the operator to run `switch`
// afterwards. This mirrors the story AC and keeps edit auditable in
// isolation from the two-phase commit path.
//
// Exit codes: 0 on success (including --dry-run); 1 on any error.
package cmd

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/pmezard/go-difflib/difflib"
	"github.com/spf13/cobra"

	"github.com/a2d2-dev/claudecm/internal/config"
	"github.com/a2d2-dev/claudecm/internal/storage"
)

// Supported --set path prefixes for edit. Mirrors cmd/add's overlay
// prefixes and adds the core.* / description shortcuts that only make
// sense on an existing profile (cmd/add uses dedicated flags for these).
// The tools.* prefixes are reused from cmd/add (setPrefixClaudeCodeEnv,
// setPrefixCodexRaw) so both surfaces agree on the overlay-path grammar.
const (
	editSetPrefixCoreDot  = "core."
	editSetKeyDescription = "description"
)

// Supported core.* subkeys the operator can flip via --set core.<field>=<value>.
// Kept in sync with config.CoreConfig; adding a new core field means adding an
// entry here.
var editCoreFieldSetters = map[string]func(*config.CoreConfig, string){
	"base_url":         func(c *config.CoreConfig, v string) { c.BaseURL = v },
	"api_key":          func(c *config.CoreConfig, v string) { c.APIKey = v },
	"model":            func(c *config.CoreConfig, v string) { c.Model = v },
	"small_fast_model": func(c *config.CoreConfig, v string) { c.SmallFastModel = v },
	"provider":         func(c *config.CoreConfig, v string) { c.Provider = v },
}

var (
	editSetFlag    []string
	editDryRunFlag bool
)

// editorEnvVar is the environment variable consulted to locate the
// user's editor when running the interactive edit path. Falls back to
// "vi" per the story AC when unset. Kept as a named constant so tests
// referring to the same seam do not typo it.
const editorEnvVar = "EDITOR"

// editorFallback is the editor used when $EDITOR is not set. POSIX
// mandates vi to be present.
const editorFallback = "vi"

// editorRunnerFn is the swap-able editor launcher. Production callers
// invoke $EDITOR via os/exec; tests override this seam to no-op or to
// synthesise specific corrupt bytes on the temp file. Documented
// exception to coding-standards rule 12 mirroring switch.isTerminalFn:
// written only at init or test-swap time, read thereafter.
var editorRunnerFn = defaultEditorRunner

// defaultEditorRunner shells out to $EDITOR (or vi) with the temp file
// path as the sole positional argument. Inherits stdin/stdout/stderr
// so an interactive editor session is transparent. The caller is
// responsible for creating and cleaning up the temp file.
func defaultEditorRunner(tmpPath string) error {
	editor := os.Getenv(editorEnvVar)
	if strings.TrimSpace(editor) == "" {
		editor = editorFallback
	}
	// Fields split lets EDITOR carry flags (e.g. "code --wait").
	parts := strings.Fields(editor)
	if len(parts) == 0 {
		return fmt.Errorf("editor is empty after trimming")
	}
	cmd := exec.Command(parts[0], append(parts[1:], tmpPath)...) //nolint:gosec // parts derived from user-controlled $EDITOR by design
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// SetEditorRunnerForTest overrides the editor launcher. Returns a
// restore closure the caller MUST defer to put the production runner
// back. Test-only helper — production code has no legitimate reason to
// swap the runner.
func SetEditorRunnerForTest(fn func(string) error) func() {
	prev := editorRunnerFn
	editorRunnerFn = fn
	return func() { editorRunnerFn = prev }
}

var editCmd = &cobra.Command{
	Use:   "edit [profile-name]",
	Short: "Edit an existing profile via $EDITOR or --set key=value",
	Long: `Edit an existing claudecm profile.

Two modes:

  * Interactive editor (default): writes the current profile YAML to a
    temp file under ~/.claudecm/tmp/, launches $EDITOR (fallback vi),
    and re-parses on save. Refuses on malformed YAML, schema_version
    drift, or a Name field change.

  * --set key=value (repeatable): applies each entry non-interactively.
    Supported paths:
      description=<text>
      core.base_url=<value>
      core.api_key=<value>
      core.model=<value>
      core.small_fast_model=<value>
      core.provider=<value>
      tools.claude_code.env.<VAR>=<value>
      tools.codex.raw.<flat.dotted.key>=<value>

--dry-run prints a unified diff of the profile YAML and exits without
writing.

edit NEVER touches tool-config files. Run 'claudecm switch <name>' to
apply any core / overlay change that affects the tool-owned files.`,
	Args:              cobra.ExactArgs(1),
	ValidArgsFunction: profileNamesCompletion,
	RunE:              runEdit,
}

func init() {
	editCmd.Flags().StringArrayVar(&editSetFlag, "set", nil,
		"Non-interactive edit entry (repeatable). Format: <path>=<value>. "+
			"See --help for supported paths.")
	editCmd.Flags().BoolVar(&editDryRunFlag, "dry-run", false,
		"Print the unified diff of the edited profile YAML and exit without writing")
	rootCmd.AddCommand(editCmd)
}

// runEdit is the testable entry point. Returns nil on success
// (including --dry-run), a non-nil error otherwise.
func runEdit(cmd *cobra.Command, args []string) error {
	name := strings.TrimSpace(args[0])
	if name == "" {
		return fmt.Errorf("profile name cannot be empty")
	}

	resv, err := storage.Default()
	if err != nil {
		return fmt.Errorf("failed to resolve HOME: %w", err)
	}
	if err := storage.Bootstrap(resv); err != nil {
		return fmt.Errorf("failed to bootstrap ~/.claudecm layout: %w", err)
	}
	store := storage.NewFileStorage(resv)

	original, err := store.LoadProfile(name)
	if err != nil {
		return fmt.Errorf("profile %q not found: %w", name, err)
	}

	originalYAML, err := config.MarshalProfile(original)
	if err != nil {
		return fmt.Errorf("marshal original profile: %w", err)
	}

	var edited *config.Profile
	// tmpPath is the path to the interactive editor's scratch file when
	// the editor mode ran. Empty for --set mode. The F3 review finding
	// requires that on ParseProfile failure or schema drift refusal the
	// file be preserved (not silently deleted) so the operator can retry
	// manually without losing their edits.
	var tmpPath string
	if len(editSetFlag) > 0 {
		edited, err = applyEditSetEntries(original, editSetFlag)
		if err != nil {
			return err
		}
	} else {
		var eErr error
		edited, tmpPath, eErr = editViaEditor(resv, name, originalYAML)
		if eErr != nil {
			// Parse rejection returns a non-empty tmpPath — preserve so
			// the operator can fix the YAML and re-run edit against it
			// (or manually copy the salvageable bits out). Editor-runner
			// failure / read failure return tmpPath="" and are cleaned
			// up inside editViaEditor already.
			if tmpPath != "" {
				fmt.Fprintf(cmd.ErrOrStderr(),
					"Your edits are preserved at %s; re-run edit with the corrected content or manually copy.\n",
					tmpPath)
			}
			return eErr
		}
	}

	// From here on, the tmp file (if any) will be removed on function
	// exit unless a later refusal flips preserveTmp. Keeping cleanup at
	// the caller means the schema-drift / name-change branches below can
	// preserve deterministically instead of racing os.Remove.
	preserveTmp := false
	defer func() {
		if tmpPath == "" || preserveTmp {
			return
		}
		_ = os.Remove(tmpPath)
	}()

	// Schema drift refusal: never let the operator downgrade / upgrade
	// the on-disk schema through an interactive edit or a scripted
	// --set. cmd/edit is not the migration surface — that lives in
	// config.ParseProfile.
	if edited.SchemaVersion != config.CurrentProfileSchemaVersion {
		if tmpPath != "" {
			preserveTmp = true
			fmt.Fprintf(cmd.ErrOrStderr(),
				"Your edits are preserved at %s; re-run edit with the corrected content or manually copy.\n",
				tmpPath)
		}
		return fmt.Errorf("refusing edit: schema_version changed to %d (expected %d)",
			edited.SchemaVersion, config.CurrentProfileSchemaVersion)
	}

	// Name change refusal: renaming is what cmd/rename is for. Refusing
	// here keeps the temp-file round-trip symmetric with the on-disk
	// profile identity — you cannot accidentally create a second profile
	// under a different name by editing an existing one.
	if edited.Name != original.Name {
		return fmt.Errorf("refusing edit: profile name changed from %q to %q (use `claudecm rename` instead)",
			original.Name, edited.Name)
	}

	// Bump UpdatedAt to the current clock read on any successful edit
	// so `current` / `list` reflect that the profile was touched.
	edited.UpdatedAt = nowFn().UTC()

	if editDryRunFlag {
		return renderEditDryRun(cmd.OutOrStdout(), name, original, edited)
	}

	if err := store.SaveProfile(edited); err != nil {
		return fmt.Errorf("failed to save edited profile %q: %w", name, err)
	}
	fmt.Fprintf(cmd.OutOrStdout(), "Profile %q updated.\n", name)
	return nil
}

// editViaEditor writes originalYAML to a per-invocation temp file
// under ~/.claudecm/tmp/, launches the configured editor, reads the
// temp file back, and parses it.
//
// Return contract (F3 review finding):
//
//   - On success: returns (profile, tmpPath, nil). The caller owns
//     tmpPath cleanup so schema / name-change refusals downstream can
//     preserve the file on rejection without racing os.Remove.
//   - On ParseProfile failure: returns (nil, tmpPath, err). Caller
//     prints the preservation hint and does NOT remove the file — the
//     operator's edits must survive the rejection so they can fix the
//     YAML and retry.
//   - On write / editor-runner / read failure: returns (nil, "", err)
//     after removing tmpPath in-place, because those failures mean the
//     editor never saw the file or the user did not save anything worth
//     preserving.
//
// Refuses on parse error so a mistyped YAML never round-trips to
// SaveProfile (NFR-S1).
func editViaEditor(resv *storage.Resolver, name string, originalYAML []byte) (*config.Profile, string, error) {
	tmpDir := filepath.Join(resv.ConfigDir(), "tmp")
	if err := storage.EnsureDir(resv, tmpDir); err != nil {
		return nil, "", fmt.Errorf("ensure tmp dir: %w", err)
	}
	suffix, err := randomEditSuffix()
	if err != nil {
		return nil, "", fmt.Errorf("random suffix: %w", err)
	}
	tmpPath := filepath.Join(tmpDir, name+"."+suffix+".yaml")
	// mode 0600 for parity with SaveProfile: the temp file carries the
	// same secrets as the persisted profile, so it must not be group-
	// or world-readable even for the seconds it lives on disk.
	if err := os.WriteFile(tmpPath, originalYAML, 0o600); err != nil {
		return nil, "", fmt.Errorf("write tmp file %q: %w", tmpPath, err)
	}

	if err := editorRunnerFn(tmpPath); err != nil {
		// Editor crashed / did not save — the pre-editor bytes on disk
		// are identical to what the caller already has, so preserving
		// tmpPath would leak unused garbage into ~/.claudecm/tmp/.
		_ = os.Remove(tmpPath)
		return nil, "", fmt.Errorf("editor %q exited non-zero: %w", os.Getenv(editorEnvVar), err)
	}

	edited, err := os.ReadFile(tmpPath)
	if err != nil {
		_ = os.Remove(tmpPath)
		return nil, "", fmt.Errorf("read tmp file %q: %w", tmpPath, err)
	}
	profile, err := config.ParseProfile(edited)
	if err != nil {
		// Preserve the file so the operator does not lose work on a
		// syntax slip — F3 review finding on PR #49. Caller prints
		// the recovery hint with tmpPath.
		return nil, tmpPath, fmt.Errorf("edited YAML rejected: %w", err)
	}
	return profile, tmpPath, nil
}

// randomEditSuffix returns a hex-encoded random string used as the
// per-invocation temp-file suffix. Sourced from crypto/rand (never
// math/rand) so tests and production share the same seam and no
// package-level PRNG state exists.
func randomEditSuffix() (string, error) {
	buf := make([]byte, 8)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}

// applyEditSetEntries applies each --set entry to a clone of the
// original profile and returns the result. Refuses on the first
// unsupported path with an error listing the supported prefixes.
func applyEditSetEntries(original *config.Profile, entries []string) (*config.Profile, error) {
	out := original.Clone()
	for _, raw := range entries {
		idx := strings.Index(raw, "=")
		if idx < 0 {
			return nil, fmt.Errorf("--set %q is malformed (expected key=value)", raw)
		}
		path := raw[:idx]
		value := raw[idx+1:]
		if path == "" {
			return nil, fmt.Errorf("--set %q has an empty key", raw)
		}

		switch {
		case path == editSetKeyDescription:
			out.Description = value

		case strings.HasPrefix(path, editSetPrefixCoreDot):
			sub := strings.TrimPrefix(path, editSetPrefixCoreDot)
			setter, ok := editCoreFieldSetters[sub]
			if !ok {
				return nil, fmt.Errorf("--set %q: unsupported core field %q (supported: %s)",
					raw, sub, joinCoreFieldNames())
			}
			setter(&out.Core, value)

		case strings.HasPrefix(path, setPrefixClaudeCodeEnv):
			varName := strings.TrimPrefix(path, setPrefixClaudeCodeEnv)
			if err := validateEnvVarName(varName); err != nil {
				return nil, fmt.Errorf("--set %q: %w", raw, err)
			}
			applyClaudeCodeEnv(out, varName, value)

		case strings.HasPrefix(path, setPrefixCodexRaw):
			key := strings.TrimPrefix(path, setPrefixCodexRaw)
			if key == "" {
				return nil, fmt.Errorf("--set %q: codex raw key is empty", raw)
			}
			applyCodexRaw(out, key, value)

		default:
			return nil, fmt.Errorf(
				"--set %q has unsupported path (supported prefixes: description, %s<field>, %s<VAR>, %s<key>)",
				raw, editSetPrefixCoreDot, setPrefixClaudeCodeEnv, setPrefixCodexRaw,
			)
		}
	}
	return out, nil
}

// applyClaudeCodeEnv installs varName=value into the claude_code
// overlay's ExtraEnv map, allocating the map lazily so an
// otherwise-empty overlay does not appear as an empty block on save.
func applyClaudeCodeEnv(p *config.Profile, varName, value string) {
	if p.Tools == nil {
		p.Tools = map[config.ToolID]config.ToolOverlay{}
	}
	ov := p.Tools[config.ToolClaudeCode]
	if ov.ExtraEnv == nil {
		ov.ExtraEnv = map[string]string{}
	}
	ov.ExtraEnv[varName] = value
	p.Tools[config.ToolClaudeCode] = ov
}

// applyCodexRaw installs key=value into the codex overlay's Raw map.
// Symmetric with applyClaudeCodeEnv.
func applyCodexRaw(p *config.Profile, key, value string) {
	if p.Tools == nil {
		p.Tools = map[config.ToolID]config.ToolOverlay{}
	}
	ov := p.Tools[config.ToolCodex]
	if ov.Raw == nil {
		ov.Raw = map[string]any{}
	}
	ov.Raw[key] = value
	p.Tools[config.ToolCodex] = ov
}

// joinCoreFieldNames returns the supported core.* field names as a
// sorted comma-separated list for use in error messages.
func joinCoreFieldNames() string {
	names := make([]string, 0, len(editCoreFieldSetters))
	for k := range editCoreFieldSetters {
		names = append(names, k)
	}
	// Stable order — same rationale as parseAddOutput's error string.
	sortStrings(names)
	return strings.Join(names, ", ")
}

// sortStrings is a tiny wrapper so this file does not have to import
// sort just for one call site. Kept local (rather than reusing
// sortedStrings from switch.go) because that helper wants an input
// slice; we want to sort in place.
func sortStrings(s []string) {
	// Simple insertion sort — the list is at most a handful of entries
	// and pulling in "sort" for a five-element slice would be more
	// noise than the loop is.
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j-1] > s[j]; j-- {
			s[j-1], s[j] = s[j], s[j-1]
		}
	}
}

// renderEditDryRun writes a unified diff of the original vs. edited
// profile YAML to w. Produces no diff (and empty output body beyond
// the header) when the two are byte-identical — the AC treats a no-op
// edit as a legitimate outcome.
//
// F2 (PR#49 review): both sides of the diff are re-marshalled from
// profile copies whose core.api_key has been passed through redactValue
// first, so a --dry-run cannot leak a plaintext key to stdout / a
// shell log. The on-disk profiles are unaffected — this redaction only
// applies to the dry-run rendering path. A future story may wire
// --reveal through this surface.
func renderEditDryRun(w io.Writer, name string, original, edited *config.Profile) error {
	originalRedacted := original.Clone()
	originalRedacted.Core.APIKey = redactValue(originalRedacted.Core.APIKey)
	originalYAML, err := config.MarshalProfile(originalRedacted)
	if err != nil {
		return fmt.Errorf("marshal original profile: %w", err)
	}
	editedRedacted := edited.Clone()
	editedRedacted.Core.APIKey = redactValue(editedRedacted.Core.APIKey)
	editedYAML, err := config.MarshalProfile(editedRedacted)
	if err != nil {
		return fmt.Errorf("marshal edited profile: %w", err)
	}
	diff := difflib.UnifiedDiff{
		A:        difflib.SplitLines(string(originalYAML)),
		B:        difflib.SplitLines(string(editedYAML)),
		FromFile: fmt.Sprintf("a/%s.yaml", name),
		ToFile:   fmt.Sprintf("b/%s.yaml", name),
		Context:  3,
	}
	text, err := difflib.GetUnifiedDiffString(diff)
	if err != nil {
		return fmt.Errorf("render diff: %w", err)
	}
	fmt.Fprintf(w, "--- dry-run: edit %q ---\n", name)
	if text == "" {
		fmt.Fprintln(w, "(no changes)")
		return nil
	}
	if _, err := io.WriteString(w, text); err != nil {
		return err
	}
	return nil
}
