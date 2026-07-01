//go:build test

package cmd

// explain_shadowed_test.go — F3 followup for PR #41.
//
// Locks the redaction contract on shadowed secret layers: when a secret
// value contributes to a lower layer that loses to a higher one, the
// shadowed entry MUST still be redacted by default. This test is
// build-tagged `test` because it uses the envextract.SetLookupForTest
// seam to inject a hermetic env universe without touching os.Environ()
// — the ambient process env is fully isolated from the assertion.
//
// Layer chain constructed under test (all three layers pin to the same
// owned key so shadowing is exercised end-to-end). Profile.Core.APIKey
// maps to env.ANTHROPIC_AUTH_TOKEN in the claudecode adapter — that is
// the key we drive here:
//
//	ProfileCore       ← sk-core-plaintext-CCCCDDDD (Profile.Core.APIKey)
//	OnDiskToolConfig  ← sk-disk-plaintext-EEEEFFFF (settings.json)
//	EnvOverride       ← sk-env-plaintext-AAAABBBB  (via envextract seam)
//
// EnvOverride wins; ProfileCore and OnDiskToolConfig are shadowed. All
// three plaintexts must be absent from the default output (no --reveal),
// in both text and JSON forms.

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/a2d2-dev/claudecm/internal/envextract"
)

// TestExplain_ShadowedSecretsAreRedacted asserts that no plaintext
// substring of any shadowed OR winning secret value leaks to output
// when --reveal is off.
func TestExplain_ShadowedSecretsAreRedacted(t *testing.T) {
	// distinctive plaintexts — every substring must be absent from the
	// default (no --reveal) output.
	const (
		envPlain  = "sk-env-plaintext-AAAABBBB"
		corePlain = "sk-core-plaintext-CCCCDDDD"
		diskPlain = "sk-disk-plaintext-EEEEFFFF"
	)

	// Route the adapter env lookup through a synthetic universe so the
	// ambient CI/dev env cannot poison the assertion.
	restore := envextract.SetLookupForTest(func(name string) (string, bool) {
		if name == "ANTHROPIC_AUTH_TOKEN" {
			return envPlain, true
		}
		return "", false
	})
	defer restore()

	h := newExplainHarness(t)
	// ProfileCore contribution: Core.APIKey → env.ANTHROPIC_AUTH_TOKEN.
	h.saveProfile("prod", corePlain, "https://api.anthropic.com", "opus")
	h.activate("prod")
	// OnDiskToolConfig contribution: settings.json env.ANTHROPIC_AUTH_TOKEN.
	h.writeSettingsJSON(`{"env":{"ANTHROPIC_AUTH_TOKEN":"` + diskPlain + `"}}`)

	// ---------------------- text output ----------------------
	resetExplainFlags()
	stdoutText, _, err := runExplainCmd(t, nil)
	if err != nil {
		t.Fatalf("text runExplain err = %v", err)
	}
	for _, needle := range []string{envPlain, corePlain, diskPlain} {
		if strings.Contains(stdoutText, needle) {
			t.Errorf("text output leaked plaintext %q without --reveal:\n%s", needle, stdoutText)
		}
	}
	if !strings.Contains(stdoutText, "winning: EnvOverride") {
		t.Errorf("text output does not report EnvOverride as winning layer for env.ANTHROPIC_AUTH_TOKEN:\n%s", stdoutText)
	}
	if !strings.Contains(stdoutText, "ProfileCore") {
		t.Errorf("text output missing ProfileCore in shadowed layers list:\n%s", stdoutText)
	}
	if !strings.Contains(stdoutText, "OnDiskToolConfig") {
		t.Errorf("text output missing OnDiskToolConfig in shadowed layers list:\n%s", stdoutText)
	}

	// ---------------------- JSON output ----------------------
	resetExplainFlags()
	explainOutputFlag = "json"
	stdoutJSON, _, err := runExplainCmd(t, nil)
	if err != nil {
		t.Fatalf("json runExplain err = %v", err)
	}
	for _, needle := range []string{envPlain, corePlain, diskPlain} {
		if strings.Contains(stdoutJSON, needle) {
			t.Errorf("json output leaked plaintext %q without --reveal:\n%s", needle, stdoutJSON)
		}
	}
	var doc jsonExplain
	if err := json.Unmarshal([]byte(stdoutJSON), &doc); err != nil {
		t.Fatalf("json stdout does not parse: %v\n%s", err, stdoutJSON)
	}

	// Walk the parsed doc and find env.ANTHROPIC_AUTH_TOKEN. Verify the
	// structural contract: EnvOverride wins, ProfileCore + OnDiskToolConfig
	// appear in Shadowed[], every rendered value is redacted (contains
	// "***"), and no shadowed value is a plaintext substring.
	var found bool
	for _, tool := range doc.Tools {
		for _, f := range tool.Fields {
			if f.Key != "env.ANTHROPIC_AUTH_TOKEN" {
				continue
			}
			found = true
			if f.WinningLayer != "EnvOverride" {
				t.Errorf("winning layer = %q, want EnvOverride", f.WinningLayer)
			}
			if !f.Secret {
				t.Errorf("field.secret = false, want true (ANTHROPIC_AUTH_TOKEN is a secret)")
			}
			// Winning value must be redacted form.
			if s, ok := f.Value.(string); !ok || !strings.Contains(s, "***") {
				t.Errorf("winning value = %v, want a redacted string containing '***'", f.Value)
			}
			// Shadowed must contain both ProfileCore and OnDiskToolConfig.
			seen := map[string]bool{}
			for _, sh := range f.Shadowed {
				seen[sh.Layer] = true
				s, ok := sh.Value.(string)
				if !ok || !strings.Contains(s, "***") {
					t.Errorf("shadowed %s value = %v, want a redacted string containing '***'", sh.Layer, sh.Value)
				}
			}
			if !seen["ProfileCore"] {
				t.Errorf("shadowed layers missing ProfileCore; got=%v", f.Shadowed)
			}
			if !seen["OnDiskToolConfig"] {
				t.Errorf("shadowed layers missing OnDiskToolConfig; got=%v", f.Shadowed)
			}
		}
	}
	if !found {
		t.Fatalf("env.ANTHROPIC_AUTH_TOKEN not found in any tool.Fields; doc=%+v", doc)
	}
}
