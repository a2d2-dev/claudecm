// Package cmd — export command (Story E6-S8).
//
// `claudecm export [profile-name] [--format shell|yaml] [--redact]` is
// the read-only "secondary activation" path per FR-9 / NFR-S8. Two
// output shapes:
//
//	shell (default): `export VAR=quoted-value` lines, one per env var
//	                 the two v1 adapters own AND the profile carries a
//	                 value for. Meant for `eval $(claudecm export)`.
//	yaml           : the profile serialised as YAML (round-trippable
//	                 with `claudecm import`).
//
// --redact rewrites secret values via redactValue in either format —
// the shell values are still emitted (so a subsequent `eval` on a
// redacted export would set the env var to the redacted string; that
// is deliberately unusable — the AC frames --redact as a
// capture-to-log workflow, not an activation flag).
//
// Value sourcing:
//
//   - Core.APIKey feeds ANTHROPIC_AUTH_TOKEN (claude_code owned env)
//     AND OPENAI_API_KEY (codex auth.json owned key). Profile is
//     provider-agnostic; the mapping is the same one adapters use in
//     their Project methods (internal/adapter/*/project.go).
//   - Core.BaseURL feeds ANTHROPIC_BASE_URL.
//   - Core.Model feeds ANTHROPIC_MODEL.
//   - Core.SmallFastModel feeds ANTHROPIC_SMALL_FAST_MODEL.
//   - Core.ExtraEnv passes through verbatim.
//   - Per-tool overlays (Tools[claude_code], Tools[codex]) win over
//     Core when they set the same field. Overlay ExtraEnv also
//     passes through.
//   - Codex overlay's Raw map is inspected for any key present in
//     the auth.json owned-key allowlist (OPENAI_API_KEY in v1).
//
// The command never writes to tool files. On empty profile (no set
// fields), shell output is a single trailing newline and yaml output
// is the schema-stamped skeleton.
package cmd

import (
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/spf13/cobra"

	"github.com/a2d2-dev/claudecm/internal/config"
	"github.com/a2d2-dev/claudecm/internal/storage"
)

// exportFormat mirrors the enum shape used across cmd/*.
type exportFormat string

const (
	exportFormatShell exportFormat = "shell"
	exportFormatYAML  exportFormat = "yaml"
)

var (
	exportFormatFlag string
	exportRedactFlag bool
)

var exportCmd = &cobra.Command{
	Use:   "export [profile-name]",
	Short: "Emit shell exports or YAML for a profile (read-only secondary activation)",
	Long: `Emit the environment-variable exports (default) or YAML
representation of a profile.

With no positional argument the active profile is used; passing a name
picks that profile instead. --format shell prints ` + "`export VAR=value`" + `
lines suitable for ` + "`eval $(claudecm export)`" + `. --format yaml prints
the schema-stamped YAML body suitable for round-tripping via
` + "`claudecm import`" + `.

--redact rewrites api_key values via the same first4***last4 mask
` + "`current`" + ` / ` + "`explain`" + ` use, so you can safely capture the
output into a shared log or ticket.

EXAMPLES
  # Load the active profile into your shell
  eval $(claudecm export)

  # Print YAML for the "prod" profile
  claudecm export prod --format yaml

  # Redacted shell dump for a ticket
  claudecm export prod --redact`,
	Args: cobra.MaximumNArgs(1),
	RunE: runExport,
}

func init() {
	exportCmd.Flags().StringVar(&exportFormatFlag, "format", "shell", "Output format (shell|yaml)")
	exportCmd.Flags().BoolVar(&exportRedactFlag, "redact", false, "Redact secrets before printing")
	rootCmd.AddCommand(exportCmd)
}

// runExport is the testable entry point.
func runExport(cmd *cobra.Command, args []string) error {
	format, err := parseExportFormat(exportFormatFlag)
	if err != nil {
		return err
	}

	resv, err := resolverFromGlobals()
	if err != nil {
		return fmt.Errorf("failed to resolve HOME: %w", err)
	}
	if err := storage.Bootstrap(resv); err != nil {
		return fmt.Errorf("failed to bootstrap ~/.claudecm layout: %w", err)
	}
	store := storage.NewFileStorage(resv)

	profile, err := selectExportProfile(store, args)
	if err != nil {
		return err
	}

	switch format {
	case exportFormatYAML:
		return renderExportYAML(cmd.OutOrStdout(), profile, exportRedactFlag)
	default:
		return renderExportShell(cmd.OutOrStdout(), profile, exportRedactFlag)
	}
}

