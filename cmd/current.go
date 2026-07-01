// Package cmd — current command (Story E6-S1).
//
// cmd/current is the "quick glance" companion to cmd/explain. Both are
// read-only over profile/tool configs, both consume the same
// resolver.Resolve output, and both apply the same NFR-S8 redaction
// discipline. Where `explain` details the FULL resolution chain for
// every owned key, `current` picks out the highlighted subset an
// operator actually checks before starting a coding session:
//
//	claude_code: Model + Base URL + API Key
//	codex:       Model + Model Provider + API Key
//
// Design pillars:
//
//   - Read-only. Bootstrap creates the ~/.claudecm layout; nothing else
//     writes to disk, env, or Profile. Mirrors cmd/explain wording.
//
//   - Default redaction (NFR-S8). Secret fields render as
//     `<first4>***<last4>` unless --reveal is set, in which case a
//     stderr warning is emitted BEFORE any stdout output so a piped
//     consumer reading only stdout cannot miss the reveal.
//
//   - Compact text form. Header (Profile + optional Description), then
//     per-tool blocks (Presence line, highlighted effective values, and
//     a single-line drift warning when EffectiveView.ExternalDrift* is
//     set). Missing keys render as `(not set)` in text and empty string
//     in JSON so the output is stable across a partially-configured
//     profile.
//
//   - --output json for shell pipelines. Same redaction rules; --reveal
//     also loudens the JSON path.
//
//   - --tool CSV narrowing (resolver.Filter). Symmetric with explain.
//
// Story AC (docs/plan/stories/E6-S1.md): a missing active profile is an
// informational condition, not an error — current prints `no active
// profile` and exits 0.
package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/spf13/cobra"

	"github.com/a2d2-dev/claudecm/internal/adapter"
	// Side-effect imports register the two v1 adapters into
	// adapter.DefaultRegistry from their init() blocks so resolver.Resolve
	// finds them. Symmetric with cmd/explain.go.
	_ "github.com/a2d2-dev/claudecm/internal/adapter/claudecode"
	_ "github.com/a2d2-dev/claudecm/internal/adapter/codex"
	"github.com/a2d2-dev/claudecm/internal/config"
	"github.com/a2d2-dev/claudecm/internal/resolver"
	"github.com/a2d2-dev/claudecm/internal/storage"
)

// currentOutputFormat enumerates the wire formats current can emit.
// Kept parallel to explainOutputFormat so both surfaces stay in lockstep
// if a third format is ever added.
type currentOutputFormat string

const (
	currentOutputText currentOutputFormat = "text"
	currentOutputJSON currentOutputFormat = "json"
)

// noActiveProfileMessage is the single-line informational string the
// text renderer emits when state.CurrentProfile is empty. Story AC
// classifies this as an informational condition, not an error.
const noActiveProfileMessage = "no active profile"

var (
	currentOutputFlag string
	currentRevealFlag bool
	currentToolFlag   string
)

var currentCmd = &cobra.Command{
	Use:   "current",
	Short: "Show a compact per-tool summary of the active profile",
	Long: `Show the active profile and a highlighted per-tool effective summary.

This is the "quick glance" companion to 'claudecm explain'. It lists
the active profile plus the effective values an operator most often
checks before starting a coding session — Base URL, Model, and the
API key (redacted by default; --reveal opts in with a stderr warning).

EXAMPLES
  # Print the active profile summary
  claudecm current

  # Emit machine-readable JSON
  claudecm current --output json

  # Reveal secrets (with warning)
  claudecm current --reveal

  # Restrict to a subset of tools
  claudecm current --tool claude_code`,
	Args: cobra.NoArgs,
	RunE: runCurrent,
}

func init() {
	currentCmd.Flags().StringVarP(&currentOutputFlag, "output", "o", "text", "Output format (text|json)")
	currentCmd.Flags().BoolVar(&currentRevealFlag, "reveal", false, "Reveal secret values in plaintext (prints stderr warning)")
	currentCmd.Flags().StringVar(&currentToolFlag, "tool", "", "Restrict to a comma-separated list of tool IDs (default all)")
	rootCmd.AddCommand(currentCmd)
}

