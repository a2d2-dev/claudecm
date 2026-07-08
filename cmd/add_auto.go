package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"strings"
	"time"
	"unicode"

	"github.com/spf13/cobra"

	claudecodeadapter "github.com/a2d2-dev/claudecm/internal/adapter/claudecode"
	codexadapter "github.com/a2d2-dev/claudecm/internal/adapter/codex"
	codextoml "github.com/a2d2-dev/claudecm/internal/adapter/codex/toml"
	"github.com/a2d2-dev/claudecm/internal/blobparse"
	"github.com/a2d2-dev/claudecm/internal/config"
	"github.com/a2d2-dev/claudecm/internal/storage"
	"github.com/a2d2-dev/claudecm/internal/writepath"
)

type addAutoSource string

const (
	addAutoSourceClipboard  addAutoSource = "clipboard"
	addAutoSourceEnv        addAutoSource = "environment"
	addAutoSourceClaudeCode addAutoSource = "claude-code"
	addAutoSourceCodex      addAutoSource = "codex"
)

type addAutoCandidate struct {
	Source      string
	NameBase    string
	Sources     []string
	Core        config.CoreConfig
	Tools       map[config.ToolID]config.ToolOverlay
	ProfileName string

	AlreadyProfile string
	DuplicateOf    string
}

type addAutoSourceResult struct {
	Source     string
	Candidates []addAutoCandidate
	Note       string
}

type addAutoClipboardReader func() (string, bool, error)

type addAutoProfileStore interface {
	LoadAllProfiles() ([]*config.Profile, error)
	ProfileExists(name string) (bool, error)
	SaveProfile(profile *config.Profile) error
}

func runAddAuto(
	cmd *cobra.Command,
	resv *storage.Resolver,
	store addAutoProfileStore,
	format addOutputFormat,
) error {
	results := sweepAddAutoSources(context.Background(), resv, readClipboardText)
	if err := markExistingAddAutoCandidates(results, store); err != nil {
		return err
	}
	markDuplicateAddAutoCandidates(results)
	if err := renderAddAutoDiscovery(cmd.OutOrStdout(), format, results); err != nil {
		return err
	}

	newCandidates, err := newAddAutoCandidates(results)
	if err != nil {
		return err
	}
	switch {
	case len(newCandidates) == 0 && countAddAutoCandidatesWithKey(results) == 0:
		return fmt.Errorf("no credentials with API keys found in swept sources: %s", addAutoSourceList(results))
	case len(newCandidates) == 0:
		if format == addOutputText {
			fmt.Fprintln(cmd.OutOrStdout(), "all discovered credentials are already recorded")
		} else if err := renderAddAutoResultJSON(cmd.OutOrStdout(), "already-recorded", results, nil, nil); err != nil {
			return err
		}
		return nil
	}

	existingNames, err := loadAddAutoProfileNames(store)
	if err != nil {
		return err
	}
	assignAddAutoProfileNames(newCandidates, existingNames)

	if addDryRunFlag {
		profiles := buildAddAutoProfiles(newCandidates, nowFn().UTC())
		return renderAddAutoDryRun(cmd.OutOrStdout(), format, results, profiles)
	}
	if !addYesFlag {
		if !isTerminal(os.Stdin) {
			profiles := buildAddAutoProfiles(newCandidates, nowFn().UTC())
			if format == addOutputText {
				fmt.Fprintln(cmd.OutOrStdout(), "profiles to create:")
				renderAddAutoProfileTextList(cmd.OutOrStdout(), profiles)
			} else if err := renderAddAutoResultJSON(cmd.OutOrStdout(), "confirm-required", results, profiles, nil); err != nil {
				return err
			}
			return fmt.Errorf("non-interactive session: pass --yes to create discovered profiles or --dry-run to preview")
		}
		if err := promptAddAutoProfileNames(cmd.OutOrStdout(), os.Stdin, newCandidates, existingNames); err != nil {
			return err
		}
	}
	profiles := buildAddAutoProfiles(newCandidates, nowFn().UTC())
	if !addYesFlag {
		if format == addOutputText {
			fmt.Fprintln(cmd.OutOrStdout(), "profiles to create:")
			renderAddAutoProfileTextList(cmd.OutOrStdout(), profiles)
		}
		ok, promptErr := promptConfirm(cmd.OutOrStdout(), os.Stdin, "Create these profiles?")
		if promptErr != nil {
			return fmt.Errorf("read confirmation: %w", promptErr)
		}
		if !ok {
			return fmt.Errorf("auto add refused by user")
		}
	}

	created := make([]*config.Profile, 0, len(profiles))
	for _, profile := range profiles {
		if err := storage.ValidateProfileName(profile.Name); err != nil {
			return fmt.Errorf("derived profile name %q is invalid: %w", profile.Name, err)
		}
		if exists, err := store.ProfileExists(profile.Name); err != nil {
			return fmt.Errorf("failed to check whether profile %q exists: %w", profile.Name, err)
		} else if exists {
			return fmt.Errorf("derived profile name %q already exists", profile.Name)
		}
		if err := store.SaveProfile(profile); err != nil {
			return addAutoPartialCreateError(created, profile.Name, err)
		}
		created = append(created, profile)
	}
	return renderAddAutoCreated(cmd.OutOrStdout(), format, results, created)
}

func sweepAddAutoSources(ctx context.Context, resv *storage.Resolver, clipboard addAutoClipboardReader) []addAutoSourceResult {
	results := make([]addAutoSourceResult, 0, len(addAutoSourceOrder()))
	results = append(results, scanAddAutoClipboard(clipboard))
	results = append(results, scanAddAutoEnv())
	results = append(results, scanAddAutoClaudeCode(ctx, resv))
	results = append(results, scanAddAutoCodex(ctx, resv))
	return results
}

