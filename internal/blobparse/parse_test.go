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

func TestParseRedactsGenericSecretNamedFieldsWithoutSecretShape(t *testing.T) {
	input := strings.Join([]string{
		`Base URL: https://api.example.com`,
		`CLIENT_SECRET=prod-secret-value`,
		`PASSWORD='plain password value'`,
		`export DATABASE_TOKEN=db-token-value`,
		`"private key": "plain-private-key-value"`,
		`model: claude-sonnet`,
	}, "\n")

	got := Parse(input)

	if got.Core.BaseURL != "https://api.example.com" {
		t.Fatalf("Core.BaseURL = %q", got.Core.BaseURL)
	}
	if got.Core.Model != "claude-sonnet" {
		t.Fatalf("Core.Model = %q", got.Core.Model)
	}
	for _, secret := range []string{
		"prod-secret-value",
		"plain password value",
		"db-token-value",
		"plain-private-key-value",
	} {
		if strings.Contains(got.Desensitized, secret) {
			t.Fatalf("Desensitized leaked %q:\n%s", secret, got.Desensitized)
		}
	}
	for _, nonSecret := range []string{"https://api.example.com", "claude-sonnet"} {
		if !strings.Contains(got.Desensitized, nonSecret) {
			t.Fatalf("Desensitized removed non-secret %q:\n%s", nonSecret, got.Desensitized)
		}
	}
	if len(got.CapturedSecrets) != 4 {
		t.Fatalf("CapturedSecrets len = %d, want 4: %#v", len(got.CapturedSecrets), got.CapturedSecrets)
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

func TestParseDesensitizesExpandedBareSecretShapes(t *testing.T) {
	secrets := []string{
		"sk-ant-secret123",
		"eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjM0NTY3ODkwIn0.signedpayload123",
		"xoxb-1234567890abcdef",
		"ghp_1234567890abcdef",
		"pat_1234567890abcdef",
		"token_secret_value_123456789",
		"AIzaSyA123456789012345678901234567890123",
		"0123456789abcdef0123456789abcdef01234567",
		"QUJDREVGR0hJSktMTU5PUFFSU1RVVldYWVo1234567890+/=",
	}
	input := strings.Join([]string{
		`first "` + secrets[0] + `", second (` + secrets[1] + `);`,
		secrets[2] + " " + secrets[3] + " " + secrets[4],
		"`" + secrets[5] + "` " + secrets[6] + ".",
		"hex=[" + secrets[7] + "] b64=" + secrets[8] + ",",
	}, "\n")

	got := Parse(input)

	for _, secret := range secrets {
		if strings.Contains(got.Desensitized, secret) {
			t.Fatalf("Desensitized leaked %q: %q", secret, got.Desensitized)
		}
	}
	assertNoSecretShapes(t, got.Desensitized)
}

func TestParseHighEntropyFallbackPreservesURLs(t *testing.T) {
	baseURL := "https://compat.example.com/v1"
	input := "OPENAI_BASE_URL=" + baseURL + " blob QUJDREVGR0hJSktMTU5PUFFSU1RVVldYWVo1234567890+/="

	got := Parse(input)

	if got.Core.BaseURL != baseURL {
		t.Fatalf("Core.BaseURL = %q, want %q", got.Core.BaseURL, baseURL)
	}
	if !strings.Contains(got.Desensitized, baseURL) {
		t.Fatalf("Desensitized removed base URL: %q", got.Desensitized)
	}
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
		`google "AIzaSyA123456789012345678901234567890123"`,
		`hex (0123456789abcdef0123456789abcdef01234567), next`,
		`base64 QUJDREVGR0hJSktMTU5PUFFSU1RVVldYWVo1234567890+/=;`,
		`multiple sk-first-secret123 xoxb-1234567890abcdef ghp_1234567890abcdef`,
		`TOKEN_SECRET_VALUE_123456789 and pat_1234567890abcdef`,
		`wrapped ['AIzaSyA123456789012345678901234567890123'].`,
	}

	for _, blob := range corpus {
		t.Run(blob, func(t *testing.T) {
			got := Parse(blob)
			assertNoSecretShapes(t, got.Desensitized)
		})
	}
}

func TestParseInfersProviderFromSelectedAPIKey(t *testing.T) {
	input := strings.Join([]string{
		"OPENAI_BASE_URL=https://compat.example.com/v1",
		"OPENAI_API_KEY=sk-openai-secret123",
		"ANTHROPIC_AUTH_TOKEN=sk-anthropic-secret123",
	}, "\n")

	got := Parse(input)

	if got.Core.BaseURL != "https://compat.example.com/v1" {
		t.Errorf("Core.BaseURL = %q, want OpenAI-compatible base URL", got.Core.BaseURL)
	}
	if got.Core.APIKey != "sk-anthropic-secret123" {
		t.Errorf("Core.APIKey = %q, want selected Anthropic auth token", got.Core.APIKey)
	}
	if got.Core.Provider != "anthropic" {
		t.Errorf("Core.Provider = %q, want anthropic", got.Core.Provider)
	}
}

func TestParseDoesNotTrimTrailingPunctuationFromAPIKey(t *testing.T) {
	input := "ANTHROPIC_AUTH_TOKEN=sk-key-ending. OPENAI_BASE_URL=https://compat.example.com/v1."

	got := Parse(input)

	if got.Core.APIKey != "sk-key-ending." {
		t.Errorf("Core.APIKey = %q, want trailing punctuation preserved", got.Core.APIKey)
	}
	if got.Core.BaseURL != "https://compat.example.com/v1" {
		t.Errorf("Core.BaseURL = %q, want trailing punctuation trimmed", got.Core.BaseURL)
	}
	if strings.Contains(got.Desensitized, "sk-key-ending.") {
		t.Fatalf("Desensitized leaked api key with trailing punctuation: %q", got.Desensitized)
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
	for _, detector := range testSecretShapeDetectors() {
		matches := detector.re.FindAllStringSubmatch(text, -1)
		for _, match := range matches {
			if len(match) < 3 {
				continue
			}
			secret := match[2]
			if detector.mixedAlphaNum && !hasTestLetterAndDigit(secret) {
				continue
			}
			t.Fatalf("desensitized text contains secret shape %q: %q", detector.name, text)
		}
	}
}

type testSecretShapeDetector struct {
	name          string
	re            *regexp.Regexp
	mixedAlphaNum bool
}

func testSecretShapeDetectors() []testSecretShapeDetector {
	return []testSecretShapeDetector{
		{name: "sk prefix", re: regexp.MustCompile(`(^|[^A-Za-z0-9_-])(sk-[A-Za-z0-9][A-Za-z0-9._=/+-]{3,})`)},
		{name: "jwt", re: regexp.MustCompile(`(^|[^A-Za-z0-9_-])([A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,})`)},
		{name: "xox gh pat token prefix", re: regexp.MustCompile(`(?i)(^|[^A-Za-z0-9_-])((?:xox[baprs]?|gh[pousr]|pat|token)[_-][A-Za-z0-9][A-Za-z0-9._=-]{7,})`)},
		{name: "embedded token", re: regexp.MustCompile(`(?i)(^|[^A-Za-z0-9_-])([A-Za-z0-9._-]{8,}token[A-Za-z0-9._-]{8,})`)},
		{name: "google api key", re: regexp.MustCompile(`(^|[^A-Za-z0-9_-])(AIza[0-9A-Za-z_-]{35,})`)},
		{name: "40 hex", re: regexp.MustCompile(`(?i)(^|[^A-Za-z0-9_-])([0-9a-f]{40})(?:$|[^0-9a-f])`)},
		{name: "high entropy token", re: regexp.MustCompile(`(^|[^A-Za-z0-9_-])([A-Za-z0-9+/=_-]{32,})`), mixedAlphaNum: true},
	}
}

func hasTestLetterAndDigit(text string) bool {
	hasLetter := false
	hasDigit := false
	for _, r := range text {
		switch {
		case r >= 'A' && r <= 'Z' || r >= 'a' && r <= 'z':
			hasLetter = true
		case r >= '0' && r <= '9':
			hasDigit = true
		}
	}
	return hasLetter && hasDigit
}
