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
	registerGenericSecrets(text, registry)

	desensitized := registry.desensitize(text)
	desensitized = scrubResidualSecretShapes(desensitized)

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
			value := cleanValue(text[match[4]:match[5]])
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
		core.Provider = inferProvider(candidates)
	}
	return core
}

func inferProvider(candidates []fieldCandidate) string {
	for _, candidate := range candidates {
		if candidate.providerHint != "" {
			return candidate.providerHint
		}
	}
	return ""
}

func cleanValue(raw string) string {
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
	secret = normalizeSecretToken(secret)
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

func registerGenericSecrets(text string, registry *secretRegistry) {
	for _, corePattern := range secretShapePatterns() {
		re := regexp.MustCompile(`(^|[^A-Za-z0-9_-])(` + corePattern + `)`)
		matches := re.FindAllStringSubmatchIndex(text, -1)
		for _, match := range matches {
			if len(match) < 6 || match[4] < 0 || match[5] < 0 {
				continue
			}
			registry.placeholderFor(text[match[4]:match[5]])
		}
	}
}

func scrubResidualSecretShapes(text string) string {
	desensitized := text
	for _, corePattern := range secretShapePatterns() {
		re := regexp.MustCompile(`(^|[^A-Za-z0-9_-])(` + corePattern + `)`)
		desensitized = re.ReplaceAllString(desensitized, `${1}`)
	}
	return desensitized
}

func secretShapePatterns() []string {
	return []string{
		`sk-[A-Za-z0-9][A-Za-z0-9._=/+-]{3,}`,
		`[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,}`,
		`(?i)(?:xox[baprs]?|gh[pousr]|pat|token)[_-][A-Za-z0-9][A-Za-z0-9._=-]{7,}`,
		`(?i)[A-Za-z0-9._-]{8,}token[A-Za-z0-9._-]{8,}`,
	}
}

func normalizeSecretToken(secret string) string {
	return strings.Trim(strings.TrimSpace(secret), `"'`+"`"+`.,;:)]}`)
}
