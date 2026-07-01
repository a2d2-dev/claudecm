// whitebox_test.go targets small, uncovered branches in plan.go that
// the E2-S5 end-to-end matrix does not naturally reach:
//
//   - ValidatePlan's ".." component defense-in-depth after path.Clean.
//   - Flatten's control-character key refusal (ErrFlattenInvalidKey).
//   - Diff's "Added unowned key" TouchesUnowned branch.
//
// Kept in a separate file (not inside matrix_test.go) so the matrix
// stays a pure end-to-end sweep and these unit-shaped tests read as
// discrete pins on specific code paths. The tests are untagged so they
// contribute to the untagged coverage number gated by the E2-S5 NFR-T1
// bar.

package writepath

import (
	"errors"
	"testing"
)

// TestValidatePlan_RejectsControlCharInTarget pins the "control char
// in Target" branch. path.Clean resolves leading ".." for absolute
// POSIX paths, so the sibling ".." defense-in-depth loop in ValidatePlan
// is effectively unreachable from valid absolute inputs — we do not
// attempt to synthesize a coverage hit on an unreachable branch. Test
// the observable ValidatePlan guard that IS reachable and adjacent in
// the source: embedded control character in Target.
func TestValidatePlan_RejectsControlCharInTarget(t *testing.T) {
	plan := WritePlan{
		Tool:       "tool",
		Target:     "/tmp/foo\nbar",
		NewContent: []byte("x"),
	}
	if err := ValidatePlan(plan); !errors.Is(err, ErrPlanInvalid) {
		t.Fatalf("err = %v; want wraps ErrPlanInvalid (control char in Target)", err)
	}
}

// TestFlatten_RejectsControlCharKey pins the ErrFlattenInvalidKey
// branch. Flatten walks any nested map[string]any and refuses keys
// containing NUL, newline, CR, or tab — such keys cannot legally
// appear in a config file claudecm manages.
func TestFlatten_RejectsControlCharKey(t *testing.T) {
	// Top-level valid key, nested invalid key so we exercise the
	// flattenInto recursion path (line 373's `if err := flattenInto`)
	// rather than only the top-level key check.
	in := map[string]any{
		"outer": map[string]any{
			"has\nnewline": 1,
		},
	}
	_, err := Flatten(in)
	if !errors.Is(err, ErrFlattenInvalidKey) {
		t.Fatalf("err = %v; want wraps ErrFlattenInvalidKey", err)
	}
}

// TestFlatten_RejectsTopLevelControlCharKey pins the same rule at the
// top level so both `validateFlattenKey` call sites in flattenInto are
// exercised.
func TestFlatten_RejectsTopLevelControlCharKey(t *testing.T) {
	in := map[string]any{
		"has\x00nul": 1,
	}
	_, err := Flatten(in)
	if !errors.Is(err, ErrFlattenInvalidKey) {
		t.Fatalf("err = %v; want wraps ErrFlattenInvalidKey", err)
	}
}

// TestDiff_AddedUnownedFlagsTouchesUnowned pins the "Added key not in
// ownedKeys → TouchesUnowned=true" branch. The overwrite scenarios in
// apply_test.go and matrix_test.go hit the Changed and Removed
// branches; the Added-unowned branch (plan.go line 449-451) has been
// slipping through covered-by-side-effect only in some runs. This
// test pins it deterministically.
func TestDiff_AddedUnownedFlagsTouchesUnowned(t *testing.T) {
	cur := map[string]any{"a": 1}
	next := map[string]any{"a": 1, "b": 2}
	d := Diff(cur, next, []string{"a"}) // "b" is unowned
	if !d.TouchesUnowned {
		t.Fatalf("TouchesUnowned = false; want true (Added unowned key 'b')")
	}
	if len(d.Added) != 1 || d.Added[0] != "b" {
		t.Fatalf("Added = %v; want [b]", d.Added)
	}
}

// TestDiff_RemovedUnownedFlagsTouchesUnowned pins the Removed-unowned
// branch as a companion so both sides of the ownership check on Removed
// keys are exercised from a whitebox angle rather than only via Apply.
func TestDiff_RemovedUnownedFlagsTouchesUnowned(t *testing.T) {
	cur := map[string]any{"a": 1, "b": 2}
	next := map[string]any{"a": 1}
	d := Diff(cur, next, []string{"a"}) // "b" is unowned
	if !d.TouchesUnowned {
		t.Fatalf("TouchesUnowned = false; want true (Removed unowned key 'b')")
	}
	if len(d.Removed) != 1 || d.Removed[0] != "b" {
		t.Fatalf("Removed = %v; want [b]", d.Removed)
	}
}
