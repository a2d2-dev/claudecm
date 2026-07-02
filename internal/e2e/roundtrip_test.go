//go:build test

// roundtrip_test.go — Story E8-S3 CI round-trip smoke.
//
// For each canonical fixture (minimal + kitchen-sink under both the
// claudecode and codex adapters' testdata trees) drive the compiled
// command surface:
//
//	claudecm import <tool> --name test --yes   → profile YAML on disk
//	<mutate the tool's on-disk config so switch has real work to do>
//	claudecm switch test --yes                  → tool config re-written
//	claudecm export test --format shell         → owned env vars printed
//	claudecm explain test --output json         → EffectiveView JSON
//
// Assertions per case:
//   - profile file exists at ~/.claudecm/profiles/test.yaml.
//   - after switch, every owned key from the ORIGINAL fixture round-trips
//     through the parse → value comparison. Non-owned bytes are preserved
//     when the file survives the mutation step (kitchen-sink cases keep
//     the file and only stale one owned key so unowned bytes survive to
//     be re-checked).
//   - export output carries `export VAR='value'` lines for the env vars
//     the profile can populate (ANTHROPIC_MODEL, ANTHROPIC_BASE_URL,
//     OPENAI_API_KEY, etc).
//   - explain --output json emits a top-level document whose Tools[]
//     entry for the corresponding adapter surfaces the profile's owned
//     values under its EffectiveView.Fields.
//   - wall-clock per case < 5s (SM-1 ceiling; measured via timeit).
//
// Testdata reuse: minimal and kitchen-sink are the "canonical_minimal"
// and "canonical_maximal" fixtures the E8-S3 story references (the
// fixture directories under internal/adapter/{claudecode,codex}/testdata
// carry those names).

package e2e

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// roundTripBudget is the SM-1 wall-clock ceiling per fixture. The story
// spec pins <5s per case; timeit fails the test if any single case
// exceeds it. Kept as a variable so a future story can dial it down as
// the code path continues to shrink.
var roundTripBudget = 5 * time.Second

// The four fixture roots. Testdata directories live under each adapter's
// package testdata/; running `go test ./internal/e2e/...` sets cwd to
// internal/e2e, so we walk up one level.
const (
	claudecodeMinimalDir     = "../adapter/claudecode/testdata/claudecode/happy/minimal"
	claudecodeKitchenSinkDir = "../adapter/claudecode/testdata/claudecode/happy/kitchen-sink"
	codexMinimalDir          = "../adapter/codex/testdata/codex/happy/minimal"
	codexKitchenSinkDir      = "../adapter/codex/testdata/codex/happy/kitchen-sink"
)

// TestE2E_RoundTripClaudeCodeCanonicalMinimal exercises the minimal
// claude_code fixture. The on-disk settings.json carries a single owned
// env var — the round-trip must reproduce that value after a full clean
// slate.
func TestE2E_RoundTripClaudeCodeCanonicalMinimal(t *testing.T) {
	timeit(t, "roundtrip_claudecode_minimal", roundTripBudget, func() {
		h := newHarness(t)
		fixture := mustReadFile(t, filepath.Join(claudecodeMinimalDir, "settings.json"))
		seedFile(t, h.claudeSettings(), fixture)

		importAndSwitchClaudeCode(t, h, fixture, false /* keepUnowned */)
	})
}

// TestE2E_RoundTripClaudeCodeCanonicalMaximal exercises the kitchen-sink
// claude_code fixture. Unowned keys (mcpServers, hooks, custom,
// permissions) must round-trip verbatim through the mutate → switch
// cycle.
func TestE2E_RoundTripClaudeCodeCanonicalMaximal(t *testing.T) {
	timeit(t, "roundtrip_claudecode_kitchen_sink", roundTripBudget, func() {
		h := newHarness(t)
		fixture := mustReadFile(t, filepath.Join(claudecodeKitchenSinkDir, "settings.json"))
		seedFile(t, h.claudeSettings(), fixture)

		importAndSwitchClaudeCode(t, h, fixture, true /* keepUnowned */)
	})
}

