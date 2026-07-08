package aiparse

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestClientParseSendsSecretFreePayloadAndDoesNotLeakCredentialInError(t *testing.T) {
	doer := roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if got := req.Header.Get("x-api-key"); got != "sk-lender-secret-1234" {
			t.Fatalf("x-api-key = %q", got)
		}
		body, err := io.ReadAll(req.Body)
		if err != nil {
			t.Fatalf("ReadAll body: %v", err)
		}
		for _, secret := range []string{
			"sk-input-secret-1234",
			"ghp_1234567890abcdef",
			"sk-lender-secret-1234",
		} {
			if strings.Contains(string(body), secret) {
				t.Fatalf("outbound payload leaked %q:\n%s", secret, body)
			}
		}
		if !strings.Contains(string(body), "{{CLAUDECM_SECRET_1}}") {
			t.Fatalf("outbound payload missing placeholder:\n%s", body)
		}
		return jsonResponse(200, `{"content":[{"type":"text","text":"{\"base_url\":\"https://api.example.com\",\"api_key\":\"{{CLAUDECM_SECRET_1}}\",\"model\":\"claude-test\",\"provider\":\"anthropic\"}"}]}`), nil
	})
	client := NewClient(doer)

	core, err := client.Parse(context.Background(),
		"Base URL: https://api.example.com API Key: {{CLAUDECM_SECRET_1}} and ",
		Credentials{
			BaseURL: "https://api.anthropic.com",
			APIKey:  "sk-lender-secret-1234",
			Model:   "claude-lender",
		},
	)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if core.APIKey != "{{CLAUDECM_SECRET_1}}" {
		t.Fatalf("APIKey = %q", core.APIKey)
	}
}

func TestClientParseRefusesOutboundSecretShapeBeforeTransport(t *testing.T) {
	called := false
	client := NewClient(roundTripFunc(func(req *http.Request) (*http.Response, error) {
		called = true
		return nil, nil
	}))

	_, err := client.Parse(context.Background(),
		"still has sk-input-secret-1234",
		Credentials{
			BaseURL: "https://api.anthropic.com",
			APIKey:  "sk-lender-secret-1234",
			Model:   "claude-lender",
		},
	)
	if err == nil {
		t.Fatalf("Parse accepted secret-shaped payload")
	}
	if called {
		t.Fatalf("transport was called despite secret-shaped payload")
	}
	if strings.Contains(err.Error(), "sk-lender-secret-1234") || strings.Contains(err.Error(), "sk-input-secret-1234") {
		t.Fatalf("error leaked secret: %v", err)
	}
}

func TestClientParseRefusesOutboundSecretNamedAssignmentBeforeTransport(t *testing.T) {
	called := false
	client := NewClient(roundTripFunc(func(req *http.Request) (*http.Response, error) {
		called = true
		return nil, nil
	}))

	_, err := client.Parse(context.Background(),
		"Base URL: https://api.example.com SECRET=not-a-secret-shape",
		Credentials{
			BaseURL: "https://api.anthropic.com",
			APIKey:  "sk-lender-secret-1234",
			Model:   "claude-lender",
		},
	)
	if err == nil {
		t.Fatalf("Parse accepted secret-named assignment payload")
	}
	if called {
		t.Fatalf("transport was called despite secret-named assignment payload")
	}
	if strings.Contains(err.Error(), "not-a-secret-shape") || strings.Contains(err.Error(), "sk-lender-secret-1234") {
		t.Fatalf("error leaked secret: %v", err)
	}
}

