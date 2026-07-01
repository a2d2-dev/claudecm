// Package cmd — explain command (Story E5-S5).
//
// cmd/explain is the first user-visible command that consumes the resolver
// (internal/resolver). It renders, per registered tool adapter, the full
// resolution chain for every owned key that any layer contributed to:
//
//	WinningLayer + Source + Value  (top of the precedence chain)
//	Shadowed[]                    (older→newer entries that lost)
//
// Read-only over profiles, tool configs, and state. Ensures the
// ~/.claudecm/ layout exists (via Bootstrap) but never mutates profile
// YAML, tool config files, or state.yaml. Every adapter's Project method
// is pure over its inputs — explain is a thin renderer.
//
// Redaction. Fields with Secret=true (adapters set this per architecture §6
// and PRD NFR-S8) are redacted to `<first4>***<last4>` unless the operator
// opts into plaintext with --reveal, which additionally emits a stderr
// warning so a hidden --reveal on a shared terminal is impossible to miss.
//
// --output json emits a machine-readable form for shell pipelines. The
// same redaction rules apply — a `--reveal` in JSON mode still requires
// the operator to opt in on the command line.
//
// --tool restricts the resolve to a comma-separated list of tool IDs
// (via resolver.Filter).
//
// --all-env appends a diagnostic section listing extant process env vars
// whose names match a per-tool prefix (ANTHROPIC_, CLAUDE_CODE_, OPENAI_,
// CODEX_) but are NOT one of the adapters' owned env-var names (those
// already surface as the EnvOverride layer). This section is purely
// diagnostic; it never alters layer resolution.
//
// External drift (E5-S4) surfaces prominently — a single-line banner in
// text output and a structured `external_drift` object in JSON.
package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"

	"github.com/spf13/cobra"

	"github.com/a2d2-dev/claudecm/internal/adapter"
	// Side-effect imports register the two v1 adapters into
	// adapter.DefaultRegistry from their init() blocks so resolver.Resolve
	// finds them. Symmetric with internal/resolver/resolver_test.go.
	_ "github.com/a2d2-dev/claudecm/internal/adapter/claudecode"
	_ "github.com/a2d2-dev/claudecm/internal/adapter/codex"
	"github.com/a2d2-dev/claudecm/internal/config"
	"github.com/a2d2-dev/claudecm/internal/resolver"
	"github.com/a2d2-dev/claudecm/internal/storage"
)

// explainOutputFormat enumerates the wire formats explain can emit.
type explainOutputFormat string

const (
	explainOutputText explainOutputFormat = "text"
	explainOutputJSON explainOutputFormat = "json"
)

// diagnosticEnvPrefixes are the per-tool env-var name prefixes explain
// scans process env for under --all-env. Kept local (not shared with the
// adapters) because this is a purely diagnostic filter — the actual
// EnvOverride layer is driven by each adapter's own frozen allowlist.
var diagnosticEnvPrefixes = []string{
	"ANTHROPIC_",
	"CLAUDE_CODE_",
	"OPENAI_",
	"CODEX_",
}

var (
	explainOutputFlag string
	explainRevealFlag bool
	explainToolFlag   string
	explainAllEnvFlag bool
)

var explainCmd = &cobra.Command{
	Use:   "explain [profile-name]",
	Short: "Show the full resolution chain (winning + shadowed layers) per tool",
	Long: `Show the layered configuration resolution for each supported tool.

For every owned key that any layer contributes a value to, prints the
winning layer plus every shadowed layer with its source. Default
redaction hides API keys and auth tokens; --reveal opts in and emits a
stderr warning.

EXAMPLES
  # Explain the active profile
  claudecm explain

  # Explain a named profile
  claudecm explain prod

  # Emit machine-readable JSON
  claudecm explain --output json

  # Reveal secrets (with warning)
  claudecm explain --reveal

  # Restrict to a subset of tools
  claudecm explain --tool claude_code

  # Include a diagnostic dump of matching env vars
  claudecm explain --all-env`,
	Args:              cobra.MaximumNArgs(1),
	ValidArgsFunction: profileNamesCompletion,
	RunE:              runExplain,
}

