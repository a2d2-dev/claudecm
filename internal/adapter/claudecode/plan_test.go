package claudecode_test

// plan_test.go — E3-S4. Exercises the Plan → Transform surface of the
// Claude Code adapter via the exported WritePlan the adapter returns.
// Tests live in the _test package (mirror of import_test.go) so we
// only ever go through the same shape cmd/* and internal/commit will
// use once they wire in.
//
// The bulk of coverage lives in pure-Transform tests: build a Profile,
// call Plan(), pull WritePlan.Transform, and feed it a synthetic
// `current` byte slice. No filesystem involved for these — Transform
// is documented as pure and the tests rely on that.
//
// A single integration test (TestPlan_ThroughApply_HappyPath) drives
// writepath.Apply end-to-end against a real per-test HOME so the AC
// item "Plan output plugs into the FR-5 write-path" is exercised, not
// just claimed.

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/a2d2-dev/claudecm/internal/adapter"
	"github.com/a2d2-dev/claudecm/internal/adapter/claudecode"
	"github.com/a2d2-dev/claudecm/internal/config"
	"github.com/a2d2-dev/claudecm/internal/storage"
	"github.com/a2d2-dev/claudecm/internal/writepath"
)

// runPlan is a shorthand for calling Adapter.Plan through the exported
// surface. Panics via t.Fatalf on error since every well-formed test
// input we hand it must succeed — Plan itself is pure and returns nil
// error unconditionally in v1.
func runPlan(t *testing.T, r *storage.Resolver, p config.Profile) writepath.WritePlan {
	t.Helper()
	plans, err := claudecode.New().Plan(context.Background(), r, p)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if len(plans) != 1 {
		t.Fatalf("Plan returned %d plans, want exactly 1", len(plans))
	}
	return plans[0]
}

// transformCurrent runs the WritePlan.Transform closure over the given
// current bytes and returns the resulting bytes. Handles t.Fatalf on
// error so the caller can go straight to assertions.
func transformCurrent(t *testing.T, plan writepath.WritePlan, current []byte) []byte {
	t.Helper()
	if plan.Transform == nil {
		t.Fatalf("Transform is nil")
	}
	out, err := plan.Transform(current)
	if err != nil {
		t.Fatalf("Transform error: %v", err)
	}
	return out
}

// mustUnmarshal parses JSON bytes into a generic map — assertions read
// against this to avoid depending on sjson's exact whitespace shape.
func mustUnmarshal(t *testing.T, b []byte) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatalf("unmarshal %q: %v", string(b), err)
	}
	return out
}

// getPath descends into a nested map by dot-separated key path. Returns
// (nil, false) when any segment is missing. Used so tests read like
// prose against the "env.ANTHROPIC_MODEL" idiom.
func getPath(m map[string]any, dotted string) (any, bool) {
	parts := strings.Split(dotted, ".")
	var cur any = m
	for _, p := range parts {
		mm, ok := cur.(map[string]any)
		if !ok {
			return nil, false
		}
		v, ok := mm[p]
		if !ok {
			return nil, false
		}
		cur = v
	}
	return cur, true
}

