// Package aiparse sends a secret-free profile parse request to an
// Anthropic-compatible Messages endpoint.
package aiparse

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/a2d2-dev/claudecm/internal/config"
)

const (
	defaultMaxTokens        = 1024
	defaultAnthropicVersion = "2023-06-01"
)

// Credentials are borrowed from an existing claudecm profile for one parse
// request. Callers must not log or persist APIKey.
type Credentials struct {
	BaseURL string
	APIKey  string
	Model   string
}

// HTTPDoer is the narrow transport seam used by Client.
type HTTPDoer interface {
	Do(req *http.Request) (*http.Response, error)
}

// Client is the production Anthropic-compatible parser.
type Client struct {
	doer HTTPDoer
}

// NewClient returns a Client using doer. Passing nil uses http.DefaultClient.
func NewClient(doer HTTPDoer) *Client {
	if doer == nil {
		doer = http.DefaultClient
	}
	return &Client{doer: doer}
}

// Parse sends desensitized text to the borrowed profile's Messages endpoint
// and returns the strict core-field JSON response.
func (c *Client) Parse(ctx context.Context, desensitized string, creds Credentials) (config.CoreConfig, error) {
	if strings.TrimSpace(desensitized) == "" {
		return config.CoreConfig{}, fmt.Errorf("desensitized text is empty")
	}
	if strings.TrimSpace(creds.BaseURL) == "" || strings.TrimSpace(creds.APIKey) == "" {
		return config.CoreConfig{}, fmt.Errorf("no credentials available for --ai parse")
	}
	if strings.TrimSpace(creds.Model) == "" {
		return config.CoreConfig{}, fmt.Errorf("credential-lending profile has no model for --ai parse")
	}
	if err := EnsureSecretFree(desensitized); err != nil {
		return config.CoreConfig{}, err
	}

	endpoint, err := messagesEndpoint(creds.BaseURL)
	if err != nil {
		return config.CoreConfig{}, err
	}

	body, err := json.Marshal(messagesRequest{
		Model:       creds.Model,
		MaxTokens:   defaultMaxTokens,
		Temperature: 0,
		System:      systemPrompt(),
		Messages: []message{{
			Role: "user",
			Content: []contentBlock{{
				Type: "text",
				Text: userPrompt(desensitized),
			}},
		}},
	})
	if err != nil {
		return config.CoreConfig{}, fmt.Errorf("marshal --ai parse request: %w", err)
	}
	if err := EnsureSecretFree(string(body)); err != nil {
		return config.CoreConfig{}, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return config.CoreConfig{}, fmt.Errorf("build --ai parse request: %w", err)
	}
	req.Header.Set("content-type", "application/json")
	req.Header.Set("accept", "application/json")
	req.Header.Set("x-api-key", creds.APIKey)
	req.Header.Set("anthropic-version", defaultAnthropicVersion)

	resp, err := c.doer.Do(req)
	if err != nil {
		return config.CoreConfig{}, fmt.Errorf("--ai parse request failed")
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		io.Copy(io.Discard, resp.Body)
		return config.CoreConfig{}, fmt.Errorf("--ai parse request failed with HTTP status %d", resp.StatusCode)
	}

	limited := io.LimitReader(resp.Body, 1<<20)
	var parsed messagesResponse
	dec := json.NewDecoder(limited)
	if err := dec.Decode(&parsed); err != nil {
		return config.CoreConfig{}, fmt.Errorf("--ai parse response was not valid Anthropic messages JSON")
	}
	if err := dec.Decode(&struct{}{}); err != io.EOF {
		return config.CoreConfig{}, fmt.Errorf("--ai parse response had trailing data")
	}

	text, err := responseText(parsed)
	if err != nil {
		return config.CoreConfig{}, err
	}
	core, err := ParseCoreJSON(text)
	if err != nil {
		return config.CoreConfig{}, err
	}
	if !coreHasAnyField(core) {
		return config.CoreConfig{}, fmt.Errorf("--ai parse response contained no profile fields")
	}
	return core, nil
}

func messagesEndpoint(rawBaseURL string) (string, error) {
	base, err := url.Parse(strings.TrimSpace(rawBaseURL))
	if err != nil {
		return "", fmt.Errorf("invalid --ai credential base URL")
	}
	if base.Scheme != "http" && base.Scheme != "https" || base.Host == "" {
		return "", fmt.Errorf("invalid --ai credential base URL")
	}
	base.User = nil
	base.RawQuery = ""
	base.Fragment = ""
	path := strings.TrimRight(base.Path, "/")
	switch {
	case path == "":
		base.Path = "/v1/messages"
	case strings.HasSuffix(path, "/v1/messages"):
		base.Path = path
	case strings.HasSuffix(path, "/v1"):
		base.Path = path + "/messages"
	default:
		base.Path = path + "/v1/messages"
	}
	return base.String(), nil
}

