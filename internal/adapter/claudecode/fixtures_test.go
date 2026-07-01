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
//	error-only.txt (optional) — error identifier for error-only cases
//	setup.txt (optional)    — human-readable notes about non-standard setup
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
// dead-end for the write-path. This asymmetry with Import will be
// resolved per docs/plan/stories/E2-FOLLOWUP-symlink-in-home-apply.md;
// once that story lands, dropping a profile.yaml into the case
// directory reactivates the Plan+Apply half of the matrix.
// Omitting profile.yaml today keeps the matrix honest about which
// stages a fixture actually exercises; forcing an Apply we know will
// error would just embed the error message in a golden.
//
// Symlink-out-of-HOME. The edge/symlink-out-of-home case wires
// ~/.claude/settings.json → a per-test outside-HOME path (a second
// t.TempDir(), which is a sibling of HOME, not a descendant). Import
// must refuse via ErrOutsideHome. The case is marked error-only and
// skips every stage golden — the error assertion IS the check.
//
// Missing / BOM / comments. Three additional error-only fixtures pin
// early-exit behaviour of Import:
//
//	edge/missing/       — no settings.json on disk → ErrNoConfig
//	edge/bom/           — UTF-8 BOM prefix → ErrParseFailed (encoding/json
//	                      does not strip BOM; treatAsEmpty does not
//	                      recognize it as whitespace)
//	edge/comments/      — leading `// line comment` before the JSON
//	                      root → ErrParseFailed (encoding/json is strict
//	                      per RFC 8259, no comments allowed)
//
// Empty / whitespace-only. Both cases exercise Import + Project
// through treatAsEmpty's "well-defined empty" policy. profile.yaml
// is deliberately omitted so Plan+Apply is skipped: the empty-file
// → Apply path currently triggers writepath's TouchesUnowned guard
// (Flatten(nil) surfaces the "" key on the current side, which is
// not in OwnedKeys). Whether that guard should special-case a
// zero-byte current is tracked as a distinct followup —
// docs/plan/stories/E2-FOLLOWUP-flatten-nil.md — and a future story
// that fixes it can drop a profile.yaml into these case dirs and
// regenerate goldens without touching this file.
//
// Silent-skip tripwire. Because a missing golden is a valid skip
// signal (used by empty/whitespace-only/symlink-in-home to omit the
// Plan+Apply half), runFixtureCase tracks a compareCount of how many
// stages actually compared against a golden. A non-error-only case
// that skipped EVERY stage's compare (compareCount == 0) fails the
// test — the fixture is either brand-new-without-goldens or someone
// deleted all its goldens. Regenerate with -update-fixtures.
//
// Golden JSON validity. TestFixtureGoldensAreValidJSON walks every
// checked-in expected/*.json and runs json.Unmarshal to guarantee no
// malformed golden slips into the repo — a corrupt golden would
// silently pass a byte-compare against a corrupt output on the next
// -update-fixtures run.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
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
// settings.json OR an error-only.txt marker, and runs one t.Run per
// case.
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

