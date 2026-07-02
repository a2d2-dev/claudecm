// Package cmd — import command (Story E6-S4).
//
// cmd/import seeds a claudecm profile from the current on-disk state of
// a supported tool (Claude Code or Codex). Read-only over the tool's
// files; write-only over ~/.claudecm/profiles/<name>.yaml. Never
// activates the imported profile — activation is `claudecm switch`.
//
// Shape:
//
//	claudecm import <tool> [--name NAME] [--yes] [--overwrite]
//	                       [--dry-run] [--description DESC]
//	                       [--output text|json]
//
// Positional <tool> is one of "claude-code" (adapter.ToolClaudeCode) or
// "codex" (adapter.ToolCodex). Anything else is refused with an error.
//
// Semantics:
//
//  1. Bootstrap ~/.claudecm/ so a fresh machine can import on first
//     invocation without a separate "init" step.
//  2. Resolve the positional to a ToolID and pull the constructor out of
//     adapter.DefaultRegistry.
//  3. Call adapter.Import(ctx, r). ErrNoConfig from the adapter package
//     is surfaced as "no <tool> configuration found on disk to import";
//     ErrParseFailed is surfaced with a "malformed" prefix (NFR-S1 —
//     refuse cleanly, no fallback writes). All other errors surface
//     as-is.
//  4. Compose a config.Profile from the returned Core + Overlay. The
//     tools map only carries the imported tool's overlay when that
//     overlay is non-empty (so the YAML stays sparse).
//  5. --dry-run: render the would-be profile bytes (YAML in text mode,
//     structured JSON in --output json), redact secrets per NFR-S8,
//     exit 0.
//  6. Non-dry-run path: validate the name, refuse on collision unless
//     --overwrite, save via storage.FileStorage.
//  7. Interactive path prompts for a name (default = tool spelling)
//     and confirms before writing; --yes skips both prompts (and
//     requires --name).
//
// Redaction. The preview render always redacts secret core fields
// (api_key) and any overlay ExtraEnv / Raw keys whose last dotted
// segment looks like a secret. --reveal is NOT wired here: import is a
// review step, not a debugging surface; the resulting YAML file is
// stored locally with the same 0600 permissions storage.SaveProfile
// already emits, so the plaintext is one `cat` away for operators who
// actually need it.
//
// Exit codes: 0 on success (including --dry-run); 1 on any error
// (unknown tool, no on-disk config, malformed config, duplicate name
// without --overwrite, invalid name, --yes without --name).
package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"

	"github.com/spf13/cobra"

	"github.com/a2d2-dev/claudecm/internal/adapter"
	claudecodeadapter "github.com/a2d2-dev/claudecm/internal/adapter/claudecode"
	codexadapter "github.com/a2d2-dev/claudecm/internal/adapter/codex"
	"github.com/a2d2-dev/claudecm/internal/config"
	"github.com/a2d2-dev/claudecm/internal/storage"
	"github.com/a2d2-dev/claudecm/internal/writepath"
)

// importOutputFormat mirrors the enum shape used by add/switch/current/
// explain so operators speak the same language across every command.
type importOutputFormat string

const (
	importOutputText importOutputFormat = "text"
	importOutputJSON importOutputFormat = "json"
)

// importToolAlias maps the CLI positional spelling to the corresponding
// adapter.ToolID. Frozen at two entries for v1 (ADR-0001 Decision 1);
// adding a third tool post-v1 lands here and in the switch of the same
// name in cmd/switch's registry iteration.
var importToolAlias = map[string]adapter.ToolID{
	"claude-code": adapter.ToolClaudeCode,
	"codex":       adapter.ToolCodex,
}

var (
	importNameFlag        string
	importYesFlag         bool
	importOverwriteFlag   bool
	importDryRunFlag      bool
	importDescriptionFlag string
	importOutputFlag      string
)

