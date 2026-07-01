//go:build !test

package codex

import "os"

// lookupEnv is the production wiring for the "read a process env var"
// step of Project. Under the default (non-test) build it is a plain
// function that delegates directly to os.Getenv — no package-level
// mutable state, no indirection cost beyond the call boundary.
//
// A companion file under `//go:build test` redeclares lookupEnv as a
// var and exports SetLookupEnvForTest so unit tests can force a
// deterministic env-var universe without touching the process
// environment. That seam is compiled only when tests are built with
// `-tags=test` so production binaries have no swap point
// (coding-standards rule 12 on package-level mutable state at
// runtime), symmetric with claudecode/project_env.go and
// storage.atomic_syncfunc.
func lookupEnv(name string) string { return os.Getenv(name) }
