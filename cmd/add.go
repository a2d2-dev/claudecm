// Package cmd — add command (Story E6-S3).
//
// cmd/add is the E6-S3 rewrite of the pre-refactor MVP. The old command
// was environment-scraping and interactive; the E6-S3 shape is a
// declarative, script-friendly profile creator that:
//
//  1. Takes the profile name as a positional argument (single source of
//     truth — no --name/-n split with the positional).
//  2. Accepts every core field (--base-url, --api-key, --model,
//     --small-fast-model, --description, --provider) as a flag.
//  3. Accepts sparse per-tool overlay entries through repeatable
//     --set "tools.<tool>.<sub>=<value>" flags (the only two supported
//     sub-paths in v1 are `claude_code.env.<VAR>` and
//     `codex.raw.<flat.dotted.key>`).
//  4. Validates the name via storage.ValidateProfileName so NFR-S5's
//     regex + reserved-name checks are enforced at exactly one gate.
//  5. Refuses to overwrite an existing profile unless --overwrite is
//     passed — silent replacement is explicitly out of scope per PRD
//     FR-15 and CLAUDE.md's no-fallback-writes rule.
//  6. Supports --dry-run: renders the would-be profile bytes (YAML in
//     text mode, structured JSON in --output json) and exits without
//     touching disk beyond the Bootstrap layout Bootstrap already owns.
//  7. Does NOT auto-activate the new profile. Activation is a `switch`
//     step (Story AC #6). state.yaml's CurrentProfile is untouched.
//
// Exit codes: 0 on success (including --dry-run); 1 on any error
// (validation, duplicate name without --overwrite, malformed --set, I/O).
package cmd

import (
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/a2d2-dev/claudecm/internal/config"
	"github.com/a2d2-dev/claudecm/internal/storage"
)

// addOutputFormat mirrors the enum shape used by switch / current /
// explain so operators speak the same language across every command.
type addOutputFormat string

const (
	addOutputText addOutputFormat = "text"
	addOutputJSON addOutputFormat = "json"
)

// addProviderDefault is the assumed provider when --provider is not
// passed. Kept as a named constant so tests and docs stay in sync with
// the code path that stamps it.
const addProviderDefault = "anthropic"

// Supported --set path prefixes. Any other path is refused with an
// error listing exactly these — no silent no-ops.
const (
	setPrefixClaudeCodeEnv = "tools.claude_code.env."
	setPrefixCodexRaw      = "tools.codex.raw."
)

// addProviderAllowed is the closed enum for --provider. Anything outside
// this set is refused so a typo does not silently write a bad file.
var addProviderAllowed = map[string]struct{}{
	"anthropic":     {},
	"openai-compat": {},
	"custom":        {},
}

var (
	addDescriptionFlag    string
	addProviderFlag       string
	addBaseURLFlag        string
	addAPIKeyFlag         string
	addModelFlag          string
	addSmallFastModelFlag string
	addSetFlag            []string
	addDryRunFlag         bool
	addOverwriteFlag      bool
	addOutputFlag         string
)

// nowFn is the timestamp seam. Tests override this via
// SetNowForTest to pin CreatedAt / UpdatedAt to a deterministic value
// so JSON / YAML dry-run assertions stay stable. Documented exception
// to coding-standards rule 12 (no package-level mutable state),
// mirroring the DefaultRegistry / envextract lookup / switch's
// isTerminalFn seams: written only at init or test-swap time, read
// thereafter.
var nowFn = time.Now

// SetNowForTest overrides the timestamp seam. Returns a restore
// closure the caller MUST defer to put the production time.Now back.
func SetNowForTest(fn func() time.Time) func() {
	prev := nowFn
	nowFn = fn
	return func() { nowFn = prev }
}

var addCmd = &cobra.Command{
	Use:   "add [profile-name]",
	Short: "Create a new profile in the unified schema (v1)",
	Long: `Create a new claudecm profile written to
~/.claudecm/profiles/<name>.yaml with schema_version: 1.

Core fields are set from flags. Per-tool overlays are set via repeatable
--set entries whose path starts with one of the supported prefixes:

  tools.claude_code.env.<VAR>=<value>
      Adds an entry to the claude_code overlay's extra_env map.
  tools.codex.raw.<flat.dotted.key>=<value>
      Adds an entry to the codex overlay's raw (escape-hatch) map.

Any other --set path is refused. The profile name is validated against
the NFR-S5 allowlist (^[a-z0-9][a-z0-9._-]{0,63}$ plus reserved-name
protection).

EXAMPLES
  # Minimal core fields
  claudecm add work \
    --base-url https://api.anthropic.com \
    --api-key sk-... \
    --model claude-opus-4-5

  # Overlay a claude_code env override
  claudecm add work \
    --base-url https://api.anthropic.com \
    --api-key sk-... \
    --set tools.claude_code.env.CLAUDE_CODE_USE_BEDROCK=1

  # Overlay a codex raw key
  claudecm add work \
    --base-url https://api.anthropic.com \
    --api-key sk-... \
    --set tools.codex.raw.model=gpt-5

  # Preview only — no writes
  claudecm add work --base-url ... --api-key ... --dry-run

  # Replace an existing profile
  claudecm add work --base-url ... --api-key ... --overwrite

add does NOT auto-activate the new profile. Use 'claudecm switch <name>'
to make it the active profile.`,
	Args: cobra.ExactArgs(1),
	RunE: runAdd,
}

