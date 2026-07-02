package cmd

// export_test.go — Story E6-S8 tests for the cmd/export surface.

import (
	"bytes"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/a2d2-dev/claudecm/internal/config"
	"github.com/a2d2-dev/claudecm/internal/storage"
)

// exportHarness owns the per-test HOME tree.
type exportHarness struct {
	t     *testing.T
	home  string
	resv  *storage.Resolver
	store *storage.FileStorage
	mgr   *config.Manager
}

func newExportHarness(t *testing.T) *exportHarness {
	t.Helper()
	resetExportFlagsForTest()
	resetGlobalFlagsForTest()
	t.Cleanup(func() {
		resetExportFlagsForTest()
		resetGlobalFlagsForTest()
	})

	home := t.TempDir()
	t.Setenv("HOME", home)
	resv, err := storage.NewResolverWithHome(home)
	if err != nil {
		t.Fatalf("NewResolverWithHome: %v", err)
	}
	if err := storage.Bootstrap(resv); err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	store := storage.NewFileStorage(resv)
	mgr := config.NewManager(store, config.NewValidator())
	return &exportHarness{t: t, home: home, resv: resv, store: store, mgr: mgr}
}

func (h *exportHarness) saveProfile(name, apiKey, baseURL, model string) *config.Profile {
	h.t.Helper()
	p := config.NewProfile(name, baseURL, apiKey)
	p.Core.Model = model
	if err := h.mgr.AddProfile(p); err != nil {
		h.t.Fatalf("AddProfile(%q): %v", name, err)
	}
	return p
}

func (h *exportHarness) activate(name string) {
	h.t.Helper()
	if err := h.mgr.SetActive(name); err != nil {
		h.t.Fatalf("SetActive(%q): %v", name, err)
	}
}

func runExportInner(t *testing.T, args ...string) (stdout, stderr string, err error) {
	t.Helper()
	var out, errBuf bytes.Buffer
	cmd := &cobra.Command{Use: "export"}
	cmd.SetOut(&out)
	cmd.SetErr(&errBuf)
	err = runExport(cmd, args)
	return out.String(), errBuf.String(), err
}

// TestExport_ShellFormatDefault: active profile → stdout has
// `export VAR=...` lines for each set env var.
func TestExport_ShellFormatDefault(t *testing.T) {
	h := newExportHarness(t)
	h.saveProfile("prod", "sk-prodverylongkey1234", "https://api.example.com", "opus")
	h.activate("prod")

	stdout, _, err := runExportInner(t)
	if err != nil {
		t.Fatalf("runExport err = %v", err)
	}
	for _, marker := range []string{
		"export ANTHROPIC_AUTH_TOKEN=",
		"export ANTHROPIC_BASE_URL=",
		"export ANTHROPIC_MODEL=",
		"export OPENAI_API_KEY=",
	} {
		if !strings.Contains(stdout, marker) {
			t.Errorf("stdout missing %q:\n%s", marker, stdout)
		}
	}
	// Plaintext value must appear (no --redact).
	if !strings.Contains(stdout, "sk-prodverylongkey1234") {
		t.Errorf("stdout missing plaintext key:\n%s", stdout)
	}
}

// TestExport_YAMLFormat: --format yaml → parseable YAML round-trips.
func TestExport_YAMLFormat(t *testing.T) {
	h := newExportHarness(t)
	h.saveProfile("prod", "sk-prodverylongkey1234", "https://api.example.com", "opus")
	h.activate("prod")

	exportFormatFlag = "yaml"
	stdout, _, err := runExportInner(t)
	if err != nil {
		t.Fatalf("runExport err = %v", err)
	}
	p, perr := config.ParseProfile([]byte(stdout))
	if perr != nil {
		t.Fatalf("ParseProfile round-trip failed: %v\nBody:\n%s", perr, stdout)
	}
	if p.Name != "prod" {
		t.Errorf("round-tripped profile Name = %q; want %q", p.Name, "prod")
	}
	if p.Core.APIKey != "sk-prodverylongkey1234" {
		t.Errorf("round-tripped API key = %q; want plaintext", p.Core.APIKey)
	}
	if p.Core.Model != "opus" {
		t.Errorf("round-tripped Model = %q; want opus", p.Core.Model)
	}
	if p.SchemaVersion != config.CurrentProfileSchemaVersion {
		t.Errorf("round-tripped SchemaVersion = %d; want %d",
			p.SchemaVersion, config.CurrentProfileSchemaVersion)
	}
}

