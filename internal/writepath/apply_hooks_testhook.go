//go:build test

package writepath

// postReadHookForTest under the `test` build tag is a swap-able var so
// drift-detection tests can inject a mutation between our step-3 read
// (which captured PreFingerprint) and the step-9 drift-check Stat. See
// apply_hooks.go for the production wiring and the rationale for the
// build-tag split.
var postReadHookForTest = func() {}

// SetPostReadHookForTest replaces the hook Apply invokes immediately
// before the drift-check Stat. It returns a restore closure the test
// must defer to put the no-op back so subsequent tests are not
// contaminated. Only compiled with `-tags=test`, symmetric with
// storage.SetSyncFuncForTest — production builds contain neither the
// var nor this exported setter.
func SetPostReadHookForTest(fn func()) func() {
	prev := postReadHookForTest
	postReadHookForTest = fn
	return func() { postReadHookForTest = prev }
}
