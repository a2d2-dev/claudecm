package codex_test

// fixtures_test.go — E4-S7. Testdata-driven end-to-end matrix that
// walks testdata/codex/{happy,edge}/<name>/ and exercises the Codex
// adapter's Import → Plan → Apply → Project pipeline against goldens
// under each case's expected/ directory.
//
// Mirror of internal/adapter/claudecode/fixtures_test.go (E3-S7) —
// same discovery model, same golden layout, same silent-skip
// tripwire — adapted for Codex's two-file layout (config.toml +
// auth.json).
//
// Fixture layout. Each case directory carries:
//
//	config.toml (optional)        — the TOML seed placed at ~/.codex/config.toml
//	auth.json (optional)          — the JSON seed placed at ~/.codex/auth.json
//	profile.yaml (optional)       — Profile fed to Plan+Apply and Project
//	error-only.txt (optional)     — expected Import error identifier
//	setup.txt (optional)          — human-readable notes
//	expected/
//	  import.json                 — canonical (Core, Overlay) after Import
//	  after_apply_config.toml     — on-disk config.toml bytes after Apply
//	  after_apply_auth.json       — on-disk auth.json bytes after Apply
//	  project.json                — canonical EffectiveView after Project
//
// A missing expected/* file for a stage means "skip the golden compare
// for that stage" (still runs the stage — the run itself must not
// error unexpectedly). A missing profile.yaml skips the Plan+Apply
// half of the pipeline entirely (the case runs Import + Project only,
// using an empty Profile for Project).
//
// Golden regeneration. Run with `-update-fixtures` or set
// CLAUDECM_UPDATE_FIXTURES=1 to (re)write every expected/* file with
// the current pipeline output. The regenerated goldens MUST be
// hand-inspected before commit — this is a review checkpoint, not an
// oracle.
//
// Two-file after_apply. Codex's Plan returns two WritePlans in
// auth-first order (auth.json then config.toml). This test walks the
// plan slice and calls Adapter.Apply per plan. The two on-disk files
// after both applies are compared byte-verbatim against
// expected/after_apply_auth.json and expected/after_apply_config.toml
// respectively. Merge-preserve (PRD §4.7) means those bytes should
// round-trip non-owned keys unchanged, so byte-exact goldens ARE the
// tightest assertion available. Auth-elision (Plan returns only the
// config plan when the profile carries zero auth content AND the
// on-disk auth.json is absent/empty) is naturally exercised by the
// edge/config-only case whose profile carries Core.APIKey — Plan
// still emits an auth plan there because the profile claims a slot.
//
// Symlink-out-of-HOME. Two symmetric cases pin the containment check
// on both files:
//
//	edge/symlink-out-of-home/       — ~/.codex/config.toml → outside HOME
//	edge/symlink-out-of-home-auth/  — ~/.codex/auth.json   → outside HOME
//
// Each seeds the target body inside the fixture dir, then the test
// setup writes those bytes to a second t.TempDir (a sibling of HOME)
// and symlinks the ~/.codex/<name> entry to that outside-HOME real
// file. Import must refuse via ErrOutsideHome for either. Both are
// marked error-only and skip every stage golden — the error assertion
// IS the check. Routing is driven by fixtureCase.SymlinkTarget
// ("config" or "auth"), keyed off the case directory name.
//
// Missing / empty / whitespace-only / malformed. Error-only fixtures
// pin Import's early-exit behaviour:
//
//	edge/missing/            — no config.toml, no auth.json → ErrNoConfig
//	edge/empty-files/        — both files zero-byte → ErrNoConfig
//	edge/whitespace-only/    — both files whitespace-only → ErrNoConfig
//	edge/malformed-config/   — config.toml unterminated → ErrParseFailed
//	edge/malformed-auth/     — auth.json unterminated → ErrParseFailed
//	edge/symlink-out-of-home/      — config.toml resolves outside HOME → ErrOutsideHome
//	edge/symlink-out-of-home-auth/ — auth.json resolves outside HOME → ErrOutsideHome
//
// Silent-skip tripwire. Because a missing golden is a valid skip
// signal, runFixtureCase tracks a compareCount of how many stages
// actually compared against a golden. A non-error-only case that
// skipped every stage's compare (compareCount == 0) fails — the
// fixture is either brand-new-without-goldens or someone deleted all
// its goldens. Regenerate with -update-fixtures.
//
// Golden JSON validity. TestFixtureGoldensAreValidJSON walks every
// checked-in expected/*.json (import.json + project.json +
// after_apply_auth.json) and runs json.Unmarshal to guarantee no
// malformed golden slips into the repo. TOML goldens
// (after_apply_config.toml) are NOT sniffed for validity here — they
// are the byte-verbatim output of the write-path, and a corrupt one
// would fail the byte-compare directly against the pipeline's next
// run.

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
	"github.com/a2d2-dev/claudecm/internal/adapter/codex"
	"github.com/a2d2-dev/claudecm/internal/config"
	"github.com/a2d2-dev/claudecm/internal/storage"
)

