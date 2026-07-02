package cmd

// list_test.go — Story E6-S10 tests for the rewritten cmd/list
// surface. See docs/plan/stories/E6-S10.md for the AC that shaped
// these cases.
//
// Isolation strategy mirrors cmd/current_test.go: per-test t.TempDir()
// rewires HOME; resetListFlagsForTest wipes package-level flag state;
// runListInner uses a synthetic cobra.Command with bytes.Buffers.

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/a2d2-dev/claudecm/internal/config"
	"github.com/a2d2-dev/claudecm/internal/storage"
)

// resetListFlagsForTest wipes the list-command flag state. Global flags
// are wiped separately via resetGlobalFlagsForTest.
func resetListFlagsForTest() {
	listOutputFlag = "text"
	listRevealFlag = false
}

// listHarness owns the per-test HOME tree.
type listHarness struct {
	t     *testing.T
	home  string
	resv  *storage.Resolver
	store *storage.FileStorage
	mgr   *config.Manager
}

func newListHarness(t *testing.T) *listHarness {
	t.Helper()
	resetListFlagsForTest()
	resetGlobalFlagsForTest()
	t.Cleanup(func() {
		resetListFlagsForTest()
		resetGlobalFlagsForTest()
	})

	home := t.TempDir()
	t.Setenv("HOME", home)
	resv, err := storage.NewResolverWithHome(home)
	if err != nil {
		t.Fatalf("NewResolverWithHome: %v", err)
	}
	if err := storage.Bootstrap(resv); err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	store := storage.NewFileStorage(resv)
	mgr := config.NewManager(store, config.NewValidator())
	return &listHarness{t: t, home: home, resv: resv, store: store, mgr: mgr}
}

// saveProfile writes a profile with the given fields via FileStorage.
func (h *listHarness) saveProfile(name, apiKey, baseURL, model, provider string) {
	h.t.Helper()
	p := config.NewProfile(name, baseURL, apiKey)
	p.Core.Model = model
	p.Core.Provider = provider
	if err := h.mgr.AddProfile(p); err != nil {
		h.t.Fatalf("AddProfile(%q): %v", name, err)
	}
}

func (h *listHarness) activate(name string) {
	h.t.Helper()
	if err := h.mgr.SetActive(name); err != nil {
		h.t.Fatalf("SetActive(%q): %v", name, err)
	}
}

// runListInner invokes runList with a synthetic cobra.Command.
func runListInner(t *testing.T) (stdout, stderr string, err error) {
	t.Helper()
	var out, errBuf bytes.Buffer
	cmd := &cobra.Command{Use: "list"}
	cmd.SetOut(&out)
	cmd.SetErr(&errBuf)
	err = runList(cmd, nil)
	return out.String(), errBuf.String(), err
}

// TestList_HappyThreeProfilesActiveMarker: three profiles on disk,
// one active → all rows present with the active marker on the right row.
func TestList_HappyThreeProfilesActiveMarker(t *testing.T) {
	h := newListHarness(t)
	h.saveProfile("alpha", "sk-alphaverylong1234", "https://api.a.example.com", "opus", "anthropic")
	h.saveProfile("beta", "sk-betaverylong5678", "https://api.b.example.com", "sonnet", "anthropic")
	h.saveProfile("gamma", "sk-gammaverylong9012", "https://api.g.example.com", "haiku", "openai-compat")
	h.activate("beta")

	stdout, _, err := runListInner(t)
	if err != nil {
		t.Fatalf("runList err = %v", err)
	}
	for _, name := range []string{"alpha", "beta", "gamma"} {
		if !strings.Contains(stdout, name) {
			t.Errorf("stdout missing profile %q:\n%s", name, stdout)
		}
	}
	// The active row starts with the "*" marker; the others start with
	// a leading space. Scan lines for the marker→profile pairing.
	activeMarked := false
	inactiveMarked := 0
	for _, line := range strings.Split(stdout, "\n") {
		trimmed := strings.TrimRight(line, " ")
		if strings.HasPrefix(trimmed, listActiveMarker+" beta") {
			activeMarked = true
		}
		for _, name := range []string{"alpha", "gamma"} {
			if strings.HasPrefix(trimmed, listActiveMarker+" "+name) {
				inactiveMarked++
			}
		}
	}
	if !activeMarked {
		t.Errorf("active marker missing on beta:\n%s", stdout)
	}
	if inactiveMarked > 0 {
		t.Errorf("active marker present on non-active row:\n%s", stdout)
	}
	// Default redaction: raw api keys must not appear.
	for _, key := range []string{"sk-alphaverylong1234", "sk-betaverylong5678", "sk-gammaverylong9012"} {
		if strings.Contains(stdout, key) {
			t.Errorf("stdout leaked plaintext key %q:\n%s", key, stdout)
		}
	}
}

