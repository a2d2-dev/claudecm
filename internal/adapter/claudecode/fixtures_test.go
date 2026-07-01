package claudecode_test

// fixtures_test.go — E3-S7. Testdata-driven end-to-end matrix that
// walks testdata/claudecode/{happy,edge}/<name>/ and exercises the
// Claude Code adapter's Import → Plan → Apply → Project pipeline
// against goldens under each case's expected/ directory.
//
// Purpose. This is the last E3 story: pin every documented adapter
// behaviour (empty-file policy, unknown-key preservation, unicode
// round-trip, typed-primitive projection, symlink-in-HOME) as a
// checked-in fixture so a future refactor that drifts on any of them
// fails on `go test ./internal/adapter/claudecode/...` — not in
// production the day cmd/switch first calls Apply.
//
// Fixture layout. Each case directory carries:
//
//	settings.json           — the tool config the fixture drives Import/Apply from
//	profile.yaml (optional) — Profile spec fed to Plan+Apply and Project
//	expected/
//	  import.json           — canonical (Core, Overlay) after Import
//	  after_apply.json      — on-disk settings.json bytes after Apply
//	  project.json          — canonical EffectiveView after Project
//	  diff.json             — WriteReport.Diff after Apply
//
// A missing expected/*.json for a stage means "skip the golden compare
// for that stage" (still runs the stage — the run itself must not error
// unexpectedly). A missing profile.yaml skips the Plan+Apply half of
// the pipeline entirely (the case runs Import + Project only, using an
// empty Profile for Project).
//
// Golden regeneration. Run with `-update-fixtures` or set
// CLAUDECM_UPDATE_FIXTURES=1 to (re)write every expected/*.json file
// with the current pipeline output. The regenerated goldens MUST be
// hand-inspected before commit — this is a review checkpoint, not an
// oracle.
//
// Symlink-in-HOME. The edge/symlink-in-home case does not copy its
// settings.json into ~/.claude directly. The test setup writes the
// fixture bytes to ~/.claude/settings-actual.json and symlinks
// ~/.claude/settings.json → settings-actual.json so Import and
// Project exercise the read-side symlink-follow behaviour
// (import.go's verifyReadTargetInHome). Apply is deliberately not
// exercised for this case: storage.Stat refuses non-regular files
// at plan.Target, so a symlinked settings.json is a documented
// dead-end for the write-path. Omitting profile.yaml here keeps the
// matrix honest about which stages a fixture actually exercises;
// forcing an Apply we know will error would just embed the error
// message in a golden.
//
// Empty / whitespace-only. Both cases exercise Import + Project
// through treatAsEmpty's "well-defined empty" policy. profile.yaml
// is deliberately omitted so Plan+Apply is skipped: the empty-file
// → Apply path currently triggers writepath's TouchesUnowned guard
// (Flatten(nil) surfaces the "" key on the current side, which is
// not in OwnedKeys). Whether that guard should special-case a
// zero-byte current is a separate architectural question — a
// future story that answers it can drop a profile.yaml into these
// case dirs and regenerate goldens without touching this file.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/a2d2-dev/claudecm/internal/adapter"
	"github.com/a2d2-dev/claudecm/internal/adapter/claudecode"
	"github.com/a2d2-dev/claudecm/internal/config"
	"github.com/a2d2-dev/claudecm/internal/storage"
)

// fixturesRoot is the on-disk location of the checked-in matrix. Kept
// relative to the test binary's working directory (Go's `go test` runs
// in the package directory) so no absolute-path resolution is needed.
const fixturesRoot = "testdata/claudecode"

// updateFixturesFlag mirrors the `-update-fixtures` CLI flag. Named
// with the fixtures suffix to avoid colliding with any future
// -update-* flag in other test files.
var updateFixturesFlag = flag.Bool(
	"update-fixtures",
	false,
	"regenerate testdata/claudecode/**/expected/*.json goldens (also enabled by CLAUDECM_UPDATE_FIXTURES=1)",
)

// shouldUpdate returns true when goldens should be (re)written.
// Env-var opt-in exists so IDE test runners that do not thread flags
// through can still trigger regeneration.
func shouldUpdate() bool {
	if updateFixturesFlag != nil && *updateFixturesFlag {
		return true
	}
	return os.Getenv("CLAUDECM_UPDATE_FIXTURES") == "1"
}

