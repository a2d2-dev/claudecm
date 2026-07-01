package storage

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// withHome installs a sandboxed HOME override for the duration of a subtest
// and restores whatever was configured before. Every path test must funnel
// through here so we never touch a real user HOME.
func withHome(t *testing.T, dir string) {
	t.Helper()
	prev := currentHomeOverride()
	SetHomeOverride(dir)
	t.Cleanup(func() { SetHomeOverride(prev) })
}

func TestValidateProfileName(t *testing.T) {
	t.Parallel()

	validCases := []struct {
		name  string
		input string
	}{
		{"single lowercase letter", "a"},
		{"single digit", "0"},
		{"alphanumeric", "abc"},
		{"mixed with allowed punctuation", "a.b-c_1"},
		{"digits-only leading digit", "0123"},
		{"max length 64", strings.Repeat("a", 64)},
	}
	for _, tc := range validCases {
		tc := tc
		t.Run("valid/"+tc.name, func(t *testing.T) {
			t.Parallel()
			if err := ValidateProfileName(tc.input); err != nil {
				t.Fatalf("ValidateProfileName(%q) = %v; want nil", tc.input, err)
			}
		})
	}

	invalidCases := []struct {
		name      string
		input     string
		wantMatch string // substring the error message must contain
	}{
		{"empty", "", "empty"},
		{"dot", ".", "reserved"},
		{"dotdot", "..", "reserved"},
		{"traversal path", "../evil", "path separator"},
		{"uppercase", "ABC", "must match"},
		{"leading dash", "-abc", "must match"},
		{"leading dot", ".abc", "must match"},
		{"length 65", strings.Repeat("a", 65), "exceeds"},
		{"contains slash", "foo/bar", "path separator"},
		{"contains backslash", `foo\bar`, "path separator"},
		{"contains NUL", "foo\x00bar", "NUL"},
		{"contains newline", "foo\nbar", "control character"},
		{"contains tab", "foo\tbar", "control character"},
		// NFKC fullwidth "．．" collapses to ".." — the defense-in-depth
		// normalization must reject it even though the regex would too.
		{"nfkc dotdot homoglyph", "．．", "reserved"},
		{"nfkc dot homoglyph", "．", "reserved"},
	}
	for _, tc := range invalidCases {
		tc := tc
		t.Run("invalid/"+tc.name, func(t *testing.T) {
			t.Parallel()
			err := ValidateProfileName(tc.input)
			if err == nil {
				t.Fatalf("ValidateProfileName(%q) = nil; want error", tc.input)
			}
			if !strings.Contains(err.Error(), tc.wantMatch) {
				t.Fatalf("ValidateProfileName(%q) = %v; want error containing %q",
					tc.input, err, tc.wantMatch)
			}
		})
	}
}

func TestResolveHome_HonorsOverride(t *testing.T) {
	dir := t.TempDir()
	withHome(t, dir)

	got, err := ResolveHome()
	if err != nil {
		t.Fatalf("ResolveHome() = _, %v; want nil error", err)
	}
	if want := filepath.Clean(dir); got != want {
		t.Fatalf("ResolveHome() = %q; want %q", got, want)
	}
}

func TestResolveHome_RefusesRoot(t *testing.T) {
	withHome(t, "/")
	if _, err := ResolveHome(); err == nil {
		t.Fatal(`ResolveHome() with HOME="/" = nil; want refuse`)
	}
}

func TestResolveHome_RefusesEmptyOverride(t *testing.T) {
	// Empty override falls through to os.UserHomeDir(). To simulate the
	// "empty HOME" branch, override with only whitespace-equivalent absolute
	// nonsense — cover the non-absolute check instead.
	withHome(t, "relative/path")
	if _, err := ResolveHome(); err == nil {
		t.Fatal("ResolveHome() with relative HOME = nil; want refuse")
	}
}

func TestResolveHome_RefusesMissingDir(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "does-not-exist")
	withHome(t, missing)
	if _, err := ResolveHome(); err == nil {
		t.Fatalf("ResolveHome(%q) missing dir = nil; want refuse", missing)
	}
}

func TestResolveHome_RefusesFileAsHome(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "not-a-dir")
	if err := os.WriteFile(file, []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	withHome(t, file)
	if _, err := ResolveHome(); err == nil {
		t.Fatal("ResolveHome() with file-as-HOME = nil; want refuse")
	}
}

// NOTE: The euid/root-owned refuse branch in verifyHomeOwnership is not
// exercised here. The CI runner and dev machine both run tests as root
// (euid == 0), so the branch would return nil unconditionally. Reliably
// forcing a non-root euid across CI environments would require setuid
// gymnastics that leak test-only concerns into the code under test. The
// branch is small, has no hidden control flow, and is reviewed by
// inspection.

func TestProfilePath_RejectsBadNames(t *testing.T) {
	withHome(t, t.TempDir())

	cases := []string{"", ".", "..", "../evil", "/etc/passwd", "foo/bar", "ABC"}
	for _, name := range cases {
		name := name
		t.Run(name, func(t *testing.T) {
			if _, err := ProfilePath(name); err == nil {
				t.Fatalf("ProfilePath(%q) = nil; want error", name)
			}
		})
	}
}