// fixturesRoot is the on-disk location of the checked-in matrix. Kept
// relative to the test binary's working directory (Go's `go test` runs
// in the package directory).
const fixturesRoot = "testdata/codex"

// updateFixturesFlag mirrors the `-update-fixtures` CLI flag. Named
// with the fixtures suffix to avoid colliding with any future
// -update-* flag in other test files.
var updateFixturesFlag = flag.Bool(
	"update-fixtures",
	false,
	"regenerate testdata/codex/**/expected/* goldens (also enabled by CLAUDECM_UPDATE_FIXTURES=1)",
)

// shouldUpdate returns true when goldens should be (re)written.
func shouldUpdate() bool {
	if updateFixturesFlag != nil && *updateFixturesFlag {
		return true
	}
	return os.Getenv("CLAUDECM_UPDATE_FIXTURES") == "1"
}

// importCanonical is the JSON shape written to expected/import.json.
type importCanonical struct {
	Core    adapter.CoreFromTool    `json:"core"`
	Overlay adapter.OverlayFromTool `json:"overlay"`
}

// planCanonical is the JSON shape written to expected/plans.json.
// Transform + Parser are intentionally omitted: closures and
// interface values do not marshal, and their behavior is pinned via
// the after_apply_*.{toml,json} byte-verbatim goldens instead. This
// projection is the "shallow" plan golden — plan slot order, target
// path, owned-key allowlist, and reason string.
type planCanonical struct {
	Tool      string   `json:"tool"`
	Target    string   `json:"target"`
	OwnedKeys []string `json:"owned_keys"`
	Reason    string   `json:"reason"`
}

// projectPlans distills a []WritePlan down to the serializable
// planCanonical projection, preserving Plan's returned order.
func projectPlans(plans []adapter.WritePlan) []planCanonical {
	out := make([]planCanonical, 0, len(plans))
	for _, p := range plans {
		out = append(out, planCanonical{
			Tool:      p.Tool,
			Target:    p.Target,
			OwnedKeys: append([]string(nil), p.OwnedKeys...),
			Reason:    p.Reason,
		})
	}
	return out
}

