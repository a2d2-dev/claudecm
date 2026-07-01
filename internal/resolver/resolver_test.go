package resolver

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"testing"

	"github.com/a2d2-dev/claudecm/internal/adapter"

	// side-effect imports register the two v1 adapters into
	// adapter.DefaultRegistry from their init() blocks. Required by
	// TestResolve_NilRegistryFallsBackToDefault.
	_ "github.com/a2d2-dev/claudecm/internal/adapter/claudecode"
	_ "github.com/a2d2-dev/claudecm/internal/adapter/codex"
	"github.com/a2d2-dev/claudecm/internal/config"
	"github.com/a2d2-dev/claudecm/internal/storage"
	"github.com/a2d2-dev/claudecm/internal/writepath"
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
		adapter.ToolID("gemini"), // future/unknown must still pass
		adapter.ToolID(""),       // even the zero value passes
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

// mockAdapter is a test-local Adapter used by the Resolve tests. Only
// Detect and Project are exercised by resolver.Resolve; the other
// methods return zero values and are present to satisfy the interface.
// Behaviour is fully configurable via the exported fields.
type mockAdapter struct {
	id adapter.ToolID

	presence adapter.Presence
	detectFn func(ctx context.Context) (adapter.Presence, error)
	detectEr error

	projectView adapter.EffectiveView
	projectFn   func(ctx context.Context) (adapter.EffectiveView, error)
	projectErr  error
}

func (m *mockAdapter) ID() adapter.ToolID { return m.id }

func (m *mockAdapter) Detect(ctx context.Context, _ *storage.Resolver) (adapter.Presence, error) {
	if m.detectFn != nil {
		return m.detectFn(ctx)
	}
	return m.presence, m.detectEr
}

func (m *mockAdapter) Files(_ *storage.Resolver) adapter.OwnedFiles { return nil }

func (m *mockAdapter) Import(_ context.Context, _ *storage.Resolver) (adapter.CoreFromTool, adapter.OverlayFromTool, error) {
	return adapter.CoreFromTool{}, adapter.OverlayFromTool{}, nil
}

func (m *mockAdapter) Plan(_ context.Context, _ *storage.Resolver, _ config.Profile) ([]adapter.WritePlan, error) {
	return nil, nil
}

func (m *mockAdapter) Apply(_ context.Context, _ *storage.Resolver, _ adapter.WritePlan) (adapter.ApplyReport, error) {
	return adapter.ApplyReport{}, nil
}

func (m *mockAdapter) Project(ctx context.Context, _ *storage.Resolver, _ config.Profile) (adapter.EffectiveView, error) {
	if m.projectFn != nil {
		return m.projectFn(ctx)
	}
	return m.projectView, m.projectErr
}

// registerMock wires a mockAdapter constructor into reg under id. The
// mock is captured by pointer so tests can mutate its behaviour after
// registration if they need to (though most tests configure once).
func registerMock(reg *adapter.Registry, m *mockAdapter) {
	reg.Register(m.id, func() adapter.Adapter { return m })
}

// testHomeResolver builds a Resolver anchored on t.TempDir() so tests
// never touch the developer's real HOME.
func testHomeResolver(t *testing.T) *storage.Resolver {
	t.Helper()
	r, err := storage.NewResolverWithHome(t.TempDir())
	if err != nil {
		t.Fatalf("NewResolverWithHome: %v", err)
	}
	return r
}

