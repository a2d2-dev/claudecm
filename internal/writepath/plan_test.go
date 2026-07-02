package writepath

import (
	"errors"
	"reflect"
	"strings"
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
		Tool:       "codex",
		Target:     "/tmp/x",
		NewContent: []byte("{}"),
		OwnedKeys:  []string{"model", ""},
	})
	if !errors.Is(err, ErrPlanInvalid) {
		t.Fatalf("want ErrPlanInvalid, got %v", err)
	}
}

func TestValidatePlan_NoTransformNoNewContent(t *testing.T) {
	// A plan with neither Transform nor NewContent would silently
	// truncate the target on Apply. ValidatePlan must refuse. Names
	// both fields in the error message so the caller knows to set one.
	err := ValidatePlan(WritePlan{
		Tool:   "codex",
		Target: "/tmp/x",
	})
	if !errors.Is(err, ErrPlanInvalid) {
		t.Fatalf("want ErrPlanInvalid, got %v", err)
	}
	if !strings.Contains(err.Error(), "Transform") || !strings.Contains(err.Error(), "NewContent") {
		t.Fatalf("error message = %q; want it to name both Transform and NewContent", err.Error())
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
		Tool:       "codex",
		Target:     "/home/user/.codex/config.toml",
		NewContent: []byte("model = \"opus\"\n"),
		OwnedKeys:  []string{"model", "model_provider"},
	})
	if err != nil {
		t.Fatalf("want nil, got %v", err)
	}
}

func TestValidatePlan_MinimalValid(t *testing.T) {
	// Empty OwnedKeys is legal — some rare adapters may own nothing.
	// NewContent is required to escape the F3 no-content guard.
	err := ValidatePlan(WritePlan{
		Tool:       "claudecode",
		Target:     "/home/user/.claude/settings.json",
		NewContent: []byte("{}"),
	})
	if err != nil {
		t.Fatalf("want nil, got %v", err)
	}
}

func TestValidatePlan_Hardening(t *testing.T) {
	// Defense-in-depth: cheap checks that catch obviously-bad plans at
	// construction time. Symlink escape / HOME containment happens
	// inside Apply; these guards only need to reject the mistakes an
	// adapter can make when assembling a plan from user input.
	cases := []struct {
		name    string
		plan    WritePlan
		wantErr bool
	}{
		{
			// path.Clean collapses ".." on absolute paths (/home/user/../etc
			// -> /home/etc), so the ".."-component guard cannot fire for
			// any well-formed absolute input. It stays as defense-in-depth
			// against future refactors that might loosen the absolute-path
			// invariant. This case pins the accepted behavior.
			name:    "dotdot_collapsed_by_clean_is_accepted",
			plan:    WritePlan{Tool: "codex", Target: "/home/user/../etc/passwd", NewContent: []byte("{}")},
			wantErr: false,
		},
		{
			name:    "leading_dotdot_collapsed_at_root_is_accepted",
			plan:    WritePlan{Tool: "codex", Target: "/../etc/passwd", NewContent: []byte("{}")},
			wantErr: false,
		},
		{
			name:    "target_contains_nul",
			plan:    WritePlan{Tool: "codex", Target: "/tmp/foo\x00bar", NewContent: []byte("{}")},
			wantErr: true,
		},
		{
			name:    "target_contains_newline",
			plan:    WritePlan{Tool: "codex", Target: "/tmp/foo\nbar", NewContent: []byte("{}")},
			wantErr: true,
		},
		{
			name:    "target_contains_cr",
			plan:    WritePlan{Tool: "codex", Target: "/tmp/foo\rbar", NewContent: []byte("{}")},
			wantErr: true,
		},
		{
			name:    "target_contains_tab",
			plan:    WritePlan{Tool: "codex", Target: "/tmp/foo\tbar", NewContent: []byte("{}")},
			wantErr: true,
		},
		{
			name: "duplicate_owned_keys",
			plan: WritePlan{
				Tool:       "codex",
				Target:     "/tmp/x",
				NewContent: []byte("{}"),
				OwnedKeys:  []string{"model", "provider", "model"},
			},
			wantErr: true,
		},
		{
			name: "case_sensitive_duplicate_is_not_duplicate",
			plan: WritePlan{
				Tool:       "codex",
				Target:     "/tmp/x",
				NewContent: []byte("{}"),
				OwnedKeys:  []string{"Model", "model"},
			},
			wantErr: false,
		},
		{
			// F3: both nil means silent zero-byte truncation. Refuse.
			name:    "no_transform_and_no_newcontent",
			plan:    WritePlan{Tool: "codex", Target: "/tmp/x"},
			wantErr: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidatePlan(tc.plan)
			gotErr := err != nil
			if gotErr != tc.wantErr {
				t.Fatalf("err = %v; wantErr = %v", err, tc.wantErr)
			}
			if tc.wantErr && !errors.Is(err, ErrPlanInvalid) {
				t.Fatalf("err = %v; want wraps ErrPlanInvalid", err)
			}
		})
	}
}