var importCmd = &cobra.Command{
	Use:   "import [claude-code|codex]",
	Short: "Seed a profile from an existing tool's on-disk configuration",
	Long: `Import an existing tool's on-disk configuration into a new
claudecm profile.

Reads the target tool's owned files via the adapter's Import method
and saves the result to ~/.claudecm/profiles/<name>.yaml with
schema_version: 1. The set of owned files per tool is documented in
the tool-specific adapter package.

Refuse-on-malformed. If the tool's on-disk file exists but does not
parse, import refuses (NFR-S1). No profile is written and no fallback
byte-shape is inferred.

No auto-activation. import does NOT flip the active profile; run
'claudecm switch <name>' after import to activate it.

EXAMPLES
  # Interactive: preview, prompt for a name, confirm.
  claudecm import claude-code

  # Non-interactive: name is required with --yes.
  claudecm import codex --name work --yes

  # Preview only — no writes.
  claudecm import claude-code --name work --dry-run

  # Replace an existing profile.
  claudecm import codex --name work --yes --overwrite`,
	Args: cobra.ExactArgs(1),
	RunE: runImport,
}

func init() {
	importCmd.Flags().StringVar(&importNameFlag, "name", "", "Profile name to save under (required with --yes)")
	importCmd.Flags().BoolVar(&importYesFlag, "yes", false, "Skip interactive prompts (requires --name)")
	importCmd.Flags().BoolVar(&importOverwriteFlag, "overwrite", false, "Allow replacing an existing profile with the same name")
	importCmd.Flags().BoolVar(&importDryRunFlag, "dry-run", false, "Print the would-be profile and exit without writing")
	importCmd.Flags().StringVar(&importDescriptionFlag, "description", "", "Optional human-readable description")
	importCmd.Flags().StringVarP(&importOutputFlag, "output", "o", "text", "Output format (text|json)")

	rootCmd.AddCommand(importCmd)
}

