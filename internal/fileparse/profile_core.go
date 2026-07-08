package fileparse

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"

	toml "github.com/pelletier/go-toml/v2"
	"gopkg.in/yaml.v3"

	"github.com/a2d2-dev/claudecm/internal/config"
)

const maxProfileCoreFileBytes = 1 << 20

var (
	errUnrecognizedFormat = errors.New("unrecognized config format")
	errInvalidQuotedValue = errors.New("invalid quoted value")
)

// ParseProfileCoreFile reads path once and parses known local config
// formats into profile core fields. It is read-only: all writes remain
// owned by cmd/add's normal SaveProfile path.
func ParseProfileCoreFile(path string) (config.CoreConfig, error) {
	clean := strings.TrimSpace(path)
	if clean == "" {
		return config.CoreConfig{}, fmt.Errorf("cannot read file: path is empty")
	}
	body, err := os.ReadFile(clean)
	if err != nil {
		return config.CoreConfig{}, fmt.Errorf("cannot read file %q: %w", clean, err)
	}
	if len(body) > maxProfileCoreFileBytes {
		return config.CoreConfig{}, fmt.Errorf("cannot read file %q: file is larger than %d bytes", clean, maxProfileCoreFileBytes)
	}
	core, err := ParseProfileCoreBytes(filepath.Ext(clean), body)
	if err != nil {
		if errors.Is(err, errUnrecognizedFormat) {
			return config.CoreConfig{}, err
		}
		return config.CoreConfig{}, fmt.Errorf("%w: %s", errUnrecognizedFormat, err)
	}
	return core, nil
}

// ParseProfileCoreBytes auto-detects dotenv, shell export, JSON, YAML,
// and TOML content and maps recognized vocabulary into CoreConfig.
func ParseProfileCoreBytes(ext string, body []byte) (config.CoreConfig, error) {
	if !utf8.Valid(body) || bytes.IndexByte(body, 0) >= 0 {
		return config.CoreConfig{}, errUnrecognizedFormat
	}
	trimmed := strings.TrimSpace(string(body))
	if trimmed == "" {
		return config.CoreConfig{}, errUnrecognizedFormat
	}

	parsers := orderedParsers(ext)
	var lastErr error
	for _, parser := range parsers {
		values, err := parser(trimmed)
		if err != nil {
			if errors.Is(err, errInvalidQuotedValue) {
				return config.CoreConfig{}, err
			}
			lastErr = err
			continue
		}
		core, err := coreFromValues(values)
		if err != nil {
			return config.CoreConfig{}, err
		}
		if !coreHasAnyField(core) {
			lastErr = fmt.Errorf("no usable profile fields found")
			continue
		}
		return core, nil
	}
	if lastErr != nil {
		return config.CoreConfig{}, fmt.Errorf("%w: %s", errUnrecognizedFormat, lastErr)
	}
	return config.CoreConfig{}, errUnrecognizedFormat
}

type valuesParser func(string) (map[string]string, error)

func orderedParsers(ext string) []valuesParser {
	switch strings.ToLower(strings.TrimSpace(ext)) {
	case ".json":
		return []valuesParser{parseJSONValues, parseKeyValueLines, parseYAMLValues, parseTOMLValues}
	case ".yaml", ".yml":
		return []valuesParser{parseYAMLValues, parseKeyValueLines, parseJSONValues, parseTOMLValues}
	case ".toml":
		return []valuesParser{parseTOMLValues, parseKeyValueLines, parseJSONValues, parseYAMLValues}
	case ".env":
		return []valuesParser{parseKeyValueLines, parseJSONValues, parseYAMLValues, parseTOMLValues}
	default:
		return []valuesParser{parseKeyValueLines, parseJSONValues, parseYAMLValues, parseTOMLValues}
	}
}