// TestExport_ShellFormatRedacted: --redact → api_key value redacted
// via first4***last4.
func TestExport_ShellFormatRedacted(t *testing.T) {
	h := newExportHarness(t)
	h.saveProfile("prod", "sk-prodverylongkey1234", "https://api.example.com", "opus")
	h.activate("prod")

	exportRedactFlag = true
	stdout, _, err := runExportInner(t)
	if err != nil {
		t.Fatalf("runExport err = %v", err)
	}
	if strings.Contains(stdout, "sk-prodverylongkey1234") {
		t.Errorf("--redact leaked plaintext key:\n%s", stdout)
	}
	// Redacted marker present on the secret-shaped lines.
	if !strings.Contains(stdout, "***") {
		t.Errorf("--redact missing sentinel:\n%s", stdout)
	}
	// Non-secret keys still appear plaintext.
	if !strings.Contains(stdout, "https://api.example.com") {
		t.Errorf("non-secret base_url should not be redacted:\n%s", stdout)
	}
}

// TestExport_YAMLFormatRedacted: --format yaml --redact → api_key
// redacted in YAML output.
func TestExport_YAMLFormatRedacted(t *testing.T) {
	h := newExportHarness(t)
	h.saveProfile("prod", "sk-prodverylongkey1234", "https://api.example.com", "opus")
	h.activate("prod")

	exportFormatFlag = "yaml"
	exportRedactFlag = true
	stdout, _, err := runExportInner(t)
	if err != nil {
		t.Fatalf("runExport err = %v", err)
	}
	if strings.Contains(stdout, "sk-prodverylongkey1234") {
		t.Errorf("--redact --format yaml leaked plaintext key:\n%s", stdout)
	}
	if !strings.Contains(stdout, "***") {
		t.Errorf("YAML output missing redaction sentinel:\n%s", stdout)
	}
	// Base URL preserved plaintext (non-secret).
	if !strings.Contains(stdout, "https://api.example.com") {
		t.Errorf("YAML missing plaintext base_url:\n%s", stdout)
	}
}

// TestExport_NamedProfile: `export <name>` uses that profile even
// when another is active.
func TestExport_NamedProfile(t *testing.T) {
	h := newExportHarness(t)
	h.saveProfile("active", "sk-activeverylongkey12345", "https://api.active.example.com", "opus")
	h.saveProfile("other", "sk-otherverylongkey67890", "https://api.other.example.com", "sonnet")
	h.activate("active")

	stdout, _, err := runExportInner(t, "other")
	if err != nil {
		t.Fatalf("runExport err = %v", err)
	}
	if !strings.Contains(stdout, "sk-otherverylongkey67890") {
		t.Errorf("named-profile export missing other's key:\n%s", stdout)
	}
	if strings.Contains(stdout, "sk-activeverylongkey12345") {
		t.Errorf("named-profile export leaked active profile's key:\n%s", stdout)
	}
	if !strings.Contains(stdout, "https://api.other.example.com") {
		t.Errorf("named-profile export missing other's base_url:\n%s", stdout)
	}
}

// TestExport_NoActiveErrors: no state, no positional → error.
func TestExport_NoActiveErrors(t *testing.T) {
	newExportHarness(t)
	_, _, err := runExportInner(t)
	if err == nil {
		t.Fatalf("runExport expected error on no-active-no-arg; got nil")
	}
	if !strings.Contains(err.Error(), "no active profile") {
		t.Errorf("error message unexpected: %v", err)
	}
}

