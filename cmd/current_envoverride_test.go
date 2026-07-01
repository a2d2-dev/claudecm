//go:build test

package cmd

// current_envoverride_test.go — Story E6-S1 tagged test.
//
// Verifies that when the process env carries ANTHROPIC_MODEL, the
// highlighted Model value in `claudecm current` reflects the env
// override rather than the profile's Core.Model. Uses the envextract
// SetLookupForTest seam so the test does not rely on ambient
// os.Environ() state; symmetric with explain_shadowed_test.go.

import (
	"strings"
	"testing"

	"github.com/a2d2-dev/claudecm/internal/envextract"
)

// TestCurrent_EnvOverrideShownInEffective seeds a profile with
// Model="opus" and injects ANTHROPIC_MODEL=env-only-model via the
// envextract seam. The compact summary must show the env value as the
// Model highlight because EnvOverride is the winning layer for that
// key.
func TestCurrent_EnvOverrideShownInEffective(t *testing.T) {
	h := newCurrentHarness(t)
	h.saveProfile("prod", "sk-longsecretvalue123", "https://api.anthropic.com", "opus")
	h.activate("prod")

	restore := envextract.SetLookupForTest(func(name string) (string, bool) {
		if name == "ANTHROPIC_MODEL" {
			return "env-only-model", true
		}
		return "", false
	})
	defer restore()

	stdout, _, err := runCurrentCmd(t)
	if err != nil {
		t.Fatalf("runCurrent err = %v", err)
	}
	if !strings.Contains(stdout, "Model: env-only-model") {
		t.Errorf("stdout missing 'Model: env-only-model'; env override should win:\n%s", stdout)
	}
	if strings.Contains(stdout, "Model: opus") {
		t.Errorf("stdout still shows profile Model 'opus'; env override should shadow it:\n%s", stdout)
	}
}
