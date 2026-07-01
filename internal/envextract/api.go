// Package envextract is the single env-reading source used by the
// claudecm resolver pipeline and every adapter Project implementation.
// It centralises os.LookupEnv access behind one seam so future stories
// have exactly one place to instrument env reads (audit, tracing,
// diagnostic rendering) rather than each adapter carrying its own.
//
// The Lookup primitive is build-tag swap-able via SetLookupForTest
// under `//go:build test`; production binaries compile with a plain
// os.LookupEnv delegate and no package-level mutable state (symmetric
// with storage.atomic_syncfunc). See lookup.go / lookup_testhook.go
// for the seam rationale.
//
// Scope note. Legacy ExtractCurrentEnv (used by cmd/add for the
// brownfield bootstrap) reads process env directly and is preserved
// verbatim in extractor.go for backwards-compat; a future story may
// migrate it to Lookup once the resolver-side migration has settled.
// Diagnostic bulk enumeration (a future `explain --all-env`) will be
// added alongside the story that needs it — the current API is
// intentionally the minimum surface the resolver + adapters use today.
package envextract

// Lookup is the single env-read primitive used by every adapter's
// Project implementation and by the resolver pipeline. It has the
// same semantics as os.LookupEnv: returns (value, true) when the env
// var is set (even to ""), and ("", false) when it is unset.
//
// Adapters that treat empty string as absent (per their runtime-tool
// behaviour) can discard the second return value and check for `""`.
// Adapters that need to distinguish "set to empty" from "unset" can
// use the boolean.
//
// Production wiring: os.LookupEnv (see lookup.go).
// Test wiring: swap-able via SetLookupForTest (see lookup_testhook.go).
func Lookup(name string) (string, bool) { return lookupFunc(name) }