func init() {
	addCmd.Flags().StringVar(&addDescriptionFlag, "description", "", "Optional human-readable description")
	addCmd.Flags().StringVar(&addProviderFlag, "provider", addProviderDefault, "Provider tag: anthropic|openai-compat|custom")
	addCmd.Flags().StringVar(&addBaseURLFlag, "base-url", "", "Core base URL")
	addCmd.Flags().StringVar(&addAPIKeyFlag, "api-key", "", "Core API key")
	addCmd.Flags().StringVar(&addModelFlag, "model", "", "Core model name")
	addCmd.Flags().StringVar(&addSmallFastModelFlag, "small-fast-model", "", "Core small/fast auxiliary model name")
	addCmd.Flags().StringArrayVar(&addSetFlag, "set", nil,
		"Sparse overlay entry (repeatable). Format: tools.<tool>.<sub>=<value>. "+
			"Supported: tools.claude_code.env.<VAR>=<value>, tools.codex.raw.<key>=<value>")
	addCmd.Flags().BoolVar(&addDryRunFlag, "dry-run", false, "Print the would-be profile and exit without writing")
	addCmd.Flags().BoolVar(&addOverwriteFlag, "overwrite", false, "Allow replacing an existing profile with the same name")
	addCmd.Flags().StringVarP(&addOutputFlag, "output", "o", "text", "Output format (text|json)")

	rootCmd.AddCommand(addCmd)
}

// runAdd is the testable entry point. Returns nil on success (including
// --dry-run), a non-nil error otherwise. Never touches os.Exit; tests
// call this directly with a synthetic cobra.Command whose Out/Err are
// bytes.Buffers.
func runAdd(cmd *cobra.Command, args []string) error {
	name := strings.TrimSpace(args[0])
	if err := storage.ValidateProfileName(name); err != nil {
		return err
	}

	format, err := parseAddOutput(addOutputFlag)
	if err != nil {
		return err
	}

	if err := validateProvider(addProviderFlag); err != nil {
		return err
	}

	// Build tools overlay from --set entries. Parsing is a pure
	// function so an invalid entry surfaces before any I/O.
	tools, err := parseSetEntries(addSetFlag)
	if err != nil {
		return err
	}

	now := nowFn().UTC()
	profile := &config.Profile{
		SchemaVersion: config.CurrentProfileSchemaVersion,
		Name:          name,
		Description:   addDescriptionFlag,
		CreatedAt:     now,
		UpdatedAt:     now,
		Core: config.CoreConfig{
			Provider:       addProviderFlag,
			BaseURL:        addBaseURLFlag,
			APIKey:         addAPIKeyFlag,
			Model:          addModelFlag,
			SmallFastModel: addSmallFastModelFlag,
		},
		Tools: tools,
	}

	// Bootstrap is required whether or not the write path fires: --dry-run
	// still needs a Resolver to exist (and, for parity with every other
	// command, we do not want a machine without ~/.claudecm to succeed
	// silently and then fail later on the first non-dry-run add).
	resv, err := storage.Default()
	if err != nil {
		return fmt.Errorf("failed to resolve HOME: %w", err)
	}
	if err := storage.Bootstrap(resv); err != nil {
		return fmt.Errorf("failed to bootstrap ~/.claudecm layout: %w", err)
	}
	store := storage.NewFileStorage(resv)

	if addDryRunFlag {
		return renderAddDryRun(cmd.OutOrStdout(), format, profile)
	}

	// Duplicate-name guard. --overwrite is the ONLY way to replace an
	// existing profile — silent overwrite is explicitly out of scope
	// (PRD FR-15, CLAUDE.md no-fallback-writes).
	exists, err := store.ProfileExists(name)
	if err != nil {
		return fmt.Errorf("failed to check whether profile %q exists: %w", name, err)
	}
	if exists && !addOverwriteFlag {
		return fmt.Errorf("profile %q already exists; use --overwrite to replace", name)
	}

	if err := store.SaveProfile(profile); err != nil {
		return fmt.Errorf("failed to save profile %q: %w", name, err)
	}

	return renderAddSuccess(cmd.OutOrStdout(), format, profile)
}