// TestFixtures is the matrix entry point. It walks fixturesRoot,
// discovers every <class>/<name>/ directory that contains a
// config.toml, an auth.json, or an error-only.txt marker, and runs
// one t.Run per case.
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
// after_apply_config.toml is NOT sniffed (byte-verbatim contract).
func TestFixtureGoldensAreValidJSON(t *testing.T) {
	var bad []string
	err := filepath.WalkDir(fixturesRoot, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
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
	Class    string
	Name     string
	Dir      string
	Expected string

	ConfigPath  string // path to fixture config.toml, "" if none
	AuthPath    string // path to fixture auth.json, "" if none
	ProfilePath string // path to profile.yaml, "" when absent

	// Setup mode. SymlinkTarget names the ~/.codex/ file that should be
	// wired as an out-of-HOME symlink for the read-side containment
	// check. Empty string means "no symlink wiring" (the common case).
	// Valid values: "" | "config" | "auth". Routing is keyed off the
	// case directory name in discoverCases.
	SymlinkTarget    string
	ErrorOnlyErrName string
	ErrorOnly        bool
}

// discoverCases walks classes then names to build the case slice.
// Deterministic order via sort.Strings.
//
// A directory qualifies as a case if it contains at least one of:
// config.toml, auth.json, or error-only.txt.
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
			configP := filepath.Join(dir, "config.toml")
			authP := filepath.Join(dir, "auth.json")
			errorOnlyPath := filepath.Join(dir, "error-only.txt")

			hasConfig := statOK(configP)
			hasAuth := statOK(authP)
			hasErrorOnly := statOK(errorOnlyPath)
			if !hasConfig && !hasAuth && !hasErrorOnly {
				continue
			}

			c := fixtureCase{
				Class:         class,
				Name:          name,
				Dir:           dir,
				Expected:      filepath.Join(dir, "expected"),
				SymlinkTarget: symlinkTargetForName(name),
			}
			if hasConfig {
				c.ConfigPath = configP
			}
			if hasAuth {
				c.AuthPath = authP
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

// symlinkTargetForName maps a case directory name to the ~/.codex/
// file that should be wired as an out-of-HOME symlink. Empty string
// means "no symlink wiring". Keyed by name so the discovery step
// stays a pure function of the on-disk layout and new symmetric
// cases (config-side vs auth-side) can be added by directory name
// alone.
func symlinkTargetForName(name string) string {
	switch name {
	case "symlink-out-of-home":
		return "config"
	case "symlink-out-of-home-auth":
		return "auth"
	default:
		return ""
	}
}

// statOK folds any error to false.
func statOK(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// readErrorOnly returns the trimmed contents of an error-only.txt
// marker.
func readErrorOnly(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(data)), nil
}

// runFixtureCase seeds a per-test HOME, replays the pipeline, and
// compares each stage's output against its golden.
func runFixtureCase(t *testing.T, tc fixtureCase) {
	t.Helper()

	// Zero every env-allowlisted variable so ambient developer env does
	// not leak into the EnvOverride layer and destabilize project.json
	// goldens.
	clearFixtureEnv(t)

	r := newFixtureResolver(t)
	seedFiles(t, r, tc)

	// Error-only cases short-circuit.
	if tc.ErrorOnly {
		_, _, err := codex.New().Import(context.Background(), r)
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

	var compareCount int

	// Stage 1: Import.
	core, overlay, err := codex.New().Import(context.Background(), r)
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
		plans, err := codex.New().Plan(context.Background(), r, profile)
		if err != nil {
			t.Fatalf("Plan: %v", err)
		}
		if len(plans) < 1 || len(plans) > 2 {
			t.Fatalf("Plan returned %d plans, want 1 or 2", len(plans))
		}

		// Plan golden. Shallow projection: {Tool, Target, OwnedKeys,
		// Reason} per WritePlan, in the order Plan returned them (auth
		// -> config for two-plan cases, single-plan otherwise). The
		// Transform + Parser fields are function/interface values and
		// intentionally NOT serializable — their behavior is covered
		// via the after_apply_*.{toml,json} byte-verbatim goldens.
		plansGolden := filepath.Join(tc.Expected, "plans.json")
		if assertOrUpdateJSON(t, plansGolden, projectPlans(plans), r) {
			compareCount++
		}

		for i, p := range plans {
			if _, err := codex.New().Apply(context.Background(), r, p); err != nil {
				t.Fatalf("Apply[%d] target=%q: %v", i, p.Target, err)
			}
		}

		// After both applies, compare the on-disk bytes for each file
		// against its golden. A missing file after Apply is fine (auth
		// elision) — the byte-compare against a non-existent golden is
		// the skip signal.
		afterConfigGolden := filepath.Join(tc.Expected, "after_apply_config.toml")
		if got, ok := readIfExists(t, codex.ConfigPath(r)); ok {
			if assertOrUpdateBytes(t, afterConfigGolden, got, r) {
				compareCount++
			}
		} else if shouldUpdate() {
			// File does not exist and we are regenerating goldens — do
			// not write an empty golden. Remove any stale one so the
			// tree stays honest.
			_ = os.Remove(afterConfigGolden)
		}

		afterAuthGolden := filepath.Join(tc.Expected, "after_apply_auth.json")
		if got, ok := readIfExists(t, codex.AuthPath(r)); ok {
			if assertOrUpdateBytes(t, afterAuthGolden, got, r) {
				compareCount++
			}
		} else if shouldUpdate() {
			_ = os.Remove(afterAuthGolden)
		}
	}

	// Stage 4: Project. Always runs; empty profile when absent so
	// overlay-only assertions still resolve.
	view, err := codex.New().Project(context.Background(), r, profile)
	if err != nil {
		t.Fatalf("Project: %v", err)
	}
	projectGolden := filepath.Join(tc.Expected, "project.json")
	if assertOrUpdateJSON(t, projectGolden, view, r) {
		compareCount++
	}

	if compareCount == 0 {
		t.Fatalf(
			"fixture %s/%s ran to completion but compared ZERO goldens — "+
				"regenerate with -update-fixtures, or add an error-only.txt marker "+
				"if this case is meant to assert an Import-time error",
			tc.Class, tc.Name,
		)
	}
}

// errorOnlyTarget maps a bare error identifier to the sentinel error
// value the case must errors.Is-match.
func errorOnlyTarget(name string) error {
	switch name {
	case "ErrNoConfig":
		return codex.ErrNoConfig
	case "ErrParseFailed":
		return codex.ErrParseFailed
	case "ErrOutsideHome":
		return codex.ErrOutsideHome
	default:
		return nil
	}
}

// newFixtureResolver builds a Bootstrap'd Resolver at a fresh
// t.TempDir. ~/.codex is pre-created at 0700 so seeders do not have to
// think about the parent-dir invariant.
func newFixtureResolver(t *testing.T) *storage.Resolver {
	t.Helper()
	r, err := storage.NewResolverWithHome(t.TempDir())
	if err != nil {
		t.Fatalf("storage.NewResolverWithHome: %v", err)
	}
	if err := storage.Bootstrap(r); err != nil {
		t.Fatalf("storage.Bootstrap: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(r.Home(), ".codex"), 0o700); err != nil {
		t.Fatalf("mkdir ~/.codex: %v", err)
	}
	return r
}

// seedFiles copies fixture config.toml / auth.json into ~/.codex/ or
// wires the special-case symlink setup.
func seedFiles(t *testing.T, r *storage.Resolver, tc fixtureCase) {
	t.Helper()

	switch tc.SymlinkTarget {
	case "":
		// no-op; fall through to the regular copy path
	case "config":
		seedOutOfHomeSymlink(t, tc.ConfigPath, "config.toml", codex.ConfigPath(r))
		return
	case "auth":
		seedOutOfHomeSymlink(t, tc.AuthPath, "auth.json", codex.AuthPath(r))
		return
	default:
		t.Fatalf("seedFiles: unknown SymlinkTarget %q for %s/%s", tc.SymlinkTarget, tc.Class, tc.Name)
	}

	if tc.ConfigPath != "" {
		src, err := os.ReadFile(tc.ConfigPath)
		if err != nil {
			t.Fatalf("read fixture config.toml %q: %v", tc.ConfigPath, err)
		}
		if err := os.WriteFile(codex.ConfigPath(r), src, 0o600); err != nil {
			t.Fatalf("write ~/.codex/config.toml: %v", err)
		}
	}
	if tc.AuthPath != "" {
		src, err := os.ReadFile(tc.AuthPath)
		if err != nil {
			t.Fatalf("read fixture auth.json %q: %v", tc.AuthPath, err)
		}
		if err := os.WriteFile(codex.AuthPath(r), src, 0o600); err != nil {
			t.Fatalf("write ~/.codex/auth.json: %v", err)
		}
	}
}

// seedOutOfHomeSymlink writes the fixture's seed body to a sibling
// t.TempDir (a directory outside HOME) and then symlinks the
// ~/.codex/ destination to that outside-HOME real file. Used by both
// symlink-out-of-home cases (config-side and auth-side). Fails the
// test if the fixture is missing the required seed file.
func seedOutOfHomeSymlink(t *testing.T, srcPath, srcName, dst string) {
	t.Helper()
	if srcPath == "" {
		t.Fatalf("symlink-out-of-home fixture missing %s", srcName)
	}
	src, err := os.ReadFile(srcPath)
	if err != nil {
		t.Fatalf("read fixture %s %q: %v", srcName, srcPath, err)
	}
	outside := t.TempDir()
	actual := filepath.Join(outside, srcName)
	if err := os.WriteFile(actual, src, 0o600); err != nil {
		t.Fatalf("write out-of-HOME %s: %v", srcName, err)
	}
	if err := os.Symlink(actual, dst); err != nil {
		t.Fatalf("symlink %s → %q: %v", srcName, actual, err)
	}
}

// loadProfile decodes tc.ProfilePath into a config.Profile. When the
// fixture omits profile.yaml, returns (zero Profile, false).
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

// readIfExists returns (bytes, true) when path exists, (nil, false)
// on ENOENT (the auth-elision or config-elision skip signal). ANY
// other error (permissions, EIO, symlink loop, …) is a bug in the
// test harness — never a signal we want to silently fold into
// "file absent" — so it fails the test loudly via t.Fatalf.
func readIfExists(t *testing.T, path string) ([]byte, bool) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, false
		}
		t.Fatalf("readIfExists %q: non-ENOENT error: %v", path, err)
	}
	return data, true
}