// parseExportFormat validates --format.
func parseExportFormat(raw string) (exportFormat, error) {
	switch trimAndLower(raw) {
	case "", "shell":
		return exportFormatShell, nil
	case "yaml":
		return exportFormatYAML, nil
	default:
		return "", fmt.Errorf("invalid --format %q (want shell|yaml)", raw)
	}
}

// selectExportProfile resolves the profile export should render. Named
// arg wins; otherwise the active profile from state.
func selectExportProfile(store *storage.FileStorage, args []string) (*config.Profile, error) {
	if len(args) > 0 {
		name := strings.TrimSpace(args[0])
		if name == "" {
			return nil, fmt.Errorf("profile name cannot be empty")
		}
		p, err := store.LoadProfile(name)
		if err != nil {
			return nil, fmt.Errorf("profile %q not found: %w", name, err)
		}
		return p, nil
	}
	state, err := store.LoadState()
	if err != nil {
		return nil, fmt.Errorf("failed to load state: %w", err)
	}
	if state.CurrentProfile == "" {
		return nil, fmt.Errorf("no active profile; specify a profile name or run 'claudecm switch' first")
	}
	p, err := store.LoadProfile(state.CurrentProfile)
	if err != nil {
		return nil, fmt.Errorf("active profile %q could not be loaded: %w", state.CurrentProfile, err)
	}
	return p, nil
}

// ownedEnvVarsCodex enumerates the auth.json owned keys the codex
// adapter treats as env-shaped. In v1 only OPENAI_API_KEY is emitted
// through the env-var boundary; the other allowlist entries (auth_mode,
// tokens.*) are tool-private and not surfaced as shell exports.
var ownedEnvVarsCodex = []string{
	"OPENAI_API_KEY",
}

// buildExportEnv computes the env var → value map for the given
// profile by walking the Claude Code and Codex allowlists and pulling
// the corresponding field off Core + Overlay. Empty-string values are
// dropped so `export VAR=""` lines do not pollute the output for
// unset fields.
//
// Precedence (low → high): Core, ToolOverlay direct fields
// (Overlay.APIKey, Overlay.BaseURL, etc), Overlay.ExtraEnv, Overlay.Raw
// (codex only). Each map key is written once — a later precedence
// layer overwrites earlier entries.
//
// The intentional cross-tool detail: Core.APIKey feeds BOTH
// ANTHROPIC_AUTH_TOKEN (claude_code) AND OPENAI_API_KEY (codex),
// because Profile is provider-agnostic and the "secondary activation"
// contract emits every owned env var that the profile can populate.
func buildExportEnv(p *config.Profile) map[string]string {
	env := map[string]string{}

	// --- Core layer -------------------------------------------------
	if p.Core.BaseURL != "" {
		env["ANTHROPIC_BASE_URL"] = p.Core.BaseURL
	}
	if p.Core.Model != "" {
		env["ANTHROPIC_MODEL"] = p.Core.Model
	}
	if p.Core.SmallFastModel != "" {
		env["ANTHROPIC_SMALL_FAST_MODEL"] = p.Core.SmallFastModel
	}
	if p.Core.APIKey != "" {
		env["ANTHROPIC_AUTH_TOKEN"] = p.Core.APIKey
		env["OPENAI_API_KEY"] = p.Core.APIKey
	}
	for k, v := range p.Core.ExtraEnv {
		if v != "" {
			env[k] = v
		}
	}

	// --- Overlay layer ---------------------------------------------
	// Iterate the two v1 tools in a fixed order so overlay wins are
	// deterministic. Claude Code first, Codex second — matches the
	// commit-order canon in FR-16.
	for _, tool := range []config.ToolID{config.ToolClaudeCode, config.ToolCodex} {
		ov, ok := p.Tools[tool]
		if !ok {
			continue
		}
		switch tool {
		case config.ToolClaudeCode:
			if ov.BaseURL != "" {
				env["ANTHROPIC_BASE_URL"] = ov.BaseURL
			}
			if ov.Model != "" {
				env["ANTHROPIC_MODEL"] = ov.Model
			}
			if ov.SmallFastModel != "" {
				env["ANTHROPIC_SMALL_FAST_MODEL"] = ov.SmallFastModel
			}
			if ov.APIKey != "" {
				env["ANTHROPIC_AUTH_TOKEN"] = ov.APIKey
			}
		case config.ToolCodex:
			if ov.APIKey != "" {
				env["OPENAI_API_KEY"] = ov.APIKey
			}
		}
		for k, v := range ov.ExtraEnv {
			if v != "" {
				env[k] = v
			}
		}
		// Codex Raw may carry an OPENAI_API_KEY entry from a raw
		// import; harvest it into the env map with the same precedence
		// as ExtraEnv.
		if tool == config.ToolCodex && ov.Raw != nil {
			for _, k := range ownedEnvVarsCodex {
				if raw, present := ov.Raw[k]; present {
					if s, ok := raw.(string); ok && s != "" {
						env[k] = s
					}
				}
			}
		}
	}

	// Drop entries that resolved to empty strings after all layers.
	for k, v := range env {
		if v == "" {
			delete(env, k)
		}
	}
	return env
}