// importCanonical is the JSON shape written to expected/import.json.
// Keeps Core and Overlay in a single object so goldens are one file
// per stage rather than two.
type importCanonical struct {
	Core    adapter.CoreFromTool    `json:"core"`
	Overlay adapter.OverlayFromTool `json:"overlay"`
}

// TestFixtures is the matrix entry point. It walks fixturesRoot,
// discovers every <class>/<name>/ directory that contains a
// settings.json, and runs one t.Run per case.
func TestFixtures(t *testing.T) {
	cases, err := discoverCases(fixturesRoot)
	if err != nil {
		t.Fatalf("discover fixtures under %q: %v", fixturesRoot, err)
	}
	if len(cases) == 0 {
		t.Fatalf("no fixture cases found under %q — did you forget to add testdata?", fixturesRoot)
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.Class+"/"+tc.Name, func(t *testing.T) {
			runFixtureCase(t, tc)
		})
	}
}

// fixtureCase captures the on-disk layout of one matrix row.
type fixtureCase struct {
	Class    string // "happy" or "edge"
	Name     string // subdirectory name
	Dir      string // fixturesRoot/<Class>/<Name>
	Expected string // fixturesRoot/<Class>/<Name>/expected

	SettingsPath string // path to the input settings.json
	ProfilePath  string // path to profile.yaml, or "" when absent
	Symlink      bool   // true → setup wires settings.json as a symlink
}

// discoverCases walks classes then names to build the case slice.
// Deterministic order: sort.Strings on class and name so `go test -v`
// output is stable across runs.
func discoverCases(root string) ([]fixtureCase, error) {
	classes, err := os.ReadDir(root)
	if err != nil {
		return nil, err
	}
	classNames := make([]string, 0, len(classes))
	for _, e := range classes {
		if !e.IsDir() {
			continue
		}
		classNames = append(classNames, e.Name())
	}
	sort.Strings(classNames)

	var out []fixtureCase
	for _, class := range classNames {
		classDir := filepath.Join(root, class)
		entries, err := os.ReadDir(classDir)
		if err != nil {
			return nil, err
		}
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			if !e.IsDir() {
				continue
			}
			names = append(names, e.Name())
		}
		sort.Strings(names)

		for _, name := range names {
			dir := filepath.Join(classDir, name)
			settings := filepath.Join(dir, "settings.json")
			if _, err := os.Stat(settings); err != nil {
				// A directory without settings.json is not a case —
				// skip silently so hidden dirs (VCS metadata, editors)
				// do not fail discovery.
				continue
			}
			c := fixtureCase{
				Class:        class,
				Name:         name,
				Dir:          dir,
				Expected:     filepath.Join(dir, "expected"),
				SettingsPath: settings,
				Symlink:      name == "symlink-in-home",
			}
			profile := filepath.Join(dir, "profile.yaml")
			if _, err := os.Stat(profile); err == nil {
				c.ProfilePath = profile
			}
			out = append(out, c)
		}
	}
	return out, nil
}

// runFixtureCase seeds a per-test HOME, replays the pipeline, and
// compares each stage's output against its golden (or regenerates the
// golden when shouldUpdate is true).
func runFixtureCase(t *testing.T, tc fixtureCase) {
	t.Helper()

	// Zero every env-allowlisted variable so ambient developer env
	// (e.g. an exported ANTHROPIC_AUTH_TOKEN in the shell that runs
	// `go test`) does not leak into the EnvOverride layer and
	// destabilize project.json goldens. t.Setenv restores the
	// previous value at test end. Empty string is treated as "not
	// set" by envOverrideLayer, so this is functionally equivalent
	// to unset without needing os.Unsetenv (which lacks a t.Cleanup
	// analogue).
	clearFixtureEnv(t)

	r := newFixtureResolver(t)
	seedSettings(t, r, tc)

	// Stage 1: Import.
	core, overlay, err := claudecode.New().Import(context.Background(), r)
	if err != nil {
		t.Fatalf("Import: %v", err)
	}
	importGolden := filepath.Join(tc.Expected, "import.json")
	assertOrUpdateJSON(t, importGolden, importCanonical{Core: core, Overlay: overlay}, r)

	// Stage 2/3: Plan → Apply. Skipped when profile.yaml is absent.
	profile, hasProfile := loadProfile(t, tc)
	if hasProfile {
		plans, err := claudecode.New().Plan(context.Background(), r, profile)
		if err != nil {
			t.Fatalf("Plan: %v", err)
		}
		if len(plans) != 1 {
			t.Fatalf("Plan returned %d plans, want 1", len(plans))
		}
		report, err := claudecode.New().Apply(context.Background(), r, plans[0])
		if err != nil {
			t.Fatalf("Apply: %v", err)
		}

		afterGolden := filepath.Join(tc.Expected, "after_apply.json")
		afterBytes := readSettingsBytes(t, r)
		assertOrUpdateBytes(t, afterGolden, afterBytes)

		diffGolden := filepath.Join(tc.Expected, "diff.json")
		assertOrUpdateJSON(t, diffGolden, report.Diff, r)
	}

	// Stage 4: Project. Always runs; profile empty when absent from
	// the fixture so overlay-only assertions still resolve.
	view, err := claudecode.New().Project(context.Background(), r, profile)
	if err != nil {
		t.Fatalf("Project: %v", err)
	}
	projectGolden := filepath.Join(tc.Expected, "project.json")
	assertOrUpdateJSON(t, projectGolden, view, r)
}