// runCurrent is the RunE callback for `claudecm current`. It resolves
// the active profile (if any) through the same resolver pipeline as
// cmd/explain, then delegates rendering to the text or JSON writer.
func runCurrent(cmd *cobra.Command, args []string) error {
	format, err := parseCurrentOutput(currentOutputFlag)
	if err != nil {
		return err
	}

	resv, err := storage.Default()
	if err != nil {
		return fmt.Errorf("failed to resolve HOME: %w", err)
	}
	if err := storage.Bootstrap(resv); err != nil {
		return fmt.Errorf("failed to bootstrap ~/.claudecm layout: %w", err)
	}
	store := storage.NewFileStorage(resv)
	validator := config.NewValidator()
	mgr := config.NewManager(store, validator)

	activeName, err := mgr.GetActiveName()
	if err != nil {
		return fmt.Errorf("failed to read active profile: %w", err)
	}

	// NFR-S8: --reveal must be loud. Emit the notice BEFORE any stdout
	// output so a piped consumer that reads only stdout does not miss
	// it, and so a user watching the terminal sees the warning next to
	// the plaintext they asked for. Symmetric with cmd/explain.
	if currentRevealFlag {
		fmt.Fprintln(cmd.ErrOrStderr(), "WARNING: --reveal exposes secret values on your terminal and in scrollback.")
	}

	if activeName == "" {
		// Story AC: no active profile is informational (exit 0).
		return renderNoActive(cmd.OutOrStdout(), format)
	}

	profile, err := mgr.GetProfile(activeName)
	if err != nil {
		return fmt.Errorf("active profile %q could not be loaded: %w", activeName, err)
	}

	filter := resolver.Filter{Tools: parseToolFilter(currentToolFlag)}

	view, err := resolver.Resolve(context.Background(), resv, adapter.DefaultRegistry, *profile, filter)
	if err != nil {
		return fmt.Errorf("resolve failed: %w", err)
	}

	switch format {
	case currentOutputJSON:
		return renderCurrentJSON(cmd.OutOrStdout(), view, currentRevealFlag)
	default:
		return renderCurrentText(cmd.OutOrStdout(), view, currentRevealFlag)
	}
}

// parseCurrentOutput validates and normalises the --output flag value.
// Independent of parseExplainOutput because both surfaces evolve on
// their own release cadence and coupling them makes a `current`-only
// format addition needlessly awkward.
func parseCurrentOutput(raw string) (currentOutputFormat, error) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "", "text":
		return currentOutputText, nil
	case "json":
		return currentOutputJSON, nil
	default:
		return "", fmt.Errorf("invalid --output %q (want text|json)", raw)
	}
}

// renderNoActive emits the "no active profile" message in the requested
// format. Text form is a single line; JSON form emits a well-typed
// document with an empty profile name and no tools so a shell pipeline
// consuming --output json never has to special-case a stdout-empty
// branch.
func renderNoActive(w io.Writer, format currentOutputFormat) error {
	if format == currentOutputJSON {
		out := jsonCurrent{Tools: []jsonCurrentTool{}}
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		return enc.Encode(out)
	}
	fmt.Fprintln(w, noActiveProfileMessage)
	return nil
}

// highlightSpec describes one highlighted field of the per-tool compact
// block: the resolver key to pull the value from, a human-readable label
// to render, and whether the field carries a secret. Held in a small
// table (highlightSpecs, keyed by ToolID) so the render loop stays a
// straight table walk instead of a per-tool if-tree.
//
// Text labels are Title-Cased (Model, Base URL, API Key) to match the
// explain header style; JSON keys use snake_case to match Go/JSON idiom
// and the jsonCurrentTool struct tags.
type highlightSpec struct {
	// FieldKey is the EffectiveField.Key the value is pulled from.
	// Matches the flat-key allowlist in each adapter's project.go.
	FieldKey string

	// TextLabel is the label rendered in the compact text output
	// ("Model", "Base URL", "API Key", "Model Provider").
	TextLabel string

	// JSONKey is the field name used in jsonCurrentTool.Effective for
	// this entry.
	JSONKey string

	// Secret is true when the adapter marks this field as a secret
	// (redaction applies unless --reveal). Mirrors the adapter's own
	// Secret flag; carried here so the table-driven renderer does not
	// have to look it up from the EffectiveField, keeping missing
	// (not-yet-projected) fields still redactable at the JSON boundary.
	Secret bool
}