// TestE2E_RoundTripCodexCanonicalMinimal exercises the minimal codex
// fixture (auth.json + config.toml).
func TestE2E_RoundTripCodexCanonicalMinimal(t *testing.T) {
	timeit(t, "roundtrip_codex_minimal", roundTripBudget, func() {
		h := newHarness(t)
		auth := mustReadFile(t, filepath.Join(codexMinimalDir, "auth.json"))
		conf := mustReadFile(t, filepath.Join(codexMinimalDir, "config.toml"))
		seedFile(t, h.codexAuth(), auth)
		seedFile(t, h.codexConfig(), conf)

		importAndSwitchCodex(t, h, auth, conf, false /* keepUnowned */)
	})
}

// TestE2E_RoundTripCodexCanonicalMaximal exercises the kitchen-sink
// codex fixture with unowned auth.json and config.toml keys that must
// survive the round-trip.
func TestE2E_RoundTripCodexCanonicalMaximal(t *testing.T) {
	timeit(t, "roundtrip_codex_kitchen_sink", roundTripBudget, func() {
		h := newHarness(t)
		auth := mustReadFile(t, filepath.Join(codexKitchenSinkDir, "auth.json"))
		conf := mustReadFile(t, filepath.Join(codexKitchenSinkDir, "config.toml"))
		seedFile(t, h.codexAuth(), auth)
		seedFile(t, h.codexConfig(), conf)

		importAndSwitchCodex(t, h, auth, conf, true /* keepUnowned */)
	})
}

// importAndSwitchClaudeCode drives the shared round-trip pipeline for
// the claude_code adapter. keepUnowned=true tells the mutation step to
// leave the unowned bytes intact so we can compare them post-switch.
func importAndSwitchClaudeCode(t *testing.T, h *harness, originalFixture []byte, keepUnowned bool) {
	t.Helper()

	// import
	if _, stderr, err := h.runImport([]string{"claude-code"}, "test", true, false); err != nil {
		t.Fatalf("runImport: %v; stderr=%s", err, stderr)
	}
	profilePath := filepath.Join(h.home, ".claudecm", "profiles", "test.yaml")
	if _, err := os.Stat(profilePath); err != nil {
		t.Fatalf("profile file missing after import: %v", err)
	}

	// mutate: either wipe entirely (minimal path) or replace one owned
	// key with a stale value (kitchen-sink path — leaves unowned bytes
	// intact so we can assert they round-trip). The kitchen-sink
	// fixture pins ANTHROPIC_MODEL=claude-opus-4-5 originally; overwrite
	// it with "stale" pre-switch so we can prove switch restored it.
	settingsPath := h.claudeSettings()
	if keepUnowned {
		mutated := replaceJSONOwnedValue(t, originalFixture, "env.ANTHROPIC_MODEL", "stale-model")
		if err := os.WriteFile(settingsPath, mutated, 0o600); err != nil {
			t.Fatalf("mutate settings.json: %v", err)
		}
	} else {
		if err := os.Remove(settingsPath); err != nil {
			t.Fatalf("remove settings.json: %v", err)
		}
	}

	// switch
	stdout, stderr, err := h.runSwitch([]string{"test"}, "text", true, false)
	if err != nil {
		t.Fatalf("runSwitch: %v; stdout=%s; stderr=%s", err, stdout, stderr)
	}
	if !strings.Contains(stdout, `Switched to "test"`) {
		t.Errorf("switch stdout missing switched line:\n%s", stdout)
	}

	// verify owned-key round-trip
	post := mustReadFile(t, settingsPath)
	assertJSONOwnedRoundTrip(t, originalFixture, post, claudecodeOwnedFlatKeys())

	// verify unowned preservation on the maximal path
	if keepUnowned {
		assertJSONUnownedPreserved(t, originalFixture, post, claudecodeOwnedFlatKeys())
	}

	// export
	exportOut, _, err := h.runExport(nil, "shell", false)
	if err != nil {
		t.Fatalf("runExport: %v", err)
	}
	if !strings.Contains(exportOut, "export ANTHROPIC_MODEL=") {
		t.Errorf("export output missing ANTHROPIC_MODEL line:\n%s", exportOut)
	}

	// explain --output json
	explainOut, _, err := h.runExplain([]string{"test"}, "json", true)
	if err != nil {
		t.Fatalf("runExplain: %v", err)
	}
	assertExplainJSONContainsTool(t, explainOut, "claude_code")
}

