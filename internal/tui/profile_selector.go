package tui

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/a2d2-dev/claudecm/internal/adapter"
	"github.com/a2d2-dev/claudecm/internal/config"
	"github.com/a2d2-dev/claudecm/internal/resolver"
	"github.com/a2d2-dev/claudecm/internal/storage"
	"golang.org/x/term"
)

var (
	ErrCanceled      = errors.New("interactive selector canceled")
	ErrAlreadyActive = errors.New("selected profile is already active")
)

type ProfileItem struct {
	Name        string
	Description string
	Provider    string
	BaseURL     string
	Model       string
	Active      bool
	Profile     config.Profile
}

type ProfileLoader func(*storage.Resolver) ([]*config.Profile, string, error)

type Selector struct {
	Terminal Terminal
	Loader   ProfileLoader
	Stdin    *os.File
	Stdout   *os.File
	Writer   io.Writer
	Reveal   bool
}

func BuildProfileItems(profiles []*config.Profile, active string) ([]ProfileItem, error) {
	if len(profiles) == 0 {
		return nil, fmt.Errorf("no profiles found; add a profile before using interactive switch")
	}
	items := make([]ProfileItem, 0, len(profiles))
	activeFound := active == ""
	for _, p := range profiles {
		if p == nil {
			continue
		}
		item := ProfileItem{
			Name:        p.Name,
			Description: p.Description,
			Provider:    p.Core.Provider,
			BaseURL:     p.Core.BaseURL,
			Model:       p.Core.Model,
			Active:      p.Name == active,
			Profile:     *p.Clone(),
		}
		if item.Active {
			activeFound = true
		}
		items = append(items, item)
	}
	if len(items) == 0 {
		return nil, fmt.Errorf("no profiles found; add a profile before using interactive switch")
	}
	sort.SliceStable(items, func(i, j int) bool { return items[i].Name < items[j].Name })
	if !activeFound {
		return nil, fmt.Errorf("active profile %q could not be loaded", active)
	}
	return items, nil
}

func FilterProfileItems(items []ProfileItem, query string) []ProfileItem {
	query = strings.TrimSpace(query)
	if query == "" {
		out := make([]ProfileItem, len(items))
		copy(out, items)
		return out
	}
	type scored struct {
		item  ProfileItem
		score int
		index int
	}
	var matches []scored
	for i, item := range items {
		score, ok := profileFuzzyScore(item, query)
		if !ok {
			continue
		}
		matches = append(matches, scored{item: item, score: score, index: i})
	}
	sort.SliceStable(matches, func(i, j int) bool {
		if matches[i].score != matches[j].score {
			return matches[i].score < matches[j].score
		}
		return matches[i].index < matches[j].index
	})
	out := make([]ProfileItem, 0, len(matches))
	for _, m := range matches {
		out = append(out, m.item)
	}
	return out
}

func profileFuzzyScore(item ProfileItem, query string) (int, bool) {
	fields := []string{
		item.Name,
		item.Description,
		item.Provider,
		item.BaseURL,
		item.Model,
	}
	best := 0
	matched := false
	for _, field := range fields {
		score, ok := fuzzyScore(field, query)
		if !ok {
			continue
		}
		if !matched || score < best {
			best = score
		}
		matched = true
	}
	return best, matched
}

func fuzzyScore(haystack, query string) (int, bool) {
	haystack = strings.ToLower(haystack)
	query = strings.ToLower(query)
	if query == "" {
		return 0, true
	}
	pos := 0
	first := -1
	last := -1
	gaps := 0
	for _, qr := range query {
		found := false
		for pos < len(haystack) {
			hr, size := utf8.DecodeRuneInString(haystack[pos:])
			if hr == qr {
				if first == -1 {
					first = pos
				}
				if last >= 0 {
					gaps += pos - last
				}
				last = pos
				pos += size
				found = true
				break
			}
			pos += size
		}
		if !found {
			return 0, false
		}
	}
	return first + gaps, true
}

