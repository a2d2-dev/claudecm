package envextract_test

// lookup_test.go — E5-S3. Unit tests for the Lookup / Snapshot /
// AllExtantMatching primitives. These tests do NOT depend on the
// build-tag seam: they exercise the real os.LookupEnv / os.Environ
// path via t.Setenv, so they run under both `go test ./...` and
// `go test -tags=test ./...`. Seam-specific tests live in
// lookup_testhook_test.go under `//go:build test`.

import (
	"reflect"
	"sort"
	"testing"

	"github.com/a2d2-dev/claudecm/internal/envextract"
)

func TestLookup_HappyRealEnv(t *testing.T) {
	// Setting a real env var via t.Setenv routes through os.LookupEnv,
	// which is both the production lookupFunc and the test-tag default.
	// The test therefore validates the same primitive on both builds.
	t.Setenv("CLAUDECM_TEST_VAR_HAPPY", "hi")
	got, ok := envextract.Lookup("CLAUDECM_TEST_VAR_HAPPY")
	if !ok {
		t.Fatalf("Lookup ok=false, want true")
	}
	if got != "hi" {
		t.Fatalf("Lookup value=%q, want %q", got, "hi")
	}
}

func TestLookup_MissingReturnsFalse(t *testing.T) {
	// Belt-and-suspenders: ensure the var is not set. t.Setenv +
	// deferred unset via test framework guarantees a clean baseline
	// across parallel goroutines, but we assert explicitly so a stray
	// external environment does not silently pass the test.
	got, ok := envextract.Lookup("CLAUDECM_TEST_VAR_DEFINITELY_NOT_SET_XYZZY_9F3A")
	if ok {
		t.Fatalf("Lookup ok=true, want false; got value=%q", got)
	}
	if got != "" {
		t.Fatalf("Lookup value=%q, want empty when absent", got)
	}
}

func TestLookup_EmptyStringIsSet(t *testing.T) {
	// A var set to "" is present (ok=true) — matches os.LookupEnv
	// semantics and lets adapters distinguish "empty" from "unset"
	// via the bool when they need to.
	t.Setenv("CLAUDECM_TEST_VAR_EMPTY", "")
	got, ok := envextract.Lookup("CLAUDECM_TEST_VAR_EMPTY")
	if !ok {
		t.Fatalf("Lookup ok=false, want true for empty-set var")
	}
	if got != "" {
		t.Fatalf("Lookup value=%q, want empty string", got)
	}
}

func TestSnapshot_HappyPartial(t *testing.T) {
	// Set two of three names; expect Snapshot to include exactly the
	// two set names and omit the absent one from the map.
	t.Setenv("CLAUDECM_TEST_SNAP_A", "aa")
	t.Setenv("CLAUDECM_TEST_SNAP_B", "bb")
	// Third name intentionally left unset via a unique suffix.
	names := []string{
		"CLAUDECM_TEST_SNAP_A",
		"CLAUDECM_TEST_SNAP_B",
		"CLAUDECM_TEST_SNAP_MISSING_XYZZY_QQQ",
	}
	got := envextract.Snapshot(names)
	want := map[string]string{
		"CLAUDECM_TEST_SNAP_A": "aa",
		"CLAUDECM_TEST_SNAP_B": "bb",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Snapshot=%v, want %v", got, want)
	}
	if _, present := got["CLAUDECM_TEST_SNAP_MISSING_XYZZY_QQQ"]; present {
		t.Fatalf("Snapshot should not include absent var as empty entry")
	}
}

func TestSnapshot_EmptyNames(t *testing.T) {
	// Contract: nil / empty input returns an empty non-nil map so
	// callers can safely do `len(m) == 0` and range over it without
	// nil-checking. Documented in Snapshot godoc.
	nilOut := envextract.Snapshot(nil)
	if nilOut == nil {
		t.Fatalf("Snapshot(nil) returned nil map; want empty non-nil")
	}
	if len(nilOut) != 0 {
		t.Fatalf("Snapshot(nil) len=%d, want 0", len(nilOut))
	}
	emptyOut := envextract.Snapshot([]string{})
	if emptyOut == nil {
		t.Fatalf("Snapshot([]) returned nil map; want empty non-nil")
	}
	if len(emptyOut) != 0 {
		t.Fatalf("Snapshot([]) len=%d, want 0", len(emptyOut))
	}
}