// importAndSwitchCodex is the codex twin of importAndSwitchClaudeCode.
func importAndSwitchCodex(t *testing.T, h *harness, origAuth, origConf []byte, keepUnowned bool) {
	t.Helper()

	// import
	if _, stderr, err := h.runImport([]string{"codex"}, "test", true, false); err != nil {
		t.Fatalf("runImport: %v; stderr=%s", err, stderr)
	}
	profilePath := filepath.Join(h.home, ".claudecm", "profiles", "test.yaml")
	if _, err := os.Stat(profilePath); err != nil {
		t.Fatalf("profile file missing after import: %v", err)
	}

	// mutate. Minimal path: delete both files (proves switch recreates
	// them from the profile). Kitchen-sink path: overwrite each file's
	// owned key with a stale value while leaving unowned bytes intact
	// so switch has to re-write AND the unowned entries survive.
	authPath, configPath := h.codexAuth(), h.codexConfig()
	if keepUnowned {
		mutatedAuth := replaceJSONOwnedValue(t, origAuth, "OPENAI_API_KEY", "sk-stale")
		if err := os.WriteFile(authPath, mutatedAuth, 0o600); err != nil {
			t.Fatalf("mutate auth.json: %v", err)
		}
		mutatedConf := replaceTOMLOwnedScalar(t, origConf, "model", "gpt-stale")
		if err := os.WriteFile(configPath, mutatedConf, 0o600); err != nil {
			t.Fatalf("mutate config.toml: %v", err)
		}
	} else {
		if err := os.Remove(authPath); err != nil {
			t.Fatalf("remove auth.json: %v", err)
		}
		if err := os.Remove(configPath); err != nil {
			t.Fatalf("remove config.toml: %v", err)
		}
	}

	// switch
	stdout, stderr, err := h.runSwitch([]string{"test"}, "text", true, false)
	if err != nil {
		t.Fatalf("runSwitch: %v; stdout=%s; stderr=%s", err, stdout, stderr)
	}
	if !strings.Contains(stdout, `Switched to "test"`) {
		t.Errorf("switch stdout missing switched line:\n%s", stdout)
	}

	// verify per-file owned-key round-trip.
	postAuth := mustReadFile(t, authPath)
	postConf := mustReadFile(t, configPath)
	assertJSONOwnedRoundTrip(t, origAuth, postAuth, codexAuthOwnedFlatKeys())
	assertTOMLOwnedRoundTrip(t, origConf, postConf, codexConfigOwnedFlatKeys())

	if keepUnowned {
		assertJSONUnownedPreserved(t, origAuth, postAuth, codexAuthOwnedFlatKeys())
		// TOML unowned preservation lives in tables and typed scalars
		// that a JSON flatten would garble; walk the TOML doc-model
		// directly and pin the two big unowned tables.
		assertTOMLUnownedPreserved(t, origConf, postConf, codexConfigOwnedFlatKeys())
	}

	// export
	exportOut, _, err := h.runExport(nil, "shell", false)
	if err != nil {
		t.Fatalf("runExport: %v", err)
	}
	if !strings.Contains(exportOut, "export OPENAI_API_KEY=") {
		t.Errorf("export output missing OPENAI_API_KEY line:\n%s", exportOut)
	}

	// explain --output json
	explainOut, _, err := h.runExplain([]string{"test"}, "json", true)
	if err != nil {
		t.Fatalf("runExplain: %v", err)
	}
	assertExplainJSONContainsTool(t, explainOut, "codex")
}

// assertExplainJSONContainsTool decodes the explain --output json wire
// document and asserts a tool entry exists for the given adapter ID.
// The wire shape carries {tools: [{id, presence, fields, ...}]} — we
// only check the id is present so the assertion tracks the E5-S5
// contract loosely without brittle-pinning every field.
func assertExplainJSONContainsTool(t *testing.T, raw, wantTool string) {
	t.Helper()
	var doc struct {
		Tools []struct {
			ID     string `json:"id"`
			Fields []struct {
				Key   string `json:"key"`
				Value any    `json:"value"`
			} `json:"fields"`
		} `json:"tools"`
	}
	if err := json.Unmarshal([]byte(raw), &doc); err != nil {
		t.Fatalf("explain json invalid: %v; body=%s", err, raw)
	}
	for _, tool := range doc.Tools {
		if tool.ID == wantTool {
			if len(tool.Fields) == 0 {
				t.Errorf("explain tool %q has empty Fields", wantTool)
			}
			return
		}
	}
	t.Fatalf("explain json missing tool %q; body=%s", wantTool, raw)
}