// TestFixtureGoldensAreValidJSON walks every expected/*.json under
// fixturesRoot and rejects the run if any file fails json.Unmarshal.
// A corrupt golden would silently byte-match a corrupt pipeline output
// on the next `-update-fixtures` run, hiding a real regression — so
// this test is a standalone gate on golden validity independent of any
// individual fixture case.
func TestFixtureGoldensAreValidJSON(t *testing.T) {
	var bad []string
	err := filepath.WalkDir(fixturesRoot, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		// Only inspect JSON goldens under an expected/ directory.
		if filepath.Ext(path) != ".json" {
			return nil
		}
		if filepath.Base(filepath.Dir(path)) != "expected" {
			return nil
		}
		data, rerr := os.ReadFile(path)
		if rerr != nil {
			return fmt.Errorf("read %q: %w", path, rerr)
		}
		var any any
		if uerr := json.Unmarshal(data, &any); uerr != nil {
			bad = append(bad, fmt.Sprintf("%s: %v", path, uerr))
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk goldens: %v", err)
	}
	if len(bad) > 0 {
		t.Fatalf("malformed golden JSON:\n  %s", strings.Join(bad, "\n  "))
	}
}

// fixtureCase captures the on-disk layout of one matrix row.
type fixtureCase struct {
	Class    string // "happy" or "edge"
	Name     string // subdirectory name
	Dir      string // fixturesRoot/<Class>/<Name>
	Expected string // fixturesRoot/<Class>/<Name>/expected

	SettingsPath string // path to the input settings.json, "" if none
	ProfilePath  string // path to profile.yaml, "" when absent

	// Setup mode. Exactly one of these is set for setups that diverge
	// from the default "copy settings.json into ~/.claude/settings.json"
	// path. Kept as separate bools rather than an enum so an unknown
	// value in a future fixture is a compile-time addition, not a
	// silent default.
	SymlinkInHome     bool // ~/.claude/settings.json → ~/.claude/settings-actual.json
	SymlinkOutOfHome  bool // ~/.claude/settings.json → outside-HOME real file
	ErrorOnlyErrName  string // expected error identifier when ErrorOnly is true
	ErrorOnly         bool  // true → assert Import error, skip all stage compares
}

// discoverCases walks classes then names to build the case slice.
// Deterministic order: sort.Strings on class and name so `go test -v`
// output is stable across runs.
//
// A directory qualifies as a case if it contains EITHER a settings.json
// (standard case) OR an error-only.txt marker (a case whose fixture
// asserts an Import-time error and has no seeded settings.json — see
// edge/missing).
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
			errorOnlyPath := filepath.Join(dir, "error-only.txt")

			hasSettings := statOK(settings)
			hasErrorOnly := statOK(errorOnlyPath)
			if !hasSettings && !hasErrorOnly {
				// A directory without either marker is not a case —
				// skip silently so hidden dirs (VCS metadata, editors)
				// do not fail discovery.
				continue
			}

			c := fixtureCase{
				Class:            class,
				Name:             name,
				Dir:              dir,
				Expected:         filepath.Join(dir, "expected"),
				SymlinkInHome:    name == "symlink-in-home",
				SymlinkOutOfHome: name == "symlink-out-of-home",
			}
			if hasSettings {
				c.SettingsPath = settings
			}
			if hasErrorOnly {
				errName, rerr := readErrorOnly(errorOnlyPath)
				if rerr != nil {
					return nil, fmt.Errorf("read %q: %w", errorOnlyPath, rerr)
				}
				c.ErrorOnly = true
				c.ErrorOnlyErrName = errName
			}
			profile := filepath.Join(dir, "profile.yaml")
			if statOK(profile) {
				c.ProfilePath = profile
			}
			out = append(out, c)
		}
	}
	return out, nil
}

// statOK is a small sugar for "does this file exist and is stattable".
// Any error (not-exist or otherwise) is folded to false — callers only
// care about the yes/no question.
func statOK(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// readErrorOnly returns the trimmed contents of an error-only.txt
// marker. The file records the expected error identifier as a bare
// symbol (e.g. "ErrOutsideHome") so the fixture harness can dispatch
// to the right errors.Is target.
func readErrorOnly(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(data)), nil
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

	// Error-only cases short-circuit: Import MUST fail with the
	// declared error identifier; no stage compares run. This is the
	// exception baked in for cases whose entire assertion IS the
	// error path (missing/, bom/, comments/, symlink-out-of-home/).
	if tc.ErrorOnly {
		_, _, err := claudecode.New().Import(context.Background(), r)
		if err == nil {
			t.Fatalf("Import: err = nil, want %s", tc.ErrorOnlyErrName)
		}
		target := errorOnlyTarget(tc.ErrorOnlyErrName)
		if target == nil {
			t.Fatalf("unknown error-only identifier %q in %q — extend errorOnlyTarget()",
				tc.ErrorOnlyErrName, tc.Dir)
		}
		if !errors.Is(err, target) {
			t.Fatalf("Import: err = %v, want errors.Is %s", err, tc.ErrorOnlyErrName)
		}
		return
	}

	// Non-error-only cases: run the full pipeline and count stages
	// that actually compared against (or regenerated) a golden. A
	// case that skipped every compare below is flagged after Project
	// returns — that indicates an incomplete fixture.
	var compareCount int

	// Stage 1: Import.
	core, overlay, err := claudecode.New().Import(context.Background(), r)
	if err != nil {
		t.Fatalf("Import: %v", err)
	}
	importGolden := filepath.Join(tc.Expected, "import.json")
	if assertOrUpdateJSON(t, importGolden, importCanonical{Core: core, Overlay: overlay}, r) {
		compareCount++
	}

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
		if assertOrUpdateBytes(t, afterGolden, afterBytes) {
			compareCount++
		}

		diffGolden := filepath.Join(tc.Expected, "diff.json")
		if assertOrUpdateJSON(t, diffGolden, report.Diff, r) {
			compareCount++
		}
	}

	// Stage 4: Project. Always runs; profile empty when absent from
	// the fixture so overlay-only assertions still resolve.
	view, err := claudecode.New().Project(context.Background(), r, profile)
	if err != nil {
		t.Fatalf("Project: %v", err)
	}
	projectGolden := filepath.Join(tc.Expected, "project.json")
	if assertOrUpdateJSON(t, projectGolden, view, r) {
		compareCount++
	}

	// Tripwire. Zero compared stages means every golden is missing —
	// the fixture is incomplete (no goldens seeded, or every golden
	// was deleted). Silent-skipping the whole case hides that. When
	// regenerating with -update-fixtures the compare paths count as
	// "compared" too, so this only fires on a real fresh-and-empty
	// case dir.
	if compareCount == 0 {
		t.Fatalf(
			"fixture %s/%s ran to completion but compared ZERO goldens — "+
				"regenerate with -update-fixtures, or add an error-only.txt marker "+
				"if this case is meant to assert an Import-time error",
			tc.Class, tc.Name,
		)
	}
}