func TestFlatten_HappyNested(t *testing.T) {
	in := map[string]any{
		"a": map[string]any{
			"b": map[string]any{
				"c": 42,
				"d": "leaf",
			},
		},
		"top": "value",
	}
	got, err := Flatten(in)
	if err != nil {
		t.Fatalf("Flatten err = %v", err)
	}
	want := map[string]any{
		"a.b.c": 42,
		"a.b.d": "leaf",
		"top":   "value",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Flatten = %+v; want %+v", got, want)
	}
}

func TestFlatten_EscapesDotAndBackslashInKeys(t *testing.T) {
	in := map[string]any{
		"a.b":   1, // literal dot in key -> escape to a\.b
		`a\b`:   2, // literal backslash -> escape to a\\b
		`a\.b`:  3, // literal backslash+dot -> a\\\.b
		"plain": 4,
		"nested": map[string]any{
			"c.d": 5, // literal dot at inner depth
		},
	}
	got, err := Flatten(in)
	if err != nil {
		t.Fatalf("Flatten err = %v", err)
	}
	want := map[string]any{
		`a\.b`:      1,
		`a\\b`:      2,
		`a\\\.b`:    3,
		`plain`:     4,
		`nested.c\.d`: 5,
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Flatten = %+v; want %+v", got, want)
	}
}

func TestFlatten_RejectsControlChars(t *testing.T) {
	cases := []struct {
		name string
		key  string
	}{
		{"newline", "bad\nkey"},
		{"nul", "bad\x00key"},
		{"tab", "bad\tkey"},
		{"cr", "bad\rkey"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Flatten(map[string]any{tc.key: 1})
			if !errors.Is(err, ErrFlattenInvalidKey) {
				t.Fatalf("err = %v; want wraps ErrFlattenInvalidKey", err)
			}
		})
	}
}

func TestFlatten_EmptyMapAtLeaf(t *testing.T) {
	// Empty map at any depth contributes nothing to the flat view.
	in := map[string]any{
		"present": 1,
		"gone":    map[string]any{},
		"nested": map[string]any{
			"also_gone": map[string]any{},
			"kept":      "yes",
		},
	}
	got, err := Flatten(in)
	if err != nil {
		t.Fatalf("Flatten err = %v", err)
	}
	want := map[string]any{
		"present":      1,
		"nested.kept":  "yes",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Flatten = %+v; want %+v", got, want)
	}
}