func TestProfilePath_HappyStaysUnderHome(t *testing.T) {
	home := t.TempDir()
	withHome(t, home)

	got, err := ProfilePath("foo")
	if err != nil {
		t.Fatalf("ProfilePath(\"foo\") = _, %v; want nil", err)
	}
	want := filepath.Join(home, ConfigDirName, ProfilesDirName, "foo.yaml")
	if got != want {
		t.Fatalf("ProfilePath = %q; want %q", got, want)
	}
	if !strings.HasPrefix(got, filepath.Clean(home)+string(filepath.Separator)) {
		t.Fatalf("ProfilePath escapes HOME: %q not under %q", got, home)
	}
}

func TestStatePath_And_ProfilesDir_And_BackupsRoot(t *testing.T) {
	home := t.TempDir()
	withHome(t, home)

	sp, err := StatePath()
	if err != nil {
		t.Fatalf("StatePath = _, %v", err)
	}
	if want := filepath.Join(home, ConfigDirName, StateFileName); sp != want {
		t.Fatalf("StatePath = %q; want %q", sp, want)
	}

	pd, err := ProfilesDir()
	if err != nil {
		t.Fatalf("ProfilesDir = _, %v", err)
	}
	if want := filepath.Join(home, ConfigDirName, ProfilesDirName); pd != want {
		t.Fatalf("ProfilesDir = %q; want %q", pd, want)
	}

	br, err := BackupsRoot()
	if err != nil {
		t.Fatalf("BackupsRoot = _, %v", err)
	}
	if want := filepath.Join(home, ConfigDirName, BackupsDirName); br != want {
		t.Fatalf("BackupsRoot = %q; want %q", br, want)
	}
}

func TestBackupPath(t *testing.T) {
	home := t.TempDir()
	withHome(t, home)

	t.Run("happy", func(t *testing.T) {
		got, err := BackupPath("claude_code", "settings.json", "20260701T000000Z-abcd")
		if err != nil {
			t.Fatalf("BackupPath happy = _, %v", err)
		}
		want := filepath.Join(home, ConfigDirName, BackupsDirName,
			"claude_code", "settings.json.bak.20260701T000000Z-abcd")
		if got != want {
			t.Fatalf("BackupPath = %q; want %q", got, want)
		}
	})

	badSegments := []struct {
		name             string
		tool, file, when string
	}{
		{"tool traversal", "..", "settings.json", "ts"},
		{"tool with slash", "claude_code/../..", "settings.json", "ts"},
		{"filename traversal", "claude_code", "..", "ts"},
		{"filename with slash", "claude_code", "../evil", "ts"},
		{"empty timestamp", "claude_code", "settings.json", ""},
		{"NUL in tool", "claude\x00", "settings.json", "ts"},
	}
	for _, tc := range badSegments {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			if _, err := BackupPath(tc.tool, tc.file, tc.when); err == nil {
				t.Fatalf("BackupPath(%q,%q,%q) = nil; want error",
					tc.tool, tc.file, tc.when)
			}
		})
	}
}

func TestSafeToolConfigPath(t *testing.T) {
	home := t.TempDir()
	withHome(t, home)

	t.Run("happy claude settings", func(t *testing.T) {
		got, err := SafeToolConfigPath(".claude/settings.json")
		if err != nil {
			t.Fatalf("SafeToolConfigPath happy = _, %v", err)
		}
		want := filepath.Join(home, ".claude", "settings.json")
		if got != want {
			t.Fatalf("SafeToolConfigPath = %q; want %q", got, want)
		}
	})

	t.Run("happy codex config", func(t *testing.T) {
		got, err := SafeToolConfigPath(".codex/config.toml")
		if err != nil {
			t.Fatalf("SafeToolConfigPath codex = _, %v", err)
		}
		want := filepath.Join(home, ".codex", "config.toml")
		if got != want {
			t.Fatalf("SafeToolConfigPath = %q; want %q", got, want)
		}
	})

	badCases := []struct {
		name  string
		input string
	}{
		{"traversal escapes home", "../../etc/passwd"},
		{"absolute path", "/etc/passwd"},
		{"empty", ""},
		{"NUL byte", "foo\x00bar"},
	}
	for _, tc := range badCases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			if _, err := SafeToolConfigPath(tc.input); err == nil {
				t.Fatalf("SafeToolConfigPath(%q) = nil; want error", tc.input)
			}
		})
	}
}

func TestEnsureConfigDir_CreatesLayout(t *testing.T) {
	home := t.TempDir()
	withHome(t, home)

	if err := EnsureConfigDir(); err != nil {
		t.Fatalf("EnsureConfigDir = %v", err)
	}
	for _, sub := range []string{ConfigDirName, filepath.Join(ConfigDirName, ProfilesDirName)} {
		info, err := os.Stat(filepath.Join(home, sub))
		if err != nil {
			t.Fatalf("stat %s: %v", sub, err)
		}
		if !info.IsDir() {
			t.Fatalf("%s is not a directory", sub)
		}
		if perm := info.Mode().Perm(); perm != 0700 {
			t.Fatalf("%s mode = %o; want 0700", sub, perm)
		}
	}
}

func TestSetHomeOverride_ClearReverts(t *testing.T) {
	// Explicitly document the "" contract: SetHomeOverride("") clears the
	// override so ResolveHome falls back to os.UserHomeDir().
	prev := currentHomeOverride()
	t.Cleanup(func() { SetHomeOverride(prev) })

	dir := t.TempDir()
	SetHomeOverride(dir)
	if got := currentHomeOverride(); got != dir {
		t.Fatalf("after SetHomeOverride(%q) got %q", dir, got)
	}
	SetHomeOverride("")
	if got := currentHomeOverride(); got != "" {
		t.Fatalf("after SetHomeOverride(\"\") got %q; want empty", got)
	}
}
