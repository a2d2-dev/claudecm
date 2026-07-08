// Package blobparse extracts claudecm profile fields from arbitrary pasted
// text while producing a secret-free copy of the same text.
package blobparse

import (
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/a2d2-dev/claudecm/internal/config"
	"github.com/a2d2-dev/claudecm/internal/envextract"
)

// Result is the deterministic output of Parse.
//
// CapturedSecrets maps each placeholder present in Desensitized back to the
// original secret value. Callers that later send Desensitized outside the
// process can keep this map local and re-inject only after the remote parse
// result returns.
type Result struct {
	Core            config.CoreConfig
	CapturedSecrets map[string]string
	Desensitized    string
}

// Parse extracts profile core fields from text and returns a desensitized copy.
//
// It performs no I/O, no network access, and uses only deterministic local
// heuristics.
func Parse(text string) Result {
	registry := newSecretRegistry()
	candidates := extractCandidates(text)
	core := buildCore(candidates)

	for _, candidate := range candidates {
		if candidate.kind == fieldAPIKey && candidate.value != "" {
			registry.placeholderFor(candidate.value)
		}
	}
	registerSecretNamedFields(text, registry)
	registerGenericSecrets(text, candidates, registry)

	desensitized := registry.desensitize(text)
	desensitized = scrubResidualSecretShapes(desensitized, candidates)

	return Result{
		Core:            core,
		CapturedSecrets: registry.captured,
		Desensitized:    desensitized,
	}
}

type fieldKind int

const (
	fieldBaseURL fieldKind = iota
	fieldAPIKey
	fieldModel
	fieldSmallFastModel
	fieldProvider
)

type fieldAlias struct {
	kind         fieldKind
	pattern      string
	priority     int
	providerHint string
}

type fieldCandidate struct {
	kind         fieldKind
	value        string
	priority     int
	position     int
	providerHint string
}

type selectedCandidate struct {
	value        string
	priority     int
	position     int
	providerHint string
	set          bool
}

func extractCandidates(text string) []fieldCandidate {
	valuePattern := `("(?:\\.|[^"\\])*"|'[^'\n]*'|` + "`" + `[^` + "`" + `\n]*` + "`" + `|[^\s,;#}\]]+)`
	candidates := []fieldCandidate{}
	for _, alias := range fieldAliases() {
		pattern := `(?i)(^|[\s{[,;])(?:export[ \t]+)?["']?` +
			alias.pattern + `["']?[ \t]*[:=][ \t]*` + valuePattern
		re := regexp.MustCompile(pattern)
		matches := re.FindAllStringSubmatchIndex(text, -1)
		for _, match := range matches {
			if len(match) < 6 || match[4] < 0 || match[5] < 0 {
				continue
			}
			value := cleanValue(text[match[4]:match[5]], alias.kind)
			if value == "" {
				continue
			}
			candidates = append(candidates, fieldCandidate{
				kind:         alias.kind,
				value:        value,
				priority:     alias.priority,
				position:     match[4],
				providerHint: alias.providerHint,
			})
		}
	}
	sort.SliceStable(candidates, func(i, j int) bool {
		return candidates[i].position < candidates[j].position
	})
	return candidates
}

func fieldAliases() []fieldAlias {
	return []fieldAlias{
		{kind: fieldBaseURL, pattern: exactAlias(envextract.EnvBaseURL), priority: 0, providerHint: "anthropic"},
		{kind: fieldBaseURL, pattern: exactAlias("OPENAI_BASE_URL"), priority: 1, providerHint: "openai-compatible"},
		{kind: fieldBaseURL, pattern: aliasWords("base", "url"), priority: 3},

		{kind: fieldAPIKey, pattern: exactAlias(envextract.EnvAuthToken), priority: 0, providerHint: "anthropic"},
		{kind: fieldAPIKey, pattern: exactAlias("ANTHROPIC_API_KEY"), priority: 1, providerHint: "anthropic"},
		{kind: fieldAPIKey, pattern: exactAlias("OPENAI_API_KEY"), priority: 1, providerHint: "openai-compatible"},
		{kind: fieldAPIKey, pattern: aliasWords("auth", "token"), priority: 3},
		{kind: fieldAPIKey, pattern: aliasWords("api", "key"), priority: 4},
		{kind: fieldAPIKey, pattern: aliasWords("access", "token"), priority: 5},

		{kind: fieldModel, pattern: exactAlias(envextract.EnvModel), priority: 0, providerHint: "anthropic"},
		{kind: fieldModel, pattern: exactAlias("CODEX_MODEL"), priority: 1, providerHint: "openai-compatible"},
		{kind: fieldModel, pattern: aliasWords("model"), priority: 3},

		{kind: fieldSmallFastModel, pattern: exactAlias(envextract.EnvSmallFastModel), priority: 0, providerHint: "anthropic"},
		{kind: fieldSmallFastModel, pattern: aliasWords("small", "fast", "model"), priority: 2},

		{kind: fieldProvider, pattern: exactAlias("CODEX_MODEL_PROVIDER"), priority: 0},
		{kind: fieldProvider, pattern: aliasWords("model", "provider"), priority: 1},
		{kind: fieldProvider, pattern: aliasWords("provider"), priority: 2},
	}
}

