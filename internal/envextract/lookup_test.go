package envextract_test

// lookup_test.go — E5-S3. Unit tests for the Lookup primitive. These
// tests do NOT depend on the build-tag seam: they exercise the real
// os.LookupEnv path via t.Setenv, so they run under both
// `go test ./...` and `go test -tags=test ./...`. Seam-specific tests
// live in lookup_testhook_test.go under `//go:build test`.

import (
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