func addAutoSourceOrder() []addAutoSource {
	return []addAutoSource{
		addAutoSourceClipboard,
		addAutoSourceEnv,
		addAutoSourceClaudeCode,
		addAutoSourceCodex,
	}
}

func scanAddAutoClipboard(reader addAutoClipboardReader) addAutoSourceResult {
	result := addAutoSourceResult{Source: string(addAutoSourceClipboard)}
	text, ok, err := reader()
	if err != nil {
		result.Note = "skipped: " + err.Error()
		return result
	}
	if !ok || strings.TrimSpace(text) == "" {
		result.Note = "skipped: no clipboard text available"
		return result
	}
	parsed := normalizeParsedProvider(blobparse.Parse(text).Core)
	if strings.TrimSpace(parsed.APIKey) == "" {
		result.Note = "no API key found"
		return result
	}
	result.Candidates = []addAutoCandidate{{
		Source:   result.Source,
		NameBase: addAutoBaseNameFor(result.Source, parsed.BaseURL),
		Sources:  []string{result.Source},
		Core:     parsed,
	}}
	return result
}

func scanAddAutoEnv() addAutoSourceResult {
	result := addAutoSourceResult{Source: string(addAutoSourceEnv)}
	candidates := scanAddAutoEnvCandidates()
	if len(candidates) == 0 {
		result.Note = "no API key found"
		return result
	}
	result.Candidates = candidates
	return result
}

func scanAddAutoEnvCandidates() []addAutoCandidate {
	var out []addAutoCandidate
	anthropic := config.CoreConfig{Provider: addProviderDefault}
	var anthropicTools map[config.ToolID]config.ToolOverlay
	if v := lookupNonEmptyEnv("ANTHROPIC_BASE_URL"); v != "" {
		anthropic.BaseURL = v
	}
	if v := lookupNonEmptyEnv("ANTHROPIC_AUTH_TOKEN"); v != "" {
		anthropic.APIKey = v
	}
	if v := lookupNonEmptyEnv("ANTHROPIC_API_KEY"); v != "" {
		if anthropic.APIKey == "" {
			anthropic.APIKey = v
		} else {
			anthropicTools = putClaudeCodeEnv(anthropicTools, "ANTHROPIC_API_KEY", v)
		}
	}
	if v := lookupNonEmptyEnv("ANTHROPIC_MODEL"); v != "" {
		anthropic.Model = v
	}
	if v := lookupNonEmptyEnv("ANTHROPIC_SMALL_FAST_MODEL"); v != "" {
		anthropic.SmallFastModel = v
	}
	if strings.TrimSpace(anthropic.APIKey) != "" {
		out = append(out, addAutoCandidate{
			Source:   string(addAutoSourceEnv),
			NameBase: addAutoBaseNameFor(string(addAutoSourceEnv), anthropic.BaseURL),
			Sources:  []string{string(addAutoSourceEnv)},
			Core:     anthropic,
			Tools:    anthropicTools,
		})
	}

	codex := config.CoreConfig{Provider: "openai-compat"}
	if v := lookupNonEmptyEnv("OPENAI_BASE_URL"); v != "" {
		codex.BaseURL = v
	}
	if v := lookupNonEmptyEnv("OPENAI_API_KEY"); v != "" {
		codex.APIKey = v
	}
	if v := lookupNonEmptyEnv("CODEX_MODEL"); v != "" {
		codex.Model = v
	}
	if v := normalizeCodexProvider(lookupNonEmptyEnv("CODEX_MODEL_PROVIDER")); v != "" {
		codex.Provider = v
	}
	if strings.TrimSpace(codex.APIKey) != "" {
		out = append(out, addAutoCandidate{
			Source:   string(addAutoSourceEnv),
			NameBase: addAutoBaseNameFor(string(addAutoSourceEnv), codex.BaseURL),
			Sources:  []string{string(addAutoSourceEnv)},
			Core:     codex,
		})
	}
	return out
}

func coreFromAddAutoAdapterOverlay(core config.CoreConfig, overlay config.ToolOverlay) config.CoreConfig {
	providerRaw := firstStringRawValue(overlay.Raw, "model_provider")
	if strings.TrimSpace(core.BaseURL) == "" {
		if providerRaw != "" {
			core.BaseURL = firstStringRawValue(overlay.Raw, "model_providers."+providerRaw+".base_url")
		}
		if strings.TrimSpace(core.BaseURL) == "" {
			core.BaseURL = firstStringRawValue(overlay.Raw, "model_providers.openai.base_url")
		}
	}
	if strings.TrimSpace(core.Model) == "" {
		core.Model = firstStringRawValue(overlay.Raw, "model")
	}
	if strings.TrimSpace(core.Provider) == "" {
		core.Provider = normalizeParsedProvider(config.CoreConfig{
			Provider: providerRaw,
		}).Provider
	}
	return core
}

func firstStringRawValue(raw map[string]any, key string) string {
	if len(raw) == 0 {
		return ""
	}
	if v, ok := raw[key]; ok {
		return strings.TrimSpace(fmt.Sprint(v))
	}
	return ""
}