func TestPlan_ReturnsExactlyOnePlan(t *testing.T) {
	// V1: Claude Code owns exactly one file. Encode the shape so a
	// future refactor that silently grows the slice tripwires.
	r := newResolver(t)
	plans, err := claudecode.New().Plan(context.Background(), r, config.Profile{Name: "p"})
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if len(plans) != 1 {
		t.Fatalf("Plan returned %d plans, want 1", len(plans))
	}
	got := plans[0]
	if got.Tool != string(adapter.ToolClaudeCode) {
		t.Errorf("Tool = %q, want %q", got.Tool, adapter.ToolClaudeCode)
	}
	if got.Target != claudecode.SettingsPath(r) {
		t.Errorf("Target = %q, want %q", got.Target, claudecode.SettingsPath(r))
	}
	if got.Transform == nil {
		t.Errorf("Transform is nil; must be authoritative")
	}
	if got.NewContent != nil {
		t.Errorf("NewContent = %v, want nil (Transform is authoritative)", got.NewContent)
	}
	if got.Parser == nil {
		t.Errorf("Parser is nil; write-path Diff/reparse steps need it")
	}
	// OwnedKeys must equal the frozen allowlist — bug-safety net if
	// Plan ever tries to advertise a narrower set than Files().
	if !reflect.DeepEqual(sortedCopy(got.OwnedKeys), sortedCopy(claudecode.OwnedKeysSettingsJSON)) {
		t.Errorf("OwnedKeys = %v, want %v", got.OwnedKeys, claudecode.OwnedKeysSettingsJSON)
	}
	if !strings.Contains(got.Reason, "p") {
		t.Errorf("Reason = %q, want to reference profile name", got.Reason)
	}
	if got.DryRun {
		t.Errorf("DryRun = true; Plan must not set DryRun")
	}
	if got.AllowUnowned {
		t.Errorf("AllowUnowned = true; Plan must not set AllowUnowned")
	}
	if got.MustNotExist {
		t.Errorf("MustNotExist = true; Plan must not set MustNotExist")
	}
}

func TestPlan_FirstWriteFromEmpty(t *testing.T) {
	// AC: given an empty settings.json (fresh install), Plan's
	// transform writes owned keys and leaves nothing else behind.
	//
	// Two rows: (a) all four Core string slots populated including
	// SmallFastModel — pins the positive-write path for
	// env.ANTHROPIC_SMALL_FAST_MODEL (previously covered only by the
	// "delete when empty" arm); (b) SmallFastModel intentionally empty
	// — pins the "absent → deleted" arm alongside the other keys.
	r := newResolver(t)

	tests := []struct {
		name               string
		profile            config.Profile
		wantBaseURL        string
		wantAuthToken      string
		wantModel          string
		wantSmallFastModel string // "" means the key must be ABSENT
	}{
		{
			name: "all four core slots populated",
			profile: config.Profile{
				Name: "anthropic-us",
				Core: config.CoreConfig{
					BaseURL:        "https://api.example.com",
					APIKey:         "sk-first-write",
					Model:          "claude-opus-4-5",
					SmallFastModel: "claude-haiku-4-5",
				},
			},
			wantBaseURL:        "https://api.example.com",
			wantAuthToken:      "sk-first-write",
			wantModel:          "claude-opus-4-5",
			wantSmallFastModel: "claude-haiku-4-5",
		},
		{
			name: "small-fast-model intentionally empty is absent",
			profile: config.Profile{
				Name: "anthropic-us",
				Core: config.CoreConfig{
					BaseURL: "https://api.example.com",
					APIKey:  "sk-first-write",
					Model:   "claude-opus-4-5",
				},
			},
			wantBaseURL:        "https://api.example.com",
			wantAuthToken:      "sk-first-write",
			wantModel:          "claude-opus-4-5",
			wantSmallFastModel: "", // absent expected
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			plan := runPlan(t, r, tc.profile)

			out := transformCurrent(t, plan, []byte(""))
			got := mustUnmarshal(t, out)

			if v, ok := getPath(got, "env.ANTHROPIC_BASE_URL"); !ok || v != tc.wantBaseURL {
				t.Errorf("env.ANTHROPIC_BASE_URL = %v (ok=%v), want %q", v, ok, tc.wantBaseURL)
			}
			if v, ok := getPath(got, "env.ANTHROPIC_AUTH_TOKEN"); !ok || v != tc.wantAuthToken {
				t.Errorf("env.ANTHROPIC_AUTH_TOKEN = %v (ok=%v), want %q", v, ok, tc.wantAuthToken)
			}
			if v, ok := getPath(got, "env.ANTHROPIC_MODEL"); !ok || v != tc.wantModel {
				t.Errorf("env.ANTHROPIC_MODEL = %v (ok=%v), want %q", v, ok, tc.wantModel)
			}
			if tc.wantSmallFastModel == "" {
				// Overlay-as-truth: empty Core.SmallFastModel → the key
				// must NOT appear on disk.
				if _, ok := getPath(got, "env.ANTHROPIC_SMALL_FAST_MODEL"); ok {
					t.Errorf("env.ANTHROPIC_SMALL_FAST_MODEL present, want absent (Core.SmallFastModel empty)")
				}
				return
			}
			// Positive-write assertion: SmallFastModel populated → must
			// land at env.ANTHROPIC_SMALL_FAST_MODEL verbatim. This is
			// the arm PR #23 reviewer flagged as missing.
			if v, ok := getPath(got, "env.ANTHROPIC_SMALL_FAST_MODEL"); !ok || v != tc.wantSmallFastModel {
				t.Errorf("env.ANTHROPIC_SMALL_FAST_MODEL = %v (ok=%v), want %q", v, ok, tc.wantSmallFastModel)
			}
		})
	}
}