// TestList_EmptyDirectory: fresh bootstrapped HOME with no profiles →
// "no profiles" text and exit 0.
func TestList_EmptyDirectory(t *testing.T) {
	newListHarness(t)
	stdout, _, err := runListInner(t)
	if err != nil {
		t.Fatalf("runList err = %v", err)
	}
	if !strings.Contains(stdout, listNoProfilesText) {
		t.Errorf("stdout missing %q:\n%s", listNoProfilesText, stdout)
	}
}

// TestList_ShortAPIKeyRedactedAsAsterisks: a profile with a <8-char
// api_key renders as "***", not first4/last4. The manager validator
// refuses short keys (minimum 10 chars) so this test writes the
// profile YAML directly, bypassing AddProfile.
func TestList_ShortAPIKeyRedactedAsAsterisks(t *testing.T) {
	h := newListHarness(t)
	shortPath := filepath.Join(h.home, ".claudecm", "profiles", "shortprofile.yaml")
	body := `schema_version: 1
name: shortprofile
core:
  base_url: https://api.example.com
  api_key: sk-12
  model: opus
  provider: anthropic
`
	if err := os.WriteFile(shortPath, []byte(body), 0600); err != nil {
		t.Fatalf("write short-key profile: %v", err)
	}

	stdout, _, err := runListInner(t)
	if err != nil {
		t.Fatalf("runList err = %v", err)
	}
	if strings.Contains(stdout, "sk-12") {
		t.Errorf("short key leaked plaintext:\n%s", stdout)
	}
	if !strings.Contains(stdout, "***") {
		t.Errorf("short key missing *** sentinel:\n%s", stdout)
	}
}

// TestList_CorruptProfileRefused: one profile file with malformed YAML
// (specifically, missing schema_version and unknown top-level key) →
// command refuses with non-zero exit, error names the file, no partial
// list emitted.
func TestList_CorruptProfileRefused(t *testing.T) {
	h := newListHarness(t)
	h.saveProfile("valid", "sk-validverylong1234", "https://api.example.com", "opus", "anthropic")

	// Write a corrupt profile file directly to bypass ParseProfile.
	corruptPath := filepath.Join(h.home, ".claudecm", "profiles", "broken.yaml")
	if err := os.WriteFile(corruptPath, []byte("name: broken\nunknown_top_level_key: whatever\n"), 0600); err != nil {
		t.Fatalf("write corrupt profile: %v", err)
	}

	stdout, _, err := runListInner(t)
	if err == nil {
		t.Fatalf("runList expected refusal; got nil error, stdout=%q", stdout)
	}
	if !strings.Contains(err.Error(), "broken.yaml") {
		t.Errorf("error message must name offending file; got %v", err)
	}
	if strings.Contains(stdout, "valid") {
		t.Errorf("partial list leaked; stdout=%q", stdout)
	}
}

// TestList_JSONOutputParses: --output json emits a JSON array with
// {name, provider, model, active, api_key} on every entry.
func TestList_JSONOutputParses(t *testing.T) {
	h := newListHarness(t)
	h.saveProfile("prod", "sk-prodverylongkey1234", "https://api.example.com", "opus", "anthropic")
	h.saveProfile("dev", "sk-devverylongkey5678", "https://api.example.com", "sonnet", "anthropic")
	h.activate("prod")

	listOutputFlag = "json"
	stdout, _, err := runListInner(t)
	if err != nil {
		t.Fatalf("runList err = %v", err)
	}
	var arr []listJSON
	if err := json.Unmarshal([]byte(stdout), &arr); err != nil {
		t.Fatalf("stdout not valid JSON: %v\nBody:\n%s", err, stdout)
	}
	if len(arr) != 2 {
		t.Fatalf("expected 2 entries; got %d: %+v", len(arr), arr)
	}
	activeCount := 0
	for _, row := range arr {
		if row.Active {
			activeCount++
			if row.Name != "prod" {
				t.Errorf("wrong entry marked active: %+v", row)
			}
		}
		if strings.Contains(row.APIKey, "sk-prodverylongkey1234") ||
			strings.Contains(row.APIKey, "sk-devverylongkey5678") {
			t.Errorf("JSON leaked plaintext api_key: %+v", row)
		}
	}
	if activeCount != 1 {
		t.Errorf("expected exactly one active row; got %d", activeCount)
	}
}