// applyRedactionToEnv rewrites secret values in place. The determination
// of "secret" is the shared isSecretKey heuristic (name suffix _KEY /
// _TOKEN / _SECRET) so the same call site works for both known env vars
// and passthrough entries from ExtraEnv.
func applyRedactionToEnv(env map[string]string) map[string]string {
	out := make(map[string]string, len(env))
	for k, v := range env {
		if isSecretKey(k) {
			out[k] = redactValue(v)
			continue
		}
		out[k] = v
	}
	return out
}

// renderExportShell emits `export VAR=quoted-value` lines. Lines are
// sorted for reproducibility; values are shell-quoted via strconv-style
// double-quoting (%q from fmt), which is a valid double-quoted shell
// literal for every byte range these adapters emit.
func renderExportShell(w io.Writer, p *config.Profile, redact bool) error {
	env := buildExportEnv(p)
	if redact {
		env = applyRedactionToEnv(env)
	}
	keys := make([]string, 0, len(env))
	for k := range env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if _, err := fmt.Fprintf(w, "export %s=%q\n", k, env[k]); err != nil {
			return err
		}
	}
	return nil
}

// renderExportYAML emits the profile as YAML via config.MarshalProfile.
// --redact clones the profile first and replaces every APIKey field
// (core + per-tool overlay) with the redacted form before marshalling
// so no plaintext leaks through the wire.
func renderExportYAML(w io.Writer, p *config.Profile, redact bool) error {
	out := p
	if redact {
		clone := p.Clone()
		if clone.Core.APIKey != "" {
			clone.Core.APIKey = redactValue(clone.Core.APIKey)
		}
		for id, ov := range clone.Tools {
			if ov.APIKey != "" {
				ov.APIKey = redactValue(ov.APIKey)
			}
			// Redact any ExtraEnv entry whose name matches the secret
			// heuristic so a leaked API-key overlay entry does not
			// slip through the yaml wire.
			for k, v := range ov.ExtraEnv {
				if isSecretKey(k) && v != "" {
					ov.ExtraEnv[k] = redactValue(v)
				}
			}
			// Redact Raw entries similarly for codex.
			for k, v := range ov.Raw {
				if isSecretKey(k) {
					if s, ok := v.(string); ok && s != "" {
						ov.Raw[k] = redactValue(s)
					}
				}
			}
			clone.Tools[id] = ov
		}
		// Redact any Core.ExtraEnv secret entries too.
		for k, v := range clone.Core.ExtraEnv {
			if isSecretKey(k) && v != "" {
				clone.Core.ExtraEnv[k] = redactValue(v)
			}
		}
		out = clone
	}
	body, err := config.MarshalProfile(out)
	if err != nil {
		return fmt.Errorf("marshal profile: %w", err)
	}
	if _, err := w.Write(body); err != nil {
		return err
	}
	return nil
}

// resetExportFlagsForTest wipes the export flag package vars so tests
// can pin them cleanly. Called from cmd/export_test.go's harness.
func resetExportFlagsForTest() {
	exportFormatFlag = "shell"
	exportRedactFlag = false
}