func exactAlias(name string) string {
	return regexp.QuoteMeta(name)
}

func aliasWords(words ...string) string {
	parts := make([]string, 0, len(words))
	for _, word := range words {
		parts = append(parts, regexp.QuoteMeta(word))
	}
	return strings.Join(parts, `[\s_-]*`)
}

func buildCore(candidates []fieldCandidate) config.CoreConfig {
	selected := map[fieldKind]selectedCandidate{}
	for _, candidate := range candidates {
		current := selected[candidate.kind]
		if !current.set ||
			candidate.priority < current.priority ||
			(candidate.priority == current.priority && candidate.position < current.position) {
			selected[candidate.kind] = selectedCandidate{
				value:        candidate.value,
				priority:     candidate.priority,
				position:     candidate.position,
				providerHint: candidate.providerHint,
				set:          true,
			}
		}
	}

	core := config.CoreConfig{}
	if c := selected[fieldBaseURL]; c.set {
		core.BaseURL = c.value
	}
	if c := selected[fieldAPIKey]; c.set {
		core.APIKey = c.value
	}
	if c := selected[fieldModel]; c.set {
		core.Model = c.value
	}
	if c := selected[fieldSmallFastModel]; c.set {
		core.SmallFastModel = c.value
	}
	if c := selected[fieldProvider]; c.set {
		core.Provider = c.value
	} else {
		core.Provider = inferProvider(selected)
	}
	return core
}

func inferProvider(selected map[fieldKind]selectedCandidate) string {
	if c := selected[fieldAPIKey]; c.set && c.providerHint != "" {
		return c.providerHint
	}
	if c := selected[fieldBaseURL]; c.set && c.providerHint != "" {
		return c.providerHint
	}
	if c := selected[fieldModel]; c.set && c.providerHint != "" {
		return c.providerHint
	}
	return ""
}

func cleanValue(raw string, kind fieldKind) string {
	value := strings.TrimSpace(raw)
	if value == "" {
		return ""
	}

	switch {
	case strings.HasPrefix(value, `"`) && strings.HasSuffix(value, `"`):
		if unquoted, err := strconv.Unquote(value); err == nil {
			return unquoted
		}
		return strings.Trim(value, `"`)
	case strings.HasPrefix(value, "'") && strings.HasSuffix(value, "'"):
		return strings.Trim(value, "'")
	case strings.HasPrefix(value, "`") && strings.HasSuffix(value, "`"):
		return strings.Trim(value, "`")
	default:
		if kind == fieldAPIKey {
			return value
		}
		return strings.TrimRight(value, ".,)]}")
	}
}

type secretRegistry struct {
	bySecret map[string]string
	captured map[string]string
	next     int
}

func newSecretRegistry() *secretRegistry {
	return &secretRegistry{
		bySecret: map[string]string{},
		captured: map[string]string{},
	}
}

func (r *secretRegistry) placeholderFor(secret string) string {
	secret = strings.TrimSpace(secret)
	if secret == "" {
		return ""
	}
	if placeholder, ok := r.bySecret[secret]; ok {
		return placeholder
	}
	r.next++
	placeholder := fmt.Sprintf("{{CLAUDECM_SECRET_%d}}", r.next)
	r.bySecret[secret] = placeholder
	r.captured[placeholder] = secret
	return placeholder
}

func (r *secretRegistry) placeholderForNormalized(secret string) string {
	return r.placeholderFor(normalizeSecretToken(secret))
}

func (r *secretRegistry) desensitize(text string) string {
	secrets := make([]string, 0, len(r.bySecret))
	for secret := range r.bySecret {
		secrets = append(secrets, secret)
	}
	sort.Slice(secrets, func(i, j int) bool {
		if len(secrets[i]) == len(secrets[j]) {
			return secrets[i] < secrets[j]
		}
		return len(secrets[i]) > len(secrets[j])
	})

	desensitized := text
	for _, secret := range secrets {
		desensitized = strings.ReplaceAll(desensitized, secret, r.bySecret[secret])
	}
	return desensitized
}

func registerGenericSecrets(text string, candidates []fieldCandidate, registry *secretRegistry) {
	for _, corePattern := range secretShapePatterns() {
		re := regexp.MustCompile(`(^|[^A-Za-z0-9_-])(` + corePattern + `)`)
		matches := re.FindAllStringSubmatchIndex(text, -1)
		for _, match := range matches {
			if len(match) < 6 || match[4] < 0 || match[5] < 0 {
				continue
			}
			registry.placeholderForNormalized(text[match[4]:match[5]])
		}
	}
	registerHighEntropySecrets(text, candidates, registry)
}