// errorOnlyTarget maps a bare error identifier (as recorded in an
// error-only.txt marker) to the concrete sentinel error value the
// case must errors.Is-match. Any unknown identifier is a signal that
// a new fixture landed without updating the dispatch table — the
// caller surfaces this as a hard fail with the case path.
func errorOnlyTarget(name string) error {
	switch name {
	case "ErrNoConfig":
		return claudecode.ErrNoConfig
	case "ErrParseFailed":
		return claudecode.ErrParseFailed
	case "ErrOutsideHome":
		return claudecode.ErrOutsideHome
	default:
		return nil
	}
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
// ~/.claude/settings.json, or wires up one of the special-case
// symlink setups. Uses os.ReadFile / os.WriteFile — never os.Rename —
// so the original fixture on disk stays byte-identical for future
// runs.
func seedSettings(t *testing.T, r *storage.Resolver, tc fixtureCase) {
	t.Helper()

	// missing/: no seed. The Import path exercises the "settings.json
	// simply is not there" branch. Any fixture that carries
	// error-only.txt but no settings.json lands here.
	if tc.SettingsPath == "" {
		return
	}

	src, err := os.ReadFile(tc.SettingsPath)
	if err != nil {
		t.Fatalf("read fixture settings.json %q: %v", tc.SettingsPath, err)
	}
	dst := claudecode.SettingsPath(r)

	switch {
	case tc.SymlinkInHome:
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
	case tc.SymlinkOutOfHome:
		// Write the real bytes to a sibling of HOME (a second
		// t.TempDir), then symlink ~/.claude/settings.json to that
		// out-of-HOME path. EvalSymlinks resolves the leaf outside
		// HOME → verifyReadTargetInHome fails with ErrOutsideHome.
		outside := t.TempDir()
		actual := filepath.Join(outside, "settings.json")
		if err := os.WriteFile(actual, src, 0o600); err != nil {
			t.Fatalf("write out-of-HOME settings.json: %v", err)
		}
		if err := os.Symlink(actual, dst); err != nil {
			t.Fatalf("symlink settings.json → %q: %v", actual, err)
		}
	default:
		if err := os.WriteFile(dst, src, 0o600); err != nil {
			t.Fatalf("write ~/.claude/settings.json: %v", err)
		}
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
// Returns true when a compare (or a golden regenerate) actually ran,
// false when the golden was missing and shouldUpdate is off (skip
// signal). runFixtureCase uses that bool to power the compareCount
// tripwire — a case that gets false from every stage never verified
// anything.
//
// The r *storage.Resolver is passed through so redactHome can
// substitute a stable placeholder for the per-test HOME temp path
// (e.g. /tmp/TestFixtures.../001/.claude/settings.json →
// <HOME>/.claude/settings.json) before compare. Without this
// substitution project.json goldens embed a random tempdir path and
// no run ever matches its own golden. Passing the resolver rather
// than a bare string keeps the substitution close to its source of
// truth (Resolver.Home()).
func assertOrUpdateJSON(t *testing.T, goldenPath string, got any, r *storage.Resolver) bool {
	t.Helper()
	buf, err := json.MarshalIndent(got, "", "  ")
	if err != nil {
		t.Fatalf("marshal for %q: %v", goldenPath, err)
	}
	// Newline-terminate so goldens are POSIX-clean text files.
	buf = append(buf, '\n')
	buf = redactHome(buf, r)
	return assertOrUpdateBytes(t, goldenPath, buf)
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
//
// Returns true when the golden was compared (or regenerated), false
// when it was missing and shouldUpdate is off.
func assertOrUpdateBytes(t *testing.T, goldenPath string, got []byte) bool {
	t.Helper()

	if shouldUpdate() {
		if err := os.MkdirAll(filepath.Dir(goldenPath), 0o755); err != nil {
			t.Fatalf("mkdir golden dir %q: %v", filepath.Dir(goldenPath), err)
		}
		if err := os.WriteFile(goldenPath, got, 0o644); err != nil {
			t.Fatalf("write golden %q: %v", goldenPath, err)
		}
		return true
	}

	want, err := os.ReadFile(goldenPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			// Skip signal — a stage without a golden is intentional
			// per fixtures_test.go's opening godoc.
			return false
		}
		t.Fatalf("read golden %q: %v", goldenPath, err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf(
			"golden mismatch %q\n--- got (%d bytes) ---\n%s\n--- want (%d bytes) ---\n%s",
			goldenPath, len(got), previewBytes(got), len(want), previewBytes(want),
		)
	}
	return true
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