// parseAddOutput validates and normalises the --output flag. Mirrors
// parseSwitchOutput / parseExplainOutput for consistency.
func parseAddOutput(raw string) (addOutputFormat, error) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "", "text":
		return addOutputText, nil
	case "json":
		return addOutputJSON, nil
	default:
		return "", fmt.Errorf("invalid --output %q (want text|json)", raw)
	}
}

// validateProvider enforces the closed enum on --provider. Empty is
// treated as an operator error because cobra's default already stamps
// addProviderDefault; getting empty here means someone explicitly passed
// --provider "".
func validateProvider(p string) error {
	if _, ok := addProviderAllowed[p]; !ok {
		want := make([]string, 0, len(addProviderAllowed))
		for k := range addProviderAllowed {
			want = append(want, k)
		}
		sort.Strings(want)
		return fmt.Errorf("invalid --provider %q (want one of: %s)", p, strings.Join(want, ", "))
	}
	return nil
}

// parseSetEntries walks each --set argument and folds it into a
// per-tool overlay map. Only the two v1 sub-paths are accepted; every
// other path is refused with an error listing the supported prefixes.
// Returns nil (not an empty map) when no --set arguments were passed so
// the caller does not emit an empty `tools: {}` block in the YAML.
func parseSetEntries(entries []string) (map[config.ToolID]config.ToolOverlay, error) {
	if len(entries) == 0 {
		return nil, nil
	}
	out := map[config.ToolID]config.ToolOverlay{}
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
		case strings.HasPrefix(path, setPrefixClaudeCodeEnv):
			varName := strings.TrimPrefix(path, setPrefixClaudeCodeEnv)
			if err := validateEnvVarName(varName); err != nil {
				return nil, fmt.Errorf("--set %q: %w", raw, err)
			}
			ov := out[config.ToolClaudeCode]
			if ov.ExtraEnv == nil {
				ov.ExtraEnv = map[string]string{}
			}
			ov.ExtraEnv[varName] = value
			out[config.ToolClaudeCode] = ov

		case strings.HasPrefix(path, setPrefixCodexRaw):
			key := strings.TrimPrefix(path, setPrefixCodexRaw)
			if key == "" {
				return nil, fmt.Errorf("--set %q: codex raw key is empty", raw)
			}
			ov := out[config.ToolCodex]
			if ov.Raw == nil {
				ov.Raw = map[string]any{}
			}
			ov.Raw[key] = value
			out[config.ToolCodex] = ov

		default:
			return nil, fmt.Errorf(
				"--set %q has unsupported path (supported prefixes: %s, %s)",
				raw, setPrefixClaudeCodeEnv, setPrefixCodexRaw,
			)
		}
	}
	return out, nil
}

// validateEnvVarName enforces the "UPPER_SNAKE_CASE, no funny chars"
// convention on env vars set via the claude_code overlay. We do not
// forbid every possible env name here (that is the resolver's business)
// but do fail-fast on obvious mistakes (empty, starts with digit, has
// dots — which would suggest the user mixed up dotted-path and env
// name).
func validateEnvVarName(name string) error {
	if name == "" {
		return fmt.Errorf("env var name is empty")
	}
	if strings.ContainsAny(name, ".= \t\n\r/") {
		return fmt.Errorf("env var name %q contains an invalid character", name)
	}
	// Leading digit → almost certainly a typo; POSIX also forbids it.
	if name[0] >= '0' && name[0] <= '9' {
		return fmt.Errorf("env var name %q cannot begin with a digit", name)
	}
	return nil
}

// renderAddDryRun prints the would-be profile bytes. Text mode uses the
// YAML shape SaveProfile would have written (via config.MarshalProfile);
// JSON mode encodes a JSON snapshot of the same struct so shell
// consumers can jq the output.
func renderAddDryRun(w io.Writer, format addOutputFormat, profile *config.Profile) error {
	body, err := config.MarshalProfile(profile)
	if err != nil {
		return fmt.Errorf("marshal profile for dry-run: %w", err)
	}
	if format == addOutputJSON {
		out := jsonAddDryRun{
			Action:  "dry-run",
			Profile: profileToJSON(profile),
			YAML:    string(body),
		}
		return writeAddJSON(w, out)
	}
	// Header hint keeps the text mode operator-friendly without
	// polluting the YAML block a copy-paste would grab.
	fmt.Fprintln(w, "--- dry-run: profile YAML (not written) ---")
	_, err = w.Write(body)
	return err
}