func TestSnapshot_PreservesEmptySetValue(t *testing.T) {
	// A var set to "" IS included (matches Lookup contract).
	t.Setenv("CLAUDECM_TEST_SNAP_EMPTY", "")
	got := envextract.Snapshot([]string{"CLAUDECM_TEST_SNAP_EMPTY"})
	v, present := got["CLAUDECM_TEST_SNAP_EMPTY"]
	if !present {
		t.Fatalf("Snapshot omitted empty-set var; want present")
	}
	if v != "" {
		t.Fatalf("Snapshot value=%q, want empty", v)
	}
}

func TestAllExtantMatching_ByPrefix(t *testing.T) {
	// Set three vars: two under FOO_, one under BAR_. Confirm the
	// prefix filter picks exactly the two FOO_ entries.
	t.Setenv("CLAUDECM_FOO_A", "aa")
	t.Setenv("CLAUDECM_FOO_B", "bb")
	t.Setenv("CLAUDECM_BAR_X", "xx")
	got := envextract.AllExtantMatching([]string{"CLAUDECM_FOO_"})
	// Filter to just the deterministic keys we set — real process env
	// may carry other CLAUDECM_FOO_ prefixed vars from a wrapper.
	// Assert the two we set are present with correct values.
	if got["CLAUDECM_FOO_A"] != "aa" {
		t.Fatalf("AllExtantMatching missing CLAUDECM_FOO_A; got=%v", got)
	}
	if got["CLAUDECM_FOO_B"] != "bb" {
		t.Fatalf("AllExtantMatching missing CLAUDECM_FOO_B; got=%v", got)
	}
	// The BAR entry must NOT surface.
	if _, present := got["CLAUDECM_BAR_X"]; present {
		t.Fatalf("AllExtantMatching wrongly included CLAUDECM_BAR_X; got=%v", got)
	}
}

func TestAllExtantMatching_MultiplePrefixes(t *testing.T) {
	// A name matches iff ANY prefix matches. Set one var per prefix
	// and confirm both surface.
	t.Setenv("CLAUDECM_ONE_A", "1")
	t.Setenv("CLAUDECM_TWO_B", "2")
	got := envextract.AllExtantMatching([]string{"CLAUDECM_ONE_", "CLAUDECM_TWO_"})
	if got["CLAUDECM_ONE_A"] != "1" {
		t.Fatalf("missing CLAUDECM_ONE_A; got=%v", got)
	}
	if got["CLAUDECM_TWO_B"] != "2" {
		t.Fatalf("missing CLAUDECM_TWO_B; got=%v", got)
	}
}

func TestAllExtantMatching_EmptyPrefixesReturnsEmpty(t *testing.T) {
	// nil and [] both return an empty non-nil map — nothing matches
	// when the prefix list is empty.
	for _, in := range [][]string{nil, {}} {
		got := envextract.AllExtantMatching(in)
		if got == nil {
			t.Fatalf("AllExtantMatching returned nil map for prefixes=%v; want empty non-nil", in)
		}
		if len(got) != 0 {
			// Filter to CLAUDECM_ so a noisy test environment does
			// not spam the failure message; but any hit is a
			// contract violation.
			keys := make([]string, 0, len(got))
			for k := range got {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			t.Fatalf("AllExtantMatching(empty) returned %d entries: %v", len(got), keys)
		}
	}
}

func TestAllExtantMatching_EmptyPrefixMatchesAll(t *testing.T) {
	// An empty-string prefix matches every name (documented behaviour).
	// We assert only that a var we deliberately set surfaces — the
	// full map may include arbitrary process env entries.
	t.Setenv("CLAUDECM_TEST_ALL_MATCH_MARK", "hit")
	got := envextract.AllExtantMatching([]string{""})
	if got["CLAUDECM_TEST_ALL_MATCH_MARK"] != "hit" {
		t.Fatalf("empty prefix should have matched our marker; got=%q", got["CLAUDECM_TEST_ALL_MATCH_MARK"])
	}
}