// newFixtureResolver builds a Bootstrap'd Resolver anchored at a fresh
// t.TempDir so every case owns its own on-disk layout. ~/.claude is
// pre-created at 0700 so the fixture writers do not have to think
// about the parent-dir invariant.
func newFixtureResolver(t *testing.T) *storage.Resolver {
	t.Helper()
	r, err := storage.NewResolverWithHome(t.TempDir())
	if err != nil {
		t.Fatalf("storage.NewResolverWithHome: %v", err)
	}
	if err := storage.Bootstrap(r); err != nil {
		t.Fatalf("storage.Bootstrap: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(r.Home(), ".claude"), 0o700); err != nil {
		t.Fatalf("mkdir ~/.claude: %v", err)
	}
	return r
}

// seedSettings copies the fixture's settings.json into
// ~/.claude/settings.json (or wires up a symlink for the symlink-in-
// HOME case). Uses os.ReadFile / os.WriteFile — never os.Rename — so
// the original fixture on disk stays byte-identical for future runs.
func seedSettings(t *testing.T, r *storage.Resolver, tc fixtureCase) {
	t.Helper()
	src, err := os.ReadFile(tc.SettingsPath)
	if err != nil {
		t.Fatalf("read fixture settings.json %q: %v", tc.SettingsPath, err)
	}
	dst := claudecode.SettingsPath(r)
	if tc.Symlink {
		// Write the real bytes to ~/.claude/settings-actual.json and
		// point ~/.claude/settings.json at it. The link target stays
		// entirely inside HOME so verifyReadTargetInHome follows it.
		actual := filepath.Join(filepath.Dir(dst), "settings-actual.json")
		if err := os.WriteFile(actual, src, 0o600); err != nil {
			t.Fatalf("write settings-actual.json: %v", err)
		}
		if err := os.Symlink(actual, dst); err != nil {
			t.Fatalf("symlink settings.json → %q: %v", actual, err)
		}
		return
	}
	if err := os.WriteFile(dst, src, 0o600); err != nil {
		t.Fatalf("write ~/.claude/settings.json: %v", err)
	}
}

// loadProfile decodes tc.ProfilePath into a config.Profile. When the
// fixture omits profile.yaml, returns (zero Profile, false) so the
// caller can skip Plan+Apply and still hand a defined Profile to
// Project.
func loadProfile(t *testing.T, tc fixtureCase) (config.Profile, bool) {
	t.Helper()
	if tc.ProfilePath == "" {
		return config.Profile{}, false
	}
	data, err := os.ReadFile(tc.ProfilePath)
	if err != nil {
		t.Fatalf("read profile.yaml %q: %v", tc.ProfilePath, err)
	}
	var p config.Profile
	if err := yaml.Unmarshal(data, &p); err != nil {
		t.Fatalf("yaml.Unmarshal profile.yaml %q: %v", tc.ProfilePath, err)
	}
	return p, true
}

// readSettingsBytes returns the on-disk ~/.claude/settings.json bytes,
// following symlinks (os.ReadFile default behaviour). Used for the
// byte-identical after_apply.json compare.
func readSettingsBytes(t *testing.T, r *storage.Resolver) []byte {
	t.Helper()
	data, err := os.ReadFile(claudecode.SettingsPath(r))
	if err != nil {
		t.Fatalf("read post-apply settings.json: %v", err)
	}
	return data
}

// assertOrUpdateJSON compares the marshaled form of got against the
// contents of goldenPath. On mismatch: if shouldUpdate, rewrite the
// golden; otherwise fail the test with a byte-count-aware error.
//
// A missing golden without shouldUpdate is a skip signal: the test
// does not fail, matching the "expected/*.json absent means skip that
// stage" rule.
//
// The r *storage.Resolver is passed through so redactHome can
// substitute a stable placeholder for the per-test HOME temp path
// (e.g. /tmp/TestFixtures.../001/.claude/settings.json →
// <HOME>/.claude/settings.json) before compare. Without this
// substitution project.json goldens embed a random tempdir path and
// no run ever matches its own golden. Passing the resolver rather
// than a bare string keeps the substitution close to its source of
// truth (Resolver.Home()).
func assertOrUpdateJSON(t *testing.T, goldenPath string, got any, r *storage.Resolver) {
	t.Helper()
	buf, err := json.MarshalIndent(got, "", "  ")
	if err != nil {
		t.Fatalf("marshal for %q: %v", goldenPath, err)
	}
	// Newline-terminate so goldens are POSIX-clean text files.
	buf = append(buf, '\n')
	buf = redactHome(buf, r)
	assertOrUpdateBytes(t, goldenPath, buf)
}

// redactHome replaces every occurrence of the per-test HOME path with
// the literal "<HOME>" so goldens survive a re-run at a different
// t.TempDir location. Applied only to JSON goldens — after_apply.json
// is byte-comparing settings.json which never contains a HOME path.
func redactHome(buf []byte, r *storage.Resolver) []byte {
	if r == nil {
		return buf
	}
	home := r.Home()
	if home == "" {
		return buf
	}
	return bytes.ReplaceAll(buf, []byte(home), []byte("<HOME>"))
}

// clearFixtureEnv wipes every env var the claudecode env-allowlist
// covers so ambient developer env cannot leak into a fixture test.
// Uses t.Setenv, which restores the previous value at test end.
// Matches the pattern in project_test.go's clearClaudeEnv, kept
// local here so fixtures_test.go stays self-contained (no header-
// order coupling with the other test files in this package).
func clearFixtureEnv(t *testing.T) {
	t.Helper()
	for _, name := range []string{
		"ANTHROPIC_API_KEY",
		"ANTHROPIC_AUTH_TOKEN",
		"ANTHROPIC_BASE_URL",
		"ANTHROPIC_MODEL",
		"ANTHROPIC_SMALL_FAST_MODEL",
		"CLAUDE_CODE_USE_BEDROCK",
		"CLAUDE_CODE_USE_VERTEX",
	} {
		t.Setenv(name, "")
	}
}

// assertOrUpdateBytes is the raw-bytes cousin of assertOrUpdateJSON.
// Used for after_apply.json where the golden is settings.json bytes
// verbatim — no re-marshaling.
func assertOrUpdateBytes(t *testing.T, goldenPath string, got []byte) {
	t.Helper()

	if shouldUpdate() {
		if err := os.MkdirAll(filepath.Dir(goldenPath), 0o755); err != nil {
			t.Fatalf("mkdir golden dir %q: %v", filepath.Dir(goldenPath), err)
		}
		if err := os.WriteFile(goldenPath, got, 0o644); err != nil {
			t.Fatalf("write golden %q: %v", goldenPath, err)
		}
		return
	}

	want, err := os.ReadFile(goldenPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			// Skip signal — a stage without a golden is intentional
			// per fixtures_test.go's opening godoc.
			return
		}
		t.Fatalf("read golden %q: %v", goldenPath, err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf(
			"golden mismatch %q\n--- got (%d bytes) ---\n%s\n--- want (%d bytes) ---\n%s",
			goldenPath, len(got), previewBytes(got), len(want), previewBytes(want),
		)
	}
}

// previewBytes clips b to a readable head for failure messages. Full
// content is available on disk; the failure log only needs enough to
// see the divergence, not the whole document.
func previewBytes(b []byte) string {
	const max = 512
	if len(b) <= max {
		return string(b)
	}
	return fmt.Sprintf("%s\n... [%d more bytes] ...", string(b[:max]), len(b)-max)
}