func parseJSONValues(text string) (map[string]string, error) {
	var root any
	dec := json.NewDecoder(strings.NewReader(text))
	dec.UseNumber()
	if err := dec.Decode(&root); err != nil {
		return nil, err
	}
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("json has trailing content")
	}
	out := map[string]string{}
	flattenValues(out, "", root)
	if len(out) == 0 {
		return nil, fmt.Errorf("json object has no scalar values")
	}
	return out, nil
}

func parseYAMLValues(text string) (map[string]string, error) {
	var root any
	if err := yaml.Unmarshal([]byte(text), &root); err != nil {
		return nil, err
	}
	out := map[string]string{}
	flattenValues(out, "", root)
	if len(out) == 0 {
		return nil, fmt.Errorf("yaml document has no scalar values")
	}
	return out, nil
}

func parseTOMLValues(text string) (map[string]string, error) {
	var root map[string]any
	if err := toml.Unmarshal([]byte(text), &root); err != nil {
		return nil, err
	}
	out := map[string]string{}
	flattenValues(out, "", root)
	if len(out) == 0 {
		return nil, fmt.Errorf("toml document has no scalar values")
	}
	return out, nil
}

func parseKeyValueLines(text string) (map[string]string, error) {
	out := map[string]string{}
	seenAssignment := false
	for lineNo, raw := range strings.Split(text, "\n") {
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if strings.HasPrefix(line, "export ") {
			line = strings.TrimSpace(strings.TrimPrefix(line, "export "))
		}
		idx := strings.Index(line, "=")
		if idx < 0 {
			return nil, fmt.Errorf("line %d is not key=value", lineNo+1)
		}
		key := strings.TrimSpace(line[:idx])
		if key == "" || strings.ContainsAny(key, " \t") {
			return nil, fmt.Errorf("line %d has invalid key", lineNo+1)
		}
		value := strings.TrimSpace(line[idx+1:])
		unquoted, err := unquoteValue(value)
		if err != nil {
			return nil, fmt.Errorf("line %d has %w: %v", lineNo+1, errInvalidQuotedValue, err)
		}
		out[key] = unquoted
		seenAssignment = true
	}
	if !seenAssignment {
		return nil, fmt.Errorf("no key=value assignments found")
	}
	return out, nil
}

func unquoteValue(value string) (string, error) {
	value = strings.TrimSpace(stripInlineComment(value))
	if value == "" {
		return value, nil
	}
	if value[0] == '\'' {
		if len(value) < 2 || value[len(value)-1] != '\'' {
			return "", fmt.Errorf("unclosed single quote")
		}
		return value[1 : len(value)-1], nil
	}
	if value[0] == '"' {
		if len(value) < 2 || value[len(value)-1] != '"' {
			return "", fmt.Errorf("unclosed double quote")
		}
		return strconv.Unquote(value)
	}
	return value, nil
}

func stripInlineComment(value string) string {
	inSingle := false
	inDouble := false
	escaped := false
	for i, r := range value {
		switch {
		case escaped:
			escaped = false
		case r == '\\' && inDouble:
			escaped = true
		case r == '\'' && !inDouble:
			inSingle = !inSingle
		case r == '"' && !inSingle:
			inDouble = !inDouble
		case r == '#' && !inSingle && !inDouble:
			if i == 0 || value[i-1] == ' ' || value[i-1] == '\t' {
				return strings.TrimSpace(value[:i])
			}
		}
	}
	return value
}

func flattenValues(out map[string]string, prefix string, value any) {
	switch v := value.(type) {
	case map[string]any:
		for k, child := range v {
			flattenValues(out, joinKey(prefix, k), child)
		}
	case map[any]any:
		for k, child := range v {
			flattenValues(out, joinKey(prefix, fmt.Sprint(k)), child)
		}
	case []any:
		return
	case nil:
		return
	case string:
		out[prefix] = v
	case bool:
		out[prefix] = strconv.FormatBool(v)
	case int:
		out[prefix] = strconv.Itoa(v)
	case int64:
		out[prefix] = strconv.FormatInt(v, 10)
	case float64:
		out[prefix] = strconv.FormatFloat(v, 'g', -1, 64)
	case json.Number:
		out[prefix] = v.String()
	default:
		out[prefix] = fmt.Sprint(v)
	}
}

