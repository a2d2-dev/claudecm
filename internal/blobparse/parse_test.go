package blobparse

import (
	"reflect"
	"regexp"
	"strings"
	"testing"
)

func TestParseExtractsFieldsAndDesensitizes(t *testing.T) {
	tests := []struct {
		name               string
		input              string
		wantBaseURL        string
		wantAPIKey         string
		wantModel          string
		wantSmallFastModel string
		wantProvider       string
	}{
		{
			name: "export anthropic env",
			input: strings.Join([]string{
				`export ANTHROPIC_BASE_URL=https://api.example.com`,
				`export ANTHROPIC_AUTH_TOKEN=sk-ant-secret123`,
				`export ANTHROPIC_MODEL=claude-sonnet-4`,
				`export ANTHROPIC_SMALL_FAST_MODEL=claude-haiku-4-5`,
			}, "\n"),
			wantBaseURL:        "https://api.example.com",
			wantAPIKey:         "sk-ant-secret123",
			wantModel:          "claude-sonnet-4",
			wantSmallFastModel: "claude-haiku-4-5",
			wantProvider:       "anthropic",
		},
		{
			name:         "prose labels",
			input:        `Base URL: https://proxy.example.test/v1, API Key: sk-prose-secret123, model: claude-opus-4`,
			wantBaseURL:  "https://proxy.example.test/v1",
			wantAPIKey:   "sk-prose-secret123",
			wantModel:    "claude-opus-4",
			wantProvider: "",
		},
		{
			name:         "json codex",
			input:        `{"OPENAI_BASE_URL":"https://compat.example.com/v1","OPENAI_API_KEY":"sk-openai-secret123","CODEX_MODEL":"gpt-4.1","CODEX_MODEL_PROVIDER":"openai"}`,
			wantBaseURL:  "https://compat.example.com/v1",
			wantAPIKey:   "sk-openai-secret123",
			wantModel:    "gpt-4.1",
			wantProvider: "openai",
		},
		{
			name:         "yaml anthropic api key fallback",
			input:        "ANTHROPIC_BASE_URL: https://anthropic.example.com\nANTHROPIC_API_KEY: sk-api-secret123\n",
			wantBaseURL:  "https://anthropic.example.com",
			wantAPIKey:   "sk-api-secret123",
			wantProvider: "anthropic",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Parse(tt.input)

			if got.Core.BaseURL != tt.wantBaseURL {
				t.Errorf("Core.BaseURL = %q, want %q", got.Core.BaseURL, tt.wantBaseURL)
			}
			if got.Core.APIKey != tt.wantAPIKey {
				t.Errorf("Core.APIKey = %q, want %q", got.Core.APIKey, tt.wantAPIKey)
			}
			if got.Core.Model != tt.wantModel {
				t.Errorf("Core.Model = %q, want %q", got.Core.Model, tt.wantModel)
			}
			if got.Core.SmallFastModel != tt.wantSmallFastModel {
				t.Errorf("Core.SmallFastModel = %q, want %q", got.Core.SmallFastModel, tt.wantSmallFastModel)
			}
			if got.Core.Provider != tt.wantProvider {
				t.Errorf("Core.Provider = %q, want %q", got.Core.Provider, tt.wantProvider)
			}
			if strings.Contains(got.Desensitized, tt.wantAPIKey) {
				t.Fatalf("Desensitized leaked api key: %q", got.Desensitized)
			}
			if len(got.CapturedSecrets) != 1 {
				t.Fatalf("CapturedSecrets len = %d, want 1: %#v", len(got.CapturedSecrets), got.CapturedSecrets)
			}
			placeholder := onlyPlaceholder(t, got.CapturedSecrets)
			if got.CapturedSecrets[placeholder] != tt.wantAPIKey {
				t.Errorf("CapturedSecrets[%q] = %q, want %q", placeholder, got.CapturedSecrets[placeholder], tt.wantAPIKey)
			}
			if !strings.Contains(got.Desensitized, placeholder) {
				t.Errorf("Desensitized = %q, want placeholder %q", got.Desensitized, placeholder)
			}
		})
	}
}

func TestParseCapturesBareSecret(t *testing.T) {
	got := Parse("paste this bare key sk-xxxx into the provider form")

	if got.Core.APIKey != "" {
		t.Errorf("Core.APIKey = %q, want empty without a label", got.Core.APIKey)
	}
	if len(got.CapturedSecrets) != 1 {
		t.Fatalf("CapturedSecrets len = %d, want 1: %#v", len(got.CapturedSecrets), got.CapturedSecrets)
	}
	placeholder := onlyPlaceholder(t, got.CapturedSecrets)
	if got.CapturedSecrets[placeholder] != "sk-xxxx" {
		t.Errorf("CapturedSecrets[%q] = %q, want sk-xxxx", placeholder, got.CapturedSecrets[placeholder])
	}
	if strings.Contains(got.Desensitized, "sk-") {
		t.Fatalf("Desensitized leaked sk token: %q", got.Desensitized)
	}
}

