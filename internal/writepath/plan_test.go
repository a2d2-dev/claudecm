package writepath

import (
	"context"
	"errors"
	"reflect"
	"testing"
)

func TestValidatePlan_EmptyTool(t *testing.T) {
	err := ValidatePlan(WritePlan{Target: "/tmp/x"})
	if !errors.Is(err, ErrPlanInvalid) {
		t.Fatalf("want ErrPlanInvalid, got %v", err)
	}
}

func TestValidatePlan_EmptyTarget(t *testing.T) {
	err := ValidatePlan(WritePlan{Tool: "codex"})
	if !errors.Is(err, ErrPlanInvalid) {
		t.Fatalf("want ErrPlanInvalid, got %v", err)
	}
}

func TestValidatePlan_NonAbsoluteTarget(t *testing.T) {
	err := ValidatePlan(WritePlan{Tool: "codex", Target: "relative/path"})
	if !errors.Is(err, ErrPlanInvalid) {
		t.Fatalf("want ErrPlanInvalid, got %v", err)
	}
}

func TestValidatePlan_OwnedKeysEmptyString(t *testing.T) {
	err := ValidatePlan(WritePlan{
		Tool:      "codex",
		Target:    "/tmp/x",
		OwnedKeys: []string{"model", ""},
	})
	if !errors.Is(err, ErrPlanInvalid) {
		t.Fatalf("want ErrPlanInvalid, got %v", err)
	}
}

func TestValidatePlan_TransformAndNewContentBothSetIsAccepted(t *testing.T) {
	// Documented design: Transform wins at Apply time. Validation does
	// not treat this combination as an error — see plan.go package doc.
	err := ValidatePlan(WritePlan{
		Tool:       "codex",
		Target:     "/tmp/x",
		NewContent: []byte("{}"),
		Transform:  func(b []byte) ([]byte, error) { return b, nil },
	})
	if err != nil {
		t.Fatalf("want nil, got %v", err)
	}
}

func TestValidatePlan_Valid(t *testing.T) {
	err := ValidatePlan(WritePlan{
		Tool:      "codex",
		Target:    "/home/user/.codex/config.toml",
		OwnedKeys: []string{"model", "model_provider"},
	})
	if err != nil {
		t.Fatalf("want nil, got %v", err)
	}
}

func TestValidatePlan_MinimalValid(t *testing.T) {
	// Empty OwnedKeys is legal — some rare adapters may own nothing.
	err := ValidatePlan(WritePlan{
		Tool:   "claudecode",
		Target: "/home/user/.claude/settings.json",
	})
	if err != nil {
		t.Fatalf("want nil, got %v", err)
	}
}

func TestDiff_MapAddedRemovedChanged(t *testing.T) {
	cur := map[string]any{"a": 1, "b": 2}
	next := map[string]any{"a": 1, "c": 3}
	got := Diff(cur, next, []string{"a"})

	if !reflect.DeepEqual(got.Removed, []string{"b"}) {
		t.Fatalf("Removed = %v; want [b]", got.Removed)
	}
	if !reflect.DeepEqual(got.Added, []string{"c"}) {
		t.Fatalf("Added = %v; want [c]", got.Added)
	}
	if len(got.Changed) != 0 {
		t.Fatalf("Changed = %v; want empty", got.Changed)
	}
	// b was removed and c was added; neither is in OwnedKeys=["a"].
	if !got.TouchesUnowned {
		t.Fatalf("TouchesUnowned = false; want true (b removed, c added, neither owned)")
	}
}

func TestDiff_MapChangedOnly(t *testing.T) {
	cur := map[string]any{"a": 1}
	next := map[string]any{"a": 2}
	got := Diff(cur, next, []string{"a"})

	if len(got.Added) != 0 || len(got.Removed) != 0 {
		t.Fatalf("Added=%v Removed=%v; want both empty", got.Added, got.Removed)
	}
	want := []KeyDelta{{Key: "a", OldValue: 1, NewValue: 2}}
	if !reflect.DeepEqual(got.Changed, want) {
		t.Fatalf("Changed = %+v; want %+v", got.Changed, want)
	}
	if got.TouchesUnowned {
		t.Fatalf("TouchesUnowned = true; want false (a is owned)")
	}
}