func TestPlan_MergePreservesUnknownKeys(t *testing.T) {
	// AC: unknown keys (permissions, hooks, mcpServers, unrelated env
	// vars, top-level scalars) round-trip byte-preserved. This is the
	// whole point of sjson.
	r := newResolver(t)
	profile := config.Profile{
		Name: "haiku",
		Core: config.CoreConfig{Model: "claude-haiku-4-5"},
	}
	current := []byte(`{"env":{"UNRELATED":"keep","OTHER":42},"top-level-other":42,"permissions":{"allowed":["fs"]}}`)
	plan := runPlan(t, r, profile)

	out := transformCurrent(t, plan, current)
	got := mustUnmarshal(t, out)

	if v, ok := getPath(got, "env.UNRELATED"); !ok || v != "keep" {
		t.Errorf("env.UNRELATED = %v, want \"keep\" (must be preserved verbatim)", v)
	}
	if v, ok := getPath(got, "env.OTHER"); !ok || v.(float64) != 42 {
		t.Errorf("env.OTHER = %v, want 42 (numeric preserved)", v)
	}
	if v, ok := got["top-level-other"]; !ok || v.(float64) != 42 {
		t.Errorf("top-level-other = %v, want 42", v)
	}
	perms, ok := getPath(got, "permissions.allowed")
	if !ok {
		t.Fatalf("permissions.allowed missing, want preserved")
	}
	if arr, ok := perms.([]any); !ok || len(arr) != 1 || arr[0] != "fs" {
		t.Errorf("permissions.allowed = %v, want [\"fs\"]", perms)
	}
	if v, ok := getPath(got, "env.ANTHROPIC_MODEL"); !ok || v != "claude-haiku-4-5" {
		t.Errorf("env.ANTHROPIC_MODEL = %v, want \"claude-haiku-4-5\"", v)
	}
}

func TestPlan_OverlayAsTruthDeletesUnsetKeys(t *testing.T) {
	// NFR-S6 overlay-as-truth: switching to a profile that no longer
	// owns a slot must REMOVE the key from settings.json, not preserve
	// the previous profile's stale value.
	r := newResolver(t)
	profile := config.Profile{
		Name: "no-base-url",
		Core: config.CoreConfig{
			// BaseURL intentionally empty — should delete the existing key.
			APIKey: "sk-new",
			Model:  "claude-opus-4-5",
		},
	}
	current := []byte(`{"env":{"ANTHROPIC_BASE_URL":"https://old.example.com","OTHER":"keep-me","ANTHROPIC_MODEL":"stale"}}`)
	plan := runPlan(t, r, profile)

	out := transformCurrent(t, plan, current)
	got := mustUnmarshal(t, out)

	if _, ok := getPath(got, "env.ANTHROPIC_BASE_URL"); ok {
		t.Errorf("env.ANTHROPIC_BASE_URL present, want deleted (Core.BaseURL empty in profile)")
	}
	if v, ok := getPath(got, "env.OTHER"); !ok || v != "keep-me" {
		t.Errorf("env.OTHER = %v, want \"keep-me\" (unrelated key must survive)", v)
	}
	if v, ok := getPath(got, "env.ANTHROPIC_MODEL"); !ok || v != "claude-opus-4-5" {
		t.Errorf("env.ANTHROPIC_MODEL = %v, want \"claude-opus-4-5\" (profile overrides stale)", v)
	}
	if v, ok := getPath(got, "env.ANTHROPIC_AUTH_TOKEN"); !ok || v != "sk-new" {
		t.Errorf("env.ANTHROPIC_AUTH_TOKEN = %v, want \"sk-new\"", v)
	}
}