func scanAddAutoClaudeCode(ctx context.Context, resv *storage.Resolver) addAutoSourceResult {
	result := addAutoSourceResult{Source: string(addAutoSourceClaudeCode)}
	core, overlay, note := lenientReadClaudeCode(ctx, resv)
	if note != "" {
		result.Note = note
	}
	if strings.TrimSpace(core.APIKey) == "" {
		if result.Note == "" {
			result.Note = "no API key found"
		}
		return result
	}
	tools := map[config.ToolID]config.ToolOverlay{}
	if !isEmptyOverlay(overlay) {
		tools[config.ToolClaudeCode] = overlay
	}
	result.Candidates = []addAutoCandidate{{
		Source:   result.Source,
		NameBase: "claude-code",
		Sources:  []string{result.Source},
		Core:     normalizeParsedProvider(core),
		Tools:    tools,
	}}
	return result
}

func scanAddAutoCodex(ctx context.Context, resv *storage.Resolver) addAutoSourceResult {
	result := addAutoSourceResult{Source: string(addAutoSourceCodex)}
	core, overlay, note := lenientReadCodex(ctx, resv)
	if note != "" {
		result.Note = note
	}
	if strings.TrimSpace(core.APIKey) == "" {
		if result.Note == "" {
			result.Note = "no API key found"
		}
		return result
	}
	tools := map[config.ToolID]config.ToolOverlay{}
	if !isEmptyOverlay(overlay) {
		tools[config.ToolCodex] = overlay
	}
	result.Candidates = []addAutoCandidate{{
		Source:   result.Source,
		NameBase: "codex",
		Sources:  []string{result.Source},
		Core:     normalizeParsedProvider(coreFromAddAutoAdapterOverlay(core, overlay)),
		Tools:    tools,
	}}
	return result
}

func lenientReadClaudeCode(ctx context.Context, resv *storage.Resolver) (config.CoreConfig, config.ToolOverlay, string) {
	if err := ctx.Err(); err != nil {
		return config.CoreConfig{}, config.ToolOverlay{}, "skipped: " + err.Error()
	}
	path := claudecodeadapter.SettingsPath(resv)
	if err := claudecodeadapter.VerifyReadTargetInHome(path, resv); err != nil {
		if errors.Is(err, claudecodeadapter.ErrNoConfig) || errors.Is(err, os.ErrNotExist) {
			return config.CoreConfig{}, config.ToolOverlay{}, "skipped: settings.json not found"
		}
		return config.CoreConfig{}, config.ToolOverlay{}, "skipped: settings.json refused: " + err.Error()
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return config.CoreConfig{}, config.ToolOverlay{}, "skipped: settings.json not found"
		}
		return config.CoreConfig{}, config.ToolOverlay{}, "skipped: " + err.Error()
	}
	if len(strings.TrimSpace(string(data))) == 0 {
		return config.CoreConfig{}, config.ToolOverlay{}, "no API key found"
	}
	var root map[string]any
	if err := json.Unmarshal(data, &root); err != nil {
		return config.CoreConfig{}, config.ToolOverlay{}, "skipped: settings.json parse failed: " + err.Error()
	}
	env, _ := root["env"].(map[string]any)
	if len(env) == 0 {
		return config.CoreConfig{}, config.ToolOverlay{}, "no API key found"
	}

	core := config.CoreConfig{Provider: addProviderDefault}
	var overlay config.ToolOverlay
	if v, ok := stringMapValue(env, "ANTHROPIC_BASE_URL"); ok {
		core.BaseURL = v
	}
	if v, ok := stringMapValue(env, "ANTHROPIC_MODEL"); ok {
		core.Model = v
	}
	if v, ok := stringMapValue(env, "ANTHROPIC_SMALL_FAST_MODEL"); ok {
		core.SmallFastModel = v
	}
	authToken, hasAuth := stringMapValue(env, "ANTHROPIC_AUTH_TOKEN")
	apiKey, hasAPIKey := stringMapValue(env, "ANTHROPIC_API_KEY")
	switch {
	case hasAuth && hasAPIKey:
		core.APIKey = authToken
		overlay.ExtraEnv = map[string]string{"ANTHROPIC_API_KEY": apiKey}
	case hasAuth:
		core.APIKey = authToken
	case hasAPIKey:
		core.APIKey = apiKey
	}
	if v, ok := stringMapValue(env, "CLAUDE_CODE_USE_BEDROCK"); ok {
		if overlay.ExtraEnv == nil {
			overlay.ExtraEnv = map[string]string{}
		}
		overlay.ExtraEnv["CLAUDE_CODE_USE_BEDROCK"] = v
	}
	if v, ok := stringMapValue(env, "CLAUDE_CODE_USE_VERTEX"); ok {
		if overlay.ExtraEnv == nil {
			overlay.ExtraEnv = map[string]string{}
		}
		overlay.ExtraEnv["CLAUDE_CODE_USE_VERTEX"] = v
	}
	return core, overlay, ""
}

func lenientReadCodex(ctx context.Context, resv *storage.Resolver) (config.CoreConfig, config.ToolOverlay, string) {
	if err := ctx.Err(); err != nil {
		return config.CoreConfig{}, config.ToolOverlay{}, "skipped: " + err.Error()
	}
	var notes []string
	core := config.CoreConfig{Provider: "openai-compat"}
	var overlay config.ToolOverlay

	authPath := codexadapter.AuthPath(resv)
	authRoot, authNote := lenientReadJSONMap(authPath, "auth.json", resv, codexadapter.VerifyReadTargetInHome)
	if authNote != "" {
		notes = append(notes, authNote)
	}
	if authRoot != nil {
		if v, ok := stringMapValue(authRoot, "OPENAI_API_KEY"); ok {
			core.APIKey = v
		}
		flat, err := writepath.Flatten(authRoot)
		if err == nil {
			for _, key := range codexadapter.OwnedKeysAuthJSON {
				if key == "OPENAI_API_KEY" {
					continue
				}
				if v, ok := flat[key]; ok && v != nil {
					putOverlayRaw(&overlay, key, v)
				}
			}
		}
	}

	configPath := codexadapter.ConfigPath(resv)
	doc, configNote := lenientReadCodexConfig(configPath, resv)
	if configNote != "" {
		notes = append(notes, configNote)
	}
	if doc != nil {
		for _, key := range codexadapter.OwnedKeysConfigTOML {
			v, ok := doc.Get(key)
			if !ok {
				continue
			}
			putOverlayRaw(&overlay, key, v)
		}
	}

	return core, overlay, strings.Join(notes, "; ")
}

