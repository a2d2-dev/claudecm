package tui

import (
	"bufio"
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/a2d2-dev/claudecm/internal/config"
)

func testProfile(name, provider, baseURL, model, description string) *config.Profile {
	p := config.NewProfile(name, baseURL, "sk-"+name+"-token")
	p.Core.Provider = provider
	p.Core.Model = model
	p.Description = description
	return p
}

func TestBuildProfileItemsSortsAndMarksActive(t *testing.T) {
	items, err := BuildProfileItems([]*config.Profile{
		testProfile("relay-b", "deepseek", "https://b.example.com", "deepseek-chat", ""),
		testProfile("official", "anthropic", "https://api.anthropic.com", "claude-sonnet", "default"),
		testProfile("relay-a", "moonshot", "https://a.example.com", "kimi-k2", ""),
	}, "relay-a")
	if err != nil {
		t.Fatalf("BuildProfileItems err=%v", err)
	}
	gotNames := []string{items[0].Name, items[1].Name, items[2].Name}
	wantNames := []string{"official", "relay-a", "relay-b"}
	for i := range wantNames {
		if gotNames[i] != wantNames[i] {
			t.Fatalf("names = %v; want %v", gotNames, wantNames)
		}
	}
	if !items[1].Active {
		t.Fatalf("relay-a not marked active: %#v", items)
	}
}

func TestBuildProfileItemsRejectsEmptyAndMissingActive(t *testing.T) {
	if _, err := BuildProfileItems(nil, ""); err == nil || !strings.Contains(err.Error(), "no profiles") {
		t.Fatalf("empty err=%v; want no profiles", err)
	}
	if _, err := BuildProfileItems([]*config.Profile{
		testProfile("relay-a", "moonshot", "https://a.example.com", "kimi-k2", ""),
	}, "missing"); err == nil || !strings.Contains(err.Error(), "active profile") {
		t.Fatalf("missing active err=%v; want active profile error", err)
	}
}

func TestFilterProfileItemsMatchesUsefulContext(t *testing.T) {
	items, err := BuildProfileItems([]*config.Profile{
		testProfile("official", "anthropic", "https://api.anthropic.com", "claude-sonnet", "work"),
		testProfile("relay-a", "moonshot", "https://relay.example.com", "kimi-k2", "fast"),
		testProfile("deep", "deepseek", "https://deep.example.com", "deepseek-chat", ""),
	}, "")
	if err != nil {
		t.Fatalf("BuildProfileItems err=%v", err)
	}
	cases := []struct {
		query string
		want  []string
	}{
		{query: "", want: []string{"deep", "official", "relay-a"}},
		{query: "relay", want: []string{"relay-a"}},
		{query: "kimi", want: []string{"relay-a"}},
		{query: "anth", want: []string{"official"}},
		{query: "fast", want: []string{"relay-a"}},
	}
	for _, tc := range cases {
		gotItems := FilterProfileItems(items, tc.query)
		got := make([]string, 0, len(gotItems))
		for _, item := range gotItems {
			got = append(got, item.Name)
		}
		if strings.Join(got, ",") != strings.Join(tc.want, ",") {
			t.Fatalf("FilterProfileItems(%q) = %v; want %v", tc.query, got, tc.want)
		}
	}
}

func TestRenderSelectorShowsActiveMarkerAndTruncates(t *testing.T) {
	items, err := BuildProfileItems([]*config.Profile{
		testProfile("official", "anthropic", "https://api.anthropic.com", "claude-sonnet", ""),
		testProfile("relay-a", "moonshot", "https://relay.example.com", "kimi-k2", ""),
	}, "official")
	if err != nil {
		t.Fatalf("BuildProfileItems err=%v", err)
	}
	state := selectorState{items: items, filtered: items, selected: 0}
	var buf bytes.Buffer
	renderSelector(&buf, state, nil, 34, 12)
	out := buf.String()
	if !strings.Contains(out, "> * official") {
		t.Fatalf("render missing selected active marker:\n%s", out)
	}
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, "\x1b") {
			continue
		}
		if len([]rune(line)) > 34 {
			t.Fatalf("line longer than width: %q", line)
		}
	}
}

func TestBuildPreviewLinesRedactsSecretsAndShowsContext(t *testing.T) {
	p := testProfile("relay-a", "moonshot", "https://relay.example.com", "kimi-k2", "fast relay")
	lines := BuildPreviewLines(context.Background(), nil, *p, true, false)
	out := strings.Join(lines, "\n")
	for _, want := range []string{
		"Profile: relay-a (active)",
		"Notes: fast relay",
		"Provider: moonshot",
		"Base URL: https://relay.example.com",
		"Model: kimi-k2",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("preview missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, p.Core.APIKey) {
		t.Fatalf("preview leaked api key:\n%s", out)
	}
	if got := previewValue("sk-secret-token", true, false); got != "sk-s***oken" {
		t.Fatalf("previewValue redacted = %q", got)
	}
	if got := previewValue("sk-secret-token", true, true); got != "sk-secret-token" {
		t.Fatalf("previewValue reveal = %q", got)
	}
}

func TestReadKey(t *testing.T) {
	cases := []struct {
		in   string
		want keyKind
	}{
		{in: "\n", want: keyEnter},
		{in: "\x7f", want: keyBackspace},
		{in: "\x1b", want: keyCancel},
		{in: "\x1b[A", want: keyUp},
		{in: "\x1b[B", want: keyDown},
		{in: "a", want: keyRune},
	}
	for _, tc := range cases {
		key, err := readKey(bufioReader(tc.in))
		if err != nil {
			t.Fatalf("readKey(%q) err=%v", tc.in, err)
		}
		if key.kind != tc.want {
			t.Fatalf("readKey(%q) = %v; want %v", tc.in, key.kind, tc.want)
		}
	}
}

func bufioReader(s string) *bufio.Reader {
	return bufio.NewReader(strings.NewReader(s))
}
