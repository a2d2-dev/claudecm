// Package envextract is the single env-reading source used by the
// claudecm resolver and every adapter Project implementation. It
// centralises os.LookupEnv access behind one seam so future stories
// have exactly one place to instrument env reads (audit, tracing,
// diagnostic rendering) rather than each adapter carrying its own.
//
// The Lookup primitive is build-tag swap-able via SetLookupForTest
// under `//go:build test`; production binaries compile with a plain
// os.LookupEnv delegate and no package-level mutable state (symmetric
// with storage.atomic_syncfunc). See lookup.go / lookup_testhook.go
// for the seam rationale.
//
// Historical note. The earlier ExtractCurrentEnv helper (used by
// cmd/add for the brownfield bootstrap) is preserved verbatim in
// extractor.go — the new Lookup/Snapshot/AllExtantMatching API is
// additive so callers can migrate one at a time without a
// package-wide flag day.
package envextract

import (
	"os"
	"strings"
)

// Lookup is the single env-read primitive used by every adapter's
// Project implementation. It has the same semantics as os.LookupEnv:
// returns (value, true) when the env var is set (even to ""), and
// ("", false) when it is unset.
//
// Adapters that treat empty string as absent (per their runtime-tool
// behaviour) can discard the second return value and check for `""`.
// Adapters that need to distinguish "set to empty" from "unset" can
// use the boolean.
//
// Production wiring: os.LookupEnv (see lookup.go).
// Test wiring: swap-able via SetLookupForTest (see lookup_testhook.go).
func Lookup(name string) (string, bool) { return lookupFunc(name) }

// Snapshot captures a stable map of env-var values for a fixed set of
// names. It is intended for callers that need to pass env values
// through a layered resolve chain without racing against process-env
// mutation between reads (e.g. a future resolver caching layer).
//
// Only names present in `names` are looked up; other process-env vars
// are ignored entirely. A name is included in the returned map iff
// Lookup reports it as set — the value may be the empty string when
// the env var was set to empty. Absent env vars are NOT included as
// empty-string entries; callers can distinguish "absent" from
// "present-but-empty" via map presence.
//
// Duplicate names in the input slice are looked up once each (the
// second write overwrites the first with the same value). A nil or
// empty `names` slice returns an empty non-nil map.
func Snapshot(names []string) map[string]string {
	out := make(map[string]string, len(names))
	for _, name := range names {
		if v, ok := lookupFunc(name); ok {
			out[name] = v
		}
	}
	return out
}

// AllExtantMatching returns every currently-set env var whose name
// starts with ANY of the given prefixes. It is intended for
// DIAGNOSTIC display (e.g. `explain --all-env` in a future story),
// NOT for production layer resolution — the resolver must only ever
// consider names in each adapter's frozen allowlist (PRD NFR-E1).
//
// Match semantics: a name matches a prefix iff strings.HasPrefix(name,
// prefix). An empty prefix matches every name; a nil or empty
// `prefixes` slice matches nothing (returned map is empty non-nil).
//
// The returned map is a fresh copy — callers may mutate freely.
// Values are returned verbatim (including empty strings when the env
// var is set to empty).
//
// Implementation note: AllExtantMatching walks os.Environ() directly
// rather than routing individual reads through the Lookup seam,
// because enumerating "everything set on the process env" has no
// per-name equivalent. Tests exercise it with t.Setenv so real
// process env drives the behaviour.
func AllExtantMatching(prefixes []string) map[string]string {
	out := map[string]string{}
	if len(prefixes) == 0 {
		return out
	}
	for _, entry := range os.Environ() {
		name, value, ok := strings.Cut(entry, "=")
		if !ok {
			// os.Environ entries are documented as "NAME=VALUE"; a
			// missing '=' would indicate a malformed environment
			// (never observed in practice). Skip defensively.
			continue
		}
		if !anyPrefix(name, prefixes) {
			continue
		}
		out[name] = value
	}
	return out
}

// anyPrefix reports whether name starts with any prefix in prefixes.
func anyPrefix(name string, prefixes []string) bool {
	for _, p := range prefixes {
		if strings.HasPrefix(name, p) {
			return true
		}
	}
	return false
}