func lenientReadJSONMap(path, label string, resv *storage.Resolver, verify func(string, *storage.Resolver) error) (map[string]any, string) {
	if verify != nil {
		if err := verify(path, resv); err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return nil, label + " not found"
			}
			return nil, label + " skipped: read target refused: " + err.Error()
		}
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, label + " not found"
		}
		return nil, label + " skipped: " + err.Error()
	}
	if len(strings.TrimSpace(string(data))) == 0 {
		return nil, label + " empty"
	}
	var root map[string]any
	if err := json.Unmarshal(data, &root); err != nil {
		return nil, label + " skipped: parse failed: " + err.Error()
	}
	if root == nil {
		return nil, label + " skipped: top-level value is null"
	}
	return root, ""
}

func lenientReadCodexConfig(path string, resv *storage.Resolver) (*codextoml.Doc, string) {
	if err := codexadapter.VerifyReadTargetInHome(path, resv); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, "config.toml not found"
		}
		return nil, "config.toml skipped: read target refused: " + err.Error()
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, "config.toml not found"
		}
		return nil, "config.toml skipped: " + err.Error()
	}
	if len(strings.TrimSpace(string(data))) == 0 {
		return nil, "config.toml empty"
	}
	doc, err := codextoml.Load(data)
	if err != nil {
		doc = lenientExtractCodexConfig(data)
		if doc == nil {
			return nil, "config.toml skipped: parse failed: " + err.Error()
		}
		return doc, "config.toml partially read: parse failed: " + err.Error()
	}
	return doc, ""
}

func lenientExtractCodexConfig(data []byte) *codextoml.Doc {
	lines := strings.Split(string(data), "\n")
	values := map[string]any{}
	var section string
	for _, line := range lines {
		line = stripAddAutoTOMLComment(strings.TrimSpace(line))
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "[") && strings.Contains(line, "]") {
			section = strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(line[:strings.Index(line, "]")+1], "["), "]"))
			continue
		}
		if !strings.HasPrefix(section, "model_providers.") {
			continue
		}
		parts := strings.SplitN(line, "=", 2)
		if len(parts) != 2 {
			continue
		}
		key := strings.TrimSpace(parts[0])
		if key == "" {
			continue
		}
		value, ok := parseAddAutoTOMLString(strings.TrimSpace(parts[1]))
		if !ok {
			continue
		}
		values[section+"."+key] = value
	}
	if len(values) == 0 {
		return nil
	}
	bySection := map[string][]string{}
	for _, key := range codexadapter.OwnedKeysConfigTOML {
		if v, ok := values[key]; ok {
			section, leaf, ok := splitAddAutoTOMLPath(key)
			if !ok {
				continue
			}
			bySection[section] = append(bySection[section], fmt.Sprintf("%s = %q", leaf, fmt.Sprint(v)))
		}
	}
	var b strings.Builder
	for _, key := range codexadapter.OwnedKeysConfigTOML {
		section, _, ok := splitAddAutoTOMLPath(key)
		if !ok || len(bySection[section]) == 0 {
			continue
		}
		fmt.Fprintf(&b, "[%s]\n", section)
		for _, line := range bySection[section] {
			fmt.Fprintln(&b, line)
		}
		fmt.Fprintln(&b)
		delete(bySection, section)
	}
	if b.Len() == 0 {
		return nil
	}
	doc, err := codextoml.Load([]byte(b.String()))
	if err != nil {
		return nil
	}
	return doc
}

func splitAddAutoTOMLPath(path string) (string, string, bool) {
	idx := strings.LastIndex(path, ".")
	if idx <= 0 || idx == len(path)-1 {
		return "", "", false
	}
	return path[:idx], path[idx+1:], true
}

func stripAddAutoTOMLComment(line string) string {
	inSingle := false
	inDouble := false
	escaped := false
	for idx, r := range line {
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
			return strings.TrimSpace(line[:idx])
		}
	}
	return line
}

func parseAddAutoTOMLString(raw string) (string, bool) {
	if len(raw) < 2 {
		return "", false
	}
	if raw[0] == '"' {
		var out string
		if err := json.Unmarshal([]byte(raw), &out); err != nil {
			return "", false
		}
		return out, true
	}
	if raw[0] == '\'' && raw[len(raw)-1] == '\'' {
		return raw[1 : len(raw)-1], true
	}
	return "", false
}

func stringMapValue(m map[string]any, key string) (string, bool) {
	v, ok := m[key]
	if !ok || v == nil {
		return "", false
	}
	return strings.TrimSpace(fmt.Sprint(v)), true
}

func putOverlayRaw(overlay *config.ToolOverlay, key string, value any) {
	if overlay.Raw == nil {
		overlay.Raw = map[string]any{}
	}
	overlay.Raw[key] = value
}