// renderAddSuccess prints the post-save confirmation. Text mode is a
// one-liner; JSON mode carries the same profile shape as the dry-run
// path so downstream consumers can decode both with the same schema.
func renderAddSuccess(w io.Writer, format addOutputFormat, profile *config.Profile) error {
	if format == addOutputJSON {
		out := jsonAddSuccess{
			Action:  "created",
			Profile: profileToJSON(profile),
		}
		return writeAddJSON(w, out)
	}
	fmt.Fprintf(w, "Profile %q created.\n", profile.Name)
	return nil
}

// ---------------------------------------------------------------------------
// JSON wire types
// ---------------------------------------------------------------------------

// jsonAddProfile is the structural view of a Profile the JSON output
// emits. Kept separate from config.Profile so the wire format does not
// accidentally drift with an internal-struct rename and so we can
// control the field order / omitempty rules without changing YAML tags.
type jsonAddProfile struct {
	SchemaVersion int                       `json:"schema_version"`
	Name          string                    `json:"name"`
	Description   string                    `json:"description,omitempty"`
	CreatedAt     time.Time                 `json:"created_at"`
	UpdatedAt     time.Time                 `json:"updated_at"`
	Core          jsonAddCore               `json:"core"`
	Tools         map[string]jsonAddOverlay `json:"tools,omitempty"`
}

type jsonAddCore struct {
	Provider       string            `json:"provider,omitempty"`
	BaseURL        string            `json:"base_url"`
	APIKey         string            `json:"api_key"`
	Model          string            `json:"model,omitempty"`
	SmallFastModel string            `json:"small_fast_model,omitempty"`
	ExtraEnv       map[string]string `json:"extra_env,omitempty"`
}

type jsonAddOverlay struct {
	BaseURL        string            `json:"base_url,omitempty"`
	APIKey         string            `json:"api_key,omitempty"`
	Model          string            `json:"model,omitempty"`
	SmallFastModel string            `json:"small_fast_model,omitempty"`
	ExtraEnv       map[string]string `json:"extra_env,omitempty"`
	Raw            map[string]any    `json:"raw,omitempty"`
}

// jsonAddDryRun is the top-level document for --dry-run --output json.
// YAML carries the marshaled bytes (so a consumer can pipe the exact
// wire form SaveProfile would have written), while Profile carries a
// pre-parsed shape.
type jsonAddDryRun struct {
	Action  string         `json:"action"`
	Profile jsonAddProfile `json:"profile"`
	YAML    string         `json:"yaml"`
}

// jsonAddSuccess is the top-level document for a successful save.
type jsonAddSuccess struct {
	Action  string         `json:"action"`
	Profile jsonAddProfile `json:"profile"`
}

func profileToJSON(p *config.Profile) jsonAddProfile {
	out := jsonAddProfile{
		SchemaVersion: p.SchemaVersion,
		Name:          p.Name,
		Description:   p.Description,
		CreatedAt:     p.CreatedAt,
		UpdatedAt:     p.UpdatedAt,
		Core: jsonAddCore{
			Provider:       p.Core.Provider,
			BaseURL:        p.Core.BaseURL,
			APIKey:         p.Core.APIKey,
			Model:          p.Core.Model,
			SmallFastModel: p.Core.SmallFastModel,
			ExtraEnv:       p.Core.ExtraEnv,
		},
	}
	if len(p.Tools) > 0 {
		out.Tools = map[string]jsonAddOverlay{}
		for id, ov := range p.Tools {
			out.Tools[string(id)] = jsonAddOverlay{
				BaseURL:        ov.BaseURL,
				APIKey:         ov.APIKey,
				Model:          ov.Model,
				SmallFastModel: ov.SmallFastModel,
				ExtraEnv:       ov.ExtraEnv,
				Raw:            ov.Raw,
			}
		}
	}
	return out
}

func writeAddJSON(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

// ---------------------------------------------------------------------------
// Legacy helpers used by other cmd/* files
// ---------------------------------------------------------------------------

// maskToken masks the authentication token for display. Kept here (not
// deleted alongside the pre-E6-S3 command body) because cmd/list.go
// still calls into it for its detailed view — moving it out is scope
// creep for this story.
func maskToken(token string) string {
	if token == "" {
		return "(not set)"
	}
	if len(token) <= 8 {
		return "****"
	}
	return token[:4] + "..." + token[len(token)-4:]
}