// TestResolve_HappyBothTools registers two mock adapters that both
// return non-error Detect + Project results. The resulting View must
// carry both ToolViews with populated Presence + Effective and no
// per-tool errors.
func TestResolve_HappyBothTools(t *testing.T) {
	t.Parallel()

	reg := adapter.NewRegistry()
	mockA := &mockAdapter{
		id:       adapter.ToolID("mock_a"),
		presence: adapter.Presence{Installed: true, Detected: true, ConfigDir: "/tmp/a", Notes: "detected"},
		projectView: adapter.EffectiveView{
			Tool:   adapter.ToolID("mock_a"),
			Fields: []adapter.EffectiveField{{Key: "core.model", Value: "opus", WinningLayer: adapter.LayerCore, Source: "profile.core:core.model"}},
		},
	}
	mockB := &mockAdapter{
		id:       adapter.ToolID("mock_b"),
		presence: adapter.Presence{Installed: true, Detected: true, ConfigDir: "/tmp/b"},
		projectView: adapter.EffectiveView{
			Tool:   adapter.ToolID("mock_b"),
			Fields: []adapter.EffectiveField{{Key: "core.base_url", Value: "https://api.example", WinningLayer: adapter.LayerCore, Source: "profile.core:core.base_url"}},
		},
	}
	registerMock(reg, mockA)
	registerMock(reg, mockB)

	profile := config.Profile{SchemaVersion: config.CurrentProfileSchemaVersion, Name: "p"}
	view, err := Resolve(context.Background(), testHomeResolver(t), reg, profile, Filter{})
	if err != nil {
		t.Fatalf("Resolve err = %v, want nil", err)
	}
	if view.Profile.Name != "p" {
		t.Errorf("view.Profile.Name = %q, want %q", view.Profile.Name, "p")
	}
	if len(view.Tools) != 2 {
		t.Fatalf("view.Tools len = %d, want 2", len(view.Tools))
	}
	for _, tv := range view.Tools {
		if len(tv.Errors) != 0 {
			t.Errorf("tool %q: unexpected Errors: %+v", tv.Tool, tv.Errors)
		}
		if !tv.Presence.Detected {
			t.Errorf("tool %q: Presence.Detected = false, want true", tv.Tool)
		}
		if len(tv.Effective.Fields) == 0 {
			t.Errorf("tool %q: Effective.Fields empty", tv.Tool)
		}
	}
}

// TestResolve_FilteredNarrowsToSubset registers three adapters and
// asks Resolve to return only two of them. Filter-excluded tools MUST
// NOT appear in View.Tools at all — not even as zero-filled entries.
func TestResolve_FilteredNarrowsToSubset(t *testing.T) {
	t.Parallel()

	reg := adapter.NewRegistry()
	for _, id := range []adapter.ToolID{"a", "b", "c"} {
		id := id
		registerMock(reg, &mockAdapter{
			id:          id,
			presence:    adapter.Presence{Detected: true},
			projectView: adapter.EffectiveView{Tool: id},
		})
	}

	f := Filter{Tools: []adapter.ToolID{"a", "c"}}
	view, err := Resolve(context.Background(), testHomeResolver(t), reg, config.Profile{}, f)
	if err != nil {
		t.Fatalf("Resolve err = %v, want nil", err)
	}
	if len(view.Tools) != 2 {
		t.Fatalf("view.Tools len = %d, want 2", len(view.Tools))
	}
	if view.Tools[0].Tool != "a" || view.Tools[1].Tool != "c" {
		t.Errorf("view.Tools order = [%q, %q], want [a, c]", view.Tools[0].Tool, view.Tools[1].Tool)
	}
}

// TestResolve_DetectFailsSurfacesToolError verifies a Detect error is
// captured as ErrorDetectFailed on the ToolView while Project still
// runs and populates Effective. The resolve as a whole succeeds.
func TestResolve_DetectFailsSurfacesToolError(t *testing.T) {
	t.Parallel()

	reg := adapter.NewRegistry()
	registerMock(reg, &mockAdapter{
		id:       adapter.ToolID("mock_a"),
		detectEr: errors.New("boom detect"),
		projectView: adapter.EffectiveView{
			Tool:   adapter.ToolID("mock_a"),
			Fields: []adapter.EffectiveField{{Key: "core.model", Value: "opus", WinningLayer: adapter.LayerCore}},
		},
	})

	view, err := Resolve(context.Background(), testHomeResolver(t), reg, config.Profile{}, Filter{})
	if err != nil {
		t.Fatalf("Resolve err = %v, want nil", err)
	}
	if len(view.Tools) != 1 {
		t.Fatalf("view.Tools len = %d, want 1", len(view.Tools))
	}
	tv := view.Tools[0]
	if len(tv.Errors) != 1 || tv.Errors[0].Kind != ErrorDetectFailed {
		t.Fatalf("tv.Errors = %+v, want one ErrorDetectFailed entry", tv.Errors)
	}
	if len(tv.Effective.Fields) != 1 {
		t.Errorf("Effective.Fields not populated despite Detect failure: %+v", tv.Effective)
	}
}