func TestPlan_ExtraEnvKeysWrittenFromOverlay(t *testing.T) {
	// The Claude Code / Bedrock / Vertex toggles are overlay-side
	// (not Core-neutral). Verify they flow from Overlay.ExtraEnv.
	r := newResolver(t)
	profile := config.Profile{
		Name: "bedrock",
		Tools: map[config.ToolID]config.ToolOverlay{
			adapter.ToolClaudeCode: {
				ExtraEnv: map[string]string{
					"CLAUDE_CODE_USE_BEDROCK": "1",
					"CLAUDE_CODE_USE_VERTEX":  "",
				},
			},
		},
	}
	plan := runPlan(t, r, profile)

	out := transformCurrent(t, plan, []byte("{}"))
	got := mustUnmarshal(t, out)

	if v, ok := getPath(got, "env.CLAUDE_CODE_USE_BEDROCK"); !ok || v != "1" {
		t.Errorf("env.CLAUDE_CODE_USE_BEDROCK = %v, want \"1\"", v)
	}
	// Explicit empty string via overlay IS a real value (see plan.go
	// godoc). Overlay-driven slots pass "" through verbatim.
	if v, ok := getPath(got, "env.CLAUDE_CODE_USE_VERTEX"); !ok || v != "" {
		t.Errorf("env.CLAUDE_CODE_USE_VERTEX = %v (ok=%v), want empty string (overlay pins '')", v, ok)
	}
}

func TestPlan_APIKeyDualHousing(t *testing.T) {
	// Round-trip fidelity vs Import: Core.APIKey → AUTH_TOKEN and
	// Overlay.ExtraEnv["ANTHROPIC_API_KEY"] → API_KEY (both present).
	r := newResolver(t)
	profile := config.Profile{
		Name: "dual",
		Core: config.CoreConfig{APIKey: "sk-auth"},
		Tools: map[config.ToolID]config.ToolOverlay{
			adapter.ToolClaudeCode: {
				ExtraEnv: map[string]string{"ANTHROPIC_API_KEY": "sk-secondary"},
			},
		},
	}
	plan := runPlan(t, r, profile)

	out := transformCurrent(t, plan, []byte("{}"))
	got := mustUnmarshal(t, out)

	if v, ok := getPath(got, "env.ANTHROPIC_AUTH_TOKEN"); !ok || v != "sk-auth" {
		t.Errorf("env.ANTHROPIC_AUTH_TOKEN = %v, want \"sk-auth\"", v)
	}
	if v, ok := getPath(got, "env.ANTHROPIC_API_KEY"); !ok || v != "sk-secondary" {
		t.Errorf("env.ANTHROPIC_API_KEY = %v, want \"sk-secondary\"", v)
	}
}