func systemPrompt() string {
	return strings.Join([]string{
		"Return only a JSON object with these optional string fields:",
		"base_url, api_key, model, small_fast_model, provider.",
		"Use only values present in the user's text.",
		"If the API key is represented by a placeholder like {{CLAUDECM_SECRET_1}}, return that placeholder as api_key.",
		"Do not include markdown, comments, explanations, nulls, arrays, nested objects, or extra fields.",
	}, " ")
}

func userPrompt(desensitized string) string {
	return "Parse this claudecm profile source text:\n" + desensitized
}

// EnsureSecretFree refuses text that still contains a secret-shaped token.
func EnsureSecretFree(text string) error {
	if secretShapePresent(text) {
		return fmt.Errorf("refusing to send --ai parse payload: desensitized text still contains a secret-shaped token")
	}
	if secretNamedAssignmentPresent(text) {
		return fmt.Errorf("refusing to send --ai parse payload: desensitized text still contains a secret-named assignment")
	}
	return nil
}

func secretShapePresent(text string) bool {
	for _, corePattern := range secretShapePatterns() {
		re := regexp.MustCompile(`(^|[^A-Za-z0-9_-])(` + corePattern + `)`)
		if re.FindStringIndex(text) != nil {
			return true
		}
	}
	for _, rawToken := range strings.Fields(text) {
		token := normalizeSecretToken(rawToken)
		if key, value, ok := strings.Cut(token, "="); ok && isAssignmentKey(key) && value != "" {
			token = normalizeSecretToken(value)
		}
		if isHighEntropySecretToken(token) {
			return true
		}
	}
	return false
}

func secretNamedAssignmentPresent(text string) bool {
	re := regexp.MustCompile(secretNamedAssignmentPattern())
	matches := re.FindAllStringSubmatch(text, -1)
	for _, match := range matches {
		if len(match) < 4 {
			continue
		}
		value := cleanAssignmentValue(match[3])
		if isSecretFieldName(match[2]) && value != "" && !isSecretPlaceholder(value) {
			return true
		}
	}
	return false
}

func secretNamedAssignmentPattern() string {
	valuePattern := `((?:\{\{CLAUDECM_SECRET_[0-9]+\}\}|"(?:\\.|[^"\\])*"|'[^'\n]*'|` + "`" + `[^` + "`" + `\n]*` + "`" + `|[^\s,;#}\]]+))`
	return `(?i)(^|[\s{[,;])(?:export[ \t]+)?["']?([A-Za-z][A-Za-z0-9 _-]{0,80})["']?[ \t]*[:=][ \t]*` + valuePattern
}

func isSecretFieldName(name string) bool {
	fields := normalizedNameFields(name)
	if len(fields) == 0 {
		return false
	}
	compact := strings.Join(fields, "")
	switch compact {
	case "apikey", "privatekey", "accesskey", "clientsecret":
		return true
	}
	for _, field := range fields {
		switch field {
		case "secret", "password", "passwd", "pwd", "token", "auth", "credential", "credentials":
			return true
		}
	}
	for i := 0; i+1 < len(fields); i++ {
		switch fields[i] + " " + fields[i+1] {
		case "api key", "private key", "access key", "client secret":
			return true
		}
	}
	return false
}

func normalizedNameFields(name string) []string {
	var b strings.Builder
	for _, r := range strings.ToLower(strings.TrimSpace(name)) {
		switch {
		case r >= 'a' && r <= 'z' || r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == '_' || r == '-' || r == ' ' || r == '\t':
			b.WriteByte(' ')
		}
	}
	return strings.Fields(b.String())
}

func cleanAssignmentValue(raw string) string {
	value := strings.TrimSpace(raw)
	if isSecretPlaceholder(value) {
		return value
	}
	switch {
	case strings.HasPrefix(value, `"`) && strings.HasSuffix(value, `"`):
		value = strings.Trim(value, `"`)
	case strings.HasPrefix(value, "'") && strings.HasSuffix(value, "'"):
		value = strings.Trim(value, "'")
	case strings.HasPrefix(value, "`") && strings.HasSuffix(value, "`"):
		value = strings.Trim(value, "`")
	default:
		value = strings.TrimRight(value, ".,)]")
	}
	return strings.TrimSpace(value)
}