// TestResolve_ProjectFailsSurfacesToolError verifies a Project error
// is captured as ErrorProjectFailed with a zero EffectiveView. The
// resolve as a whole succeeds.
func TestResolve_ProjectFailsSurfacesToolError(t *testing.T) {
	t.Parallel()

	reg := adapter.NewRegistry()
	registerMock(reg, &mockAdapter{
		id:         adapter.ToolID("mock_a"),
		presence:   adapter.Presence{Detected: true},
		projectErr: errors.New("boom project"),
	})

	view, err := Resolve(context.Background(), testHomeResolver(t), reg, config.Profile{}, Filter{})
	if err != nil {
		t.Fatalf("Resolve err = %v, want nil", err)
	}
	if len(view.Tools) != 1 {
		t.Fatalf("view.Tools len = %d, want 1", len(view.Tools))
	}
	tv := view.Tools[0]
	if len(tv.Errors) != 1 || tv.Errors[0].Kind != ErrorProjectFailed {
		t.Fatalf("tv.Errors = %+v, want one ErrorProjectFailed entry", tv.Errors)
	}
	if !reflect.DeepEqual(tv.Effective, adapter.EffectiveView{}) {
		t.Errorf("Effective = %+v, want zero value on Project failure", tv.Effective)
	}
	// Presence still populated from Detect.
	if !tv.Presence.Detected {
		t.Errorf("Presence.Detected lost despite Project-only failure: %+v", tv.Presence)
	}
}

// TestResolve_ProjectReturnsParseFailedCategorized asserts a Project
// error wrapping writepath.ErrParseFailed is classified as
// ErrorParseFailed.
func TestResolve_ProjectReturnsParseFailedCategorized(t *testing.T) {
	t.Parallel()

	reg := adapter.NewRegistry()
	registerMock(reg, &mockAdapter{
		id:         adapter.ToolID("mock_a"),
		projectErr: fmt.Errorf("codex project: %w", writepath.ErrParseFailed),
	})

	view, err := Resolve(context.Background(), testHomeResolver(t), reg, config.Profile{}, Filter{})
	if err != nil {
		t.Fatalf("Resolve err = %v, want nil", err)
	}
	tv := view.Tools[0]
	if len(tv.Errors) != 1 || tv.Errors[0].Kind != ErrorParseFailed {
		t.Fatalf("tv.Errors = %+v, want one ErrorParseFailed entry", tv.Errors)
	}
}

// TestResolve_ProjectReturnsOutsideHomeCategorized asserts a Project
// error wrapping storage.ErrOutsideHome is classified as
// ErrorOutsideHome.
func TestResolve_ProjectReturnsOutsideHomeCategorized(t *testing.T) {
	t.Parallel()

	reg := adapter.NewRegistry()
	registerMock(reg, &mockAdapter{
		id:         adapter.ToolID("mock_a"),
		projectErr: fmt.Errorf("claudecode project: %w", storage.ErrOutsideHome),
	})

	view, err := Resolve(context.Background(), testHomeResolver(t), reg, config.Profile{}, Filter{})
	if err != nil {
		t.Fatalf("Resolve err = %v, want nil", err)
	}
	tv := view.Tools[0]
	if len(tv.Errors) != 1 || tv.Errors[0].Kind != ErrorOutsideHome {
		t.Fatalf("tv.Errors = %+v, want one ErrorOutsideHome entry", tv.Errors)
	}
}