func TestParseRedactsSecretNamedFieldsWithoutSecretShape(t *testing.T) {
	got := Parse("ANTHROPIC_AUTH_TOKEN=plain-token-value\nOPENAI_BASE_URL=https://compat.example.com/v1")

	if got.Core.APIKey != "plain-token-value" {
		t.Errorf("Core.APIKey = %q, want plain-token-value", got.Core.APIKey)
	}
	if strings.Contains(got.Desensitized, "plain-token-value") {
		t.Fatalf("Desensitized leaked secret-named field: %q", got.Desensitized)
	}
	if len(got.CapturedSecrets) != 1 {
		t.Fatalf("CapturedSecrets len = %d, want 1: %#v", len(got.CapturedSecrets), got.CapturedSecrets)
	}
}

func TestParseNoRecognizableFieldsPreservesPlainText(t *testing.T) {
	input := "hello there\nthis blob has no profile fields"

	got := Parse(input)

	if got.Core.BaseURL != "" || got.Core.APIKey != "" || got.Core.Model != "" || got.Core.SmallFastModel != "" || got.Core.Provider != "" {
		t.Fatalf("Core = %#v, want empty", got.Core)
	}
	if len(got.CapturedSecrets) != 0 {
		t.Fatalf("CapturedSecrets = %#v, want empty", got.CapturedSecrets)
	}
	if got.Desensitized != input {
		t.Errorf("Desensitized = %q, want input unchanged", got.Desensitized)
	}
}

func TestParseReusesStablePlaceholderForRepeatedSecret(t *testing.T) {
	input := "ANTHROPIC_AUTH_TOKEN=sk-repeat-secret and API Key: sk-repeat-secret"

	got := Parse(input)

	if len(got.CapturedSecrets) != 1 {
		t.Fatalf("CapturedSecrets len = %d, want 1: %#v", len(got.CapturedSecrets), got.CapturedSecrets)
	}
	placeholder := onlyPlaceholder(t, got.CapturedSecrets)
	if count := strings.Count(got.Desensitized, placeholder); count != 2 {
		t.Errorf("placeholder count = %d, want 2 in %q", count, got.Desensitized)
	}
}

func TestParseStripsResidualSecretShapes(t *testing.T) {
	input := "unmapped jwt aaaabbbbcccc.ddddeeeeffff.gggghhhhiiii and token token_secret_value_123456789"

	got := Parse(input)

	assertNoSecretShapes(t, got.Desensitized)
}

func TestParsePropertyNoSecretShapeSurvives(t *testing.T) {
	corpus := []string{
		`export ANTHROPIC_BASE_URL=https://api.example.com ANTHROPIC_AUTH_TOKEN=sk-ant-property123`,
		`Base URL: https://proxy.example.com API Key: sk-label-property123`,
		`bare sk-xxxx should still be removed`,
		`{"OPENAI_API_KEY":"sk-json-property123","OPENAI_BASE_URL":"https://compat.example.com/v1"}`,
		`plain text with no token`,
		`oauth token: token_secret_value_123456789`,
		`jwt eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjM0NTY3ODkwIn0.signedpayload123`,
	}

	for _, blob := range corpus {
		t.Run(blob, func(t *testing.T) {
			got := Parse(blob)
			assertNoSecretShapes(t, got.Desensitized)
		})
	}
}

func TestParseDeterministic(t *testing.T) {
	input := `export ANTHROPIC_AUTH_TOKEN=sk-deterministic123
API Key: sk-second-deterministic`

	first := Parse(input)
	second := Parse(input)

	if !reflect.DeepEqual(first, second) {
		t.Fatalf("Parse is not deterministic:\nfirst=%#v\nsecond=%#v", first, second)
	}
}

func onlyPlaceholder(t *testing.T, secrets map[string]string) string {
	t.Helper()
	for placeholder := range secrets {
		return placeholder
	}
	t.Fatal("no placeholder")
	return ""
}

func assertNoSecretShapes(t *testing.T, text string) {
	t.Helper()
	for _, corePattern := range secretShapePatterns() {
		re := regexp.MustCompile(`(^|[^A-Za-z0-9_-])(` + corePattern + `)`)
		if re.MatchString(text) {
			t.Fatalf("desensitized text contains secret shape %q: %q", corePattern, text)
		}
	}
}