// highlightSpecs maps each v1 tool to its highlighted key subset. The
// order of the slice is the render order in text output; JSON preserves
// the same order in the Effective slice via jsonCurrentTool.Effective.
//
// Rationale for the specific keys:
//   - claude_code: Model (env.ANTHROPIC_MODEL), Base URL
//     (env.ANTHROPIC_BASE_URL), API Key (env.ANTHROPIC_AUTH_TOKEN,
//     falling back to env.ANTHROPIC_API_KEY when the former is unset).
//     The two API key candidates cover both the Auth Token and the
//     legacy API key paths — see internal/adapter/claudecode/project.go.
//   - codex: Model, Model Provider, API Key (OPENAI_API_KEY). Symmetric
//     with the codex adapter's owned-key allowlist.
var highlightSpecs = map[adapter.ToolID][]highlightSpec{
	adapter.ToolClaudeCode: {
		{FieldKey: "env.ANTHROPIC_MODEL", TextLabel: "Model", JSONKey: "model", Secret: false},
		{FieldKey: "env.ANTHROPIC_BASE_URL", TextLabel: "Base URL", JSONKey: "base_url", Secret: false},
		// API Key: sentinel key handled by selectHighlightFields —
		// env.ANTHROPIC_AUTH_TOKEN wins when set, otherwise
		// env.ANTHROPIC_API_KEY. Both are secret.
		{FieldKey: "env.ANTHROPIC_AUTH_TOKEN", TextLabel: "API Key", JSONKey: "api_key", Secret: true},
	},
	adapter.ToolCodex: {
		{FieldKey: "model", TextLabel: "Model", JSONKey: "model", Secret: false},
		{FieldKey: "model_provider", TextLabel: "Model Provider", JSONKey: "model_provider", Secret: false},
		{FieldKey: "OPENAI_API_KEY", TextLabel: "API Key", JSONKey: "api_key", Secret: true},
	},
}

// claudeCodeAPIKeyFallback is the field the claude_code highlight uses
// when env.ANTHROPIC_AUTH_TOKEN is absent. The adapter emits either or
// both depending on which env override / on-disk / core value populated
// it; the compact summary prefers AUTH_TOKEN (the newer path) but falls
// back to API_KEY so a legacy-configured profile still surfaces a value.
const claudeCodeAPIKeyFallback = "env.ANTHROPIC_API_KEY"

// highlightRender is the rendered form of one highlighted field for
// one tool: the label + JSON key from highlightSpec, the raw effective
// value (nil when unset), whether the field is a secret, and the
// display-ready string (already redaction-applied when relevant). The
// intermediate struct keeps text and JSON renderers reading the same
// shape.
type highlightRender struct {
	Label   string
	JSONKey string
	Value   any
	Secret  bool
	// Display is the redaction-applied, missing-normalised string used
	// by the text renderer. JSON uses (Value, Secret, reveal) directly
	// via jsonValue so the wire form keeps native Go types when reveal
	// is on and non-secret.
	Display string
}