// TestResolve_ProjectReturnsWritepathOutsideHomeCategorized asserts a
// Project error wrapping writepath.ErrOutsideHome (which itself wraps
// storage.ErrOutsideHome but has its own sentinel) is classified as
// ErrorOutsideHome.
func TestResolve_ProjectReturnsWritepathOutsideHomeCategorized(t *testing.T) {
	t.Parallel()

	reg := adapter.NewRegistry()
	registerMock(reg, &mockAdapter{
		id:         adapter.ToolID("mock_a"),
		projectErr: fmt.Errorf("wrap: %w", writepath.ErrOutsideHome),
	})

	view, err := Resolve(context.Background(), testHomeResolver(t), reg, config.Profile{}, Filter{})
	if err != nil {
		t.Fatalf("Resolve err = %v, want nil", err)
	}
	tv := view.Tools[0]
	if len(tv.Errors) != 1 || tv.Errors[0].Kind != ErrorOutsideHome {
		t.Fatalf("tv.Errors = %+v, want one ErrorOutsideHome entry", tv.Errors)
	}
}

// TestResolve_ProjectContextCanceledCategorized asserts a Project
// error that wraps context.Canceled is classified as ErrorCanceled
// (not ErrorProjectFailed). The resolve as a whole still succeeds:
// per-tool cancel is a non-fatal condition, only ctx.Err() at the
// top level aborts the walk.
func TestResolve_ProjectContextCanceledCategorized(t *testing.T) {
	t.Parallel()

	reg := adapter.NewRegistry()
	registerMock(reg, &mockAdapter{
		id:         adapter.ToolID("mock_a"),
		projectErr: fmt.Errorf("mid-project: %w", context.Canceled),
	})

	view, err := Resolve(context.Background(), testHomeResolver(t), reg, config.Profile{}, Filter{})
	if err != nil {
		t.Fatalf("Resolve err = %v, want nil (per-tool cancel is non-fatal)", err)
	}
	tv := view.Tools[0]
	if len(tv.Errors) != 1 || tv.Errors[0].Kind != ErrorCanceled {
		t.Fatalf("tv.Errors = %+v, want one ErrorCanceled entry", tv.Errors)
	}
}

// TestResolve_CtxCanceledBetweenTools cancels the outer context after
// the first tool has completed. The second tool's ctx.Err() check
// must catch it and Resolve must return the partial View built so far
// plus context.Canceled.
func TestResolve_CtxCanceledBetweenTools(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	reg := adapter.NewRegistry()
	// mockA: cancel the outer ctx as a side effect of Project so
	// mockB's top-of-loop ctx.Err() check catches it before Project
	// runs.
	registerMock(reg, &mockAdapter{
		id:       adapter.ToolID("aa"),
		presence: adapter.Presence{Detected: true},
		projectFn: func(_ context.Context) (adapter.EffectiveView, error) {
			cancel()
			return adapter.EffectiveView{
				Tool:   adapter.ToolID("aa"),
				Fields: []adapter.EffectiveField{{Key: "core.model", Value: "opus"}},
			}, nil
		},
	})
	// mockB: must NOT run — if it does, this test fails via the
	// projectFn panic.
	registerMock(reg, &mockAdapter{
		id: adapter.ToolID("bb"),
		projectFn: func(_ context.Context) (adapter.EffectiveView, error) {
			t.Errorf("mockB.Project ran after ctx cancellation")
			return adapter.EffectiveView{}, nil
		},
		detectFn: func(_ context.Context) (adapter.Presence, error) {
			t.Errorf("mockB.Detect ran after ctx cancellation")
			return adapter.Presence{}, nil
		},
	})

	view, err := Resolve(ctx, testHomeResolver(t), reg, config.Profile{}, Filter{})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Resolve err = %v, want context.Canceled", err)
	}
	if len(view.Tools) != 1 {
		t.Fatalf("view.Tools len = %d, want 1 (partial view of mockA only)", len(view.Tools))
	}
	if view.Tools[0].Tool != adapter.ToolID("aa") {
		t.Errorf("partial view first tool = %q, want %q", view.Tools[0].Tool, "aa")
	}
}

