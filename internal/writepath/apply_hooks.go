//go:build !test

package writepath

// postReadHookForTest is the production wiring for the drift-detection
// test seam. Under the default (non-test) build, it is a plain no-op
// function — no package-level mutable state, no indirection cost beyond
// the call boundary, no way for a runtime caller to swap in behavior.
//
// A companion file under `//go:build test` redeclares postReadHookForTest
// as a swap-able var and exports SetPostReadHookForTest so drift
// detection unit tests can mutate the target file between our step-3
// read and the step-9 drift-check Stat. That seam is compiled only when
// tests are built with `-tags=test` so production binaries have no swap
// point (coding-standards rule 12: no runtime package-level mutable
// state in production build).
func postReadHookForTest() {}
