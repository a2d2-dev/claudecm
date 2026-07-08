package cmd

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/a2d2-dev/claudecm/internal/aiparse"
	"github.com/a2d2-dev/claudecm/internal/config"
)

func TestAdd_FromTextDryRunRedactsAndDoesNotWrite(t *testing.T) {
	h := newAddHarness(t)
	addFromTextFlag = strings.Join([]string{
		"export ANTHROPIC_BASE_URL=https://text.example.com",
		"export ANTHROPIC_AUTH_TOKEN=sk-text-token-1234",
		"export ANTHROPIC_MODEL=claude-text-model",
	}, "\n")
	addDryRunFlag = true

	stdout, _, err := runAddInner(t, "textprof")
	if err != nil {
		t.Fatalf("runAdd --from-text: %v", err)
	}
	for _, want := range []string{
		"base_url: https://text.example.com",
		"model: claude-text-model",
	} {
		if !strings.Contains(stdout, want) {
			t.Fatalf("dry-run missing %q:\n%s", want, stdout)
		}
	}
	if strings.Contains(stdout, "sk-text-token-1234") {
		t.Fatalf("dry-run leaked plaintext api key:\n%s", stdout)
	}
	if !strings.Contains(stdout, "sk-t***1234") {
		t.Fatalf("dry-run missing redacted api key:\n%s", stdout)
	}
	if _, statErr := os.Stat(filepath.Join(h.home, ".claudecm", "profiles", "textprof.yaml")); !os.IsNotExist(statErr) {
		t.Fatalf("profile file written despite --dry-run: %v", statErr)
	}
}

func TestAdd_FromTextStdinAndExplicitFlagsOverride(t *testing.T) {
	h := newAddHarness(t)
	addFromTextFlag = "-"
	addAPIKeyFlag = "sk-flag-text-1234"
	addModelFlag = "flag-model"

	stdout, _, err := runAddInnerWithInput(t,
		"ANTHROPIC_BASE_URL=https://stdin.example.com\nANTHROPIC_AUTH_TOKEN=sk-stdin-text-1234\nANTHROPIC_MODEL=blob-model\n",
		"stdinprof",
	)
	if err != nil {
		t.Fatalf("runAdd --from-text -: %v\nstdout=%s", err, stdout)
	}
	loaded, err := h.store.LoadProfile("stdinprof")
	if err != nil {
		t.Fatalf("LoadProfile: %v", err)
	}
	if loaded.Core.APIKey != "sk-flag-text-1234" {
		t.Fatalf("APIKey = %q, want explicit flag", loaded.Core.APIKey)
	}
	if loaded.Core.Model != "flag-model" {
		t.Fatalf("Model = %q, want explicit flag", loaded.Core.Model)
	}
}