// TestResolve_CtxAlreadyCanceledBeforeAnyTool asserts the top-level
// pre-walk ctx.Err() check fires when the caller passes an already-
// canceled context. Resolve returns View{Profile: profile} + the
// context error without touching any adapter.
func TestResolve_CtxAlreadyCanceledBeforeAnyTool(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	reg := adapter.NewRegistry()
	registerMock(reg, &mockAdapter{
		id: adapter.ToolID("aa"),
		detectFn: func(_ context.Context) (adapter.Presence, error) {
			t.Errorf("Detect ran despite pre-canceled ctx")
			return adapter.Presence{}, nil
		},
	})

	profile := config.Profile{Name: "p"}
	view, err := Resolve(ctx, testHomeResolver(t), reg, profile, Filter{})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Resolve err = %v, want context.Canceled", err)
	}
	if view.Profile.Name != "p" {
		t.Errorf("view.Profile.Name = %q, want %q", view.Profile.Name, "p")
	}
	if len(view.Tools) != 0 {
		t.Errorf("view.Tools len = %d, want 0", len(view.Tools))
	}
}

// TestResolve_NilRegistryFallsBackToDefault asserts that reg == nil
// causes Resolve to walk adapter.DefaultRegistry, which the blank
// imports at the top of this file populate with the two v1 adapters.
// Detect/Project may return per-tool errors on a bare tempdir HOME,
// but Resolve itself must NOT return a top-level error.
func TestResolve_NilRegistryFallsBackToDefault(t *testing.T) {
	t.Parallel()

	profile := config.Profile{SchemaVersion: config.CurrentProfileSchemaVersion, Name: "p"}
	view, err := Resolve(context.Background(), testHomeResolver(t), nil, profile, Filter{})
	if err != nil {
		t.Fatalf("Resolve(nil reg) err = %v, want nil", err)
	}

	seen := map[adapter.ToolID]bool{}
	for _, tv := range view.Tools {
		seen[tv.Tool] = true
	}
	if !seen[adapter.ToolClaudeCode] {
		t.Errorf("DefaultRegistry walk did not visit %q; view.Tools=%+v", adapter.ToolClaudeCode, view.Tools)
	}
	if !seen[adapter.ToolCodex] {
		t.Errorf("DefaultRegistry walk did not visit %q; view.Tools=%+v", adapter.ToolCodex, view.Tools)
	}
}

// TestResolve_RegistryInconsistencyIsTopLevelError registers a
// constructor that returns nil. Registry.Get therefore returns
// (nil, true) — an impossible state in real code (Register rejects a
// nil ctor and there is no Unregister) — which the resolver must
// surface as a top-level "registry inconsistency" error rather than
// silently skipping the tool. This is the second of the two allowed
// top-level errors per E5-S1 contract.
func TestResolve_RegistryInconsistencyIsTopLevelError(t *testing.T) {
	t.Parallel()

	reg := adapter.NewRegistry()
	reg.Register(adapter.ToolID("foo"), func() adapter.Adapter { return nil })

	profile := config.Profile{Name: "p"}
	view, err := Resolve(context.Background(), testHomeResolver(t), reg, profile, Filter{})
	if err == nil {
		t.Fatalf("Resolve err = nil, want registry-inconsistency error")
	}
	// Caller diagnostics benefit from "even on error, tell me what
	// profile was attempted"; Profile is preserved, Tools is nil
	// (empty because inconsistency hit before any tool completed).
	if view.Profile.Name != "p" {
		t.Errorf("view.Profile.Name = %q, want %q (Profile must be preserved even on registry inconsistency)", view.Profile.Name, "p")
	}
	if view.Tools != nil {
		t.Errorf("view.Tools = %+v, want nil (inconsistency hit before any tool completed)", view.Tools)
	}
	if got := err.Error(); got == "" || !containsAll(got, "registry inconsistency", "foo") {
		t.Errorf("err.Error() = %q, want mention of 'registry inconsistency' and 'foo'", got)
	}
}