// TestExport_UnknownProfileErrors: named profile that does not exist.
func TestExport_UnknownProfileErrors(t *testing.T) {
	newExportHarness(t)
	_, _, err := runExportInner(t, "ghost")
	if err == nil {
		t.Fatalf("runExport expected error on missing profile; got nil")
	}
	if !strings.Contains(err.Error(), "ghost") {
		t.Errorf("error message must name the missing profile; got %v", err)
	}
}

// TestExport_InvalidFormatRefused: --format foo → error.
func TestExport_InvalidFormatRefused(t *testing.T) {
	newExportHarness(t)
	exportFormatFlag = "toml"
	_, _, err := runExportInner(t)
	if err == nil {
		t.Fatalf("runExport expected error on invalid --format; got nil")
	}
	if !strings.Contains(err.Error(), "invalid --format") {
		t.Errorf("error message unexpected: %v", err)
	}
}

// TestExport_ClaudeCodeOverlayWins asserts a Tools[claude_code]
// overlay setting BaseURL / Model / SmallFastModel / APIKey supersedes
// the Core values in the shell output.
func TestExport_ClaudeCodeOverlayWins(t *testing.T) {
	h := newExportHarness(t)
	p := h.saveProfile("prod", "sk-coreverylongkey1234", "https://api.core.example.com", "opus")
	p.Core.SmallFastModel = "core-small"
	p.Tools = map[config.ToolID]config.ToolOverlay{
		config.ToolClaudeCode: {
			BaseURL:        "https://api.overlay.example.com",
			APIKey:         "sk-overlayverylong5678",
			Model:          "overlay-opus",
			SmallFastModel: "overlay-small",
			ExtraEnv:       map[string]string{"OVERLAY_KNOB": "on"},
		},
	}
	if err := h.mgr.UpdateProfile("prod", p); err != nil {
		t.Fatalf("UpdateProfile: %v", err)
	}
	h.activate("prod")

	stdout, _, err := runExportInner(t)
	if err != nil {
		t.Fatalf("runExport err = %v", err)
	}
	for _, want := range []string{
		"https://api.overlay.example.com",
		"sk-overlayverylong5678",
		"overlay-opus",
		"overlay-small",
		"OVERLAY_KNOB",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("overlay value %q missing:\n%s", want, stdout)
		}
	}
	// Codex OPENAI_API_KEY should still reflect the Core value; no
	// codex overlay is present so overlay wins are Claude Code-only.
	if !strings.Contains(stdout, "export OPENAI_API_KEY=") {
		t.Errorf("codex OPENAI_API_KEY missing:\n%s", stdout)
	}
}

// TestExport_CodexOverlayAPIKeyOverridesCore asserts a Tools[codex]
// overlay APIKey supersedes the Core APIKey for OPENAI_API_KEY only,
// while ANTHROPIC_AUTH_TOKEN still comes from Core.
func TestExport_CodexOverlayAPIKeyOverridesCore(t *testing.T) {
	h := newExportHarness(t)
	p := h.saveProfile("prod", "sk-coreverylongkey1234", "https://api.example.com", "opus")
	p.Tools = map[config.ToolID]config.ToolOverlay{
		config.ToolCodex: {
			APIKey: "sk-codexoverlay9999",
		},
	}
	if err := h.mgr.UpdateProfile("prod", p); err != nil {
		t.Fatalf("UpdateProfile: %v", err)
	}
	h.activate("prod")

	stdout, _, err := runExportInner(t)
	if err != nil {
		t.Fatalf("runExport err = %v", err)
	}
	if !strings.Contains(stdout, "sk-codexoverlay9999") {
		t.Errorf("codex overlay API key missing:\n%s", stdout)
	}
	// ANTHROPIC_AUTH_TOKEN unchanged from Core.
	if !strings.Contains(stdout, `export ANTHROPIC_AUTH_TOKEN="sk-coreverylongkey1234"`) {
		t.Errorf("core ANTHROPIC_AUTH_TOKEN missing or overridden:\n%s", stdout)
	}
}