func TestAdd_FromTextNoKeyRefusesUnlessExplicitKey(t *testing.T) {
	h := newAddHarness(t)
	addFromTextFlag = "ANTHROPIC_BASE_URL=https://text.example.com\nANTHROPIC_MODEL=claude-text-model"

	_, _, err := runAddInner(t, "nokeytext")
	if err == nil {
		t.Fatalf("--from-text without key accepted")
	}
	if !strings.Contains(err.Error(), "no API key found in input source") {
		t.Fatalf("error = %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(h.home, ".claudecm", "profiles", "nokeytext.yaml")); !os.IsNotExist(statErr) {
		t.Fatalf("profile file written despite missing text key: %v", statErr)
	}

	resetAddFlags()
	addFromTextFlag = "ANTHROPIC_BASE_URL=https://text.example.com\nANTHROPIC_MODEL=claude-text-model"
	addAPIKeyFlag = "sk-explicit-text-1234"
	if _, _, err := runAddInner(t, "textkeyflag"); err != nil {
		t.Fatalf("runAdd --from-text explicit key: %v", err)
	}
	loaded, err := h.store.LoadProfile("textkeyflag")
	if err != nil {
		t.Fatalf("LoadProfile: %v", err)
	}
	if loaded.Core.APIKey != "sk-explicit-text-1234" {
		t.Fatalf("APIKey = %q", loaded.Core.APIKey)
	}
}

func TestAdd_FromTextNothingRecognizableRefuses(t *testing.T) {
	h := newAddHarness(t)
	addFromTextFlag = "hello there no usable profile fields"

	_, _, err := runAddInner(t, "nothing")
	if err == nil {
		t.Fatalf("--from-text with nothing recognizable accepted")
	}
	if !strings.Contains(err.Error(), "could not extract profile fields from text") {
		t.Fatalf("error = %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(h.home, ".claudecm", "profiles", "nothing.yaml")); !os.IsNotExist(statErr) {
		t.Fatalf("profile file written despite unrecognized text: %v", statErr)
	}
}

func TestAddAI_HappyMockReinjectsSecretAndDryRunRedacts(t *testing.T) {
	h := newAddHarness(t)
	seedAIProfile(t, h, "lender", config.CoreConfig{
		Provider: "anthropic",
		BaseURL:  "https://api.anthropic.com",
		APIKey:   "sk-lender-secret-1234",
		Model:    "claude-lender",
	}, true)
	parser := &mockAddLLMParser{
		core: config.CoreConfig{
			Provider: "anthropic",
			BaseURL:  "https://ai.example.com",
			APIKey:   "{{CLAUDECM_SECRET_1}}",
			Model:    "claude-ai-model",
		},
	}
	restore := SetAddLLMParserForTest(func() addLLMParser { return parser })
	t.Cleanup(restore)
	addFromTextFlag = "please configure base url https://ai.example.com with token sk-ai-input-1234 model claude-ai-model"
	addAIFlag = true
	addDryRunFlag = true

	stdout, _, err := runAddInner(t, "aiprof")
	if err != nil {
		t.Fatalf("runAdd --from-text --ai: %v", err)
	}
	if parser.calls != 1 {
		t.Fatalf("parser calls = %d, want 1", parser.calls)
	}
	if parser.creds.APIKey != "sk-lender-secret-1234" {
		t.Fatalf("parser credential seam did not receive borrowed key")
	}
	assertNoSecretShapeInText(t, parser.desensitized)
	for _, secret := range []string{"sk-ai-input-1234", "sk-lender-secret-1234"} {
		if strings.Contains(parser.desensitized, secret) {
			t.Fatalf("outbound desensitized payload leaked %q:\n%s", secret, parser.desensitized)
		}
	}
	if strings.Contains(stdout, "sk-ai-input-1234") || strings.Contains(stdout, "sk-lender-secret-1234") {
		t.Fatalf("dry-run leaked secret:\n%s", stdout)
	}
	if !strings.Contains(stdout, "sk-a***1234") {
		t.Fatalf("dry-run missing redacted re-injected key:\n%s", stdout)
	}
	if _, statErr := os.Stat(filepath.Join(h.home, ".claudecm", "profiles", "aiprof.yaml")); !os.IsNotExist(statErr) {
		t.Fatalf("profile file written despite --dry-run: %v", statErr)
	}
}

func TestAddAI_ExplicitAPIKeyOverridesReinjectedSecret(t *testing.T) {
	h := newAddHarness(t)
	seedAIProfile(t, h, "lender", config.CoreConfig{
		Provider: "anthropic",
		BaseURL:  "https://api.anthropic.com",
		APIKey:   "sk-lender-secret-1234",
		Model:    "claude-lender",
	}, true)
	restore := SetAddLLMParserForTest(func() addLLMParser {
		return &mockAddLLMParser{core: config.CoreConfig{
			Provider: "anthropic",
			BaseURL:  "https://ai.example.com",
			APIKey:   "{{CLAUDECM_SECRET_1}}",
			Model:    "claude-ai-model",
		}}
	})
	t.Cleanup(restore)
	addFromTextFlag = "API Key: sk-ai-input-1234 Base URL: https://ai.example.com"
	addAIFlag = true
	addAPIKeyFlag = "sk-explicit-ai-1234"

	if _, _, err := runAddInner(t, "aiexplicit"); err != nil {
		t.Fatalf("runAdd --ai explicit key: %v", err)
	}
	loaded, err := h.store.LoadProfile("aiexplicit")
	if err != nil {
		t.Fatalf("LoadProfile: %v", err)
	}
	if loaded.Core.APIKey != "sk-explicit-ai-1234" {
		t.Fatalf("APIKey = %q, want explicit flag", loaded.Core.APIKey)
	}
}

func TestAddAI_EdgeCredentialAndProtocolRefusals(t *testing.T) {
	t.Run("no active", func(t *testing.T) {
		h := newAddHarness(t)
		addFromTextFlag = "API Key: sk-ai-input-1234"
		addAIFlag = true
		_, _, err := runAddInner(t, "noactive")
		if err == nil || !strings.Contains(err.Error(), "no credentials available for --ai parse") {
			t.Fatalf("err = %v", err)
		}
		if _, statErr := os.Stat(filepath.Join(h.home, ".claudecm", "profiles", "noactive.yaml")); !os.IsNotExist(statErr) {
			t.Fatalf("profile file written despite no active: %v", statErr)
		}
	})

	t.Run("missing ai profile", func(t *testing.T) {
		newAddHarness(t)
		addFromTextFlag = "API Key: sk-ai-input-1234"
		addAIFlag = true
		addAIProfileFlag = "missing"
		_, _, err := runAddInner(t, "missing")
		if err == nil || !strings.Contains(err.Error(), `profile "missing" for --ai parse could not be loaded`) {
			t.Fatalf("err = %v", err)
		}
	})

	t.Run("non anthropic", func(t *testing.T) {
		h := newAddHarness(t)
		seedAIProfile(t, h, "compat", config.CoreConfig{
			Provider: "openai-compat",
			BaseURL:  "https://api.openai.com/v1",
			APIKey:   "sk-lender-secret-1234",
			Model:    "gpt-5",
		}, true)
		addFromTextFlag = "API Key: sk-ai-input-1234"
		addAIFlag = true
		_, _, err := runAddInner(t, "nonanth")
		if err == nil || !strings.Contains(err.Error(), "Anthropic-compatible messages endpoints") {
			t.Fatalf("err = %v", err)
		}
		if strings.Contains(err.Error(), "sk-lender-secret-1234") {
			t.Fatalf("error leaked credential: %v", err)
		}
	})
}

func TestAddAI_MalformedMockResponseRefusesWithoutWriteAndNoCredentialLeak(t *testing.T) {
	h := newAddHarness(t)
	seedAIProfile(t, h, "lender", config.CoreConfig{
		Provider: "anthropic",
		BaseURL:  "https://api.anthropic.com",
		APIKey:   "sk-lender-secret-1234",
		Model:    "claude-lender",
	}, true)
	_, parseErr := aiparse.ParseCoreJSON(`{"base_url":"https://ai.example.com","extra":"nope"}`)
	restore := SetAddLLMParserForTest(func() addLLMParser {
		return &mockAddLLMParser{err: parseErr}
	})
	t.Cleanup(restore)
	addFromTextFlag = "API Key: sk-ai-input-1234"
	addAIFlag = true

	_, _, err := runAddInner(t, "badai")
	if err == nil {
		t.Fatalf("malformed AI output accepted")
	}
	if strings.Contains(err.Error(), "sk-lender-secret-1234") || strings.Contains(err.Error(), "sk-ai-input-1234") {
		t.Fatalf("error leaked secret: %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(h.home, ".claudecm", "profiles", "badai.yaml")); !os.IsNotExist(statErr) {
		t.Fatalf("profile file written despite malformed AI output: %v", statErr)
	}
}

func TestAddAI_ResponseAPIKeyMustBeCapturedPlaceholder(t *testing.T) {
	h := newAddHarness(t)
	seedAIProfile(t, h, "lender", config.CoreConfig{
		Provider: "anthropic",
		BaseURL:  "https://api.anthropic.com",
		APIKey:   "sk-lender-secret-1234",
		Model:    "claude-lender",
	}, true)
	restore := SetAddLLMParserForTest(func() addLLMParser {
		return &mockAddLLMParser{core: config.CoreConfig{
			Provider: "anthropic",
			BaseURL:  "https://ai.example.com",
			APIKey:   "sk-guessed-by-ai-1234",
			Model:    "claude-ai-model",
		}}
	})
	t.Cleanup(restore)
	addFromTextFlag = "API Key: sk-ai-input-1234"
	addAIFlag = true

	_, _, err := runAddInner(t, "aiguess")
	if err == nil {
		t.Fatalf("AI guessed api_key accepted")
	}
	if !strings.Contains(err.Error(), "did not match a locally captured secret placeholder") {
		t.Fatalf("err = %v", err)
	}
	if strings.Contains(err.Error(), "sk-guessed-by-ai-1234") || strings.Contains(err.Error(), "sk-ai-input-1234") {
		t.Fatalf("error leaked secret: %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(h.home, ".claudecm", "profiles", "aiguess.yaml")); !os.IsNotExist(statErr) {
		t.Fatalf("profile file written despite guessed AI key: %v", statErr)
	}
}

func runAddInnerWithInput(t *testing.T, input string, args ...string) (stdout, stderr string, err error) {
	t.Helper()
	var out, errBuf bytes.Buffer
	cmd := &cobra.Command{Use: "add"}
	bindSyntheticAddFlags(cmd)
	cmd.SetOut(&out)
	cmd.SetErr(&errBuf)
	cmd.SetIn(strings.NewReader(input))
	err = runAdd(cmd, args)
	return out.String(), errBuf.String(), err
}

type mockAddLLMParser struct {
	calls        int
	desensitized string
	creds        aiparse.Credentials
	core         config.CoreConfig
	err          error
}

func (m *mockAddLLMParser) Parse(ctx context.Context, desensitized string, creds aiparse.Credentials) (config.CoreConfig, error) {
	m.calls++
	m.desensitized = desensitized
	m.creds = creds
	if m.err != nil {
		return config.CoreConfig{}, m.err
	}
	return m.core, nil
}

func seedAIProfile(t *testing.T, h *addHarness, name string, core config.CoreConfig, active bool) {
	t.Helper()
	p := config.NewProfile(name, core.BaseURL, core.APIKey)
	p.Core.Provider = core.Provider
	p.Core.Model = core.Model
	p.Core.SmallFastModel = core.SmallFastModel
	p.CreatedAt = nowFn().UTC()
	p.UpdatedAt = p.CreatedAt
	if err := h.store.SaveProfile(p); err != nil {
		t.Fatalf("SaveProfile(%q): %v", name, err)
	}
	if active {
		state, err := h.store.LoadState()
		if err != nil {
			t.Fatalf("LoadState: %v", err)
		}
		state.SetCurrentProfile(name)
		if err := h.store.SaveState(state); err != nil {
			t.Fatalf("SaveState: %v", err)
		}
	}
}

func assertNoSecretShapeInText(t *testing.T, text string) {
	t.Helper()
	detectors := []*regexp.Regexp{
		regexp.MustCompile(`(^|[^A-Za-z0-9_-])(sk-[A-Za-z0-9][A-Za-z0-9._=/+-]{3,})`),
		regexp.MustCompile(`(^|[^A-Za-z0-9_-])([A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,})`),
		regexp.MustCompile(`(?i)(^|[^A-Za-z0-9_-])((?:xox[baprs]?|gh[pousr]|pat|token)[_-][A-Za-z0-9][A-Za-z0-9._=-]{7,})`),
		regexp.MustCompile(`(?i)(^|[^A-Za-z0-9_-])([A-Za-z0-9._-]{8,}token[A-Za-z0-9._-]{8,})`),
		regexp.MustCompile(`(^|[^A-Za-z0-9_-])(AIza[0-9A-Za-z_-]{35,})`),
		regexp.MustCompile(`(?i)(^|[^A-Za-z0-9_-])([0-9a-f]{40})(?:$|[^0-9a-f])`),
	}
	for _, re := range detectors {
		if re.FindStringIndex(text) != nil {
			t.Fatalf("text contains secret shape:\n%s", text)
		}
	}
}