func registerSecretNamedFields(text string, registry *secretRegistry) {
	for _, assignment := range secretNamedAssignments(text) {
		if !isSecretFieldName(assignment.name) {
			continue
		}
		value := cleanValue(assignment.value, fieldAPIKey)
		if value == "" {
			continue
		}
		registry.placeholderFor(value)
	}
}

type secretNamedAssignment struct {
	name  string
	value string
}

func secretNamedAssignments(text string) []secretNamedAssignment {
	prefixRe := regexp.MustCompile(`(?i)(^|[\s{[,;])(?:export[ \t]+)?["']?([A-Za-z][A-Za-z0-9 _-]{0,80})["']?[ \t]*[:=][ \t]*`)
	var out []secretNamedAssignment
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSuffix(line, "\r")
		matches := prefixRe.FindAllStringSubmatchIndex(line, -1)
		for _, match := range matches {
			if len(match) < 6 || match[4] < 0 || match[5] < 0 {
				continue
			}
			out = append(out, secretNamedAssignment{
				name:  line[match[4]:match[5]],
				value: assignmentLineValue(line[match[1]:]),
			})
		}
	}
	return out
}

func assignmentLineValue(raw string) string {
	value := strings.TrimSpace(raw)
	if value == "" {
		return ""
	}
	switch value[0] {
	case '"':
		return consumeQuotedValue(value, '"', true)
	case '\'':
		return consumeQuotedValue(value, '\'', false)
	case '`':
		return consumeQuotedValue(value, '`', false)
	default:
		return value
	}
}

func consumeQuotedValue(value string, quote byte, allowEscape bool) string {
	escaped := false
	for i := 1; i < len(value); i++ {
		if allowEscape && !escaped && value[i] == '\\' {
			escaped = true
			continue
		}
		if !escaped && value[i] == quote {
			return value[:i+1]
		}
		escaped = false
	}
	return value
}

func isSecretFieldName(name string) bool {
	fields := normalizedNameFields(name)
	if len(fields) == 0 {
		return false
	}
	compact := strings.Join(fields, "")
	for _, marker := range secretFieldNameMarkers() {
		if compact == marker || strings.HasPrefix(compact, marker) || strings.Contains(compact, marker) {
			return true
		}
	}
	return false
}

func secretFieldNameMarkers() []string {
	return []string{
		"secret",
		"password",
		"passwd",
		"pwd",
		"token",
		"apikey",
		"auth",
		"authorization",
		"credential",
		"credentials",
		"privatekey",
		"accesskey",
		"clientsecret",
	}
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

func scrubResidualSecretShapes(text string, candidates []fieldCandidate) string {
	desensitized := text
	for _, corePattern := range secretShapePatterns() {
		re := regexp.MustCompile(`(^|[^A-Za-z0-9_-])(` + corePattern + `)`)
		desensitized = re.ReplaceAllString(desensitized, `${1}`)
	}
	return scrubResidualHighEntropyTokens(desensitized, candidates)
}

func registerHighEntropySecrets(text string, candidates []fieldCandidate, registry *secretRegistry) {
	for _, rawToken := range strings.Fields(text) {
		token := highEntropyTokenCandidate(rawToken)
		if isHighEntropySecretToken(token, candidates) {
			registry.placeholderFor(token)
		}
	}
}

func scrubResidualHighEntropyTokens(text string, candidates []fieldCandidate) string {
	fields := strings.Fields(text)
	if len(fields) == 0 {
		return text
	}

	desensitized := text
	for _, rawToken := range fields {
		token := highEntropyTokenCandidate(rawToken)
		if !isHighEntropySecretToken(token, candidates) {
			continue
		}
		desensitized = strings.ReplaceAll(desensitized, token, "")
	}
	return desensitized
}

func highEntropyTokenCandidate(rawToken string) string {
	token := normalizeSecretToken(rawToken)
	if key, value, ok := strings.Cut(token, "="); ok && isAssignmentKey(key) && value != "" {
		return normalizeSecretToken(value)
	}
	return token
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

func isHighEntropySecretToken(token string, candidates []fieldCandidate) bool {
	if len(token) < 32 {
		return false
	}
	if isKnownBaseURLValue(token, candidates) || looksLikeURL(token) {
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

func isKnownBaseURLValue(token string, candidates []fieldCandidate) bool {
	for _, candidate := range candidates {
		if candidate.kind == fieldBaseURL && candidate.value == token {
			return true
		}
	}
	return false
}

func looksLikeURL(token string) bool {
	// The high-entropy fallback intentionally leaves URL-looking values alone,
	// even if they contain token-like path text, so base_url extraction is not
	// damaged before downstream provider inference. Bare base64 can contain '/',
	// so only clear URLs or path-shaped values are excluded here.
	return strings.Contains(token, "://") || strings.HasPrefix(token, "/")
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

func normalizeSecretToken(secret string) string {
	return strings.Trim(strings.TrimSpace(secret), `"'`+"`"+`.,;:()[]{}<>`)
}
