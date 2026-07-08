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
	"github.com/a2d2-dev/claudecm/internal/envextract"
	"github.com/a2d2-dev/claudecm/internal/fileparse"
	"github.com/a2d2-dev/claudecm/internal/presets"
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
	"deepseek":      {},
	"glm":           {},
	"moonshot":      {},
	"openai-compat": {},
	"qwen":          {},
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
	addPresetFlag         string
	addFromEnvFlag        bool
	addFromFileFlag       string
	addListPresetsFlag    bool
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

Provider presets are convenience templates only. --preset <name> fills
base_url, model, provider, and supported tool overlays from the built-in
catalog; users still supply secrets, every generated value can be
overridden by an explicit flag or --set, and presets are not official
provider support, certification, endorsement, or compatibility guarantees.

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

  # Start from a built-in provider preset; generated fields are visible
  # in dry-run output and can be overridden.
  claudecm add work --preset moonshot --api-key sk-... --dry-run
  claudecm add work --preset moonshot --api-key sk-... --model kimi-k2-latest

  # Discover built-in presets
  claudecm add --list-presets

  # Preview only — no writes
  claudecm add work --base-url ... --api-key ... --dry-run

  # Replace an existing profile
  claudecm add work --base-url ... --api-key ... --overwrite

add does NOT auto-activate the new profile. Use 'claudecm switch <name>'
to make it the active profile.`,
	Args: func(cmd *cobra.Command, args []string) error {
		if addListPresetsFlag {
			return cobra.NoArgs(cmd, args)
		}
		return cobra.ExactArgs(1)(cmd, args)
	},
	RunE: runAdd,
}

func init() {
	addCmd.Flags().StringVar(&addDescriptionFlag, "description", "", "Optional human-readable description")
	addCmd.Flags().StringVar(&addProviderFlag, "provider", addProviderDefault, "Provider tag: anthropic|openai-compat|custom")
	addCmd.Flags().StringVar(&addBaseURLFlag, "base-url", "", "Core base URL")
	addCmd.Flags().StringVar(&addAPIKeyFlag, "api-key", "", "Core API key")
	addCmd.Flags().StringVar(&addModelFlag, "model", "", "Core model name")
	addCmd.Flags().StringVar(&addSmallFastModelFlag, "small-fast-model", "", "Core small/fast auxiliary model name")
	addCmd.Flags().StringVar(&addPresetFlag, "preset", "", "Built-in provider preset name (run --list-presets to discover)")
	addCmd.Flags().BoolVar(&addFromEnvFlag, "from-env", false, "Build the profile draft from Claude Code / Codex environment variables")
	addCmd.Flags().StringVar(&addFromFileFlag, "from-file", "", "Build the profile draft from a dotenv, shell, JSON, YAML, or TOML file")
	addCmd.Flags().BoolVar(&addListPresetsFlag, "list-presets", false, "List built-in provider presets and exit")
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
	if addListPresetsFlag {
		format, err := parseAddOutput(addOutputFlag)
		if err != nil {
			return err
		}
		return renderPresetList(cmd.OutOrStdout(), format)
	}

	name := strings.TrimSpace(args[0])
	if err := storage.ValidateProfileName(name); err != nil {
		return err
	}

	format, err := parseAddOutput(addOutputFlag)
	if err != nil {
		return err
	}

	providerFlagSet := flagWasExplicit(cmd, "provider", addProviderFlag != addProviderDefault)
	baseURLFlagSet := flagWasExplicit(cmd, "base-url", addBaseURLFlag != "")
	apiKeyFlagSet := flagWasExplicit(cmd, "api-key", addAPIKeyFlag != "")
	modelFlagSet := flagWasExplicit(cmd, "model", addModelFlag != "")
	smallFastModelFlagSet := flagWasExplicit(cmd, "small-fast-model", addSmallFastModelFlag != "")

	preset, hasPreset, err := resolveAddPreset(addPresetFlag)
	if err != nil {
		return err
	}
	if err := validateAddInputSources(hasPreset); err != nil {
		return err
	}
	fromInputSource := addFromEnvFlag || strings.TrimSpace(addFromFileFlag) != ""
	if fromInputSource {
		providerFlagSet = flagWasExplicit(cmd, "provider", false)
		baseURLFlagSet = flagWasExplicit(cmd, "base-url", false)
		apiKeyFlagSet = flagWasExplicit(cmd, "api-key", false)
		modelFlagSet = flagWasExplicit(cmd, "model", false)
		smallFastModelFlagSet = flagWasExplicit(cmd, "small-fast-model", false)
	}

	provider := addProviderFlag
	baseURL := addBaseURLFlag
	apiKey := addAPIKeyFlag
	model := addModelFlag
	smallFastModel := addSmallFastModelFlag
	var tools map[config.ToolID]config.ToolOverlay
	if hasPreset {
		provider = preset.ProviderKey
		baseURL = preset.BaseURL
		model = preset.Model
		tools = cloneToolMap(preset.Tools)
	}
	if addFromEnvFlag {
		core, envTools, err := profileDraftFromEnv()
		if err != nil {
			return err
		}
		if core.Provider != "" {
			provider = core.Provider
		}
		baseURL = core.BaseURL
		apiKey = core.APIKey
		model = core.Model
		smallFastModel = core.SmallFastModel
		tools = mergeToolMaps(tools, envTools)
	}
	if strings.TrimSpace(addFromFileFlag) != "" {
		core, err := fileparse.ParseProfileCoreFile(addFromFileFlag)
		if err != nil {
			return err
		}
		if core.Provider != "" {
			provider = core.Provider
		}
		baseURL = core.BaseURL
		apiKey = core.APIKey
		model = core.Model
		smallFastModel = core.SmallFastModel
	}
	if providerFlagSet {
		provider = addProviderFlag
	}
	if baseURLFlagSet {
		baseURL = addBaseURLFlag
	}
	if apiKeyFlagSet {
		apiKey = addAPIKeyFlag
	}
	if modelFlagSet {
		model = addModelFlag
	}
	if smallFastModelFlagSet {
		smallFastModel = addSmallFastModelFlag
	}
	if hasPreset {
		applyExplicitPresetFlagOverrides(tools, preset.Name, provider, baseURL, model, providerFlagSet, baseURLFlagSet, modelFlagSet)
	}

	if err := validateProvider(provider); err != nil {
		return err
	}
	if hasPreset && addAPIKeyFlag == "" {
		return fmt.Errorf("preset %q requires --api-key in non-interactive add", preset.Name)
	}
	if (addFromEnvFlag || strings.TrimSpace(addFromFileFlag) != "") && strings.TrimSpace(apiKey) == "" {
		return fmt.Errorf("no API key found in input source")
	}

	// Build tools overlay from --set entries. Parsing is a pure
	// function so an invalid entry surfaces before any I/O.
	setTools, err := parseSetEntries(addSetFlag)
	if err != nil {
		return err
	}
	tools = mergeToolMaps(tools, setTools)

	now := nowFn().UTC()
	profile := &config.Profile{
		SchemaVersion: config.CurrentProfileSchemaVersion,
		Name:          name,
		Description:   addDescriptionFlag,
		CreatedAt:     now,
		UpdatedAt:     now,
		Core: config.CoreConfig{
			Provider:       provider,
			BaseURL:        baseURL,
			APIKey:         apiKey,
			Model:          model,
			SmallFastModel: smallFastModel,
		},
		Tools: tools,
	}

	// Bootstrap is required whether or not the write path fires: --dry-run
	// still needs a Resolver to exist (and, for parity with every other
	// command, we do not want a machine without ~/.claudecm to succeed
	// silently and then fail later on the first non-dry-run add).
	resv, err := resolverFromGlobals()
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

func resolveAddPreset(raw string) (presets.Preset, bool, error) {
	if strings.TrimSpace(raw) == "" {
		return presets.Preset{}, false, nil
	}
	p, err := presets.Lookup(raw)
	if err != nil {
		return presets.Preset{}, false, err
	}
	return p, true, nil
}

func validateAddInputSources(hasPreset bool) error {
	fromFileSet := strings.TrimSpace(addFromFileFlag) != ""
	count := 0
	if hasPreset {
		count++
	}
	if addFromEnvFlag {
		count++
	}
	if fromFileSet {
		count++
	}
	if count > 1 {
		return fmt.Errorf("choose only one add input source: --preset, --from-env, or --from-file")
	}
	return nil
}

func flagWasExplicit(cmd *cobra.Command, name string, fallback bool) bool {
	if cmd != nil && cmd.Flags() != nil {
		if f := cmd.Flags().Lookup(name); f != nil && f.Changed {
			return true
		}
	}
	return fallback
}

func profileDraftFromEnv() (config.CoreConfig, map[config.ToolID]config.ToolOverlay, error) {
	var core config.CoreConfig
	var tools map[config.ToolID]config.ToolOverlay

	core.Provider = addProviderDefault
	if v := lookupNonEmptyEnv("ANTHROPIC_BASE_URL"); v != "" {
		core.BaseURL = v
	}
	if v := lookupNonEmptyEnv("ANTHROPIC_AUTH_TOKEN"); v != "" {
		core.APIKey = v
	}
	if v := lookupNonEmptyEnv("ANTHROPIC_API_KEY"); v != "" {
		if core.APIKey == "" {
			core.APIKey = v
		} else {
			tools = putClaudeCodeEnv(tools, "ANTHROPIC_API_KEY", v)
		}
	}
	if v := lookupNonEmptyEnv("ANTHROPIC_MODEL"); v != "" {
		core.Model = v
	}
	if v := lookupNonEmptyEnv("ANTHROPIC_SMALL_FAST_MODEL"); v != "" {
		core.SmallFastModel = v
	}

	codexKey := lookupNonEmptyEnv("OPENAI_API_KEY")
	codexBaseURL := lookupNonEmptyEnv("OPENAI_BASE_URL")
	codexModel := lookupNonEmptyEnv("CODEX_MODEL")
	codexProvider := normalizeCodexProvider(lookupNonEmptyEnv("CODEX_MODEL_PROVIDER"))
	if core.APIKey == "" && codexKey != "" {
		core.APIKey = codexKey
	}
	if core.BaseURL == "" && codexBaseURL != "" {
		core.BaseURL = codexBaseURL
	}
	if core.Model == "" && codexModel != "" {
		core.Model = codexModel
	}
	if codexProvider != "" {
		core.Provider = codexProvider
	}

	return core, tools, nil
}

func lookupNonEmptyEnv(name string) string {
	v, ok := envextract.Lookup(name)
	if !ok {
		return ""
	}
	return strings.TrimSpace(v)
}

func normalizeCodexProvider(provider string) string {
	switch strings.TrimSpace(provider) {
	case "":
		return ""
	case "openai":
		return "openai-compat"
	default:
		return provider
	}
}

func putClaudeCodeEnv(
	tools map[config.ToolID]config.ToolOverlay,
	name, value string,
) map[config.ToolID]config.ToolOverlay {
	if tools == nil {
		tools = map[config.ToolID]config.ToolOverlay{}
	}
	ov := tools[config.ToolClaudeCode]
	if ov.ExtraEnv == nil {
		ov.ExtraEnv = map[string]string{}
	}
	ov.ExtraEnv[name] = value
	tools[config.ToolClaudeCode] = ov
	return tools
}

func applyExplicitPresetFlagOverrides(
	tools map[config.ToolID]config.ToolOverlay,
	presetName, provider, baseURL, model string,
	providerFlagSet, baseURLFlagSet, modelFlagSet bool,
) {
	if tools == nil {
		return
	}
	ov, ok := tools[config.ToolCodex]
	if !ok || ov.Raw == nil {
		return
	}
	if providerFlagSet {
		ov.Raw["model_provider"] = provider
	}
	if modelFlagSet {
		ov.Raw["model"] = model
	}
	if baseURLFlagSet {
		ov.Raw["model_providers."+presetName+".base_url"] = baseURL
	}
	tools[config.ToolCodex] = ov
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

func mergeToolMaps(base, override map[config.ToolID]config.ToolOverlay) map[config.ToolID]config.ToolOverlay {
	out := cloneToolMap(base)
	if len(override) == 0 {
		return out
	}
	if out == nil {
		out = map[config.ToolID]config.ToolOverlay{}
	}
	for id, ov := range override {
		out[id] = mergeOverlay(out[id], ov)
	}
	return out
}

func mergeOverlay(base, override config.ToolOverlay) config.ToolOverlay {
	out := cloneOverlay(base)
	if override.BaseURL != "" {
		out.BaseURL = override.BaseURL
	}
	if override.APIKey != "" {
		out.APIKey = override.APIKey
	}
	if override.Model != "" {
		out.Model = override.Model
	}
	if override.SmallFastModel != "" {
		out.SmallFastModel = override.SmallFastModel
	}
	if len(override.ExtraEnv) > 0 {
		if out.ExtraEnv == nil {
			out.ExtraEnv = map[string]string{}
		}
		for k, v := range override.ExtraEnv {
			out.ExtraEnv[k] = v
		}
	}
	if len(override.Raw) > 0 {
		if out.Raw == nil {
			out.Raw = map[string]any{}
		}
		for k, v := range override.Raw {
			out.Raw[k] = v
		}
	}
	return out
}

func cloneToolMap(in map[config.ToolID]config.ToolOverlay) map[config.ToolID]config.ToolOverlay {
	if len(in) == 0 {
		return nil
	}
	out := make(map[config.ToolID]config.ToolOverlay, len(in))
	for id, ov := range in {
		out[id] = cloneOverlay(ov)
	}
	return out
}

func cloneOverlay(ov config.ToolOverlay) config.ToolOverlay {
	out := config.ToolOverlay{
		BaseURL:        ov.BaseURL,
		APIKey:         ov.APIKey,
		Model:          ov.Model,
		SmallFastModel: ov.SmallFastModel,
	}
	if ov.ExtraEnv != nil {
		out.ExtraEnv = make(map[string]string, len(ov.ExtraEnv))
		for k, v := range ov.ExtraEnv {
			out.ExtraEnv[k] = v
		}
	}
	if ov.Raw != nil {
		out.Raw = make(map[string]any, len(ov.Raw))
		for k, v := range ov.Raw {
			out.Raw[k] = v
		}
	}
	return out
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
	redacted := redactProfileForAddOutput(profile)
	body, err := config.MarshalProfile(redacted)
	if err != nil {
		return fmt.Errorf("marshal profile for dry-run: %w", err)
	}
	if format == addOutputJSON {
		out := jsonAddDryRun{
			Action:  "dry-run",
			Profile: profileToJSON(redacted),
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
			Profile: profileToJSON(redactProfileForAddOutput(profile)),
		}
		return writeAddJSON(w, out)
	}
	fmt.Fprintf(w, "Profile %q created.\n", profile.Name)
	return nil
}

func renderPresetList(w io.Writer, format addOutputFormat) error {
	switch format {
	case addOutputJSON:
		out := jsonAddPresetList{
			Action:  "list-presets",
			Presets: presetListToJSON(),
		}
		return writeAddJSON(w, out)
	default:
		fmt.Fprintln(w, "Built-in provider presets (convenience templates only; not official provider support or endorsement):")
		for _, p := range presets.Catalog {
			fmt.Fprintf(w, "  %s\t%s\tbase_url=%s\tmodel=%s\tprovider=%s\n",
				p.Name, p.DisplayName, p.BaseURL, p.Model, p.ProviderKey)
		}
		return nil
	}
}

func redactProfileForAddOutput(profile *config.Profile) *config.Profile {
	if profile == nil {
		return nil
	}
	out := profile.Clone()
	out.Core.APIKey = redactValue(out.Core.APIKey)
	for id, ov := range out.Tools {
		if ov.APIKey != "" {
			ov.APIKey = redactValue(ov.APIKey)
		}
		for k, v := range ov.ExtraEnv {
			if isSecretKey(k) {
				ov.ExtraEnv[k] = redactValue(v)
			}
		}
		for k, v := range ov.Raw {
			if s, ok := v.(string); ok && isSecretKey(k) {
				ov.Raw[k] = redactValue(s)
			}
		}
		out.Tools[id] = ov
	}
	for k, v := range out.Core.ExtraEnv {
		if isSecretKey(k) {
			out.Core.ExtraEnv[k] = redactValue(v)
		}
	}
	return out
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

type jsonAddPreset struct {
	Name        string   `json:"name"`
	DisplayName string   `json:"display_name"`
	ProviderKey string   `json:"provider_key"`
	BaseURL     string   `json:"base_url"`
	Model       string   `json:"model"`
	Tools       []string `json:"tools"`
	Secrets     []string `json:"expected_secret_fields"`
	Disclaimer  string   `json:"disclaimer"`
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

type jsonAddPresetList struct {
	Action  string          `json:"action"`
	Presets []jsonAddPreset `json:"presets"`
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

func presetListToJSON() []jsonAddPreset {
	out := make([]jsonAddPreset, 0, len(presets.Catalog))
	for _, p := range presets.Catalog {
		tools := make([]string, 0, len(p.Tools))
		for id := range p.Tools {
			tools = append(tools, string(id))
		}
		sort.Strings(tools)
		secrets := make([]string, 0, len(p.Secrets))
		for _, s := range p.Secrets {
			secrets = append(secrets, s.Name)
		}
		out = append(out, jsonAddPreset{
			Name:        p.Name,
			DisplayName: p.DisplayName,
			ProviderKey: p.ProviderKey,
			BaseURL:     p.BaseURL,
			Model:       p.Model,
			Tools:       tools,
			Secrets:     secrets,
			Disclaimer:  p.Disclaimer,
		})
	}
	return out
}

func writeAddJSON(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

// maskToken was retained here until the E6-S10 cmd/list rewrite migrated
// list off the legacy detailed view. The E6-S10 refactor uses redactValue
// (cmd/explain.go) uniformly, so maskToken has no callers and is
// removed to keep the linter's "unused" gate honest. Any future need
// for a "…" style mask should route through redactValue rather than
// resurrect a competing helper.