func TestFlatten_TopLevelEmptyMap(t *testing.T) {
	got, err := Flatten(map[string]any{})
	if err != nil {
		t.Fatalf("Flatten err = %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("Flatten top-level empty map = %+v; want empty", got)
	}
}

// TestFlatten_TopLevelNilReturnsEmptyMap pins the contract that a nil
// top-level input flattens to an empty map (not {"": nil}). This is
// the fix path for E2-FOLLOWUP-flatten-nil: on a fresh install where
// the parser returns (nil, nil) for a zero-byte config, writepath and
// commit must see an empty current-side flat map so the
// TouchesUnowned guard is decided against next's keys alone. Emitting
// {"": nil} would fabricate an "" key that is never in any adapter's
// OwnedKeys and would refuse every first-write with
// ErrDryRunUnownedTouched, blocking cmd/switch on fresh installs.
func TestFlatten_TopLevelNilReturnsEmptyMap(t *testing.T) {
	got, err := Flatten(nil)
	if err != nil {
		t.Fatalf("Flatten(nil) err = %v", err)
	}
	if got == nil {
		t.Fatalf("Flatten(nil) = nil map; want non-nil empty map")
	}
	if len(got) != 0 {
		t.Fatalf("Flatten(nil) = %+v (len=%d); want empty map", got, len(got))
	}
	if _, hasEmptyKey := got[""]; hasEmptyKey {
		t.Fatalf("Flatten(nil) unexpectedly emitted the empty-string key; want no key at all")
	}
}

// TestFlatten_NestedNilLeafStillEmitsKey pins the split between the
// top-level nil case (see TestFlatten_TopLevelNilReturnsEmptyMap) and
// nested nil leaves. A nil at a known key path is a real value the
// parser chose to emit and must remain visible to Diff — otherwise a
// caller that removes a key by nulling it out would be
// indistinguishable from a caller that never set it. Only the
// top-level nil short-circuits to an empty map.
func TestFlatten_NestedNilLeafStillEmitsKey(t *testing.T) {
	in := map[string]any{"a": nil, "b": 1}
	got, err := Flatten(in)
	if err != nil {
		t.Fatalf("Flatten err = %v", err)
	}
	want := map[string]any{"a": nil, "b": 1}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Flatten = %+v; want %+v", got, want)
	}
	if v, ok := got["a"]; !ok || v != nil {
		t.Fatalf("Flatten[a] = %v, ok=%v; want (nil, true)", v, ok)
	}
}

// TestFlatten_NonMapNonNilLeafEmitsEmptyKey pins the remaining edge:
// a non-nil, non-map top-level input is still returned as {"": v}.
// This shortcut is intentional (see Flatten godoc) and no adapter in
// v1 relies on it, but the test locks the shape so a future change
// that removes it must do so deliberately.
func TestFlatten_NonMapNonNilLeafEmitsEmptyKey(t *testing.T) {
	got, err := Flatten("scalar")
	if err != nil {
		t.Fatalf("Flatten err = %v", err)
	}
	want := map[string]any{"": "scalar"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Flatten(\"scalar\") = %+v; want %+v", got, want)
	}
}

// TestDiff_EmptyCurrentAgainstEmptyNext validates that the end-to-end
// TouchesUnowned guard is clean on a truly no-op fresh-install case:
// nil parsed current -> Flatten -> empty map; nil parsed new ->
// Flatten -> empty map; Diff -> zero DiffResult, TouchesUnowned=false.
// This is the writepath-level regression for cmd/switch on a fresh
// install where the codex TOML parser returns (nil, nil) both sides.
func TestDiff_EmptyCurrentAgainstEmptyNext(t *testing.T) {
	curFlat, err := Flatten(nil)
	if err != nil {
		t.Fatalf("Flatten(nil) current err = %v", err)
	}
	nextFlat, err := Flatten(nil)
	if err != nil {
		t.Fatalf("Flatten(nil) next err = %v", err)
	}
	got := Diff(curFlat, nextFlat, nil)
	if !reflect.DeepEqual(got, DiffResult{}) {
		t.Fatalf("Diff(empty, empty, nil) = %+v; want zero DiffResult", got)
	}
	if got.TouchesUnowned {
		t.Fatalf("TouchesUnowned = true; want false (no keys on either side)")
	}
}