// TestResolve_EmptyRegistryReturnsEmptyView asserts an empty Registry
// yields a View with the Profile carried through but Tools nil / empty
// and no top-level error.
func TestResolve_EmptyRegistryReturnsEmptyView(t *testing.T) {
	t.Parallel()

	reg := adapter.NewRegistry()
	profile := config.Profile{Name: "p"}
	view, err := Resolve(context.Background(), testHomeResolver(t), reg, profile, Filter{})
	if err != nil {
		t.Fatalf("Resolve err = %v, want nil", err)
	}
	if view.Profile.Name != "p" {
		t.Errorf("view.Profile.Name = %q, want %q", view.Profile.Name, "p")
	}
	if len(view.Tools) != 0 {
		t.Errorf("view.Tools len = %d, want 0", len(view.Tools))
	}
}

// TestResolve_SortedToolsOrder registers mocks in reverse
// lexicographic order and asserts View.Tools comes back sorted, because
// Registry.List already sorts. Regression guard: a future refactor
// that iterates the ctors map directly would silently break
// determinism here.
func TestResolve_SortedToolsOrder(t *testing.T) {
	t.Parallel()

	reg := adapter.NewRegistry()
	for _, id := range []adapter.ToolID{"zz", "mm", "aa"} {
		id := id
		registerMock(reg, &mockAdapter{
			id:          id,
			projectView: adapter.EffectiveView{Tool: id},
		})
	}

	view, err := Resolve(context.Background(), testHomeResolver(t), reg, config.Profile{}, Filter{})
	if err != nil {
		t.Fatalf("Resolve err = %v, want nil", err)
	}
	if len(view.Tools) != 3 {
		t.Fatalf("view.Tools len = %d, want 3", len(view.Tools))
	}
	want := []adapter.ToolID{"aa", "mm", "zz"}
	for i, tv := range view.Tools {
		if tv.Tool != want[i] {
			t.Errorf("view.Tools[%d].Tool = %q, want %q", i, tv.Tool, want[i])
		}
	}
}