// assertOrUpdateJSON compares the marshaled form of got against the
// golden. Returns true when a compare or a regenerate actually ran,
// false when the golden was missing and shouldUpdate is off.
func assertOrUpdateJSON(t *testing.T, goldenPath string, got any, r *storage.Resolver) bool {
	t.Helper()
	buf, err := json.MarshalIndent(got, "", "  ")
	if err != nil {
		t.Fatalf("marshal for %q: %v", goldenPath, err)
	}
	buf = append(buf, '\n')
	return assertOrUpdateBytes(t, goldenPath, buf, r)
}

// redactHome replaces the per-test HOME path with the literal "<HOME>"
// so goldens survive re-runs at a different t.TempDir location.
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

// clearFixtureEnv wipes every env var the codex env-allowlist covers
// so ambient developer env cannot leak into a fixture test. Matches
// the list in project_test.go's clearCodexEnv, kept local so
// fixtures_test.go stays self-contained.
func clearFixtureEnv(t *testing.T) {
	t.Helper()
	for _, name := range []string{
		"OPENAI_API_KEY",
		"OPENAI_BASE_URL",
		"CODEX_MODEL",
		"CODEX_MODEL_PROVIDER",
		"CODEX_HOME",
	} {
		t.Setenv(name, "")
	}
}

// assertOrUpdateBytes is the raw-bytes cousin of assertOrUpdateJSON.
// Used for after_apply_*.toml/.json bytes. Applies redactHome to
// keep goldens portable when a future fixture embeds a HOME-derived
// absolute path in an owned string field — otherwise the golden
// would only match on the regen machine.
func assertOrUpdateBytes(t *testing.T, goldenPath string, got []byte, r *storage.Resolver) bool {
	t.Helper()
	got = redactHome(got, r)

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

// previewBytes clips b to a readable head for failure messages.
func previewBytes(b []byte) string {
	const max = 512
	if len(b) <= max {
		return string(b)
	}
	return fmt.Sprintf("%s\n... [%d more bytes] ...", string(b[:max]), len(b)-max)
}
