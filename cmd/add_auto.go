package cmd

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"github.com/a2d2-dev/claudecm/internal/adapter"
	"github.com/a2d2-dev/claudecm/internal/blobparse"
	"github.com/a2d2-dev/claudecm/internal/config"
	"github.com/a2d2-dev/claudecm/internal/storage"
)

type addAutoSource string

const (
	addAutoSourceClipboard  addAutoSource = "clipboard"
	addAutoSourceEnv        addAutoSource = "environment"
	addAutoSourceClaudeCode addAutoSource = "~/.claude/settings.json"
	addAutoSourceCodex      addAutoSource = "~/.codex/auth.json + config.toml"
)

type addAutoCandidate struct {
	Source string
	Core   config.CoreConfig
	Tools  map[config.ToolID]config.ToolOverlay

	AlreadyProfile string
	DuplicateOf    string
}

type addAutoSourceResult struct {
	Source     string
	Candidates []addAutoCandidate
	Note       string
}

type addAutoClipboardReader func() (string, bool, error)

func profileDraftFromAuto(
	cmd *cobra.Command,
	resv *storage.Resolver,
	store *storage.FileStorage,
	format addOutputFormat,
) (config.CoreConfig, map[config.ToolID]config.ToolOverlay, bool, error) {
	results := sweepAddAutoSources(context.Background(), resv, readClipboardText)
	if err := markExistingAddAutoCandidates(results, store); err != nil {
		return config.CoreConfig{}, nil, false, err
	}
	markDuplicateAddAutoCandidates(results)
	if err := renderAddAutoDiscovery(cmd.OutOrStdout(), format, results); err != nil {
		return config.CoreConfig{}, nil, false, err
	}

	var newCandidates []addAutoCandidate
	for _, result := range results {
		for _, candidate := range result.Candidates {
			if candidate.AlreadyProfile == "" && candidate.DuplicateOf == "" {
				newCandidates = append(newCandidates, candidate)
			}
		}
	}

	switch {
	case len(newCandidates) == 0 && countAddAutoCandidatesWithKey(results) == 0:
		return config.CoreConfig{}, nil, false, fmt.Errorf("no credentials with API keys found in swept sources: %s", addAutoSourceList(results))
	case len(newCandidates) == 0:
		if format == addOutputText {
			fmt.Fprintln(cmd.OutOrStdout(), "all discovered credentials are already recorded")
		}
		return config.CoreConfig{}, nil, true, nil
	case len(newCandidates) == 1:
		return newCandidates[0].Core, newCandidates[0].Tools, false, nil
	default:
		chosen, err := chooseAddAutoCandidate(cmd.OutOrStdout(), os.Stdin, format, newCandidates)
		if err != nil {
			return config.CoreConfig{}, nil, false, err
		}
		return chosen.Core, chosen.Tools, false, nil
	}
}

func sweepAddAutoSources(ctx context.Context, resv *storage.Resolver, clipboard addAutoClipboardReader) []addAutoSourceResult {
	results := make([]addAutoSourceResult, 0, len(addAutoSourceOrder()))
	results = append(results, scanAddAutoClipboard(clipboard))
	results = append(results, scanAddAutoEnv())
	results = append(results, scanAddAutoAdapter(ctx, resv, addAutoSourceClaudeCode, adapter.ToolClaudeCode))
	results = append(results, scanAddAutoAdapter(ctx, resv, addAutoSourceCodex, adapter.ToolCodex))
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
		Source: result.Source,
		Core:   parsed,
	}}
	return result
}

