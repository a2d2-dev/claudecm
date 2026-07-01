//go:build test

package envextract

import "os"

// lookupFunc under the `test` build tag is a swap-able var so tests can
// inject a synthetic env-var universe via SetLookupForTest. See
// lookup.go for the production wiring and rationale for the build-tag
// split (symmetric with storage.atomic_syncfunc_testhook.go).
var lookupFunc = os.LookupEnv

// SetLookupForTest replaces the lookupFunc used by Lookup / Snapshot /
// AllExtantMatching for the duration of a test. It returns a restore
// closure the test must defer to put the production function back.
// Only compiled with `-tags=test`.
//
// Substitution is process-global for the duration of the swap — parallel
// tests that share the seam must coordinate through this restore
// closure. Adapter unit tests do not run in parallel with each other on
// this seam.
func SetLookupForTest(fn func(string) (string, bool)) func() {
	prev := lookupFunc
	lookupFunc = fn
	return func() { lookupFunc = prev }
}