func SelectProfile(ctx context.Context, r *storage.Resolver, opts Selector) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	term := opts.Terminal
	if term == nil {
		term = XTerm{}
	}
	stdin := opts.Stdin
	if stdin == nil {
		stdin = os.Stdin
	}
	stdout := opts.Stdout
	if stdout == nil {
		stdout = os.Stdout
	}
	writer := opts.Writer
	if writer == nil {
		writer = stdout
	}
	if err := CheckCapabilities(term, stdout); err != nil {
		return "", err
	}
	loader := opts.Loader
	if loader == nil {
		return "", fmt.Errorf("interactive selector loader is not configured")
	}
	profiles, active, err := loader(r)
	if err != nil {
		return "", err
	}
	items, err := BuildProfileItems(profiles, active)
	if err != nil {
		return "", err
	}

	oldState, err := term.MakeRaw(stdin)
	if err != nil {
		return "", fmt.Errorf("enter raw terminal mode: %w", err)
	}
	return withRestoredTerminal(term, stdin, oldState, func() (string, error) {
		fmt.Fprint(writer, "\x1b[?25l")
		defer fmt.Fprint(writer, "\x1b[?25h\x1b[0m\n")

		state := selectorState{items: items, filtered: FilterProfileItems(items, ""), selected: 0, reveal: opts.Reveal}
		reader := bufio.NewReader(stdin)
		for {
			if err := ctx.Err(); err != nil {
				return "", err
			}
			width, height, err := term.Size(stdout)
			if err != nil {
				return "", fmt.Errorf("read terminal size: %w", err)
			}
			renderSelector(writer, state, r, width, height)
			key, err := readKey(reader)
			if err != nil {
				if errors.Is(err, io.EOF) {
					return "", ErrCanceled
				}
				return "", fmt.Errorf("read selector input: %w", err)
			}
			switch key.kind {
			case keyCancel:
				return "", ErrCanceled
			case keyEnter:
				if len(state.filtered) == 0 {
					continue
				}
				chosen := state.filtered[state.selected]
				if chosen.Active {
					return chosen.Name, ErrAlreadyActive
				}
				return chosen.Name, nil
			case keyBackspace:
				state.query = dropLastRune(state.query)
				state.filtered = FilterProfileItems(state.items, state.query)
				state.selected = clampSelection(state.selected, len(state.filtered))
			case keyRune:
				if !unicode.IsControl(key.r) {
					state.query += string(key.r)
					state.filtered = FilterProfileItems(state.items, state.query)
					state.selected = 0
				}
			case keyUp:
				if state.selected > 0 {
					state.selected--
				}
			case keyDown:
				if state.selected < len(state.filtered)-1 {
					state.selected++
				}
			}
		}
	})
}

func withRestoredTerminal(tty Terminal, stdin *os.File, oldState *term.State, fn func() (string, error)) (selected string, err error) {
	defer func() {
		if restoreErr := tty.Restore(stdin, oldState); restoreErr != nil {
			restoreErr = fmt.Errorf("restore terminal mode: %w", restoreErr)
			if err == nil {
				err = restoreErr
			} else {
				err = errors.Join(err, restoreErr)
			}
		}
	}()
	return fn()
}

type selectorState struct {
	items    []ProfileItem
	filtered []ProfileItem
	query    string
	selected int
	reveal   bool
}

func renderSelector(w io.Writer, state selectorState, r *storage.Resolver, width, height int) {
	if width <= 0 {
		width = minTerminalWidth
	}
	if height <= 0 {
		height = minTerminalHeight
	}
	fmt.Fprint(w, "\x1b[H\x1b[2J")
	fmt.Fprintln(w, truncate(fmt.Sprintf("Select profile: %s", state.query), width))
	fmt.Fprintln(w, truncate("Type to filter, Up/Down to move, Enter to switch, Esc/Ctrl-C to cancel", width))
	fmt.Fprintln(w)
	previewLines := renderPreviewLines(state, r)
	rows := height - 4 - len(previewLines)
	if rows < 1 {
		rows = 1
	}
	if len(state.filtered) == 0 {
		fmt.Fprintln(w, truncate("  no matching profiles", width))
		return
	}
	start := 0
	if state.selected >= rows {
		start = state.selected - rows + 1
	}
	end := start + rows
	if end > len(state.filtered) {
		end = len(state.filtered)
	}
	for i := start; i < end; i++ {
		item := state.filtered[i]
		cursor := " "
		if i == state.selected {
			cursor = ">"
		}
		active := " "
		if item.Active {
			active = "*"
		}
		context := strings.TrimSpace(strings.Join(nonEmptyStrings(item.Provider, item.Model), " / "))
		if context != "" {
			context = "  " + context
		}
		fmt.Fprintln(w, truncate(fmt.Sprintf("%s %s %s%s", cursor, active, item.Name, context), width))
	}
	if len(previewLines) > 0 {
		fmt.Fprintln(w)
		for _, line := range previewLines {
			fmt.Fprintln(w, truncate(line, width))
		}
	}
}

func renderPreviewLines(state selectorState, r *storage.Resolver) []string {
	if len(state.filtered) == 0 || state.selected >= len(state.filtered) {
		return nil
	}
	item := state.filtered[state.selected]
	return BuildPreviewLines(context.Background(), r, item.Profile, item.Active, state.reveal)
}

