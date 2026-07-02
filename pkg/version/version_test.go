package version

import "testing"

// TestDefaults asserts the sentinel values ldflags-less builds expose.
// The defaults are what `claudecm version` renders when no release
// pipeline stamped the binary; changing them is a user-visible break
// (an operator scripting `claudecm version --output json | jq
// .version` against the sentinel would silently break), so the test
// pins them.
func TestDefaults(t *testing.T) {
	if Version == "" {
		t.Errorf("Version default must be non-empty; got %q", Version)
	}
	if Commit == "" {
		t.Errorf("Commit default must be non-empty; got %q", Commit)
	}
	if Date == "" {
		t.Errorf("Date default must be non-empty; got %q", Date)
	}
	if Version != "dev" {
		t.Errorf("Version default = %q; want %q", Version, "dev")
	}
	if Commit != "none" {
		t.Errorf("Commit default = %q; want %q", Commit, "none")
	}
	if Date != "unknown" {
		t.Errorf("Date default = %q; want %q", Date, "unknown")
	}
}

// TestOverridable confirms the vars can be reassigned at runtime — the
// same seam ldflags takes advantage of. A test-only override reassigns
// then restores, so this test does not leak state across the file.
func TestOverridable(t *testing.T) {
	origV, origC, origD := Version, Commit, Date
	t.Cleanup(func() { Version, Commit, Date = origV, origC, origD })

	Version = "v1.2.3"
	Commit = "abcdef1"
	Date = "2026-06-30T12:00:00Z"

	if Version != "v1.2.3" {
		t.Errorf("Version override failed: got %q", Version)
	}
	if Commit != "abcdef1" {
		t.Errorf("Commit override failed: got %q", Commit)
	}
	if Date != "2026-06-30T12:00:00Z" {
		t.Errorf("Date override failed: got %q", Date)
	}
}