// runImport is the testable entry point. Returns nil on success (including
// --dry-run), a non-nil error otherwise. Never touches os.Exit; tests call
// this directly with a synthetic cobra.Command whose Out/Err are
// bytes.Buffers.
func runImport(cmd *cobra.Command, args []string) error {
	toolArg := strings.TrimSpace(args[0])
	toolID, ok := importToolAlias[toolArg]
	if !ok {
		return fmt.Errorf("unknown tool %q (want one of: %s)", toolArg, strings.Join(sortedImportToolAliases(), ", "))
	}

	format, err := parseImportOutput(importOutputFlag)
	if err != nil {
		return err
	}

	// --yes without --name is a scripting footgun: the operator asked
	// for a non-interactive run but did not tell us what to call the
	// profile. Fail early — before any I/O — so the error text tells
	// them exactly what to add.
	if importYesFlag && strings.TrimSpace(importNameFlag) == "" {
		return fmt.Errorf("--yes requires --name (non-interactive mode has no way to prompt for a name)")
	}

	resv, err := storage.Default()
	if err != nil {
		return fmt.Errorf("failed to resolve HOME: %w", err)
	}
	if err := storage.Bootstrap(resv); err != nil {
		return fmt.Errorf("failed to bootstrap ~/.claudecm layout: %w", err)
	}
	store := storage.NewFileStorage(resv)

	a, ok := adapter.DefaultRegistry.Get(toolID)
	if !ok {
		return fmt.Errorf("no adapter registered for tool %q", toolArg)
	}

	ctx := context.Background()
	core, overlay, importErr := a.Import(ctx, resv)
	if importErr != nil {
		return translateImportError(toolArg, importErr)
	}

	// Compose the candidate Profile. Name is deferred: we render the
	// preview under a placeholder-safe empty name and stamp the real
	// one after the operator (or --name) tells us what it should be.
	// This keeps the preview stable across a name change and avoids
	// re-rendering after the prompt.
	now := nowFn().UTC()
	profile := &config.Profile{
		SchemaVersion: config.CurrentProfileSchemaVersion,
		Description:   importDescriptionFlag,
		CreatedAt:     now,
		UpdatedAt:     now,
		Core:          core,
	}
	if !isEmptyOverlay(overlay) {
		profile.Tools = map[config.ToolID]config.ToolOverlay{
			toolID: overlay,
		}
	}

	// Resolve the name we'll use. --name wins uncontested; otherwise
	// prompt on a TTY (default = tool spelling) or fail loudly when
	// stdin is not a terminal so a piped script cannot hang.
	name, err := resolveImportName(cmd, format, toolArg)
	if err != nil {
		return err
	}
	profile.Name = name

	// Interactive preview + confirmation. --yes skips both. --dry-run
	// renders the preview but stops before any write regardless of
	// --yes.
	if !importYesFlag && !importDryRunFlag {
		if err := renderImportPreview(cmd.OutOrStdout(), format, toolArg, profile); err != nil {
			return err
		}
		if !isTerminal(os.Stdin) {
			return fmt.Errorf("non-interactive session: pass --yes to confirm the import or --dry-run to preview")
		}
		yes, promptErr := promptConfirm(cmd.OutOrStdout(), os.Stdin, "Save this profile?")
		if promptErr != nil {
			return fmt.Errorf("read confirmation: %w", promptErr)
		}
		if !yes {
			if format == importOutputText {
				fmt.Fprintln(cmd.OutOrStdout(), "aborted; no profile written.")
			}
			return fmt.Errorf("aborted by user")
		}
	}

	if importDryRunFlag {
		return renderImportDryRun(cmd.OutOrStdout(), format, toolArg, profile)
	}

	// Validate the name once, at the last gate before writing. Every
	// other command routes name validation through this same
	// storage.ValidateProfileName (NFR-S5), so import stays consistent
	// with add/switch/delete's naming rules.
	if err := storage.ValidateProfileName(profile.Name); err != nil {
		return err
	}

	exists, err := store.ProfileExists(profile.Name)
	if err != nil {
		return fmt.Errorf("failed to check whether profile %q exists: %w", profile.Name, err)
	}
	if exists && !importOverwriteFlag {
		return fmt.Errorf("profile %q already exists; use --overwrite to replace", profile.Name)
	}

	if err := store.SaveProfile(profile); err != nil {
		return fmt.Errorf("failed to save profile %q: %w", profile.Name, err)
	}

	return renderImportSuccess(cmd.OutOrStdout(), format, toolArg, profile)
}

// translateImportError maps the adapter package's sentinel error surface
// into user-facing messages that name the tool and give a next step.
// ErrNoConfig ("nothing to import") and ErrParseFailed ("malformed
// source") are the two documented shapes; anything else surfaces
// verbatim so an unexpected error (permission denied, I/O) is not
// silently swallowed.
func translateImportError(toolArg string, err error) error {
	switch {
	case errors.Is(err, claudecodeadapter.ErrNoConfig),
		errors.Is(err, codexadapter.ErrNoConfig):
		return fmt.Errorf("no %s configuration found on disk to import from", toolArg)
	case errors.Is(err, claudecodeadapter.ErrParseFailed),
		errors.Is(err, codexadapter.ErrParseFailed),
		errors.Is(err, writepath.ErrParseFailed):
		return fmt.Errorf("cannot import: %s configuration is malformed: %w", toolArg, err)
	default:
		return fmt.Errorf("import %s: %w", toolArg, err)
	}
}

// resolveImportName picks the profile name to save under.
//   - --name wins uncontested.
//   - Otherwise, on a TTY, prompt with the tool spelling as default.
//   - On non-TTY without --name, return an error naming --name / --yes
//     so a piped invocation is fatal and self-documenting rather than
//     silently hanging.
func resolveImportName(cmd *cobra.Command, format importOutputFormat, toolArg string) (string, error) {
	if n := strings.TrimSpace(importNameFlag); n != "" {
		return n, nil
	}
	if !isTerminal(os.Stdin) {
		return "", fmt.Errorf("--name is required when stdin is not a terminal; pass --name <name> (with --yes for non-interactive)")
	}
	// Preview first so the operator has context before naming.
	// Build a scratch profile with a placeholder name — the caller will
	// re-render after the name is known if --dry-run fires.
	_ = format // reserved for future JSON-preview-then-prompt flows
	fmt.Fprintf(cmd.OutOrStdout(), "Save as [%s]: ", toolArg)
	line, err := readLine(os.Stdin)
	if err != nil {
		return "", fmt.Errorf("read profile name: %w", err)
	}
	line = strings.TrimSpace(line)
	if line == "" {
		return toolArg, nil
	}
	return line, nil
}