func markExistingAddAutoCandidates(results []addAutoSourceResult, store addAutoProfileStore) error {
	if store == nil {
		return nil
	}
	profiles, err := store.LoadAllProfiles()
	if err != nil {
		return fmt.Errorf("load existing profiles for auto dedup: %w", err)
	}
	exactIndex := map[string]string{}
	anyKeyIndex := map[string]string{}
	emptyKeyIndex := map[string]string{}
	for _, profile := range profiles {
		if profile == nil || strings.TrimSpace(profile.Core.APIKey) == "" {
			continue
		}
		apiKey := strings.TrimSpace(profile.Core.APIKey)
		base := normalizeAddAutoBaseURL(profile.Core.BaseURL)
		if _, exists := anyKeyIndex[apiKey]; !exists {
			anyKeyIndex[apiKey] = profile.Name
		}
		if base == "" {
			if _, exists := emptyKeyIndex[apiKey]; !exists {
				emptyKeyIndex[apiKey] = profile.Name
			}
			continue
		}
		exactKey := base + "\x00" + apiKey
		if _, exists := exactIndex[exactKey]; !exists {
			exactIndex[exactKey] = profile.Name
		}
	}
	for resultIdx := range results {
		for candidateIdx := range results[resultIdx].Candidates {
			candidate := &results[resultIdx].Candidates[candidateIdx]
			apiKey := strings.TrimSpace(candidate.Core.APIKey)
			if apiKey == "" {
				continue
			}
			base := normalizeAddAutoBaseURL(candidate.Core.BaseURL)
			if base == "" {
				candidate.AlreadyProfile = anyKeyIndex[apiKey]
			} else if name := exactIndex[base+"\x00"+apiKey]; name != "" {
				candidate.AlreadyProfile = name
			} else {
				candidate.AlreadyProfile = emptyKeyIndex[apiKey]
			}
		}
	}
	return nil
}

func markDuplicateAddAutoCandidates(results []addAutoSourceResult) {
	exactSeen := map[string]string{}
	anyKeySeen := map[string]string{}
	emptyKeySeen := map[string]string{}
	for resultIdx := range results {
		for candidateIdx := range results[resultIdx].Candidates {
			candidate := &results[resultIdx].Candidates[candidateIdx]
			if candidate.AlreadyProfile != "" {
				continue
			}
			apiKey := strings.TrimSpace(candidate.Core.APIKey)
			if apiKey == "" {
				continue
			}
			base := normalizeAddAutoBaseURL(candidate.Core.BaseURL)
			if base == "" {
				candidate.DuplicateOf = anyKeySeen[apiKey]
			} else if firstSource := exactSeen[base+"\x00"+apiKey]; firstSource != "" {
				candidate.DuplicateOf = firstSource
			} else {
				candidate.DuplicateOf = emptyKeySeen[apiKey]
			}
			if candidate.DuplicateOf != "" {
				continue
			}
			if _, exists := anyKeySeen[apiKey]; !exists {
				anyKeySeen[apiKey] = candidate.Source
			}
			if base == "" {
				if _, exists := emptyKeySeen[apiKey]; !exists {
					emptyKeySeen[apiKey] = candidate.Source
				}
			} else if _, exists := exactSeen[base+"\x00"+apiKey]; !exists {
				exactSeen[base+"\x00"+apiKey] = candidate.Source
			}
		}
	}
}

func renderAddAutoDiscovery(w io.Writer, format addOutputFormat, results []addAutoSourceResult) error {
	if format == addOutputJSON {
		return nil
	}
	fmt.Fprintln(w, "auto-discovery results:")
	for _, result := range results {
		if len(result.Candidates) == 0 {
			note := result.Note
			if note == "" {
				note = "no API key found"
			}
			fmt.Fprintf(w, "  - %s: %s\n", result.Source, note)
			continue
		}
		for _, candidate := range result.Candidates {
			status := "NEW"
			if candidate.AlreadyProfile != "" {
				status = fmt.Sprintf("already recorded as %s", candidate.AlreadyProfile)
			} else if candidate.DuplicateOf != "" {
				status = fmt.Sprintf("duplicate of %s", candidate.DuplicateOf)
			}
			fmt.Fprintf(w, "  - %s: %s base_url=%s api_key=%s",
				candidate.Source,
				status,
				displayAddAutoValue(candidate.Core.BaseURL),
				redactedValueDisplay("api_key", candidate.Core.APIKey),
			)
			if strings.TrimSpace(candidate.Core.Model) != "" {
				fmt.Fprintf(w, " model=%s", candidate.Core.Model)
			}
			fmt.Fprintln(w)
		}
		if strings.TrimSpace(result.Note) != "" {
			fmt.Fprintf(w, "  - %s: note: %s\n", result.Source, result.Note)
		}
	}
	return nil
}

func countAddAutoCandidatesWithKey(results []addAutoSourceResult) int {
	count := 0
	for _, result := range results {
		for _, candidate := range result.Candidates {
			if strings.TrimSpace(candidate.Core.APIKey) != "" {
				count++
			}
		}
	}
	return count
}

func addAutoSourceList(results []addAutoSourceResult) string {
	names := make([]string, 0, len(results))
	for _, result := range results {
		names = append(names, result.Source)
	}
	return strings.Join(names, ", ")
}

func displayAddAutoValue(value string) string {
	if strings.TrimSpace(value) == "" {
		return "<empty>"
	}
	return value
}

type jsonAddAutoCandidate struct {
	Source      string   `json:"source"`
	Sources     []string `json:"sources,omitempty"`
	ProfileName string   `json:"profile_name,omitempty"`
	BaseURL     string   `json:"base_url"`
	APIKey      string   `json:"api_key"`
	Model       string   `json:"model,omitempty"`
	Status      string   `json:"status"`
	Reason      string   `json:"reason,omitempty"`
}