// TestDiff_EmptyCurrentAgainstAddedKey_TouchesUnownedWhenNotOwned
// pins the "fresh install, adapter writes a new key not in
// OwnedKeys" edge: an empty flat current side plus a next side with
// one key that is NOT in OwnedKeys must report Added=["a"] and
// TouchesUnowned=true. This proves the guard fires the way it
// should — the fix must not silently disable it, only stop it
// firing on the phantom "" key.
func TestDiff_EmptyCurrentAgainstAddedKey_TouchesUnownedWhenNotOwned(t *testing.T) {
	curFlat, err := Flatten(nil)
	if err != nil {
		t.Fatalf("Flatten(nil) err = %v", err)
	}
	nextFlat := map[string]any{"a": 1}
	got := Diff(curFlat, nextFlat, nil)
	if !reflect.DeepEqual(got.Added, []string{"a"}) {
		t.Fatalf("Added = %v; want [a]", got.Added)
	}
	if !got.TouchesUnowned {
		t.Fatalf("TouchesUnowned = false; want true (a not in OwnedKeys)")
	}
}

// TestDiff_EmptyCurrentAgainstAddedKey_ClearWhenOwned mirrors the
// previous test but with "a" declared in OwnedKeys. This is the
// end-to-end shape cmd/switch relies on: an adapter writing an owned
// key against a fresh install must pass the guard cleanly.
func TestDiff_EmptyCurrentAgainstAddedKey_ClearWhenOwned(t *testing.T) {
	curFlat, err := Flatten(nil)
	if err != nil {
		t.Fatalf("Flatten(nil) err = %v", err)
	}
	nextFlat := map[string]any{"a": 1}
	got := Diff(curFlat, nextFlat, []string{"a"})
	if !reflect.DeepEqual(got.Added, []string{"a"}) {
		t.Fatalf("Added = %v; want [a]", got.Added)
	}
	if got.TouchesUnowned {
		t.Fatalf("TouchesUnowned = true; want false (a is owned)")
	}
}