func scanAddAutoEnv() addAutoSourceResult {
	core, tools, err := profileDraftFromEnv()
	result := addAutoSourceResult{Source: string(addAutoSourceEnv)}
	if err != nil {
		result.Note = "skipped: " + err.Error()
		return result
	}
	if strings.TrimSpace(core.APIKey) == "" {
		result.Note = "no API key found"
		return result
	}
	result.Candidates = []addAutoCandidate{{
		Source: result.Source,
		Core:   core,
		Tools:  tools,
	}}
	return result
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

func scanAddAutoAdapter(ctx context.Context, resv *storage.Resolver, source addAutoSource, tool adapter.ToolID) addAutoSourceResult {
	result := addAutoSourceResult{Source: string(source)}
	a, ok := adapter.DefaultRegistry.Get(tool)
	if !ok {
		result.Note = "skipped: no adapter registered"
		return result
	}
	core, overlay, err := a.Import(ctx, resv)
	if err != nil {
		result.Note = "skipped: " + err.Error()
		return result
	}
	core = coreFromAddAutoAdapterOverlay(core, overlay)
	if strings.TrimSpace(core.APIKey) == "" {
		result.Note = "no API key found"
		return result
	}
	tools := map[config.ToolID]config.ToolOverlay{}
	if !isEmptyOverlay(overlay) {
		tools[tool] = overlay
	}
	result.Candidates = []addAutoCandidate{{
		Source: result.Source,
		Core:   normalizeParsedProvider(core),
		Tools:  tools,
	}}
	return result
}

func markExistingAddAutoCandidates(results []addAutoSourceResult, store *storage.FileStorage) error {
	if store == nil {
		return nil
	}
	profiles, err := store.LoadAllProfiles()
	if err != nil {
		return fmt.Errorf("load existing profiles for auto dedup: %w", err)
	}
	index := map[string]string{}
	for _, profile := range profiles {
		if profile == nil || strings.TrimSpace(profile.Core.APIKey) == "" {
			continue
		}
		key := addAutoDedupKey(profile.Core.BaseURL, profile.Core.APIKey)
		if key == "" {
			continue
		}
		if _, exists := index[key]; !exists {
			index[key] = profile.Name
		}
	}
	for resultIdx := range results {
		for candidateIdx := range results[resultIdx].Candidates {
			candidate := &results[resultIdx].Candidates[candidateIdx]
			if name := index[addAutoDedupKey(candidate.Core.BaseURL, candidate.Core.APIKey)]; name != "" {
				candidate.AlreadyProfile = name
			}
		}
	}
	return nil
}

func markDuplicateAddAutoCandidates(results []addAutoSourceResult) {
	seen := map[string]string{}
	for resultIdx := range results {
		for candidateIdx := range results[resultIdx].Candidates {
			candidate := &results[resultIdx].Candidates[candidateIdx]
			if candidate.AlreadyProfile != "" {
				continue
			}
			key := addAutoDedupKey(candidate.Core.BaseURL, candidate.Core.APIKey)
			if key == "" {
				continue
			}
			if firstSource := seen[key]; firstSource != "" {
				candidate.DuplicateOf = firstSource
				continue
			}
			seen[key] = candidate.Source
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
	}
	return nil
}

func chooseAddAutoCandidate(
	w io.Writer,
	in *os.File,
	format addOutputFormat,
	candidates []addAutoCandidate,
) (addAutoCandidate, error) {
	if !isTerminal(in) {
		if format == addOutputText {
			fmt.Fprintln(w, "multiple new credentials discovered:")
			renderAddAutoCandidateTextList(w, candidates, true)
		} else if err := renderAddAutoDisambiguationJSON(w, candidates); err != nil {
			return addAutoCandidate{}, err
		}
		return addAutoCandidate{}, fmt.Errorf("multiple new credentials discovered; rerun in an interactive terminal or use a specific add input source to disambiguate")
	}

	if format == addOutputText {
		fmt.Fprintln(w, "multiple new credentials discovered; choose one:")
		renderAddAutoCandidateTextList(w, candidates, true)
	}
	fmt.Fprintf(w, "Select credential [1-%d]: ", len(candidates))
	line, err := bufio.NewReader(in).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return addAutoCandidate{}, fmt.Errorf("read credential selection: %w", err)
	}
	choice, err := strconv.Atoi(strings.TrimSpace(line))
	if err != nil || choice < 1 || choice > len(candidates) {
		return addAutoCandidate{}, fmt.Errorf("invalid credential selection %q", strings.TrimSpace(line))
	}
	return candidates[choice-1], nil
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

func renderAddAutoCandidateTextList(w io.Writer, candidates []addAutoCandidate, numbered bool) {
	for idx, candidate := range candidates {
		if numbered {
			fmt.Fprintf(w, "  %d. ", idx+1)
		} else {
			fmt.Fprint(w, "  - ")
		}
		fmt.Fprintf(w, "%s base_url=%s api_key=%s",
			candidate.Source,
			displayAddAutoValue(candidate.Core.BaseURL),
			redactedValueDisplay("api_key", candidate.Core.APIKey),
		)
		if strings.TrimSpace(candidate.Core.Model) != "" {
			fmt.Fprintf(w, " model=%s", candidate.Core.Model)
		}
		fmt.Fprintln(w)
	}
}

type jsonAddAutoDisambiguation struct {
	Action     string                 `json:"action"`
	Candidates []jsonAddAutoCandidate `json:"candidates"`
}

type jsonAddAutoCandidate struct {
	Source  string `json:"source"`
	BaseURL string `json:"base_url"`
	APIKey  string `json:"api_key"`
	Status  string `json:"status"`
}

func renderAddAutoDisambiguationJSON(w io.Writer, candidates []addAutoCandidate) error {
	out := jsonAddAutoDisambiguation{
		Action:     "auto-disambiguation-required",
		Candidates: make([]jsonAddAutoCandidate, 0, len(candidates)),
	}
	for _, candidate := range candidates {
		out.Candidates = append(out.Candidates, jsonAddAutoCandidate{
			Source:  candidate.Source,
			BaseURL: candidate.Core.BaseURL,
			APIKey:  redactedValueDisplay("api_key", candidate.Core.APIKey),
			Status:  addAutoCandidateStatus(candidate),
		})
	}
	return writeAddJSON(w, out)
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