type jsonAddAutoDiscoverySource struct {
	Source     string                 `json:"source"`
	Note       string                 `json:"note,omitempty"`
	Candidates []jsonAddAutoCandidate `json:"candidates,omitempty"`
}

func addAutoCandidateStatus(candidate addAutoCandidate) string {
	if candidate.AlreadyProfile != "" {
		return "already recorded as " + candidate.AlreadyProfile
	}
	if candidate.DuplicateOf != "" {
		return "duplicate of " + candidate.DuplicateOf
	}
	return "NEW"
}

func newAddAutoCandidates(results []addAutoSourceResult) ([]addAutoCandidate, error) {
	exactByKey := map[string]int{}
	anyByAPIKey := map[string]int{}
	emptyByAPIKey := map[string]int{}
	out := []addAutoCandidate{}
	for _, result := range results {
		for _, candidate := range result.Candidates {
			if candidate.AlreadyProfile != "" || candidate.DuplicateOf != "" {
				continue
			}
			apiKey := strings.TrimSpace(candidate.Core.APIKey)
			if apiKey == "" {
				continue
			}
			base := normalizeAddAutoBaseURL(candidate.Core.BaseURL)
			mergeIdx := -1
			if base == "" {
				if idx, ok := anyByAPIKey[apiKey]; ok {
					mergeIdx = idx
				}
			} else if idx, ok := exactByKey[base+"\x00"+apiKey]; ok {
				mergeIdx = idx
			} else if idx, ok := emptyByAPIKey[apiKey]; ok {
				mergeIdx = idx
			}
			if mergeIdx >= 0 {
				out[mergeIdx] = mergeAddAutoCandidate(out[mergeIdx], candidate)
				indexAddAutoCandidate(out[mergeIdx], mergeIdx, exactByKey, anyByAPIKey, emptyByAPIKey)
				continue
			}
			candidate.Core = normalizeParsedProvider(candidate.Core)
			candidate.Sources = normalizeAddAutoSources(candidate.Sources, candidate.Source)
			if candidate.NameBase == "" {
				candidate.NameBase = addAutoBaseNameFor(candidate.Source, candidate.Core.BaseURL)
			}
			indexAddAutoCandidate(candidate, len(out), exactByKey, anyByAPIKey, emptyByAPIKey)
			out = append(out, candidate)
		}
	}
	return out, nil
}

func indexAddAutoCandidate(candidate addAutoCandidate, idx int, exactByKey, anyByAPIKey, emptyByAPIKey map[string]int) {
	apiKey := strings.TrimSpace(candidate.Core.APIKey)
	if apiKey == "" {
		return
	}
	base := normalizeAddAutoBaseURL(candidate.Core.BaseURL)
	if _, exists := anyByAPIKey[apiKey]; !exists {
		anyByAPIKey[apiKey] = idx
	}
	if base == "" {
		if _, exists := emptyByAPIKey[apiKey]; !exists {
			emptyByAPIKey[apiKey] = idx
		}
		return
	}
	if _, exists := exactByKey[base+"\x00"+apiKey]; !exists {
		exactByKey[base+"\x00"+apiKey] = idx
	}
}

func mergeAddAutoCandidate(base, next addAutoCandidate) addAutoCandidate {
	if strings.TrimSpace(base.Core.BaseURL) == "" {
		base.Core.BaseURL = next.Core.BaseURL
	}
	if strings.TrimSpace(base.Core.Provider) == "" {
		base.Core.Provider = next.Core.Provider
	}
	if strings.TrimSpace(base.Core.Model) == "" {
		base.Core.Model = next.Core.Model
	}
	if strings.TrimSpace(base.Core.SmallFastModel) == "" {
		base.Core.SmallFastModel = next.Core.SmallFastModel
	}
	base.Tools = mergeToolMaps(base.Tools, next.Tools)
	base.Sources = appendUniqueStrings(base.Sources, normalizeAddAutoSources(next.Sources, next.Source)...)
	if preferAddAutoNameBase(next.NameBase, base.NameBase) {
		base.NameBase = next.NameBase
	}
	return base
}

func normalizeAddAutoSources(sources []string, fallback string) []string {
	if len(sources) == 0 && fallback != "" {
		sources = []string{fallback}
	}
	out := make([]string, 0, len(sources))
	for _, source := range sources {
		source = strings.TrimSpace(source)
		if source != "" {
			out = appendUniqueStrings(out, source)
		}
	}
	return out
}

func appendUniqueStrings(base []string, values ...string) []string {
	seen := map[string]struct{}{}
	for _, v := range base {
		seen[v] = struct{}{}
	}
	for _, v := range values {
		if _, ok := seen[v]; ok {
			continue
		}
		base = append(base, v)
		seen[v] = struct{}{}
	}
	return base
}

func preferAddAutoNameBase(candidate, current string) bool {
	if candidate == "" {
		return false
	}
	if current == "" {
		return true
	}
	return addAutoNamePriority(candidate) < addAutoNamePriority(current)
}

func addAutoNamePriority(name string) int {
	switch name {
	case "claude-code", "codex":
		return 0
	case "env", "clipboard":
		return 2
	default:
		return 1
	}
}

func loadAddAutoProfileNames(store addAutoProfileStore) (map[string]struct{}, error) {
	names := map[string]struct{}{}
	if store == nil {
		return names, nil
	}
	profiles, err := store.LoadAllProfiles()
	if err != nil {
		return nil, fmt.Errorf("load existing profile names for auto naming: %w", err)
	}
	for _, profile := range profiles {
		if profile == nil {
			continue
		}
		names[profile.Name] = struct{}{}
	}
	return names, nil
}

