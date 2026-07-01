package resolver

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/a2d2-dev/claudecm/internal/adapter"
	"github.com/a2d2-dev/claudecm/internal/config"
)

// TestFilter_EmptyAllowsAll asserts the zero Filter (Tools nil) and the
// explicitly-empty Filter (Tools: []) both allow every ToolID. This is
// the cmd/current default: no filter flag means "every registered
// adapter participates".
func TestFilter_EmptyAllowsAll(t *testing.T) {
	t.Parallel()

	nilFilter := Filter{}
	emptyFilter := Filter{Tools: []adapter.ToolID{}}

	cases := []adapter.ToolID{
		adapter.ToolClaudeCode,
		adapter.ToolCodex,
		adapter.ToolID("gemini"),   // future/unknown must still pass
		adapter.ToolID(""),          // even the zero value passes
	}

	for _, tc := range cases {
		if !nilFilter.Allows(tc) {
			t.Errorf("nil Filter must allow %q, got false", tc)
		}
		if !emptyFilter.Allows(tc) {
			t.Errorf("empty Filter must allow %q, got false", tc)
		}
	}
}

// TestFilter_ExplicitList asserts a populated Filter is an allowlist:
// listed IDs pass, unlisted IDs do not.
func TestFilter_ExplicitList(t *testing.T) {
	t.Parallel()

	f := Filter{Tools: []adapter.ToolID{adapter.ToolCodex}}

	if !f.Allows(adapter.ToolCodex) {
		t.Errorf("Filter{Codex}.Allows(Codex) = false, want true")
	}
	if f.Allows(adapter.ToolClaudeCode) {
		t.Errorf("Filter{Codex}.Allows(ClaudeCode) = true, want false")
	}
	if f.Allows(adapter.ToolID("gemini")) {
		t.Errorf("Filter{Codex}.Allows(gemini) = true, want false")
	}
}

// TestFilter_MultipleAllows asserts multi-entry filters behave as a
// set-membership check.
func TestFilter_MultipleAllows(t *testing.T) {
	t.Parallel()

	f := Filter{Tools: []adapter.ToolID{adapter.ToolClaudeCode, adapter.ToolCodex}}

	if !f.Allows(adapter.ToolClaudeCode) {
		t.Errorf("Allows(ClaudeCode) = false, want true")
	}
	if !f.Allows(adapter.ToolCodex) {
		t.Errorf("Allows(Codex) = false, want true")
	}
	if f.Allows(adapter.ToolID("gemini")) {
		t.Errorf("Allows(gemini) = true, want false")
	}
}

// TestErrorKind_ValuesFrozen sanity-checks the ErrorKind enum string
// values. The wire format is exposed via cmd/current --output json;
// changing a value here is a break for downstream JSON consumers.
func TestErrorKind_ValuesFrozen(t *testing.T) {
	t.Parallel()

	want := map[ErrorKind]string{
		ErrorDetectFailed:  "DetectFailed",
		ErrorProjectFailed: "ProjectFailed",
		ErrorParseFailed:   "ParseFailed",
		ErrorOutsideHome:   "OutsideHome",
		ErrorCanceled:      "Canceled",
	}
	for k, s := range want {
		if string(k) != s {
			t.Errorf("ErrorKind %q underlying string = %q, want %q", s, string(k), s)
		}
	}
}

// TestResolve_StubReturnsNotImplemented verifies the E5-S1 stub
// returns ErrNotImplemented (via errors.Is) and an empty View. E5-S2
// replaces this behaviour; this test is expected to be updated (not
// deleted) when the aggregation loop lands.
func TestResolve_StubReturnsNotImplemented(t *testing.T) {
	t.Parallel()

	profile := config.Profile{
		SchemaVersion: config.CurrentProfileSchemaVersion,
		Name:          "test-profile",
	}

	view, err := Resolve(context.Background(), nil, nil, profile, Filter{})

	if !errors.Is(err, ErrNotImplemented) {
		t.Fatalf("Resolve error = %v, want ErrNotImplemented", err)
	}
	if !reflect.DeepEqual(view, View{}) {
		t.Errorf("Resolve view = %#v, want zero View", view)
	}
}