// TestResolve_FileExtractionFromErrorMessage checks that the best-
// effort file-path extractor lifts a quoted absolute path out of a
// realistic adapter error format for ParseFailed / OutsideHome kinds.
// If extraction fails, File="" is acceptable — but this fixture is
// shaped exactly like the codex/claudecode adapter wraps so the
// extractor MUST succeed.
func TestResolve_FileExtractionFromErrorMessage(t *testing.T) {
	t.Parallel()

	reg := adapter.NewRegistry()
	// Parse failure with a quoted absolute path.
	registerMock(reg, &mockAdapter{
		id:         adapter.ToolID("mock_parse"),
		projectErr: fmt.Errorf(`codex apply "/foo/bar/config.toml": %w`, writepath.ErrParseFailed),
	})
	// OutsideHome with a quoted absolute path.
	registerMock(reg, &mockAdapter{
		id:         adapter.ToolID("mock_outside"),
		projectErr: fmt.Errorf(`claudecode project "/home/x/.claude/settings.json": %w`, storage.ErrOutsideHome),
	})
	// Project error with no quoted path — File must be empty.
	registerMock(reg, &mockAdapter{
		id:         adapter.ToolID("mock_bare"),
		projectErr: fmt.Errorf("adapter internal state corruption"),
	})

	view, err := Resolve(context.Background(), testHomeResolver(t), reg, config.Profile{}, Filter{})
	if err != nil {
		t.Fatalf("Resolve err = %v, want nil", err)
	}
	if len(view.Tools) != 3 {
		t.Fatalf("view.Tools len = %d, want 3", len(view.Tools))
	}

	byID := map[adapter.ToolID]ToolView{}
	for _, tv := range view.Tools {
		byID[tv.Tool] = tv
	}

	parseTV := byID[adapter.ToolID("mock_parse")]
	if len(parseTV.Errors) != 1 || parseTV.Errors[0].Kind != ErrorParseFailed {
		t.Fatalf("mock_parse Errors = %+v, want one ErrorParseFailed", parseTV.Errors)
	}
	if got := parseTV.Errors[0].File; got != "/foo/bar/config.toml" {
		t.Errorf("mock_parse File = %q, want %q", got, "/foo/bar/config.toml")
	}

	outsideTV := byID[adapter.ToolID("mock_outside")]
	if len(outsideTV.Errors) != 1 || outsideTV.Errors[0].Kind != ErrorOutsideHome {
		t.Fatalf("mock_outside Errors = %+v, want one ErrorOutsideHome", outsideTV.Errors)
	}
	if got := outsideTV.Errors[0].File; got != "/home/x/.claude/settings.json" {
		t.Errorf("mock_outside File = %q, want %q", got, "/home/x/.claude/settings.json")
	}

	bareTV := byID[adapter.ToolID("mock_bare")]
	if len(bareTV.Errors) != 1 || bareTV.Errors[0].Kind != ErrorProjectFailed {
		t.Fatalf("mock_bare Errors = %+v, want one ErrorProjectFailed", bareTV.Errors)
	}
	// bareTV categorized as ProjectFailed; File is not populated for
	// this kind by design (only Parse/OutsideHome try extraction).
	if bareTV.Errors[0].File != "" {
		t.Errorf("mock_bare File = %q, want empty (ProjectFailed skips extraction)", bareTV.Errors[0].File)
	}
}

// TestResolve_DetectFailsCarriesPartialPresence asserts that when
// Detect returns a non-nil error AND a non-zero Presence, the resolver
// preserves the Presence value verbatim (best-effort — adapters may
// fill some fields before hitting the error) rather than clobbering
// it. The ErrorDetectFailed entry still lands on the ToolView.
func TestResolve_DetectFailsCarriesPartialPresence(t *testing.T) {
	t.Parallel()

	reg := adapter.NewRegistry()
	registerMock(reg, &mockAdapter{
		id: adapter.ToolID("mock_a"),
		presence: adapter.Presence{
			Installed: true,
			Detected:  false,
			ConfigDir: "/partial/dir",
			Notes:     "partial",
		},
		detectEr: errors.New("boom partial detect"),
	})

	view, err := Resolve(context.Background(), testHomeResolver(t), reg, config.Profile{}, Filter{})
	if err != nil {
		t.Fatalf("Resolve err = %v, want nil", err)
	}
	if len(view.Tools) != 1 {
		t.Fatalf("view.Tools len = %d, want 1", len(view.Tools))
	}
	tv := view.Tools[0]
	if len(tv.Errors) != 1 || tv.Errors[0].Kind != ErrorDetectFailed {
		t.Fatalf("tv.Errors = %+v, want one ErrorDetectFailed entry", tv.Errors)
	}
	// The non-zero Presence returned by Detect must be preserved.
	if !tv.Presence.Installed {
		t.Errorf("Presence.Installed = false, want true (partial Presence must be carried through)")
	}
	if tv.Presence.ConfigDir != "/partial/dir" {
		t.Errorf("Presence.ConfigDir = %q, want %q", tv.Presence.ConfigDir, "/partial/dir")
	}
	if tv.Presence.Notes != "partial" {
		t.Errorf("Presence.Notes = %q, want %q", tv.Presence.Notes, "partial")
	}
}