// readLine reads one newline-terminated line from r. Mirrors the tiny
// scanner-free reader in cmd/switch's promptConfirm so import stays
// consistent with the rest of the cmd/* prompt discipline.
func readLine(in io.Reader) (string, error) {
	var line string
	buf := make([]byte, 1)
	for {
		n, err := in.Read(buf)
		if n > 0 {
			if buf[0] == '\n' {
				break
			}
			line += string(buf[0])
		}
		if err != nil {
			if err == io.EOF {
				break
			}
			return "", err
		}
	}
	return line, nil
}

// parseImportOutput validates and normalises the --output flag. Mirrors
// parseAddOutput / parseSwitchOutput for consistency.
func parseImportOutput(raw string) (importOutputFormat, error) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "", "text":
		return importOutputText, nil
	case "json":
		return importOutputJSON, nil
	default:
		return "", fmt.Errorf("invalid --output %q (want text|json)", raw)
	}
}

// isEmptyOverlay reports whether every field of the overlay is the zero
// value. Used to decide whether to populate the profile's Tools map: a
// zero overlay would emit an empty `tools: { <tool>: {} }` block in the
// YAML, which is round-trip noise.
func isEmptyOverlay(ov config.ToolOverlay) bool {
	return ov.BaseURL == "" &&
		ov.APIKey == "" &&
		ov.Model == "" &&
		ov.SmallFastModel == "" &&
		len(ov.ExtraEnv) == 0 &&
		len(ov.Raw) == 0
}

