//go:build !test

package envextract

import "os"

// lookupFunc is the production wiring for the single "read a process env
// var" primitive used by both adapter Project implementations. Under the
// default (non-test) build it is a plain function that delegates
// directly to os.LookupEnv — no package-level mutable state, no
// indirection cost beyond the call boundary.
//
// A companion file under `//go:build test` (lookup_testhook.go)
// redeclares lookupFunc as a var and exports SetLookupForTest so unit
// tests can force a deterministic env-var universe without touching the
// process environment. That seam is compiled only when tests are built
// with `-tags=test` so production binaries have no swap point
// (coding-standards rule 12 on package-level mutable state at runtime),
// symmetric with storage.atomic_syncfunc.
func lookupFunc(name string) (string, bool) { return os.LookupEnv(name) }