func assignAddAutoProfileNames(candidates []addAutoCandidate, used map[string]struct{}) {
	if used == nil {
		used = map[string]struct{}{}
	}
	for idx := range candidates {
		base := sanitizeAddAutoProfileName(candidates[idx].NameBase)
		if base == "" {
			base = "auto"
		}
		name := base
		for suffix := 2; ; suffix++ {
			if _, exists := used[name]; !exists && storage.ValidateProfileName(name) == nil {
				break
			}
			name = addAutoNameWithSuffix(base, suffix)
		}
		candidates[idx].ProfileName = name
		used[name] = struct{}{}
	}
}

func promptAddAutoProfileNames(w io.Writer, in io.Reader, candidates []addAutoCandidate, used map[string]struct{}) error {
	if used == nil {
		used = map[string]struct{}{}
	}
	for idx := range candidates {
		defaultName := candidates[idx].ProfileName
		for {
			fmt.Fprintf(w, "Save profile for %s, %s as [%s]: ",
				candidates[idx].Source,
				redactedValueDisplay("api_key", candidates[idx].Core.APIKey),
				defaultName,
			)
			line, err := readLine(in)
			if err != nil {
				return fmt.Errorf("read profile name: %w", err)
			}
			name := strings.TrimSpace(line)
			if name == "" {
				name = defaultName
			}
			if err := storage.ValidateProfileName(name); err != nil {
				fmt.Fprintf(w, "Invalid profile name %q: %v\n", name, err)
				continue
			}
			if _, exists := used[name]; exists && name != candidates[idx].ProfileName {
				fmt.Fprintf(w, "Profile name %q already exists; choose another name.\n", name)
				continue
			}
			candidates[idx].ProfileName = name
			used[name] = struct{}{}
			break
		}
	}
	return nil
}

func addAutoNameWithSuffix(base string, suffix int) string {
	s := fmt.Sprintf("-%d", suffix)
	if len(base)+len(s) <= storage.MaxProfileNameLen {
		return base + s
	}
	trimmed := strings.TrimRight(base[:storage.MaxProfileNameLen-len(s)], ".-_")
	if trimmed == "" {
		trimmed = "auto"
	}
	return trimmed + s
}