func joinKey(prefix, key string) string {
	if prefix == "" {
		return key
	}
	return prefix + "." + key
}

func coreFromValues(values map[string]string) (config.CoreConfig, error) {
	var core config.CoreConfig
	assignments := canonicalAssignments(values)
	for _, canonical := range sortedStringKeys(assignments) {
		selected, err := selectCanonicalValue(canonical, assignments[canonical])
		if err != nil {
			return config.CoreConfig{}, err
		}
		assignCoreField(&core, canonical, selected.value)
	}
	if core.Provider == "openai" {
		core.Provider = "openai-compat"
	}
	return core, nil
}

type fieldAssignment struct {
	key   string
	value string
}

func canonicalAssignments(values map[string]string) map[string][]fieldAssignment {
	out := map[string][]fieldAssignment{}
	for key, value := range values {
		canonical := canonicalFieldKey(key)
		if canonical == "" {
			continue
		}
		out[canonical] = append(out[canonical], fieldAssignment{key: key, value: value})
	}
	for canonical := range out {
		sort.Slice(out[canonical], func(i, j int) bool {
			return out[canonical][i].key < out[canonical][j].key
		})
	}
	return out
}

func selectCanonicalValue(canonical string, assignments []fieldAssignment) (fieldAssignment, error) {
	var selected fieldAssignment
	for _, assignment := range assignments {
		if strings.TrimSpace(assignment.value) == "" {
			continue
		}
		if selected.key == "" {
			selected = assignment
			continue
		}
		if assignment.value != selected.value {
			return fieldAssignment{}, fmt.Errorf("conflicting values for %s: %s=%s conflicts with %s=%s",
				canonical,
				selected.key,
				displayParsedValue(canonical, selected.value),
				assignment.key,
				displayParsedValue(canonical, assignment.value))
		}
	}
	if selected.key != "" {
		return selected, nil
	}
	if len(assignments) == 0 {
		return fieldAssignment{}, nil
	}
	return assignments[0], nil
}

func assignCoreField(core *config.CoreConfig, key, value string) {
	switch key {
	case "provider":
		core.Provider = value
	case "base_url":
		core.BaseURL = value
	case "api_key":
		core.APIKey = value
	case "model":
		core.Model = value
	case "small_fast_model":
		core.SmallFastModel = value
	}
}

func displayParsedValue(canonical, value string) string {
	if canonical == "api_key" {
		return redactParsedSecret(value)
	}
	return strconv.Quote(value)
}

func redactParsedSecret(value string) string {
	if len(value) >= 8 {
		return strconv.Quote(value[:4] + "***" + value[len(value)-4:])
	}
	return strconv.Quote("***")
}

func sortedStringKeys[T any](m map[string]T) []string {
	keys := make([]string, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func canonicalFieldKey(key string) string {
	normalized := strings.ToLower(strings.TrimSpace(key))
	normalized = strings.TrimPrefix(normalized, "env.")
	switch normalized {
	case "provider", "core.provider", "model_provider", "codex_model_provider":
		return "provider"
	case "base_url", "baseurl", "core.base_url", "anthropic_base_url", "openai_base_url":
		return "base_url"
	case "api_key", "apikey", "auth_token", "token", "core.api_key",
		"anthropic_auth_token", "anthropic_api_key", "openai_api_key":
		return "api_key"
	case "model", "core.model", "anthropic_model", "codex_model":
		return "model"
	case "small_fast_model", "smallfastmodel", "core.small_fast_model",
		"anthropic_small_fast_model":
		return "small_fast_model"
	default:
		return ""
	}
}

func coreHasAnyField(core config.CoreConfig) bool {
	return core.Provider != "" ||
		core.BaseURL != "" ||
		core.APIKey != "" ||
		core.Model != "" ||
		core.SmallFastModel != ""
}