// TestList_JSONEmptyProfilesEmitsBrackets: empty profiles dir → "[]"
// with --output json, per the AC.
func TestList_JSONEmptyProfilesEmitsBrackets(t *testing.T) {
	newListHarness(t)
	listOutputFlag = "json"
	stdout, _, err := runListInner(t)
	if err != nil {
		t.Fatalf("runList err = %v", err)
	}
	var arr []listJSON
	if err := json.Unmarshal([]byte(stdout), &arr); err != nil {
		t.Fatalf("stdout not valid JSON: %v\nBody:\n%s", err, stdout)
	}
	if len(arr) != 0 {
		t.Errorf("expected empty array; got %+v", arr)
	}
}

// TestList_RevealShowsPlaintext: --reveal at root (globalRevealFlag)
// → api_key renders plaintext AND stderr carries the warning.
func TestList_RevealShowsPlaintext(t *testing.T) {
	h := newListHarness(t)
	h.saveProfile("prod", "sk-prodverylongkey1234", "https://api.example.com", "opus", "anthropic")

	globalRevealFlag = true
	stdout, stderr, err := runListInner(t)
	if err != nil {
		t.Fatalf("runList err = %v", err)
	}
	if !strings.Contains(stdout, "sk-prodverylongkey1234") {
		t.Errorf("--reveal did not surface plaintext:\n%s", stdout)
	}
	if !strings.Contains(stderr, "WARNING") {
		t.Errorf("stderr missing reveal warning: %q", stderr)
	}
}

// TestList_LocalRevealFlagPlaintextToo asserts the legacy local seam
// (listRevealFlag) also flips redaction. It exists so future flag
// migrations do not silently drop the local override.
func TestList_LocalRevealFlagPlaintextToo(t *testing.T) {
	h := newListHarness(t)
	h.saveProfile("prod", "sk-prodverylongkey1234", "https://api.example.com", "opus", "anthropic")

	listRevealFlag = true
	stdout, _, err := runListInner(t)
	if err != nil {
		t.Fatalf("runList err = %v", err)
	}
	if !strings.Contains(stdout, "sk-prodverylongkey1234") {
		t.Errorf("local reveal did not surface plaintext:\n%s", stdout)
	}
}

// TestList_MissingStateTreatsAsNoActiveProfile asserts that a
// bootstrapped tree whose state.yaml is missing (delete + reload)
// still yields a successful list run with no active marker. The
// reviewer's F7 finding: pre-fix readActiveName propagated the
// "file does not exist" error, breaking list on any tree that had
// never called SetActive.
func TestList_MissingStateTreatsAsNoActiveProfile(t *testing.T) {
	h := newListHarness(t)
	h.saveProfile("solo", "sk-soloverylongkey12345", "https://api.example.com", "opus", "anthropic")
	// Explicitly delete state.yaml — a fresh install would not have
	// it yet.
	statePath := filepath.Join(h.home, ".claudecm", "state.yaml")
	if err := os.Remove(statePath); err != nil && !os.IsNotExist(err) {
		t.Fatalf("remove state.yaml: %v", err)
	}

	stdout, _, err := runListInner(t)
	if err != nil {
		t.Fatalf("runList err = %v; expected success on missing state.yaml", err)
	}
	if !strings.Contains(stdout, "solo") {
		t.Errorf("stdout missing profile row for %q:\n%s", "solo", stdout)
	}
	// No line should carry the active marker — there is nothing active.
	for _, line := range strings.Split(stdout, "\n") {
		if strings.HasPrefix(strings.TrimRight(line, " "), listActiveMarker+" ") {
			t.Errorf("list surfaced an active marker despite missing state.yaml: %q", line)
		}
	}
}

// TestList_InvalidOutputRefused: --output yaml → error.
func TestList_InvalidOutputRefused(t *testing.T) {
	newListHarness(t)
	listOutputFlag = "yaml"

	_, _, err := runListInner(t)
	if err == nil {
		t.Fatalf("runList expected error on invalid --output; got nil")
	}
	if !strings.Contains(err.Error(), "invalid --output") {
		t.Errorf("error message unexpected: %v", err)
	}
}