func TestDiff_ScalarFallback(t *testing.T) {
	got := Diff("x", "y", nil)
	want := []KeyDelta{{Key: "", OldValue: "x", NewValue: "y"}}
	if !reflect.DeepEqual(got.Changed, want) {
		t.Fatalf("Changed = %+v; want %+v", got.Changed, want)
	}
	if !got.TouchesUnowned {
		t.Fatalf("TouchesUnowned = false; want true (empty key not in nil OwnedKeys)")
	}
}

func TestDiff_Identical(t *testing.T) {
	cur := map[string]any{"a": 1, "b": []any{"x", "y"}}
	next := map[string]any{"a": 1, "b": []any{"x", "y"}}
	got := Diff(cur, next, []string{"a", "b"})
	if !reflect.DeepEqual(got, DiffResult{}) {
		t.Fatalf("Diff of identical values = %+v; want zero DiffResult", got)
	}
}

func TestDiff_IdenticalScalars(t *testing.T) {
	got := Diff("x", "x", nil)
	if !reflect.DeepEqual(got, DiffResult{}) {
		t.Fatalf("Diff of identical scalars = %+v; want zero DiffResult", got)
	}
}

func TestDiff_UnownedTouched(t *testing.T) {
	cur := map[string]any{"a": 1}
	next := map[string]any{"a": 1, "b": 2}
	got := Diff(cur, next, []string{"a"})
	if !reflect.DeepEqual(got.Added, []string{"b"}) {
		t.Fatalf("Added = %v; want [b]", got.Added)
	}
	if !got.TouchesUnowned {
		t.Fatalf("TouchesUnowned = false; want true (b not in OwnedKeys)")
	}
}

func TestDiff_OwnedAddedDoesNotTouchUnowned(t *testing.T) {
	cur := map[string]any{}
	next := map[string]any{"a": 1}
	got := Diff(cur, next, []string{"a"})
	if got.TouchesUnowned {
		t.Fatalf("TouchesUnowned = true; want false (a is owned)")
	}
}

func TestDiff_DeterministicOrdering(t *testing.T) {
	// Iterate multiple times because Go map iteration order is
	// randomized; we want Added / Removed / Changed to come back in
	// sorted order every time to make the FR-4 pre-apply diff stable.
	cur := map[string]any{"z": 1, "a": 1, "m": 1}
	next := map[string]any{"z": 2, "a": 2, "m": 2}
	for i := 0; i < 32; i++ {
		got := Diff(cur, next, []string{"a", "m", "z"})
		wantKeys := []string{"a", "m", "z"}
		gotKeys := make([]string, 0, len(got.Changed))
		for _, d := range got.Changed {
			gotKeys = append(gotKeys, d.Key)
		}
		if !reflect.DeepEqual(gotKeys, wantKeys) {
			t.Fatalf("iter %d: Changed keys = %v; want %v", i, gotKeys, wantKeys)
		}
	}
}

func TestApply_StubReturnsNotImplemented(t *testing.T) {
	// E2-S1 ships only the signature. The stub must return a typed
	// sentinel so callers can compile against Apply without accidentally
	// treating a nil error as success. E2-S2 replaces this test.
	report, err := Apply(context.Background(), nil, WritePlan{
		Tool:   "codex",
		Target: "/tmp/does-not-matter",
	})
	if !errors.Is(err, ErrNotImplemented) {
		t.Fatalf("Apply err = %v; want wraps ErrNotImplemented", err)
	}
	if !reflect.DeepEqual(report, WriteReport{}) {
		t.Fatalf("Apply report = %+v; want zero WriteReport", report)
	}
}

func TestParserFunc_ImplementsParser(t *testing.T) {
	// Guard against accidental interface drift.
	var p Parser = ParserFunc(func(data []byte) (any, error) {
		return string(data), nil
	})
	got, err := p.Parse([]byte("hello"))
	if err != nil {
		t.Fatalf("Parse err = %v", err)
	}
	if got != "hello" {
		t.Fatalf("Parse = %v; want hello", got)
	}
}