// selectHighlightFields walks the highlight table for a given tool view
// and returns one highlightRender per configured highlight. The returned
// slice preserves highlightSpecs order so the text and JSON renderers
// share ordering.
//
// The claude_code API-key fallback: when spec.FieldKey is
// env.ANTHROPIC_AUTH_TOKEN and no field with that key resolved a value,
// selectHighlightFields probes env.ANTHROPIC_API_KEY and uses whichever
// carries a non-nil Value. Absent both, Value is nil and Display is
// "(not set)" — the compact summary never crashes on a
// partially-configured profile.
func selectHighlightFields(tv resolver.ToolView, reveal bool) []highlightRender {
	specs, ok := highlightSpecs[tv.Tool]
	if !ok {
		return nil
	}

	// Index the resolved fields by Key for O(1) lookup during the
	// spec walk. Fields is small (single-digit) so map alloc is
	// cheaper than a repeated linear scan for readability.
	byKey := make(map[string]adapter.EffectiveField, len(tv.Effective.Fields))
	for _, f := range tv.Effective.Fields {
		byKey[f.Key] = f
	}

	out := make([]highlightRender, 0, len(specs))
	for _, spec := range specs {
		render := highlightRender{
			Label:   spec.TextLabel,
			JSONKey: spec.JSONKey,
			Secret:  spec.Secret,
		}

		if f, ok := byKey[spec.FieldKey]; ok && f.Value != nil {
			render.Value = f.Value
			// The adapter's Secret flag is authoritative. Prefer it
			// over the highlightSpec.Secret hint so an adapter change
			// (e.g. flipping a field's secret classification) is
			// picked up without a table update here.
			render.Secret = f.Secret
		} else if spec.FieldKey == "env.ANTHROPIC_AUTH_TOKEN" {
			// claude_code API-key fallback path.
			if f, ok := byKey[claudeCodeAPIKeyFallback]; ok && f.Value != nil {
				render.Value = f.Value
				render.Secret = f.Secret
			}
		}

		render.Display = displayHighlight(render.Value, render.Secret, reveal)
		out = append(out, render)
	}
	return out
}

// displayHighlight formats one highlighted value for the text renderer:
// missing values become "(not set)"; secret values pass through the
// shared redactValue helper unless reveal is on; everything else
// stringifies via fmt.Sprint.
func displayHighlight(v any, secret, reveal bool) string {
	if v == nil {
		return "(not set)"
	}
	if secret && !reveal {
		return redactValue(v)
	}
	return fmt.Sprint(v)
}

// renderCurrentText prints the compact text summary to w. Deterministic
// ordering: profile header, then per-tool blocks in resolver.View.Tools
// order (already sorted). Missing tools produce no block; an empty
// View.Tools produces "(no tools resolved)" so operators can tell an
// empty registry from a filter mismatch.
func renderCurrentText(w io.Writer, view resolver.View, reveal bool) error {
	fmt.Fprintf(w, "Profile: %s\n", view.Profile.Name)
	if view.Profile.Description != "" {
		fmt.Fprintf(w, "Description: %s\n", view.Profile.Description)
	}
	fmt.Fprintln(w)

	if len(view.Tools) == 0 {
		fmt.Fprintln(w, "(no tools resolved)")
		return nil
	}

	for i, tv := range view.Tools {
		renderCurrentToolText(w, tv, reveal)
		// Blank line between tool blocks for readability. Skip after
		// the final block so trailing whitespace does not accumulate.
		if i != len(view.Tools)-1 {
			fmt.Fprintln(w)
		}
	}
	return nil
}

// renderCurrentToolText prints one compact tool block:
//
//	<tool>:
//	  Presence: Installed=<bool> ConfigDir=<path>
//	  <Label>: <display>
//	  ...
//	  Drift: <file> edited externally since last apply
func renderCurrentToolText(w io.Writer, tv resolver.ToolView, reveal bool) {
	fmt.Fprintf(w, "%s:\n", tv.Tool)
	fmt.Fprintf(w, "  Presence: Installed=%v ConfigDir=%s\n",
		tv.Presence.Installed,
		orDash(tv.Presence.ConfigDir),
	)

	// A tool that is not installed still gets a Presence line but no
	// effective-value block — the operator cares that the tool is
	// missing, not that its Model would have been "opus" hypothetically.
	if !tv.Presence.Installed {
		return
	}

	for _, h := range selectHighlightFields(tv, reveal) {
		fmt.Fprintf(w, "  %s: %s\n", h.Label, h.Display)
	}

	if tv.Effective.ExternalDriftDetected {
		for _, file := range tv.Effective.ExternalDriftFiles {
			fmt.Fprintf(w, "  Drift: %s has been externally edited since last apply.\n", file)
		}
	}
}