func init() {
	explainCmd.Flags().StringVarP(&explainOutputFlag, "output", "o", "text", "Output format (text|json)")
	explainCmd.Flags().BoolVar(&explainRevealFlag, "reveal", false, "Reveal secret values in plaintext (prints stderr warning)")
	explainCmd.Flags().StringVar(&explainToolFlag, "tool", "", "Restrict to a comma-separated list of tool IDs (default all)")
	explainCmd.Flags().BoolVar(&explainAllEnvFlag, "all-env", false, "Also list extant process env vars matching per-tool prefixes")
	rootCmd.AddCommand(explainCmd)
}

func runExplain(cmd *cobra.Command, args []string) error {
	format, err := parseExplainOutput(explainOutputFlag)
	if err != nil {
		return err
	}

	// Build storage + manager (same bootstrap pattern as other cmd/* entries).
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

	profile, err := selectExplainProfile(mgr, args)
	if err != nil {
		return err
	}

	filter := resolver.Filter{Tools: parseToolFilter(explainToolFlag)}

	view, err := resolver.Resolve(context.Background(), resv, adapter.DefaultRegistry, *profile, filter)
	if err != nil {
		return fmt.Errorf("resolve failed: %w", err)
	}

	// NFR-S8: --reveal must be loud. Emit the notice BEFORE any stdout
	// output so a piped consumer that only reads stdout does not miss it,
	// and so a user watching the terminal sees the warning next to the
	// plaintext they asked for.
	if explainRevealFlag {
		fmt.Fprintln(cmd.ErrOrStderr(), "WARNING: --reveal exposes secret values on your terminal and in scrollback.")
	}

	var diagnostic map[string]string
	if explainAllEnvFlag {
		diagnostic = collectDiagnosticEnv(os.Environ())
	}

	switch format {
	case explainOutputJSON:
		return renderExplainJSON(cmd.OutOrStdout(), view, diagnostic, explainRevealFlag)
	default:
		return renderExplainText(cmd.OutOrStdout(), view, diagnostic, explainRevealFlag)
	}
}

// parseExplainOutput validates and normalises the --output flag value.
func parseExplainOutput(raw string) (explainOutputFormat, error) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "", "text":
		return explainOutputText, nil
	case "json":
		return explainOutputJSON, nil
	default:
		return "", fmt.Errorf("invalid --output %q (want text|json)", raw)
	}
}

// parseToolFilter splits a comma-separated tool ID list into a
// []adapter.ToolID. Empty input → nil (filter allows everything).
// Whitespace around each entry is trimmed.
func parseToolFilter(raw string) []adapter.ToolID {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	parts := strings.Split(raw, ",")
	out := make([]adapter.ToolID, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		out = append(out, adapter.ToolID(p))
	}
	return out
}

// selectExplainProfile resolves the profile explain should render. Named
// args[0] wins; otherwise the active profile from state is used. Errors
// carry a clear "how to fix" suffix so operators do not have to read
// the source to recover.
func selectExplainProfile(mgr *config.Manager, args []string) (*config.Profile, error) {
	if len(args) > 0 {
		name := strings.TrimSpace(args[0])
		if name == "" {
			return nil, fmt.Errorf("profile name cannot be empty")
		}
		p, err := mgr.GetProfile(name)
		if err != nil {
			return nil, fmt.Errorf("profile %q not found: %w", name, err)
		}
		return p, nil
	}
	activeName, err := mgr.GetActiveName()
	if err != nil {
		return nil, fmt.Errorf("failed to read active profile: %w", err)
	}
	if activeName == "" {
		return nil, fmt.Errorf("no active profile; specify a profile name or run 'claudecm switch' first")
	}
	p, err := mgr.GetProfile(activeName)
	if err != nil {
		return nil, fmt.Errorf("active profile %q could not be loaded: %w", activeName, err)
	}
	return p, nil
}

