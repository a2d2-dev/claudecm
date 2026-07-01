package storage

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// mustResolver builds a Resolver rooted at dir. Every path test funnels
// through here so we never touch a real user HOME.
func mustResolver(t *testing.T, dir string) *Resolver {
	t.Helper()
	r, err := NewResolverWithHome(dir)
	if err != nil {
		t.Fatalf("NewResolverWithHome(%q) = _, %v; want nil", dir, err)
	}
	return r
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

func TestResolver_HonorsExplicitHome(t *testing.T) {
	dir := t.TempDir()
	r := mustResolver(t, dir)
	if got, want := r.Home(), filepath.Clean(dir); got != want {
		t.Fatalf("Resolver.Home() = %q; want %q", got, want)
	}
}

func TestResolver_RefusesRoot(t *testing.T) {
	if _, err := NewResolverWithHome("/"); err == nil {
		t.Fatal(`NewResolverWithHome("/") = nil; want refuse`)
	}
}

func TestResolver_RefusesRelativeHome(t *testing.T) {
	if _, err := NewResolverWithHome("relative/path"); err == nil {
		t.Fatal("NewResolverWithHome(relative) = nil; want refuse")
	}
}

// TestResolver_RefusesEmptyHome exercises both the --home "" branch and the
// real $HOME fallback branch: an unset HOME must be refused rather than
// silently defaulting to something dangerous.
func TestResolver_RefusesEmptyHome(t *testing.T) {
	t.Run("explicit empty", func(t *testing.T) {
		if _, err := NewResolverWithHome(""); err == nil {
			t.Fatal(`NewResolverWithHome("") = nil; want refuse`)
		}
	})
	t.Run("empty $HOME", func(t *testing.T) {
		t.Setenv("HOME", "")
		if _, err := NewResolver(); err == nil {
			t.Fatal("NewResolver() with HOME=\"\" = nil; want refuse")
		}
	})
}

func TestResolver_RefusesMissingDir(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "does-not-exist")
	if _, err := NewResolverWithHome(missing); err == nil {
		t.Fatalf("NewResolverWithHome(%q) missing = nil; want refuse", missing)
	}
}

func TestResolver_RefusesFileAsHome(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "not-a-dir")
	if err := os.WriteFile(file, []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := NewResolverWithHome(file); err == nil {
		t.Fatal("NewResolverWithHome(file) = nil; want refuse")
	}
}

// NOTE: The euid/root-owned refuse branch in verifyHomeOwnership is not
// exercised here. The CI runner and dev machine both run tests as root
// (euid == 0), so the branch would return nil unconditionally. Reliably
// forcing a non-root euid across CI environments would require setuid
// gymnastics that leak test-only concerns into the code under test. The
// branch is small, has no hidden control flow, and is reviewed by
// inspection.

func TestResolver_ProfilePath_RejectsBadNames(t *testing.T) {
	r := mustResolver(t, t.TempDir())

	cases := []string{"", ".", "..", "../evil", "/etc/passwd", "foo/bar", "ABC"}
	for _, name := range cases {
		name := name
		t.Run(name, func(t *testing.T) {
			if _, err := r.ProfilePath(name); err == nil {
				t.Fatalf("ProfilePath(%q) = nil; want error", name)
			}
		})
	}
}

func TestResolver_ProfilePath_HappyStaysUnderHome(t *testing.T) {
	home := t.TempDir()
	r := mustResolver(t, home)

	got, err := r.ProfilePath("foo")
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

func TestResolver_StatePath_And_ProfilesDir_And_BackupsRoot(t *testing.T) {
	home := t.TempDir()
	r := mustResolver(t, home)

	sp, err := r.StatePath()
	if err != nil {
		t.Fatalf("StatePath = _, %v", err)
	}
	if want := filepath.Join(home, ConfigDirName, StateFileName); sp != want {
		t.Fatalf("StatePath = %q; want %q", sp, want)
	}

	pd := r.ProfilesDir()
	if want := filepath.Join(home, ConfigDirName, ProfilesDirName); pd != want {
		t.Fatalf("ProfilesDir = %q; want %q", pd, want)
	}

	br := r.BackupsRoot()
	if want := filepath.Join(home, ConfigDirName, BackupsDirName); br != want {
		t.Fatalf("BackupsRoot = %q; want %q", br, want)
	}
}

func TestResolver_BackupPath(t *testing.T) {
	home := t.TempDir()
	r := mustResolver(t, home)

	t.Run("happy", func(t *testing.T) {
		got, err := r.BackupPath("claude_code", "settings.json", "20260701T000000Z-abcd")
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
			if _, err := r.BackupPath(tc.tool, tc.file, tc.when); err == nil {
				t.Fatalf("BackupPath(%q,%q,%q) = nil; want error",
					tc.tool, tc.file, tc.when)
			}
		})
	}
}

// TestResolver_LexicalToolConfigPath verifies the lexical-only, HOME-relative
// join. This method deliberately does NOT resolve symlinks — that job
// belongs to writepath (E1-S3 / FR-5). We only prove: HOME-relative inputs
// under HOME succeed; absolute, empty, NUL, and traversal-escaping inputs
// are refused.
func TestResolver_LexicalToolConfigPath(t *testing.T) {
	home := t.TempDir()
	r := mustResolver(t, home)

	t.Run("happy claude settings", func(t *testing.T) {
		got, err := r.LexicalToolConfigPath(".claude/settings.json")
		if err != nil {
			t.Fatalf("LexicalToolConfigPath happy = _, %v", err)
		}
		want := filepath.Join(home, ".claude", "settings.json")
		if got != want {
			t.Fatalf("LexicalToolConfigPath = %q; want %q", got, want)
		}
	})

	t.Run("happy codex config", func(t *testing.T) {
		got, err := r.LexicalToolConfigPath(".codex/config.toml")
		if err != nil {
			t.Fatalf("LexicalToolConfigPath codex = _, %v", err)
		}
		want := filepath.Join(home, ".codex", "config.toml")
		if got != want {
			t.Fatalf("LexicalToolConfigPath = %q; want %q", got, want)
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
			if _, err := r.LexicalToolConfigPath(tc.input); err == nil {
				t.Fatalf("LexicalToolConfigPath(%q) = nil; want error", tc.input)
			}
		})
	}
}

func TestResolver_EnsureConfigDir_CreatesLayout(t *testing.T) {
	home := t.TempDir()
	r := mustResolver(t, home)

	if err := r.EnsureConfigDir(); err != nil {
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