// jsonCurrentProfile is the JSON header block. Same shape as
// jsonProfile in explain.go, kept as its own type so the current JSON
// surface can evolve without a coordinated explain change.
type jsonCurrentProfile struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
}

// jsonCurrentPresence is the compact presence subset — Installed +
// ConfigDir only. `explain --output json` carries the fuller shape
// (Detected, Files, Version, Notes); `current` deliberately omits them.
type jsonCurrentPresence struct {
	Installed bool   `json:"installed"`
	ConfigDir string `json:"config_dir,omitempty"`
}

// jsonCurrentEffective is one highlighted key: JSON key + the raw
// (possibly redacted) value. Slice-shaped in jsonCurrentTool so
// ordering is a property of the wire, matching text output.
type jsonCurrentEffective struct {
	Key   string `json:"key"`
	Value any    `json:"value"`
}

// jsonCurrentDrift mirrors jsonDrift from explain.go.
type jsonCurrentDrift struct {
	Detected bool     `json:"detected"`
	Files    []string `json:"files,omitempty"`
}

// jsonCurrentTool is one tool block in the JSON wire form.
//
// Effective is a slice of {key, value} rather than a flat map so
// ordering is preserved on the wire — a JSON.Marshal over map[string]any
// re-sorts alphabetically, which would silently swap the intended
// (model, base_url, api_key) presentation order.
type jsonCurrentTool struct {
	ID        string                 `json:"id"`
	Presence  jsonCurrentPresence    `json:"presence"`
	Effective []jsonCurrentEffective `json:"effective"`
	Drift     jsonCurrentDrift       `json:"drift"`
}

// jsonCurrent is the top-level JSON document.
type jsonCurrent struct {
	Profile jsonCurrentProfile `json:"profile"`
	Tools   []jsonCurrentTool  `json:"tools"`
}

// renderCurrentJSON emits the JSON wire form. Redaction is applied via
// jsonValue (the shared helper in explain.go) on secret fields unless
// --reveal.
func renderCurrentJSON(w io.Writer, view resolver.View, reveal bool) error {
	out := jsonCurrent{
		Profile: jsonCurrentProfile{
			Name:        view.Profile.Name,
			Description: view.Profile.Description,
		},
		Tools: make([]jsonCurrentTool, 0, len(view.Tools)),
	}
	for _, tv := range view.Tools {
		jt := jsonCurrentTool{
			ID: string(tv.Tool),
			Presence: jsonCurrentPresence{
				Installed: tv.Presence.Installed,
				ConfigDir: tv.Presence.ConfigDir,
			},
			Drift: jsonCurrentDrift{
				Detected: tv.Effective.ExternalDriftDetected,
				Files:    tv.Effective.ExternalDriftFiles,
			},
			Effective: make([]jsonCurrentEffective, 0),
		}
		if tv.Presence.Installed {
			for _, h := range selectHighlightFields(tv, reveal) {
				jt.Effective = append(jt.Effective, jsonCurrentEffective{
					Key:   h.JSONKey,
					Value: jsonCurrentValue(h.Value, h.Secret, reveal),
				})
			}
		}
		out.Tools = append(out.Tools, jt)
	}
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(out)
}

// jsonCurrentValue is the JSON-side counterpart of displayHighlight:
// missing values become "" (empty string) so the JSON key is always
// present and the shell consumer never has to distinguish null-vs-absent;
// secret values pass through redactValue unless reveal is on; everything
// else preserves the underlying Go type via jsonValue (from explain.go).
func jsonCurrentValue(v any, secret, reveal bool) any {
	if v == nil {
		return ""
	}
	return jsonValue(v, secret, reveal)
}