func isSecretPlaceholder(value string) bool {
	return regexp.MustCompile(`^\{\{CLAUDECM_SECRET_[0-9]+\}\}$`).MatchString(strings.TrimSpace(value))
}

func secretShapePatterns() []string {
	return []string{
		`sk-[A-Za-z0-9][A-Za-z0-9._=/+-]{3,}`,
		`[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,}`,
		`(?i)(?:xox[baprs]?|gh[pousr]|pat|token)[_-][A-Za-z0-9][A-Za-z0-9._=-]{7,}`,
		`(?i)[A-Za-z0-9._-]{8,}token[A-Za-z0-9._-]{8,}`,
		`AIza[0-9A-Za-z_-]{35}`,
		`(?i)[0-9a-f]{40}`,
	}
}

func isAssignmentKey(text string) bool {
	for _, r := range text {
		switch {
		case r >= 'A' && r <= 'Z' || r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '_':
		default:
			return false
		}
	}
	return text != ""
}

func isHighEntropySecretToken(token string) bool {
	if len(token) < 32 || looksLikeURL(token) {
		return false
	}
	hasLetter := false
	hasDigit := false
	for _, r := range token {
		switch {
		case r >= 'A' && r <= 'Z' || r >= 'a' && r <= 'z':
			hasLetter = true
		case r >= '0' && r <= '9':
			hasDigit = true
		case r == '+' || r == '/' || r == '=' || r == '_' || r == '-':
		default:
			return false
		}
	}
	return hasLetter && hasDigit
}

func looksLikeURL(token string) bool {
	return strings.Contains(token, "://") || strings.HasPrefix(token, "/")
}

func normalizeSecretToken(secret string) string {
	return strings.Trim(strings.TrimSpace(secret), `"'`+"`"+`.,;:()[]{}<>`)
}

// ParseCoreJSON parses the LLM's strict core JSON object.
func ParseCoreJSON(text string) (config.CoreConfig, error) {
	dec := json.NewDecoder(strings.NewReader(strings.TrimSpace(text)))
	dec.DisallowUnknownFields()
	var raw map[string]string
	if err := dec.Decode(&raw); err != nil {
		return config.CoreConfig{}, fmt.Errorf("--ai parse response was not strict core JSON")
	}
	if err := dec.Decode(&struct{}{}); err != io.EOF {
		return config.CoreConfig{}, fmt.Errorf("--ai parse response had trailing data")
	}
	var core config.CoreConfig
	for key, value := range raw {
		switch key {
		case "base_url":
			core.BaseURL = value
		case "api_key":
			core.APIKey = value
		case "model":
			core.Model = value
		case "small_fast_model":
			core.SmallFastModel = value
		case "provider":
			core.Provider = value
		default:
			return config.CoreConfig{}, fmt.Errorf("--ai parse response contained unsupported field %q", key)
		}
	}
	return core, nil
}

func coreHasAnyField(core config.CoreConfig) bool {
	return core.BaseURL != "" ||
		core.APIKey != "" ||
		core.Model != "" ||
		core.SmallFastModel != "" ||
		core.Provider != ""
}

type messagesRequest struct {
	Model       string    `json:"model"`
	MaxTokens   int       `json:"max_tokens"`
	Temperature float64   `json:"temperature"`
	System      string    `json:"system"`
	Messages    []message `json:"messages"`
}

type message struct {
	Role    string         `json:"role"`
	Content []contentBlock `json:"content"`
}

type contentBlock struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type messagesResponse struct {
	ID           string          `json:"id,omitempty"`
	Type         string          `json:"type,omitempty"`
	Role         string          `json:"role,omitempty"`
	Model        string          `json:"model,omitempty"`
	Content      []contentBlock  `json:"content"`
	StopReason   string          `json:"stop_reason,omitempty"`
	StopSequence *string         `json:"stop_sequence,omitempty"`
	Usage        json.RawMessage `json:"usage,omitempty"`
	CreatedAt    time.Time       `json:"created_at,omitempty"`
}

func responseText(resp messagesResponse) (string, error) {
	var parts []string
	for _, block := range resp.Content {
		if block.Type == "text" {
			parts = append(parts, block.Text)
		}
	}
	if len(parts) == 0 {
		return "", fmt.Errorf("--ai parse response contained no text block")
	}
	return strings.TrimSpace(strings.Join(parts, "\n")), nil
}
