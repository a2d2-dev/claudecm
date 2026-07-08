package fileparse

import (
	"strings"
	"testing"
)

func TestParseProfileCoreBytes_KnownFormats(t *testing.T) {
	cases := []struct {
		name      string
		ext       string
		body      string
		baseURL   string
		apiKey    string
		model     string
		provider  string
		smallFast string
	}{
		{
			name:      "dotenv anthropic names",
			ext:       ".env",
			body:      "ANTHROPIC_BASE_URL=https://dotenv.example.com\nANTHROPIC_AUTH_TOKEN=sk-dotenv\nANTHROPIC_SMALL_FAST_MODEL=haiku\n",
			baseURL:   "https://dotenv.example.com",
			apiKey:    "sk-dotenv",
			smallFast: "haiku",
		},
		{
			name:   "shell export",
			ext:    ".sh",
			body:   "export ANTHROPIC_MODEL='claude-shell'\nexport ANTHROPIC_AUTH_TOKEN=sk-shell\n",
			apiKey: "sk-shell",
			model:  "claude-shell",
		},
		{
			name:    "json core names",
			ext:     ".json",
			body:    `{"base_url":"https://json.example.com","api_key":"sk-json","model":"json-model"}`,
			baseURL: "https://json.example.com",
			apiKey:  "sk-json",
			model:   "json-model",
		},
		{
			name:     "yaml provider",
			ext:      ".yaml",
			body:     "provider: openai\napi_key: sk-yaml\nmodel: yaml-model\n",
			provider: "openai-compat",
			apiKey:   "sk-yaml",
			model:    "yaml-model",
		},
		{
			name:    "toml codex names",
			ext:     ".toml",
			body:    "OPENAI_API_KEY = \"sk-toml\"\nOPENAI_BASE_URL = \"https://toml.example.com\"\nCODEX_MODEL = \"toml-model\"\n",
			baseURL: "https://toml.example.com",
			apiKey:  "sk-toml",
			model:   "toml-model",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ParseProfileCoreBytes(tc.ext, []byte(tc.body))
			if err != nil {
				t.Fatalf("ParseProfileCoreBytes: %v", err)
			}
			if got.BaseURL != tc.baseURL {
				t.Fatalf("BaseURL = %q, want %q", got.BaseURL, tc.baseURL)
			}
			if got.APIKey != tc.apiKey {
				t.Fatalf("APIKey = %q, want %q", got.APIKey, tc.apiKey)
			}
			if got.Model != tc.model {
				t.Fatalf("Model = %q, want %q", got.Model, tc.model)
			}
			if got.Provider != tc.provider {
				t.Fatalf("Provider = %q, want %q", got.Provider, tc.provider)
			}
			if got.SmallFastModel != tc.smallFast {
				t.Fatalf("SmallFastModel = %q, want %q", got.SmallFastModel, tc.smallFast)
			}
		})
	}
}

func TestParseProfileCoreBytes_UnrecognizedAndNoFields(t *testing.T) {
	for _, body := range [][]byte{
		{0x00, 0xff, 0x01},
		[]byte("this is not config"),
		[]byte("UNRELATED=value"),
	} {
		_, err := ParseProfileCoreBytes("", body)
		if err == nil {
			t.Fatalf("ParseProfileCoreBytes(%q): want error", string(body))
		}
		if !strings.Contains(err.Error(), "unrecognized") &&
			!strings.Contains(err.Error(), "no usable profile fields") {
			t.Fatalf("unexpected error: %v", err)
		}
	}
}