// TestResolve_DetectReturnsOutsideHomeCategorized asserts a Detect
// error wrapping storage.ErrOutsideHome is classified as
// ErrorOutsideHome (not ErrorDetectFailed) — the classifier is shared
// with Project by design.
func TestResolve_DetectReturnsOutsideHomeCategorized(t *testing.T) {
	t.Parallel()

	reg := adapter.NewRegistry()
	registerMock(reg, &mockAdapter{
		id:       adapter.ToolID("mock_a"),
		detectEr: fmt.Errorf("claudecode detect \"/some/other/root/settings.json\": %w", storage.ErrOutsideHome),
	})

	view, err := Resolve(context.Background(), testHomeResolver(t), reg, config.Profile{}, Filter{})
	if err != nil {
		t.Fatalf("Resolve err = %v, want nil", err)
	}
	if len(view.Tools) != 1 {
		t.Fatalf("view.Tools len = %d, want 1", len(view.Tools))
	}
	tv := view.Tools[0]
	if len(tv.Errors) != 1 {
		t.Fatalf("tv.Errors = %+v, want one entry", tv.Errors)
	}
	if tv.Errors[0].Kind != ErrorOutsideHome {
		t.Errorf("tv.Errors[0].Kind = %q, want %q", tv.Errors[0].Kind, ErrorOutsideHome)
	}
	if tv.Errors[0].File != "/some/other/root/settings.json" {
		t.Errorf("tv.Errors[0].File = %q, want %q", tv.Errors[0].File, "/some/other/root/settings.json")
	}
}

// TestResolve_DetectReturnsParseFailedCategorized asserts a Detect
// error wrapping writepath.ErrParseFailed classifies as
// ErrorParseFailed.
func TestResolve_DetectReturnsParseFailedCategorized(t *testing.T) {
	t.Parallel()

	reg := adapter.NewRegistry()
	registerMock(reg, &mockAdapter{
		id:       adapter.ToolID("mock_a"),
		detectEr: fmt.Errorf("codex detect \"/home/x/.codex/config.toml\": %w", writepath.ErrParseFailed),
	})

	view, err := Resolve(context.Background(), testHomeResolver(t), reg, config.Profile{}, Filter{})
	if err != nil {
		t.Fatalf("Resolve err = %v, want nil", err)
	}
	tv := view.Tools[0]
	if len(tv.Errors) != 1 || tv.Errors[0].Kind != ErrorParseFailed {
		t.Fatalf("tv.Errors = %+v, want one ErrorParseFailed entry", tv.Errors)
	}
	if tv.Errors[0].File != "/home/x/.codex/config.toml" {
		t.Errorf("tv.Errors[0].File = %q, want %q", tv.Errors[0].File, "/home/x/.codex/config.toml")
	}
}

// TestResolve_DetectContextCanceledCategorized asserts a Detect error
// wrapping context.Canceled classifies as ErrorCanceled (and, per the
// updated ErrorCanceled contract, the outer walk does NOT abort — it
// only aborts when the parent ctx itself is canceled).
func TestResolve_DetectContextCanceledCategorized(t *testing.T) {
	t.Parallel()

	reg := adapter.NewRegistry()
	registerMock(reg, &mockAdapter{
		id:       adapter.ToolID("mock_a"),
		detectEr: fmt.Errorf("mid-detect: %w", context.Canceled),
	})

	view, err := Resolve(context.Background(), testHomeResolver(t), reg, config.Profile{}, Filter{})
	if err != nil {
		t.Fatalf("Resolve err = %v, want nil (per-tool Detect cancel is non-fatal)", err)
	}
	tv := view.Tools[0]
	if len(tv.Errors) != 1 || tv.Errors[0].Kind != ErrorCanceled {
		t.Fatalf("tv.Errors = %+v, want one ErrorCanceled entry", tv.Errors)
	}
}

// containsAll returns true when s contains every substring in subs.
// Local helper to keep the RegistryInconsistency assertion readable
// without pulling strings.Contains into every equality check.
func containsAll(s string, subs ...string) bool {
	for _, sub := range subs {
		if !contains(s, sub) {
			return false
		}
	}
	return true
}

func contains(s, sub string) bool {
	if sub == "" {
		return true
	}
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