// TestExport_CodexOverlayRawOPENAIKey asserts a Tools[codex].Raw entry
// keyed on OPENAI_API_KEY (per the codex auth.json allowlist) also
// lands in the shell output.
func TestExport_CodexOverlayRawOPENAIKey(t *testing.T) {
	h := newExportHarness(t)
	p := h.saveProfile("prod", "sk-coreverylongkey1234", "https://api.example.com", "opus")
	p.Tools = map[config.ToolID]config.ToolOverlay{
		config.ToolCodex: {
			Raw: map[string]any{
				"OPENAI_API_KEY": "sk-rawverylong7777",
			},
		},
	}
	if err := h.mgr.UpdateProfile("prod", p); err != nil {
		t.Fatalf("UpdateProfile: %v", err)
	}
	h.activate("prod")

	stdout, _, err := runExportInner(t)
	if err != nil {
		t.Fatalf("runExport err = %v", err)
	}
	if !strings.Contains(stdout, "sk-rawverylong7777") {
		t.Errorf("codex Raw OPENAI_API_KEY missing:\n%s", stdout)
	}
}

// TestExport_YAMLRedactsOverlayAPIKeys: Tools[*].APIKey entries in
// the overlay are redacted in the yaml output too.
func TestExport_YAMLRedactsOverlayAPIKeys(t *testing.T) {
	h := newExportHarness(t)
	p := h.saveProfile("prod", "sk-coreverylongkey1234", "https://api.example.com", "opus")
	p.Tools = map[config.ToolID]config.ToolOverlay{
		config.ToolClaudeCode: {
			APIKey:   "sk-ccoverlay9999long",
			ExtraEnv: map[string]string{"SOME_SECRET_TOKEN": "supersecret9999", "PLAIN_KNOB": "keep"},
		},
		config.ToolCodex: {
			APIKey: "sk-codexoverlay8888long",
			Raw:    map[string]any{"OPENAI_API_KEY": "sk-rawlong7777more"},
		},
	}
	if err := h.mgr.UpdateProfile("prod", p); err != nil {
		t.Fatalf("UpdateProfile: %v", err)
	}
	h.activate("prod")

	exportFormatFlag = "yaml"
	exportRedactFlag = true
	stdout, _, err := runExportInner(t)
	if err != nil {
		t.Fatalf("runExport err = %v", err)
	}
	for _, plain := range []string{
		"sk-coreverylongkey1234",
		"sk-ccoverlay9999long",
		"sk-codexoverlay8888long",
		"sk-rawlong7777more",
		"supersecret9999",
	} {
		if strings.Contains(stdout, plain) {
			t.Errorf("yaml --redact leaked %q:\n%s", plain, stdout)
		}
	}
	if !strings.Contains(stdout, "PLAIN_KNOB: keep") && !strings.Contains(stdout, "PLAIN_KNOB:keep") {
		t.Errorf("yaml --redact wrongly rewrote non-secret PLAIN_KNOB:\n%s", stdout)
	}
}

// TestExport_ExtraEnvPassthrough: Core.ExtraEnv entries land in the
// shell output.
func TestExport_ExtraEnvPassthrough(t *testing.T) {
	h := newExportHarness(t)
	p := h.saveProfile("prod", "sk-prodverylongkey1234", "https://api.example.com", "opus")
	p.Core.ExtraEnv = map[string]string{"CLAUDECM_TEST_VAR": "hello"}
	if err := h.mgr.UpdateProfile("prod", p); err != nil {
		t.Fatalf("UpdateProfile: %v", err)
	}
	h.activate("prod")

	stdout, _, err := runExportInner(t)
	if err != nil {
		t.Fatalf("runExport err = %v", err)
	}
	if !strings.Contains(stdout, "export CLAUDECM_TEST_VAR=") {
		t.Errorf("extra_env passthrough missing:\n%s", stdout)
	}
	if !strings.Contains(stdout, "hello") {
		t.Errorf("extra_env value missing:\n%s", stdout)
	}
}