// collectDiagnosticEnv scans a snapshot of os.Environ()-shaped strings
// and returns the subset whose NAME matches one of the per-tool
// diagnostic prefixes AND is not one of the owned env-var names any
// adapter would already surface as an EnvOverride layer. The returned
// map is name→value; the caller decides how to render it. Empty when
// no matching var is present.
//
// The env universe is passed in (rather than read inline) so tests can
// hand explain a synthetic snapshot without touching the process env.
func collectDiagnosticEnv(env []string) map[string]string {
	owned := ownedEnvVarNames()
	out := map[string]string{}
	for _, entry := range env {
		eq := strings.IndexByte(entry, '=')
		if eq <= 0 {
			continue
		}
		name := entry[:eq]
		value := entry[eq+1:]
		if !hasDiagnosticPrefix(name) {
			continue
		}
		if owned[name] {
			continue
		}
		out[name] = value
	}
	return out
}

// hasDiagnosticPrefix returns true when name starts with one of the
// per-tool diagnostic prefixes.
func hasDiagnosticPrefix(name string) bool {
	for _, p := range diagnosticEnvPrefixes {
		if strings.HasPrefix(name, p) {
			return true
		}
	}
	return false
}

// ownedEnvVarNames enumerates every env-var name any registered adapter
// projects as an owned key. Anything in this set is EXCLUDED from the
// --all-env diagnostic dump because it already flows through the
// EnvOverride layer of the corresponding tool's EffectiveView.
//
// Hardcoded here to avoid growing a new cross-cutting adapter API just
// for the diagnostic filter. Matches the two adapters' envVarForOwnedKey
// tables verbatim (project.go in each adapter). A future adapter must
// extend this table alongside its owned-key allowlist.
func ownedEnvVarNames() map[string]bool {
	return map[string]bool{
		// Claude Code adapter (internal/adapter/claudecode/project.go).
		"ANTHROPIC_API_KEY":          true,
		"ANTHROPIC_AUTH_TOKEN":       true,
		"ANTHROPIC_BASE_URL":         true,
		"ANTHROPIC_MODEL":            true,
		"ANTHROPIC_SMALL_FAST_MODEL": true,
		"CLAUDE_CODE_USE_BEDROCK":    true,
		"CLAUDE_CODE_USE_VERTEX":     true,
		// Codex adapter (internal/adapter/codex/project.go).
		"OPENAI_API_KEY":       true,
		"OPENAI_BASE_URL":      true,
		"CODEX_MODEL":          true,
		"CODEX_MODEL_PROVIDER": true,
	}
}

// redactValue applies the default redaction shape:
//
//	value length >= 8 → first4 + "***" + last4
//	shorter          → "***"
//
// Used for every Value/ShadowedLayer.Value whose Secret flag is true and
// --reveal is off. Non-string values are stringified via fmt.Sprint so a
// bool/number-typed secret still renders in redacted form.
func redactValue(v any) string {
	if v == nil {
		return "***"
	}
	s := fmt.Sprint(v)
	if len(s) >= 8 {
		return s[:4] + "***" + s[len(s)-4:]
	}
	return "***"
}

// formatValueForDisplay stringifies a Value/ShadowedLayer.Value for the
// text renderer, applying redaction when the field is secret and reveal
// is off.
func formatValueForDisplay(v any, secret, reveal bool) string {
	if secret && !reveal {
		return redactValue(v)
	}
	if v == nil {
		return ""
	}
	return fmt.Sprint(v)
}

