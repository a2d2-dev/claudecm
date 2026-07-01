//go:build test

package envextract_test

// lookup_testhook_test.go — E5-S3. Exercises the SetLookupForTest
// build-tag seam. Compiled only under `-tags=test`; symmetric with
// storage.atomic_syncfunc_test.go's seam coverage.

import (
	"testing"

	"github.com/a2d2-dev/claudecm/internal/envextract"
)

// TestSetLookupForTest_SubstitutesAndRestores verifies the seam
// substitutes the lookup primitive for the duration of the swap and
// the returned closure reverts on restore. Without this test the
// build-tag file would go uncovered.
func TestSetLookupForTest_SubstitutesAndRestores(t *testing.T) {
	// Baseline: an unset var reports absent under the real primitive.
	baselineName := "CLAUDECM_TEST_SEAM_XYZZY_9F3A"
	if _, ok := envextract.Lookup(baselineName); ok {
		t.Fatalf("baseline: %s unexpectedly set on real env", baselineName)
	}

	// Substitute a fake universe that resolves the baseline name to
	// a known value, and confirm Lookup / Snapshot both route through
	// the fake.
	called := 0
	restore := envextract.SetLookupForTest(func(name string) (string, bool) {
		called++
		if name == baselineName {
			return "seamed", true
		}
		return "", false
	})

	got, ok := envextract.Lookup(baselineName)
	if !ok || got != "seamed" {
		t.Fatalf("under seam: Lookup=(%q,%v), want (\"seamed\", true)", got, ok)
	}
	if _, ok := envextract.Lookup("SOMETHING_ELSE"); ok {
		t.Fatalf("under seam: fake resolved SOMETHING_ELSE; want absent")
	}
	// Snapshot also routes through the seam.
	snap := envextract.Snapshot([]string{baselineName, "SOMETHING_ELSE"})
	if snap[baselineName] != "seamed" {
		t.Fatalf("under seam: Snapshot missing seamed entry; got=%v", snap)
	}
	if _, present := snap["SOMETHING_ELSE"]; present {
		t.Fatalf("under seam: Snapshot wrongly included absent entry")
	}
	if called < 3 {
		t.Fatalf("under seam: fake lookup call count=%d, want >=3", called)
	}

	// Restore and confirm the baseline behaviour returns.
	restore()
	if _, ok := envextract.Lookup(baselineName); ok {
		t.Fatalf("after restore: %s unexpectedly set", baselineName)
	}
}