func TestPlan_MalformedCurrentBytesError(t *testing.T) {
	// NFR-S1 / FR-5 step 3: refuse on malformed current bytes. No
	// silent fallback rewrite. Cover four shapes so a future
	// permissive regression (e.g. sjson quietly writing over `null` at
	// the root, or a bare scalar sneaking past gjson.ValidBytes) is
	// caught by the exact assertion here rather than by a downstream
	// mystery. All four rows must produce ErrParseFailed — the
	// object-root check in renderSettings is the load-bearing invariant
	// that overlay-as-truth only applies to object-shaped documents.
	r := newResolver(t)
	plan := runPlan(t, r, config.Profile{Name: "p", Core: config.CoreConfig{Model: "x"}})

	if plan.Transform == nil {
		t.Fatalf("Transform is nil")
	}

	tests := []struct {
		name    string
		current []byte
	}{
		{
			// Truncated object: gjson.ValidBytes rejects — the
			// original coverage row, kept as the baseline.
			name:    "unterminated string",
			current: []byte(`{"env":`),
		},
		{
			// Bare `null` at root: gjson.ValidBytes ACCEPTS `null` as
			// a legal JSON value. Without an explicit "root must be
			// object" check the render would either silently succeed
			// writing over `null` or wander into a confusing sjson
			// error path. Refuse loudly with ErrParseFailed.
			name:    "bare null at root",
			current: []byte(`null`),
		},
		{
			// Trailing junk after an otherwise valid object.
			// encoding/json.Valid rejects this shape; gjson may or may
			// not depending on release. The belt-and-brace json.Valid
			// check in renderSettings must catch it either way.
			name:    "trailing junk after object",
			current: []byte(`{"env":{}} garbage`),
		},
		{
			// Bare scalar (string) at root. Same category as bare
			// null: legal JSON, illegal settings.json shape.
			name:    "bare scalar string at root",
			current: []byte(`"hello"`),
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := plan.Transform(tc.current)
			if err == nil {
				t.Fatalf("Transform on %s returned nil error, want ErrParseFailed", tc.name)
			}
			if !errors.Is(err, writepath.ErrParseFailed) {
				t.Errorf("Transform err = %v, want errors.Is ErrParseFailed", err)
			}
		})
	}
}

func TestPlan_EmptyProfileClearsAllOwnedKeys(t *testing.T) {
	// Overlay-as-truth extreme: an empty Profile owns nothing, so
	// every owned key must be deleted while unrelated keys survive.
	r := newResolver(t)
	current := []byte(`{
	"env": {
		"ANTHROPIC_API_KEY": "old-key",
		"ANTHROPIC_AUTH_TOKEN": "old-token",
		"ANTHROPIC_BASE_URL": "https://old.example.com",
		"ANTHROPIC_MODEL": "old-model",
		"ANTHROPIC_SMALL_FAST_MODEL": "old-haiku",
		"CLAUDE_CODE_USE_BEDROCK": "1",
		"CLAUDE_CODE_USE_VERTEX": "1",
		"UNRELATED": "keep"
	},
	"permissions": {"allowed": ["fs"]},
	"model": "keep-this-untouched"
}`)
	plan := runPlan(t, r, config.Profile{Name: "empty"})

	out := transformCurrent(t, plan, current)
	got := mustUnmarshal(t, out)

	for _, key := range claudecode.OwnedKeysSettingsJSON {
		if _, ok := getPath(got, key); ok {
			t.Errorf("owned key %q still present, want deleted (empty profile)", key)
		}
	}
	if v, ok := getPath(got, "env.UNRELATED"); !ok || v != "keep" {
		t.Errorf("env.UNRELATED = %v, want \"keep\"", v)
	}
	if v, ok := got["model"]; !ok || v != "keep-this-untouched" {
		t.Errorf("model = %v, want \"keep-this-untouched\" (unowned)", v)
	}
	perms, ok := getPath(got, "permissions.allowed")
	if !ok {
		t.Fatalf("permissions.allowed missing")
	}
	if arr, ok := perms.([]any); !ok || len(arr) != 1 || arr[0] != "fs" {
		t.Errorf("permissions.allowed = %v, want [\"fs\"]", perms)
	}
}

func TestPlan_WhitespaceOnlyCurrentTreatedAsEmpty(t *testing.T) {
	// A settings.json that contains only whitespace ("\n" or "   ")
	// is a legal empty file; Transform must treat it as {}.
	r := newResolver(t)
	profile := config.Profile{Name: "ws", Core: config.CoreConfig{Model: "claude-opus-4-5"}}
	plan := runPlan(t, r, profile)

	out := transformCurrent(t, plan, []byte("   \n\t\n"))
	got := mustUnmarshal(t, out)
	if v, ok := getPath(got, "env.ANTHROPIC_MODEL"); !ok || v != "claude-opus-4-5" {
		t.Errorf("env.ANTHROPIC_MODEL = %v, want \"claude-opus-4-5\"", v)
	}
}