func TestEnsureSecretFreeRefusesAuthorizationAndAuthLineRemainders(t *testing.T) {
	tests := []struct {
		name string
		text string
	}{
		{
			name: "authorization bearer",
			text: "Base URL: https://api.example.com\nAuthorization: Bearer opaque-session-id-123456\nmodel claude-sonnet",
		},
		{
			name: "auth bearer",
			text: "Base URL: https://api.example.com\nAUTH=Bearer opaque-session-id-123456\nmodel claude-sonnet",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := EnsureSecretFree(tt.text)
			if err == nil {
				t.Fatalf("EnsureSecretFree accepted residual secret-named assignment")
			}
			if strings.Contains(err.Error(), "opaque-session-id-123456") {
				t.Fatalf("error leaked secret: %v", err)
			}
		})
	}
}

func TestClientParseStripsUserinfoFromEndpoint(t *testing.T) {
	var gotURL string
	client := NewClient(roundTripFunc(func(req *http.Request) (*http.Response, error) {
		gotURL = req.URL.String()
		if req.URL.User != nil {
			t.Fatalf("request URL retained userinfo: %s", req.URL.Redacted())
		}
		if strings.Contains(gotURL, "sk-url-secret-123456") || strings.Contains(gotURL, "@") {
			t.Fatalf("request URL leaked userinfo: %s", gotURL)
		}
		return jsonResponse(200, `{"content":[{"type":"text","text":"{\"base_url\":\"https://api.example.com\",\"model\":\"claude-test\"}"}]}`), nil
	}))

	if _, err := client.Parse(context.Background(),
		"Base URL: https://api.example.com model claude-test",
		Credentials{
			BaseURL: "https://user:sk-url-secret-123456@api.anthropic.com/custom?secret=drop#frag",
			APIKey:  "sk-lender-secret-1234",
			Model:   "claude-lender",
		},
	); err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if gotURL != "https://api.anthropic.com/custom/v1/messages" {
		t.Fatalf("request URL = %q", gotURL)
	}
}

func TestClientParseRefusesMalformedAndNonConformingResponses(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{name: "malformed messages", body: `{`},
		{name: "no text block", body: `{"content":[{"type":"tool_use","text":"{}"}]}`},
		{name: "malformed core json", body: `{"content":[{"type":"text","text":"not json"}]}`},
		{name: "extra field", body: `{"content":[{"type":"text","text":"{\"base_url\":\"https://api.example.com\",\"extra\":\"nope\"}"}]}`},
		{name: "empty object", body: `{"content":[{"type":"text","text":"{}"}]}`},
		{name: "array", body: `{"content":[{"type":"text","text":"[]"}]}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client := NewClient(roundTripFunc(func(req *http.Request) (*http.Response, error) {
				return jsonResponse(200, tt.body), nil
			}))
			_, err := client.Parse(context.Background(), "plain desensitized payload", Credentials{
				BaseURL: "https://api.anthropic.com",
				APIKey:  "sk-lender-secret-1234",
				Model:   "claude-lender",
			})
			if err == nil {
				t.Fatalf("Parse accepted %s", tt.name)
			}
			if strings.Contains(err.Error(), "sk-lender-secret-1234") {
				t.Fatalf("error leaked credential: %v", err)
			}
		})
	}
}

func TestParseCoreJSONStrictSchema(t *testing.T) {
	core, err := ParseCoreJSON(`{"base_url":"https://api.example.com","api_key":"{{CLAUDECM_SECRET_1}}","model":"claude","small_fast_model":"haiku","provider":"anthropic"}`)
	if err != nil {
		t.Fatalf("ParseCoreJSON: %v", err)
	}
	if core.BaseURL != "https://api.example.com" || core.APIKey != "{{CLAUDECM_SECRET_1}}" || core.SmallFastModel != "haiku" {
		t.Fatalf("Core = %#v", core)
	}

	if _, err := ParseCoreJSON(`{"base_url":"https://api.example.com","nested":{"no":"no"}}`); err == nil {
		t.Fatalf("ParseCoreJSON accepted nested object")
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) Do(req *http.Request) (*http.Response, error) {
	return f(req)
}

func jsonResponse(status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Body:       io.NopCloser(strings.NewReader(body)),
		Header:     make(http.Header),
	}
}