func TestDiff_NestedInputFlattenedBeforeCall_Realistic(t *testing.T) {
	// Mimics real adapter usage: parse -> flatten -> diff.
	current := map[string]any{
		"env": map[string]any{
			"ANTHROPIC_API_KEY": "old",
			"PATH":              "/usr/bin",
		},
		"model": "opus-4",
	}
	next := map[string]any{
		"env": map[string]any{
			"ANTHROPIC_API_KEY": "new",
			"EXTRA":             "added",
		},
		"model": "opus-4",
	}
	curFlat, err := Flatten(current)
	if err != nil {
		t.Fatalf("Flatten current err = %v", err)
	}
	nextFlat, err := Flatten(next)
	if err != nil {
		t.Fatalf("Flatten next err = %v", err)
	}
	got := Diff(curFlat, nextFlat, []string{"env.ANTHROPIC_API_KEY", "model"})

	if !reflect.DeepEqual(got.Removed, []string{"env.PATH"}) {
		t.Fatalf("Removed = %v; want [env.PATH]", got.Removed)
	}
	if !reflect.DeepEqual(got.Added, []string{"env.EXTRA"}) {
		t.Fatalf("Added = %v; want [env.EXTRA]", got.Added)
	}
	if len(got.Changed) != 1 || got.Changed[0].Key != "env.ANTHROPIC_API_KEY" {
		t.Fatalf("Changed = %+v; want single env.ANTHROPIC_API_KEY delta", got.Changed)
	}
	// env.PATH removed and env.EXTRA added — neither is in OwnedKeys.
	if !got.TouchesUnowned {
		t.Fatalf("TouchesUnowned = false; want true (env.PATH/env.EXTRA not owned)")
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

func TestDiff_AllThreeSlicesNonEmpty(t *testing.T) {
	cur := map[string]any{"a": 1, "b": 2}
	next := map[string]any{"a": 2, "c": 3}
	got := Diff(cur, next, []string{"a", "b", "c"})

	if !reflect.DeepEqual(got.Added, []string{"c"}) {
		t.Fatalf("Added = %v; want [c]", got.Added)
	}
	if !reflect.DeepEqual(got.Removed, []string{"b"}) {
		t.Fatalf("Removed = %v; want [b]", got.Removed)
	}
	wantChanged := []KeyDelta{{Key: "a", OldValue: 1, NewValue: 2}}
	if !reflect.DeepEqual(got.Changed, wantChanged) {
		t.Fatalf("Changed = %+v; want %+v", got.Changed, wantChanged)
	}
	if got.TouchesUnowned {
		t.Fatalf("TouchesUnowned = true; want false (a,b,c all owned)")
	}
}

func TestDiff_NumericTypeAsymmetry(t *testing.T) {
	// KNOWN: adapters MUST normalize numeric types before Diff.
	// reflect.DeepEqual treats int64(1) and float64(1) as unequal
	// because their concrete types differ. Diff has no way to know
	// which numeric shape is "correct" for the target file's format,
	// so it faithfully reports a Changed delta. This pins that
	// behavior so a future change that starts collapsing numeric
	// types silently would be caught here.
	cur := map[string]any{"n": int64(1)}
	next := map[string]any{"n": float64(1)}
	got := Diff(cur, next, []string{"n"})

	if len(got.Changed) != 1 || got.Changed[0].Key != "n" {
		t.Fatalf("Changed = %+v; want single delta on key 'n'", got.Changed)
	}
	if _, ok := got.Changed[0].OldValue.(int64); !ok {
		t.Fatalf("OldValue = %T; want int64", got.Changed[0].OldValue)
	}
	if _, ok := got.Changed[0].NewValue.(float64); !ok {
		t.Fatalf("NewValue = %T; want float64", got.Changed[0].NewValue)
	}
	if got.TouchesUnowned {
		t.Fatalf("TouchesUnowned = true; want false (n is owned)")
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

func TestOwnedKeys_ExactMatchOnly_NoPrefixSemantics(t *testing.T) {
	// Ownership is EXACT string match against flattened keys. An
	// OwnedKeys entry of "env" does NOT own "env.foo" — those are two
	// distinct keys after Flatten. This guards against a future
	// refactor accidentally introducing prefix or glob semantics.
	cur := map[string]any{}
	next := map[string]any{"env.foo": "value"}
	got := Diff(cur, next, []string{"env"})
	if !reflect.DeepEqual(got.Added, []string{"env.foo"}) {
		t.Fatalf("Added = %v; want [env.foo]", got.Added)
	}
	if !got.TouchesUnowned {
		t.Fatalf("TouchesUnowned = false; want true ('env' does not own 'env.foo')")
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

func TestSentinels_AllDistinct(t *testing.T) {
	// Pin that new pipeline sentinels are distinct values (not aliases).
	// This test would catch a copy-paste bug where two sentinels share
	// the same errors.New instance.
	sentinels := []error{
		ErrPlanInvalid,
		ErrConcurrentEdit,
		ErrLockTimeout,
		ErrParseFailed,
		ErrOutsideHome,
		ErrBackupFailed,
		ErrPostWriteReparse,
		ErrRollback,
		ErrRollbackFailed,
		ErrDryRunUnownedTouched,
		ErrFlattenInvalidKey,
	}
	for i, a := range sentinels {
		for j, b := range sentinels {
			if i == j {
				continue
			}
			if errors.Is(a, b) {
				t.Fatalf("sentinel[%d] errors.Is sentinel[%d]; want distinct", i, j)
			}
		}
	}
}

// TestWriteReport_ShapeIncludesRolledBack pins the E2-S3 addition of
// the RolledBack field on WriteReport. Guards against accidental shape
// drift (rename/remove) since adapters and cmd/* will assert on this
// field once wiring lands. Zero-value is false — Skipped/DryRun/happy
// paths never flip it true.
func TestWriteReport_ShapeIncludesRolledBack(t *testing.T) {
	var zero WriteReport
	if zero.RolledBack {
		t.Fatalf("zero WriteReport.RolledBack = true; want false")
	}
	// The field must be assignable as a plain bool. This line is a
	// compile-time contract more than a runtime check.
	zero.RolledBack = true
	if !zero.RolledBack {
		t.Fatalf("WriteReport.RolledBack not settable")
	}
}