// renderExplainText prints the text view to w. Deterministic ordering:
// tools iterate in the order Resolve returned (Registry lexicographic);
// fields iterate in the order Project + adapter.SortFields emitted
// (lexicographic by Key); shadowed layers iterate in older→newer
// precedence.
func renderExplainText(w io.Writer, view resolver.View, diagnostic map[string]string, reveal bool) error {
	fmt.Fprintf(w, "Profile: %s\n", view.Profile.Name)
	if view.Profile.Description != "" {
		fmt.Fprintf(w, "  Description: %s\n", view.Profile.Description)
	}
	fmt.Fprintln(w)

	if len(view.Tools) == 0 {
		fmt.Fprintln(w, "  (no tools resolved)")
	}
	for _, tv := range view.Tools {
		renderToolText(w, tv, reveal)
	}

	if len(diagnostic) > 0 {
		fmt.Fprintln(w, "Diagnostic env vars (not shadowing any layer):")
		names := make([]string, 0, len(diagnostic))
		for k := range diagnostic {
			names = append(names, k)
		}
		sort.Strings(names)
		for _, name := range names {
			fmt.Fprintf(w, "  - %s = %s\n", name, diagnostic[name])
		}
		fmt.Fprintln(w)
	}
	return nil
}

// renderToolText renders one ToolView section: presence line, per-field
// resolution chain, external-drift banner, and per-tool errors.
func renderToolText(w io.Writer, tv resolver.ToolView, reveal bool) {
	fmt.Fprintf(w, "Tool: %s\n", tv.Tool)
	fmt.Fprintf(w, "  Presence: Installed=%v Detected=%v ConfigDir=%s Version=%s Notes=%s\n",
		tv.Presence.Installed,
		tv.Presence.Detected,
		orDash(tv.Presence.ConfigDir),
		orDash(tv.Presence.Version),
		orDash(tv.Presence.Notes),
	)
	if len(tv.Presence.Files) > 0 {
		fmt.Fprintln(w, "  Files:")
		for _, f := range tv.Presence.Files {
			fmt.Fprintf(w, "    - %s\n", f)
		}
	}

	if len(tv.Effective.Fields) == 0 {
		fmt.Fprintln(w, "  Owned Fields: (none set by any layer)")
	} else {
		fmt.Fprintln(w, "  Owned Fields (winning → shadowed):")
		for _, f := range tv.Effective.Fields {
			renderFieldText(w, f, reveal)
		}
	}

	if tv.Effective.ExternalDriftDetected {
		fmt.Fprintln(w, "  External drift:")
		for _, file := range tv.Effective.ExternalDriftFiles {
			fmt.Fprintf(w, "    - %s has been externally edited since last apply.\n", file)
		}
	}

	if len(tv.Errors) > 0 {
		fmt.Fprintln(w, "  Errors:")
		for _, e := range tv.Errors {
			file := ""
			if e.File != "" {
				file = " (" + e.File + ")"
			}
			fmt.Fprintf(w, "    - %s: %s%s\n", e.Kind, e.Message, file)
		}
	}
	fmt.Fprintln(w)
}

// renderFieldText prints one EffectiveField block with winning + shadowed
// lines. Secret marker and redaction are applied per --reveal.
func renderFieldText(w io.Writer, f adapter.EffectiveField, reveal bool) {
	secretMark := ""
	if f.Secret {
		secretMark = "  [SECRET]"
	}
	fmt.Fprintf(w, "    %s = %s%s\n", f.Key, formatValueForDisplay(f.Value, f.Secret, reveal), secretMark)
	fmt.Fprintf(w, "      winning: %s (%s)\n", f.WinningLayer, f.Source)
	if len(f.Shadowed) > 0 {
		fmt.Fprintln(w, "      shadowed:")
		for _, s := range f.Shadowed {
			fmt.Fprintf(w, "        - %s (%s): %s\n", s.Layer, orDash(s.Source), formatValueForDisplay(s.Value, s.Secret, reveal))
		}
	}
}

// orDash returns "-" for empty strings so text output columns never
// collapse when a field is absent.
func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// jsonProfile is the JSON shape of the profile header block. Fields
// are cherry-picked to what an operator actually needs — cmd/list --json
// already dumps the full profile shape.
type jsonProfile struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
}