func BuildPreviewLines(ctx context.Context, r *storage.Resolver, profile config.Profile, active bool, reveal bool) []string {
	lines := []string{"Preview:"}
	marker := ""
	if active {
		marker = " (active)"
	}
	lines = append(lines, fmt.Sprintf("  Profile: %s%s", profile.Name, marker))
	if profile.Description != "" {
		lines = append(lines, "  Notes: "+profile.Description)
	}
	if profile.Core.Provider != "" {
		lines = append(lines, "  Provider: "+profile.Core.Provider)
	}
	if profile.Core.BaseURL != "" {
		lines = append(lines, "  Base URL: "+profile.Core.BaseURL)
	}
	if profile.Core.Model != "" {
		lines = append(lines, "  Model: "+profile.Core.Model)
	}
	if r == nil {
		return lines
	}
	view, err := resolver.Resolve(ctx, r, adapter.DefaultRegistry, profile, resolver.Filter{})
	if err != nil {
		return append(lines, "  Preview error: "+err.Error())
	}
	for _, tv := range view.Tools {
		lines = append(lines, fmt.Sprintf("  %s:", tv.Tool))
		if len(tv.Errors) > 0 {
			for _, te := range tv.Errors {
				lines = append(lines, fmt.Sprintf("    Preview error: %s: %s", te.Kind, te.Message))
			}
			continue
		}
		fields := append([]adapter.EffectiveField(nil), tv.Effective.Fields...)
		adapter.SortFields(fields)
		if len(fields) == 0 {
			lines = append(lines, "    (no effective fields)")
			continue
		}
		for _, field := range fields {
			if !previewField(field.Key) {
				continue
			}
			lines = append(lines, fmt.Sprintf("    %s: %s", field.Key, previewValue(field.Value, field.Secret, reveal)))
		}
	}
	return lines
}

func previewField(key string) bool {
	lower := strings.ToLower(key)
	return strings.Contains(lower, "provider") ||
		strings.Contains(lower, "base_url") ||
		strings.Contains(lower, "api_key") ||
		strings.Contains(lower, "auth_token") ||
		strings.Contains(lower, "token") ||
		strings.Contains(lower, "model")
}

func previewValue(value any, secret bool, reveal bool) string {
	if secret && !reveal {
		return redactPreviewValue(value)
	}
	if value == nil {
		return ""
	}
	return fmt.Sprint(value)
}

func redactPreviewValue(value any) string {
	if value == nil {
		return "***"
	}
	s := fmt.Sprint(value)
	if len(s) >= 8 {
		return s[:4] + "***" + s[len(s)-4:]
	}
	return "***"
}

func nonEmptyStrings(values ...string) []string {
	out := make([]string, 0, len(values))
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			out = append(out, v)
		}
	}
	return out
}

func truncate(s string, width int) string {
	if width <= 0 {
		return ""
	}
	runes := []rune(s)
	if len(runes) <= width {
		return s
	}
	if width <= 3 {
		return string(runes[:width])
	}
	return string(runes[:width-3]) + "..."
}

func dropLastRune(s string) string {
	if s == "" {
		return ""
	}
	_, size := utf8.DecodeLastRuneInString(s)
	return s[:len(s)-size]
}

func clampSelection(selected, count int) int {
	if count <= 0 {
		return 0
	}
	if selected >= count {
		return count - 1
	}
	if selected < 0 {
		return 0
	}
	return selected
}

type keyKind int

const (
	keyUnknown keyKind = iota
	keyRune
	keyUp
	keyDown
	keyEnter
	keyBackspace
	keyCancel
)

type keyEvent struct {
	kind keyKind
	r    rune
}

func readKey(r *bufio.Reader) (keyEvent, error) {
	ch, _, err := r.ReadRune()
	if err != nil {
		return keyEvent{}, err
	}
	switch ch {
	case '\r', '\n':
		return keyEvent{kind: keyEnter}, nil
	case 0x03, 0x1b:
		if ch == 0x1b && r.Buffered() >= 2 {
			b1, _ := r.ReadByte()
			b2, _ := r.ReadByte()
			if b1 == '[' {
				switch b2 {
				case 'A':
					return keyEvent{kind: keyUp}, nil
				case 'B':
					return keyEvent{kind: keyDown}, nil
				}
			}
		}
		return keyEvent{kind: keyCancel}, nil
	case 0x7f, '\b':
		return keyEvent{kind: keyBackspace}, nil
	default:
		return keyEvent{kind: keyRune, r: ch}, nil
	}
}