// TestResolve_StubIgnoresFilter checks the stub ignores every argument
// symmetrically (no argument crashes the stub, all combinations return
// ErrNotImplemented). Guards against a future accidental partial
// implementation slipping in without a test.
func TestResolve_StubIgnoresFilter(t *testing.T) {
	t.Parallel()

	reg := adapter.NewRegistry()
	filter := Filter{Tools: []adapter.ToolID{adapter.ToolClaudeCode}}
	profile := config.Profile{Name: "x"}

	view, err := Resolve(context.Background(), nil, reg, profile, filter)
	if !errors.Is(err, ErrNotImplemented) {
		t.Fatalf("Resolve error = %v, want ErrNotImplemented", err)
	}
	if len(view.Tools) != 0 {
		t.Errorf("stub view.Tools len = %d, want 0", len(view.Tools))
	}
}

// TestView_Shape asserts the compile-time shape of View, ToolView, and
// ToolError. Refactoring the struct layout in a way that breaks a
// downstream renderer (e.g. renaming Effective to something else) will
// fail this test.
func TestView_Shape(t *testing.T) {
	t.Parallel()

	v := View{
		Profile: config.Profile{Name: "p"},
		Tools: []ToolView{
			{
				Tool:     adapter.ToolClaudeCode,
				Presence: adapter.Presence{Installed: true, Detected: true, ConfigDir: "/home/x/.claude"},
				Effective: adapter.EffectiveView{
					Tool: adapter.ToolClaudeCode,
					Fields: []adapter.EffectiveField{
						{Key: "env.ANTHROPIC_API_KEY", Value: "sk-live", Secret: true, WinningLayer: adapter.LayerOnDisk, Source: "/home/x/.claude/settings.json:env.ANTHROPIC_API_KEY"},
					},
					ExternalDriftDetected: true,
					ExternalDriftFile:     "/home/x/.claude/settings.json",
				},
				Errors: []ToolError{
					{Kind: ErrorParseFailed, Message: "unparseable JSON", File: "/home/x/.claude/settings.json"},
				},
			},
		},
	}

	if v.Profile.Name != "p" {
		t.Errorf("View.Profile.Name = %q, want %q", v.Profile.Name, "p")
	}
	if len(v.Tools) != 1 {
		t.Fatalf("View.Tools len = %d, want 1", len(v.Tools))
	}
	tv := v.Tools[0]
	if tv.Tool != adapter.ToolClaudeCode {
		t.Errorf("ToolView.Tool = %q, want %q", tv.Tool, adapter.ToolClaudeCode)
	}
	if !tv.Presence.Installed {
		t.Errorf("ToolView.Presence.Installed = false, want true")
	}
	if !tv.Effective.ExternalDriftDetected {
		t.Errorf("ToolView.Effective.ExternalDriftDetected = false, want true")
	}
	if len(tv.Effective.Fields) != 1 || tv.Effective.Fields[0].Key != "env.ANTHROPIC_API_KEY" {
		t.Errorf("ToolView.Effective.Fields not carried through, got %#v", tv.Effective.Fields)
	}
	if len(tv.Errors) != 1 || tv.Errors[0].Kind != ErrorParseFailed {
		t.Errorf("ToolView.Errors not carried through, got %#v", tv.Errors)
	}
}

// TestErrNotImplemented_StableIdentity guards against a refactor that
// accidentally reallocates the sentinel each call. errors.Is must
// keep matching across multiple Resolve invocations because E5-S2's
// removal plan hinges on grep-able callers checking the same sentinel.
func TestErrNotImplemented_StableIdentity(t *testing.T) {
	t.Parallel()

	_, err1 := Resolve(context.Background(), nil, nil, config.Profile{}, Filter{})
	_, err2 := Resolve(context.Background(), nil, nil, config.Profile{}, Filter{})

	if !errors.Is(err1, ErrNotImplemented) || !errors.Is(err2, ErrNotImplemented) {
		t.Fatalf("Resolve did not return ErrNotImplemented consistently: err1=%v err2=%v", err1, err2)
	}
	if err1 != err2 {
		t.Errorf("ErrNotImplemented identity drift: err1=%p err2=%p", err1, err2)
	}
}
