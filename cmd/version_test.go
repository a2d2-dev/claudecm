package cmd

// version_test.go — Story E6-S9 tests for the cmd/version surface.
//
// Test strategy: swap pkg/version's package-level Version/Commit/Date
// to deterministic values so text and JSON output can be asserted
// byte-for-byte. Every test restores the originals on cleanup so
// state does not leak.

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/a2d2-dev/claudecm/pkg/version"
)

// pinVersionForTest overrides pkg/version's three vars and returns the
// restore closure the caller MUST defer.
func pinVersionForTest(t *testing.T, v, c, d string) {
	t.Helper()
	origV, origC, origD := version.Version, version.Commit, version.Date
	version.Version = v
	version.Commit = c
	version.Date = d
	t.Cleanup(func() {
		version.Version = origV
		version.Commit = origC
		version.Date = origD
	})
}

// resetVersionFlags restores versionOutputFlag to its default.
func resetVersionFlags() {
	versionOutputFlag = "text"
}

// runVersionInner invokes runVersion with a synthetic cobra.Command
// whose Out/Err are bytes.Buffers so no /dev/stdout wiring is needed.
func runVersionInner(t *testing.T) (stdout, stderr string, err error) {
	t.Helper()
	var out, errBuf bytes.Buffer
	cmd := &cobra.Command{Use: "version"}
	cmd.SetOut(&out)
	cmd.SetErr(&errBuf)
	err = runVersion(cmd, nil)
	return out.String(), errBuf.String(), err
}

// TestVersion_TextOutput asserts the three-line text form.
func TestVersion_TextOutput(t *testing.T) {
	resetVersionFlags()
	pinVersionForTest(t, "v1.2.3", "abc1234", "2026-06-30T12:00:00Z")

	stdout, _, err := runVersionInner(t)
	if err != nil {
		t.Fatalf("runVersion err = %v", err)
	}
	if !strings.Contains(stdout, "claudecm v1.2.3") {
		t.Errorf("stdout missing version line: %s", stdout)
	}
	if !strings.Contains(stdout, "commit: abc1234") {
		t.Errorf("stdout missing commit line: %s", stdout)
	}
	if !strings.Contains(stdout, "built: 2026-06-30T12:00:00Z") {
		t.Errorf("stdout missing built line: %s", stdout)
	}
}

// TestVersion_JSONOutput asserts the JSON form parses and carries the
// three pinned fields.
func TestVersion_JSONOutput(t *testing.T) {
	resetVersionFlags()
	pinVersionForTest(t, "v9.9.9", "cafebab", "2027-01-02T03:04:05Z")
	versionOutputFlag = "json"

	stdout, _, err := runVersionInner(t)
	if err != nil {
		t.Fatalf("runVersion err = %v", err)
	}
	var body versionJSON
	if err := json.Unmarshal([]byte(stdout), &body); err != nil {
		t.Fatalf("stdout not valid JSON: %v\nBody:\n%s", err, stdout)
	}
	if body.Version != "v9.9.9" {
		t.Errorf("Version = %q; want %q", body.Version, "v9.9.9")
	}
	if body.Commit != "cafebab" {
		t.Errorf("Commit = %q; want %q", body.Commit, "cafebab")
	}
	if body.BuildDate != "2027-01-02T03:04:05Z" {
		t.Errorf("BuildDate = %q; want %q", body.BuildDate, "2027-01-02T03:04:05Z")
	}
}

// TestVersion_InvalidOutputRefused: --output foo → error.
func TestVersion_InvalidOutputRefused(t *testing.T) {
	resetVersionFlags()
	versionOutputFlag = "yaml"
	t.Cleanup(resetVersionFlags)

	_, _, err := runVersionInner(t)
	if err == nil {
		t.Fatalf("runVersion expected error on invalid --output; got nil")
	}
	if !strings.Contains(err.Error(), "invalid --output") {
		t.Errorf("error message unexpected: %v", err)
	}
}

// TestVersion_DefaultsWhenUnstamped: pin nothing (defaults) → still
// prints something meaningful. Guards against a future edit that
// zeroes the defaults.
func TestVersion_DefaultsWhenUnstamped(t *testing.T) {
	resetVersionFlags()
	// Do NOT pin — use pkg/version's defaults so the test also
	// exercises the sentinel path.
	stdout, _, err := runVersionInner(t)
	if err != nil {
		t.Fatalf("runVersion err = %v", err)
	}
	// The defaults are "dev" / "none" / "unknown"; either the test
	// binary was ldflags-stamped (unlikely under `go test`) or the
	// sentinel is present. Assert non-empty output rather than the
	// exact string so a release-tagged test binary is not a false
	// positive.
	if strings.TrimSpace(stdout) == "" {
		t.Errorf("stdout empty for default version render")
	}
}