func sanitizeAddAutoProfileName(raw string) string {
	raw = strings.ToLower(strings.TrimSpace(raw))
	var b strings.Builder
	lastDash := false
	for _, r := range raw {
		valid := (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9')
		if valid {
			b.WriteRune(r)
			lastDash = false
			continue
		}
		if r == '.' || r == '_' || r == '-' || unicode.IsSpace(r) {
			if !lastDash {
				b.WriteByte('-')
				lastDash = true
			}
			continue
		}
		if !lastDash {
			b.WriteByte('-')
			lastDash = true
		}
	}
	out := strings.Trim(b.String(), "-._")
	out = strings.TrimLeft(out, ".-_")
	if out == "" {
		return ""
	}
	if !((out[0] >= 'a' && out[0] <= 'z') || (out[0] >= '0' && out[0] <= '9')) {
		out = "auto-" + out
	}
	if len(out) > storage.MaxProfileNameLen {
		out = strings.TrimRight(out[:storage.MaxProfileNameLen], "-._")
	}
	if storage.ValidateProfileName(out) != nil {
		return ""
	}
	return out
}

func addAutoBaseNameFor(source, baseURL string) string {
	switch source {
	case string(addAutoSourceClaudeCode):
		return "claude-code"
	case string(addAutoSourceCodex):
		return "codex"
	case string(addAutoSourceEnv):
		if host := addAutoHostSlug(baseURL); host != "" {
			return host
		}
		return "env"
	case string(addAutoSourceClipboard):
		if host := addAutoHostSlug(baseURL); host != "" {
			return host
		}
		return "clipboard"
	default:
		if host := addAutoHostSlug(baseURL); host != "" {
			return host
		}
		return source
	}
}

func addAutoHostSlug(raw string) string {
	value := strings.TrimSpace(raw)
	if value == "" {
		return ""
	}
	u, err := url.Parse(value)
	if err != nil || u.Hostname() == "" {
		if !strings.Contains(value, "://") {
			u, err = url.Parse("https://" + value)
		}
		if err != nil || u.Hostname() == "" {
			return ""
		}
	}
	host := strings.TrimPrefix(strings.ToLower(u.Hostname()), "www.")
	return sanitizeAddAutoProfileName(host)
}

func buildAddAutoProfiles(candidates []addAutoCandidate, now time.Time) []*config.Profile {
	profiles := make([]*config.Profile, 0, len(candidates))
	for _, candidate := range candidates {
		profiles = append(profiles, &config.Profile{
			SchemaVersion: config.CurrentProfileSchemaVersion,
			Name:          candidate.ProfileName,
			Description:   addDescriptionFlag,
			CreatedAt:     now,
			UpdatedAt:     now,
			Core:          candidate.Core,
			Tools:         cloneToolMap(candidate.Tools),
		})
	}
	return profiles
}

func renderAddAutoDryRun(w io.Writer, format addOutputFormat, results []addAutoSourceResult, profiles []*config.Profile) error {
	if format == addOutputJSON {
		return renderAddAutoResultJSON(w, "dry-run", results, profiles, nil)
	}
	fmt.Fprintln(w, "--- dry-run: auto profiles (not written) ---")
	for _, profile := range profiles {
		if err := renderAddDryRun(w, addOutputText, profile); err != nil {
			return err
		}
	}
	return nil
}

func renderAddAutoCreated(w io.Writer, format addOutputFormat, results []addAutoSourceResult, profiles []*config.Profile) error {
	if format == addOutputJSON {
		return renderAddAutoResultJSON(w, "created", results, profiles, nil)
	}
	fmt.Fprintln(w, "created profiles:")
	for _, profile := range profiles {
		fmt.Fprintf(w, "  - %s\n", profile.Name)
	}
	return nil
}

func renderAddAutoProfileTextList(w io.Writer, profiles []*config.Profile) {
	for _, profile := range profiles {
		fmt.Fprintf(w, "  - %s base_url=%s api_key=%s",
			profile.Name,
			displayAddAutoValue(profile.Core.BaseURL),
			redactedValueDisplay("api_key", profile.Core.APIKey),
		)
		if strings.TrimSpace(profile.Core.Model) != "" {
			fmt.Fprintf(w, " model=%s", profile.Core.Model)
		}
		fmt.Fprintln(w)
	}
}

type jsonAddAutoProfiles struct {
	Action    string                       `json:"action"`
	Discovery []jsonAddAutoDiscoverySource `json:"discovery,omitempty"`
	Created   []string                     `json:"created,omitempty"`
	Skipped   []jsonAddAutoCandidate       `json:"skipped,omitempty"`
	Profiles  []jsonAddProfile             `json:"profiles"`
}

func renderAddAutoProfilesJSON(w io.Writer, action string, profiles []*config.Profile) error {
	out := jsonAddAutoProfiles{
		Action:   action,
		Profiles: make([]jsonAddProfile, 0, len(profiles)),
	}
	for _, profile := range profiles {
		out.Profiles = append(out.Profiles, profileToJSON(redactProfileForAddOutput(profile)))
	}
	return writeAddJSON(w, out)
}

func renderAddAutoResultJSON(w io.Writer, action string, results []addAutoSourceResult, profiles []*config.Profile, skipped []jsonAddAutoCandidate) error {
	out := jsonAddAutoProfiles{
		Action:    action,
		Discovery: jsonAddAutoDiscovery(results),
		Created:   profileNames(profiles),
		Skipped:   skipped,
		Profiles:  make([]jsonAddProfile, 0, len(profiles)),
	}
	for _, profile := range profiles {
		out.Profiles = append(out.Profiles, profileToJSON(redactProfileForAddOutput(profile)))
	}
	out.Skipped = append(out.Skipped, jsonAddAutoSkipped(results)...)
	return writeAddJSON(w, out)
}

func jsonAddAutoDiscovery(results []addAutoSourceResult) []jsonAddAutoDiscoverySource {
	out := make([]jsonAddAutoDiscoverySource, 0, len(results))
	for _, result := range results {
		source := jsonAddAutoDiscoverySource{
			Source: result.Source,
			Note:   result.Note,
		}
		if len(result.Candidates) == 0 && source.Note == "" {
			source.Note = "no API key found"
		}
		for _, candidate := range result.Candidates {
			source.Candidates = append(source.Candidates, jsonAddAutoCandidateFromCandidate(candidate))
		}
		out = append(out, source)
	}
	return out
}

func jsonAddAutoSkipped(results []addAutoSourceResult) []jsonAddAutoCandidate {
	var out []jsonAddAutoCandidate
	for _, result := range results {
		for _, candidate := range result.Candidates {
			if candidate.AlreadyProfile == "" && candidate.DuplicateOf == "" {
				continue
			}
			out = append(out, jsonAddAutoCandidateFromCandidate(candidate))
		}
	}
	return out
}

func jsonAddAutoCandidateFromCandidate(candidate addAutoCandidate) jsonAddAutoCandidate {
	status := addAutoCandidateStatus(candidate)
	out := jsonAddAutoCandidate{
		Source:      candidate.Source,
		Sources:     normalizeAddAutoSources(candidate.Sources, candidate.Source),
		ProfileName: candidate.ProfileName,
		BaseURL:     candidate.Core.BaseURL,
		APIKey:      redactedValueDisplay("api_key", candidate.Core.APIKey),
		Model:       candidate.Core.Model,
		Status:      status,
	}
	if candidate.AlreadyProfile != "" {
		out.Reason = "already-recorded"
	} else if candidate.DuplicateOf != "" {
		out.Reason = "duplicate"
	}
	return out
}

func profileNames(profiles []*config.Profile) []string {
	names := make([]string, 0, len(profiles))
	for _, profile := range profiles {
		if profile != nil {
			names = append(names, profile.Name)
		}
	}
	return names
}

func addAutoPartialCreateError(created []*config.Profile, failedName string, saveErr error) error {
	names := profileNames(created)
	if len(names) == 0 {
		return fmt.Errorf("失败于：%s: %w", failedName, saveErr)
	}
	return fmt.Errorf("已创建：%s; 失败于：%s: %w", strings.Join(names, ", "), failedName, saveErr)
}

func addAutoDedupKey(baseURL, apiKey string) string {
	apiKey = strings.TrimSpace(apiKey)
	if apiKey == "" {
		return ""
	}
	return normalizeAddAutoBaseURL(baseURL) + "\x00" + apiKey
}

func normalizeAddAutoBaseURL(raw string) string {
	value := strings.TrimSpace(raw)
	value = strings.TrimRight(value, "/")
	if value == "" {
		return ""
	}
	u, err := url.Parse(value)
	if err != nil || u.Host == "" {
		return value
	}
	u.Scheme = strings.ToLower(u.Scheme)
	host := strings.ToLower(u.Hostname())
	if port := u.Port(); port != "" {
		if (u.Scheme != "https" || port != "443") && (u.Scheme != "http" || port != "80") {
			host = net.JoinHostPort(host, port)
		}
	}
	u.Host = host
	return strings.TrimRight(u.String(), "/")
}