func TestPlan_ThroughApply_HappyPath(t *testing.T) {
	// End-to-end: Plan → writepath.Apply → verify on-disk bytes.
	// Exercises the FR-5 pipeline against a real per-test HOME so
	// "Plan output plugs into the locked write-path" is proved, not
	// just claimed. First-write (no prior file) so no backup expected.
	r := newResolver(t)
	settings := claudecode.SettingsPath(r)
	if err := os.MkdirAll(filepath.Dir(settings), 0o700); err != nil {
		t.Fatalf("mkdir parent: %v", err)
	}

	profile := config.Profile{
		Name: "e2e",
		Core: config.CoreConfig{
			BaseURL: "https://api.example.com",
			APIKey:  "sk-e2e",
			Model:   "claude-opus-4-5",
		},
	}
	plan := runPlan(t, r, profile)

	report, err := writepath.Apply(context.Background(), r, plan)
	if err != nil {
		t.Fatalf("writepath.Apply: %v", err)
	}
	if report.Skipped {
		t.Errorf("report.Skipped = true, want false (first write)")
	}
	if report.RolledBack {
		t.Errorf("report.RolledBack = true, want false")
	}
	if report.Backup.BackupPath != "" {
		t.Errorf("report.Backup.BackupPath = %q, want empty (first-write case has no backup)", report.Backup.BackupPath)
	}

	data, err := os.ReadFile(settings)
	if err != nil {
		t.Fatalf("read settings after Apply: %v", err)
	}
	got := mustUnmarshal(t, data)
	if v, ok := getPath(got, "env.ANTHROPIC_BASE_URL"); !ok || v != "https://api.example.com" {
		t.Errorf("on-disk env.ANTHROPIC_BASE_URL = %v, want \"https://api.example.com\"", v)
	}
	if v, ok := getPath(got, "env.ANTHROPIC_AUTH_TOKEN"); !ok || v != "sk-e2e" {
		t.Errorf("on-disk env.ANTHROPIC_AUTH_TOKEN = %v, want \"sk-e2e\"", v)
	}
	if v, ok := getPath(got, "env.ANTHROPIC_MODEL"); !ok || v != "claude-opus-4-5" {
		t.Errorf("on-disk env.ANTHROPIC_MODEL = %v, want \"claude-opus-4-5\"", v)
	}
	// Second Apply against the now-existing file: must take a backup
	// (there's a prior file to preserve) and not skip when the profile
	// changes an owned value.
	profile2 := config.Profile{
		Name: "e2e-v2",
		Core: config.CoreConfig{
			BaseURL: "https://api.example.com",
			APIKey:  "sk-e2e",
			Model:   "claude-haiku-4-5", // change
		},
	}
	plan2 := runPlan(t, r, profile2)
	report2, err := writepath.Apply(context.Background(), r, plan2)
	if err != nil {
		t.Fatalf("writepath.Apply (second): %v", err)
	}
	if report2.Skipped {
		t.Errorf("second Apply Skipped = true, want false (model changed)")
	}
	if report2.Backup.BackupPath == "" {
		t.Errorf("second Apply Backup.Path empty, want populated (prior file existed)")
	}
	data2, err := os.ReadFile(settings)
	if err != nil {
		t.Fatalf("read settings after second Apply: %v", err)
	}
	got2 := mustUnmarshal(t, data2)
	if v, ok := getPath(got2, "env.ANTHROPIC_MODEL"); !ok || v != "claude-haiku-4-5" {
		t.Errorf("on-disk env.ANTHROPIC_MODEL = %v, want \"claude-haiku-4-5\" (second Apply)", v)
	}
}

// sortedCopy returns a sorted copy of s. Used by TestPlan_ReturnsExactlyOnePlan
// so the OwnedKeys reflect.DeepEqual check does not tickle a false positive
// on sort-order variance between the plan-returned slice and the frozen
// allowlist (both should already be sorted, but assert the property, not
// the incidental ordering).
func sortedCopy(s []string) []string {
	out := make([]string, len(s))
	copy(out, s)
	sort.Strings(out)
	return out
}