// sortedImportToolAliases returns the CLI-visible tool spellings sorted
// for a stable error message.
func sortedImportToolAliases() []string {
	out := make([]string, 0, len(importToolAlias))
	for k := range importToolAlias {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// ---------------------------------------------------------------------------
// Rendering
// ---------------------------------------------------------------------------

// renderImportPreview prints the pre-save preview block. Text mode is a
// human-friendly summary; JSON mode emits the structured shape (same as
// the dry-run body, but with action="preview") so machine consumers can
// still route a preview separately from a dry-run.
func renderImportPreview(w io.Writer, format importOutputFormat, toolArg string, profile *config.Profile) error {
	if format == importOutputJSON {
		return writeImportJSON(w, jsonImportPreview{
			Action:  "preview",
			Tool:    toolArg,
			Profile: profileToImportJSON(profile),
		})
	}
	fmt.Fprintf(w, "--- imported %s profile (preview) ---\n", toolArg)
	renderImportProfileText(w, profile)
	fmt.Fprintln(w)
	return nil
}

// renderImportDryRun prints the would-be profile bytes. Text mode uses
// the same layout as renderImportPreview but with a "dry-run" header;
// JSON mode emits a structured document consumers can jq without
// stripping a header line.
func renderImportDryRun(w io.Writer, format importOutputFormat, toolArg string, profile *config.Profile) error {
	if format == importOutputJSON {
		yaml, err := config.MarshalProfile(profile)
		if err != nil {
			return fmt.Errorf("marshal profile for dry-run: %w", err)
		}
		return writeImportJSON(w, jsonImportDryRun{
			Action:  "dry-run",
			Tool:    toolArg,
			Profile: profileToImportJSON(profile),
			YAML:    string(yaml),
		})
	}
	fmt.Fprintf(w, "--- dry-run: imported %s profile (not written) ---\n", toolArg)
	renderImportProfileText(w, profile)
	return nil
}

// renderImportSuccess prints the post-save confirmation.
func renderImportSuccess(w io.Writer, format importOutputFormat, toolArg string, profile *config.Profile) error {
	if format == importOutputJSON {
		return writeImportJSON(w, jsonImportSuccess{
			Action:  "created",
			Tool:    toolArg,
			Profile: profileToImportJSON(profile),
		})
	}
	fmt.Fprintf(w, "Imported %s configuration as profile %q.\n", toolArg, profile.Name)
	fmt.Fprintf(w, "Now activate with 'claudecm switch %s'.\n", profile.Name)
	return nil
}

// renderImportProfileText writes the human-readable preview body. Secrets
// are redacted via redactedValueDisplay so a shared terminal cannot leak
// a plaintext API key.
func renderImportProfileText(w io.Writer, profile *config.Profile) {
	fmt.Fprintf(w, "  name:        %s\n", profile.Name)
	if profile.Description != "" {
		fmt.Fprintf(w, "  description: %s\n", profile.Description)
	}
	fmt.Fprintf(w, "  schema:      %d\n", profile.SchemaVersion)
	fmt.Fprintln(w, "  core:")
	if profile.Core.Provider != "" {
		fmt.Fprintf(w, "    provider:         %s\n", profile.Core.Provider)
	}
	if profile.Core.BaseURL != "" {
		fmt.Fprintf(w, "    base_url:         %s\n", profile.Core.BaseURL)
	}
	if profile.Core.APIKey != "" {
		fmt.Fprintf(w, "    api_key:          %s\n", redactedValueDisplay("api_key", profile.Core.APIKey))
	}
	if profile.Core.Model != "" {
		fmt.Fprintf(w, "    model:            %s\n", profile.Core.Model)
	}
	if profile.Core.SmallFastModel != "" {
		fmt.Fprintf(w, "    small_fast_model: %s\n", profile.Core.SmallFastModel)
	}
	if len(profile.Core.ExtraEnv) > 0 {
		fmt.Fprintln(w, "    extra_env:")
		for _, k := range sortedStringKeys(profile.Core.ExtraEnv) {
			fmt.Fprintf(w, "      %s: %s\n", k, redactedValueDisplay(k, profile.Core.ExtraEnv[k]))
		}
	}
	if len(profile.Tools) == 0 {
		return
	}
	fmt.Fprintln(w, "  tools:")
	for _, id := range sortedToolIDs(profile.Tools) {
		ov := profile.Tools[id]
		fmt.Fprintf(w, "    %s:\n", id)
		if ov.BaseURL != "" {
			fmt.Fprintf(w, "      base_url: %s\n", ov.BaseURL)
		}
		if ov.APIKey != "" {
			fmt.Fprintf(w, "      api_key:  %s\n", redactedValueDisplay("api_key", ov.APIKey))
		}
		if ov.Model != "" {
			fmt.Fprintf(w, "      model:    %s\n", ov.Model)
		}
		if ov.SmallFastModel != "" {
			fmt.Fprintf(w, "      small_fast_model: %s\n", ov.SmallFastModel)
		}
		if len(ov.ExtraEnv) > 0 {
			fmt.Fprintln(w, "      extra_env:")
			for _, k := range sortedStringKeys(ov.ExtraEnv) {
				fmt.Fprintf(w, "        %s: %s\n", k, redactedValueDisplay(k, ov.ExtraEnv[k]))
			}
		}
		if len(ov.Raw) > 0 {
			fmt.Fprintln(w, "      raw:")
			for _, k := range sortedRawKeys(ov.Raw) {
				fmt.Fprintf(w, "        %s: %s\n", k, redactedValueDisplay(k, ov.Raw[k]))
			}
		}
	}
}

// sortedStringKeys returns m's keys sorted lexicographically.
func sortedStringKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// sortedRawKeys returns m's keys sorted lexicographically.
func sortedRawKeys(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// sortedToolIDs returns m's keys sorted lexicographically as ToolIDs.
func sortedToolIDs(m map[config.ToolID]config.ToolOverlay) []config.ToolID {
	out := make([]config.ToolID, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// ---------------------------------------------------------------------------
// JSON wire types
// ---------------------------------------------------------------------------

// jsonImportProfile is the structural view of a Profile the JSON output
// emits. Secret fields render as their redacted form so a --output json
// pipeline cannot leak an API key any more than the text preview does.
type jsonImportProfile struct {
	SchemaVersion int                          `json:"schema_version"`
	Name          string                       `json:"name"`
	Description   string                       `json:"description,omitempty"`
	Core          jsonImportCore               `json:"core"`
	Tools         map[string]jsonImportOverlay `json:"tools,omitempty"`
}

type jsonImportCore struct {
	Provider       string            `json:"provider,omitempty"`
	BaseURL        string            `json:"base_url,omitempty"`
	APIKey         string            `json:"api_key,omitempty"`
	Model          string            `json:"model,omitempty"`
	SmallFastModel string            `json:"small_fast_model,omitempty"`
	ExtraEnv       map[string]string `json:"extra_env,omitempty"`
}

type jsonImportOverlay struct {
	BaseURL        string            `json:"base_url,omitempty"`
	APIKey         string            `json:"api_key,omitempty"`
	Model          string            `json:"model,omitempty"`
	SmallFastModel string            `json:"small_fast_model,omitempty"`
	ExtraEnv       map[string]string `json:"extra_env,omitempty"`
	Raw            map[string]string `json:"raw,omitempty"`
}

type jsonImportPreview struct {
	Action  string            `json:"action"`
	Tool    string            `json:"tool"`
	Profile jsonImportProfile `json:"profile"`
}

type jsonImportDryRun struct {
	Action  string            `json:"action"`
	Tool    string            `json:"tool"`
	Profile jsonImportProfile `json:"profile"`
	YAML    string            `json:"yaml"`
}

type jsonImportSuccess struct {
	Action  string            `json:"action"`
	Tool    string            `json:"tool"`
	Profile jsonImportProfile `json:"profile"`
}

// profileToImportJSON is the profile → JSON adapter for the import
// wire types. Secrets are redacted at this boundary via
// redactedValueDisplay so downstream JSON consumers see the same shape
// the text preview does.
func profileToImportJSON(p *config.Profile) jsonImportProfile {
	out := jsonImportProfile{
		SchemaVersion: p.SchemaVersion,
		Name:          p.Name,
		Description:   p.Description,
		Core: jsonImportCore{
			Provider:       p.Core.Provider,
			BaseURL:        p.Core.BaseURL,
			Model:          p.Core.Model,
			SmallFastModel: p.Core.SmallFastModel,
		},
	}
	if p.Core.APIKey != "" {
		out.Core.APIKey = redactedValueDisplay("api_key", p.Core.APIKey)
	}
	if len(p.Core.ExtraEnv) > 0 {
		out.Core.ExtraEnv = map[string]string{}
		for k, v := range p.Core.ExtraEnv {
			out.Core.ExtraEnv[k] = redactedValueDisplay(k, v)
		}
	}
	if len(p.Tools) > 0 {
		out.Tools = map[string]jsonImportOverlay{}
		for id, ov := range p.Tools {
			entry := jsonImportOverlay{
				BaseURL:        ov.BaseURL,
				Model:          ov.Model,
				SmallFastModel: ov.SmallFastModel,
			}
			if ov.APIKey != "" {
				entry.APIKey = redactedValueDisplay("api_key", ov.APIKey)
			}
			if len(ov.ExtraEnv) > 0 {
				entry.ExtraEnv = map[string]string{}
				for k, v := range ov.ExtraEnv {
					entry.ExtraEnv[k] = redactedValueDisplay(k, v)
				}
			}
			if len(ov.Raw) > 0 {
				entry.Raw = map[string]string{}
				for k, v := range ov.Raw {
					entry.Raw[k] = redactedValueDisplay(k, v)
				}
			}
			out.Tools[string(id)] = entry
		}
	}
	return out
}

func writeImportJSON(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}