type jsonPresence struct {
	Installed bool     `json:"installed"`
	Detected  bool     `json:"detected"`
	ConfigDir string   `json:"config_dir,omitempty"`
	Files     []string `json:"files,omitempty"`
	Version   string   `json:"version,omitempty"`
	Notes     string   `json:"notes,omitempty"`
}

type jsonShadowed struct {
	Layer  string `json:"layer"`
	Source string `json:"source,omitempty"`
	Value  any    `json:"value,omitempty"`
}

type jsonField struct {
	Key          string         `json:"key"`
	Value        any            `json:"value,omitempty"`
	WinningLayer string         `json:"winning_layer"`
	Source       string         `json:"source,omitempty"`
	Secret       bool           `json:"secret"`
	Shadowed     []jsonShadowed `json:"shadowed,omitempty"`
}

type jsonDrift struct {
	Detected bool     `json:"detected"`
	Files    []string `json:"files,omitempty"`
}

type jsonError struct {
	Kind    string `json:"kind"`
	Message string `json:"message"`
	File    string `json:"file,omitempty"`
}

type jsonTool struct {
	ID            string       `json:"id"`
	Presence      jsonPresence `json:"presence"`
	Fields        []jsonField  `json:"fields"`
	ExternalDrift jsonDrift    `json:"external_drift"`
	Errors        []jsonError  `json:"errors,omitempty"`
}

type jsonExplain struct {
	Profile       jsonProfile       `json:"profile"`
	Tools         []jsonTool        `json:"tools"`
	DiagnosticEnv map[string]string `json:"diagnostic_env,omitempty"`
}

// renderExplainJSON emits the JSON wire form. Redaction is applied to
// Value/ShadowedLayer.Value on secret fields unless --reveal.
func renderExplainJSON(w io.Writer, view resolver.View, diagnostic map[string]string, reveal bool) error {
	out := jsonExplain{
		Profile: jsonProfile{
			Name:        view.Profile.Name,
			Description: view.Profile.Description,
		},
	}
	for _, tv := range view.Tools {
		jt := jsonTool{
			ID: string(tv.Tool),
			Presence: jsonPresence{
				Installed: tv.Presence.Installed,
				Detected:  tv.Presence.Detected,
				ConfigDir: tv.Presence.ConfigDir,
				Files:     tv.Presence.Files,
				Version:   tv.Presence.Version,
				Notes:     tv.Presence.Notes,
			},
			Fields: make([]jsonField, 0, len(tv.Effective.Fields)),
			ExternalDrift: jsonDrift{
				Detected: tv.Effective.ExternalDriftDetected,
				Files:    tv.Effective.ExternalDriftFiles,
			},
		}
		for _, f := range tv.Effective.Fields {
			jf := jsonField{
				Key:          f.Key,
				WinningLayer: string(f.WinningLayer),
				Source:       f.Source,
				Secret:       f.Secret,
				Value:        jsonValue(f.Value, f.Secret, reveal),
			}
			for _, s := range f.Shadowed {
				jf.Shadowed = append(jf.Shadowed, jsonShadowed{
					Layer:  string(s.Layer),
					Source: s.Source,
					Value:  jsonValue(s.Value, s.Secret, reveal),
				})
			}
			jt.Fields = append(jt.Fields, jf)
		}
		for _, e := range tv.Errors {
			jt.Errors = append(jt.Errors, jsonError{
				Kind:    string(e.Kind),
				Message: e.Message,
				File:    e.File,
			})
		}
		out.Tools = append(out.Tools, jt)
	}
	if len(diagnostic) > 0 {
		out.DiagnosticEnv = diagnostic
	}
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(out)
}

// jsonValue routes a value through redaction when the field is secret
// and --reveal is off. Redacted values are emitted as strings (they
// carry no other useful shape); revealed values keep their original Go
// type so a boolean/number owned key stays a JSON boolean/number.
func jsonValue(v any, secret, reveal bool) any {
	if secret && !reveal {
		return redactValue(v)
	}
	return v
}
