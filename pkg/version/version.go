// Package version exposes the build-time version metadata baked into a
// claudecm binary. The three vars below are the sole export surface; a
// GoReleaser / Makefile invocation pins them at link time via
// `-ldflags "-X github.com/a2d2-dev/claudecm/pkg/version.Version=..."`
// (see docs/plan/stories/E6-S9.md).
//
// Rationale for a dedicated package (vs. exporting these from cmd/):
//
//   - Any package that wants the version string (a future release script,
//     an audit-log entry, an HTTP User-Agent header) can import
//     pkg/version without pulling in cobra / cmd/ as a dependency.
//   - Ldflag rewrites need a stable, importable path. Nesting the
//     variables under cmd/ makes the -X path awkward and couples build
//     metadata to command-line plumbing.
//   - Tests that need to pin version bytes can do so via a t.Cleanup
//     rebind — the package holds NO other state, and the three vars are
//     the documented exception to coding-standards rule 12 (no
//     package-level mutable state): written only at init / test-swap
//     time, read thereafter. Symmetric with adapter.DefaultRegistry and
//     envextract.SetLookupForTest.
package version

// Version is the semantic version of this build. Defaults to "dev" so
// an unlabeled `go build` still produces something legible on
// `claudecm version` output. Overridden via -ldflags at release time.
var Version = "dev"

// Commit is the short git commit hash the binary was built from.
// Defaults to "none" so an unlabeled build is loud about not carrying
// commit info. Overridden via -ldflags at release time.
var Commit = "none"

// Date is the RFC 3339 build timestamp. Defaults to "unknown" so an
// unlabeled build is loud about missing metadata. Overridden via
// -ldflags at release time.
var Date = "unknown"
