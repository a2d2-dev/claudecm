//go:build test

package codex

import "os"

// lookupEnv under the `test` build tag is a swap-able var so tests can
// inject a synthetic env-var universe via SetLookupEnvForTest. See
// project_env.go for the production wiring and rationale for the
// build-tag split (symmetric with claudecode/project_env_testhook.go).
var lookupEnv = func(name string) string { return os.Getenv(name) }

// SetLookupEnvForTest replaces the lookupEnv function used by Project
// for the duration of a test. It returns a restore closure the test
// must defer to put the production function back. Only compiled with
// `-tags=test`.
func SetLookupEnvForTest(fn func(string) string) func() {
	prev := lookupEnv
	lookupEnv = fn
	return func() { lookupEnv = prev }
}
