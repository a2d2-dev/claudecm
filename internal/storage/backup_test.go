package storage

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// backupHome mirrors atomicHome: t.TempDir HOME, EnsureConfigDir'd, ready
// for Backup to write into backups/<tool>/. Kept small so each test can
// build its own sandbox without touching a real user HOME.
func backupHome(t *testing.T) (*Resolver, string) {
	t.Helper()
	home := t.TempDir()
	r := mustResolver(t, home)
	if err := r.EnsureConfigDir(); err != nil {
		t.Fatalf("EnsureConfigDir: %v", err)
	}
	return r, home
}

// writeSource drops fixture bytes at a HOME-relative path and returns the
// absolute path. Uses os.WriteFile directly — this is a test fixture, not a
// tool-config write, so the AtomicWrite ritual is not required here.
func writeSource(t *testing.T, home, homeRelPath string, data []byte) string {
	t.Helper()
	full := filepath.Join(home, homeRelPath)
	if err := os.MkdirAll(filepath.Dir(full), 0o700); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(full), err)
	}
	if err := os.WriteFile(full, data, 0o600); err != nil {
		t.Fatalf("writefile %s: %v", full, err)
	}
	return full
}

func TestBackup_HappyProducesReceiptAndCopy(t *testing.T) {
	r, home := backupHome(t)
	data := []byte(`{"env":{"KEY":"value"}}` + "\n")
	src := writeSource(t, home, ".claude/settings.json", data)

	rec, err := Backup(r, "claudecode", "settings.json", src)
	if err != nil {
		t.Fatalf("Backup = %v", err)
	}

	// Receipt fields match the fixture.
	if rec.Tool != "claudecode" || rec.Basename != "settings.json" || rec.SourcePath != src {
		t.Fatalf("BackupRecord identity fields = %+v; want tool=claudecode basename=settings.json source=%s",
			rec, src)
	}
	if rec.Timestamp.IsZero() {
		t.Fatalf("BackupRecord.Timestamp is zero")
	}
	if rec.Timestamp.Location() != time.UTC {
		t.Fatalf("BackupRecord.Timestamp = %v; want UTC location", rec.Timestamp)
	}
	if rec.Fingerprint.Size != int64(len(data)) {
		t.Fatalf("Fingerprint.Size = %d; want %d", rec.Fingerprint.Size, len(data))
	}
	want := sha256.Sum256(data)
	if rec.Fingerprint.SHA256 != hex.EncodeToString(want[:]) {
		t.Fatalf("Fingerprint.SHA256 = %q; want %q", rec.Fingerprint.SHA256, hex.EncodeToString(want[:]))
	}

	// BackupPath lives under backups/<tool>/ and starts with the expected
	// "<basename>.bak.<ts>" scheme.
	expectDir := filepath.Join(home, ConfigDirName, BackupsDirName, "claudecode")
	if got := filepath.Dir(rec.BackupPath); got != expectDir {
		t.Fatalf("BackupPath dir = %q; want %q", got, expectDir)
	}
	if base := filepath.Base(rec.BackupPath); !strings.HasPrefix(base, "settings.json.bak.") {
		t.Fatalf("BackupPath basename = %q; want prefix %q", base, "settings.json.bak.")
	}

	// The dst file exists with mode 0600 and byte-identical content.
	got, err := os.ReadFile(rec.BackupPath)
	if err != nil {
		t.Fatalf("readfile backup: %v", err)
	}
	if string(got) != string(data) {
		t.Fatalf("backup bytes = %q; want %q", got, data)
	}
	info, err := os.Lstat(rec.BackupPath)
	if err != nil {
		t.Fatalf("lstat backup: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("backup file mode = %o; want 0600", perm)
	}

	// The backups/<tool>/ dir is created with mode 0700.
	dirInfo, err := os.Lstat(expectDir)
	if err != nil {
		t.Fatalf("lstat %s: %v", expectDir, err)
	}
	if perm := dirInfo.Mode().Perm(); perm != 0o700 {
		t.Fatalf("backup dir mode = %o; want 0700", perm)
	}

	// Original file is untouched.
	orig, err := os.ReadFile(src)
	if err != nil {
		t.Fatalf("readfile src: %v", err)
	}
	if string(orig) != string(data) {
		t.Fatalf("source mutated after Backup: got %q want %q", orig, data)
	}
}

func TestBackup_MissingSourceReturnsErrNothingToBackup(t *testing.T) {
	r, home := backupHome(t)
	src := filepath.Join(home, ".claude", "does-not-exist.json")

	rec, err := Backup(r, "claudecode", "does-not-exist.json", src)
	if !errors.Is(err, ErrNothingToBackup) {
		t.Fatalf("Backup missing src = %v; want ErrNothingToBackup", err)
	}
	if rec != (BackupRecord{}) {
		t.Fatalf("BackupRecord = %+v; want zero value", rec)
	}

	// No file may have been created under backups/ (specifically, no tool
	// dir spring into existence just because we tried).
	backupsRoot := r.BackupsRoot()
	if _, err := os.Stat(filepath.Join(backupsRoot, "claudecode")); !os.IsNotExist(err) {
		t.Fatalf("claudecode backup dir created despite ErrNothingToBackup: %v", err)
	}
}

func TestBackup_SourceTooLargeIsRefused(t *testing.T) {
	r, home := backupHome(t)
	// Sparse-file trick: Truncate to MaxBackupSourceBytes+1 without actually
	// writing 32 MiB of bytes. Backup only inspects Size() before deciding
	// to refuse, so the disk cost stays negligible while the size check is
	// exercised faithfully.
	src := filepath.Join(home, "huge.bin")
	f, err := os.OpenFile(src, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatalf("create huge: %v", err)
	}
	if err := f.Truncate(MaxBackupSourceBytes + 1); err != nil {
		_ = f.Close()
		t.Fatalf("truncate huge: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close huge: %v", err)
	}

	rec, err := Backup(r, "claudecode", "huge.bin", src)
	if !errors.Is(err, ErrSourceTooLarge) {
		t.Fatalf("Backup huge = %v; want ErrSourceTooLarge", err)
	}
	if rec != (BackupRecord{}) {
		t.Fatalf("BackupRecord = %+v; want zero value", rec)
	}
	if _, err := os.Stat(filepath.Join(r.BackupsRoot(), "claudecode")); !os.IsNotExist(err) {
		t.Fatalf("claudecode backup dir created despite ErrSourceTooLarge: %v", err)
	}
}

func TestBackup_SymlinkPointingOutsideHomeIsRefused(t *testing.T) {
	r, home := backupHome(t)

	// A file living outside HOME that a symlink inside HOME points to. The
	// symlink is inside HOME so a naive prefix check would accept it;
	// checkUnderHome's EvalSymlinks + Rel dance is what catches it.
	outside := t.TempDir()
	victim := filepath.Join(outside, "secrets.json")
	if err := os.WriteFile(victim, []byte(`{"api_key":"leaked"}`), 0o600); err != nil {
		t.Fatalf("writefile outside: %v", err)
	}
	link := filepath.Join(home, "link-out.json")
	if err := os.Symlink(victim, link); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	rec, err := Backup(r, "claudecode", "link-out.json", link)
	if !errors.Is(err, ErrOutsideHome) {
		t.Fatalf("Backup outside-symlink = %v; want ErrOutsideHome", err)
	}
	if rec != (BackupRecord{}) {
		t.Fatalf("BackupRecord = %+v; want zero value", rec)
	}
	if _, err := os.Stat(filepath.Join(r.BackupsRoot(), "claudecode")); !os.IsNotExist(err) {
		t.Fatalf("claudecode backup dir created despite ErrOutsideHome: %v", err)
	}
}

func TestBackup_PreExistingDestinationSurfacesErrTargetExists(t *testing.T) {
	// The simple, no-clock-seam variant of the "same-nanosecond collision"
	// AC row: we call Backup once, then pre-create a placeholder file at
	// the exact path the receipt names, then attempt to write over it via
	// AtomicWrite with MustNotExist. This confirms the MustNotExist gate
	// is wired into Backup even when the collision is externally simulated.
	// The clock-seam variant that forces two real Backup calls into the
	// same nanosecond lives in backup_clockseam_test.go under
	// //go:build test.
	r, home := backupHome(t)
	data := []byte("hello")
	src := writeSource(t, home, ".claude/settings.json", data)

	rec, err := Backup(r, "claudecode", "settings.json", src)
	if err != nil {
		t.Fatalf("first Backup = %v", err)
	}

	// Now hand-invoke AtomicWrite at the same destination with MustNotExist=true.
	// This is the exact operation the second Backup call would perform if it
	// picked the same timestamp — but done deterministically without needing
	// to control the clock.
	_, err = AtomicWrite(r, rec.BackupPath, []byte("second"), AtomicWriteOptions{Mode: 0o600, MustNotExist: true})
	if !errors.Is(err, ErrTargetExists) {
		t.Fatalf("AtomicWrite second write = %v; want ErrTargetExists", err)
	}

	// Contents at the destination remain the first Backup's bytes.
	got, readErr := os.ReadFile(rec.BackupPath)
	if readErr != nil {
		t.Fatalf("readfile backup: %v", readErr)
	}
	if string(got) != string(data) {
		t.Fatalf("backup mutated by refused overwrite: got %q want %q", got, data)
	}
}

func TestBackup_PathSafetyRefusals(t *testing.T) {
	r, home := backupHome(t)
	src := writeSource(t, home, ".claude/settings.json", []byte("ok"))

	cases := []struct {
		name     string
		tool     string
		basename string
		src      string
	}{
		{"tool traversal", "../evil", "settings.json", src},
		{"tool with slash", "claude/../..", "settings.json", src},
		{"tool empty", "", "settings.json", src},
		{"basename traversal", "claudecode", "..", src},
		{"basename with slash", "claudecode", "/etc/passwd", src},
		{"basename empty", "claudecode", "", src},
		{"src empty", "claudecode", "settings.json", ""},
		{"src relative", "claudecode", "settings.json", "relative/path.json"},
		{"src with NUL", "claudecode", "settings.json", "/tmp/nul\x00path"},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			rec, err := Backup(r, tc.tool, tc.basename, tc.src)
			if err == nil {
				t.Fatalf("Backup(%q,%q,%q) = %+v, nil; want error", tc.tool, tc.basename, tc.src, rec)
			}
		})
	}
}

func TestListBackups_MissingToolDirReturnsEmpty(t *testing.T) {
	r, _ := backupHome(t)
	got, err := ListBackups(r, "claudecode", "settings.json")
	if err != nil {
		t.Fatalf("ListBackups on missing tool dir = %v; want nil", err)
	}
	if len(got) != 0 {
		t.Fatalf("ListBackups on missing tool dir = %+v; want empty", got)
	}
}

func TestListBackups_TwoBackupsSortedNewestFirst(t *testing.T) {
	r, home := backupHome(t)
	src := writeSource(t, home, ".claude/settings.json", []byte(`{"a":1}`))

	rec1, err := Backup(r, "claudecode", "settings.json", src)
	if err != nil {
		t.Fatalf("first Backup = %v", err)
	}
	// Bump the source so the second backup's hash differs. This makes the
	// per-record fingerprint distinguishable in the returned slice.
	if err := os.WriteFile(src, []byte(`{"a":2}`), 0o600); err != nil {
		t.Fatalf("rewrite src: %v", err)
	}
	// Ensure the second Backup lands on a strictly later timestamp than the
	// first, even on a coarse clock. 2 ms is well above any realistic
	// resolution and keeps this test independent of the clock seam.
	time.Sleep(2 * time.Millisecond)
	rec2, err := Backup(r, "claudecode", "settings.json", src)
	if err != nil {
		t.Fatalf("second Backup = %v", err)
	}
	if !rec2.Timestamp.After(rec1.Timestamp) {
		t.Fatalf("rec2.Timestamp %v not after rec1.Timestamp %v", rec2.Timestamp, rec1.Timestamp)
	}

	got, err := ListBackups(r, "claudecode", "settings.json")
	if err != nil {
		t.Fatalf("ListBackups = %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("ListBackups len = %d; want 2 (%+v)", len(got), got)
	}
	if got[0].BackupPath != rec2.BackupPath {
		t.Fatalf("ListBackups newest = %q; want %q", got[0].BackupPath, rec2.BackupPath)
	}
	if got[1].BackupPath != rec1.BackupPath {
		t.Fatalf("ListBackups oldest = %q; want %q", got[1].BackupPath, rec1.BackupPath)
	}
	// Timestamps round-trip through the filename layout.
	if !got[0].Timestamp.Equal(rec2.Timestamp) {
		t.Fatalf("newest Timestamp = %v; want %v", got[0].Timestamp, rec2.Timestamp)
	}
	if !got[1].Timestamp.Equal(rec1.Timestamp) {
		t.Fatalf("oldest Timestamp = %v; want %v", got[1].Timestamp, rec1.Timestamp)
	}
	// Fingerprint is computed on read; sizes should agree with the source
	// bytes that were live at each Backup call.
	if got[0].Fingerprint.Size != int64(len(`{"a":2}`)) {
		t.Fatalf("newest Fingerprint.Size = %d; want %d", got[0].Fingerprint.Size, len(`{"a":2}`))
	}
	if got[1].Fingerprint.Size != int64(len(`{"a":1}`)) {
		t.Fatalf("oldest Fingerprint.Size = %d; want %d", got[1].Fingerprint.Size, len(`{"a":1}`))
	}
}

func TestListBackups_PathSafetyRefusals(t *testing.T) {
	r, _ := backupHome(t)
	cases := []struct {
		name, tool, basename string
	}{
		{"tool traversal", "..", "settings.json"},
		{"tool with slash", "claudecode/..", "settings.json"},
		{"tool empty", "", "settings.json"},
		{"basename traversal", "claudecode", ".."},
		{"basename with slash", "claudecode", "/etc/passwd"},
		{"basename empty", "claudecode", ""},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			if _, err := ListBackups(r, tc.tool, tc.basename); err == nil {
				t.Fatalf("ListBackups(%q,%q) = nil; want error", tc.tool, tc.basename)
			}
		})
	}
}

func TestListBackups_IgnoresForeignEntries(t *testing.T) {
	r, home := backupHome(t)
	src := writeSource(t, home, ".claude/settings.json", []byte(`{"a":1}`))
	if _, err := Backup(r, "claudecode", "settings.json", src); err != nil {
		t.Fatalf("Backup = %v", err)
	}
	toolDir := filepath.Join(r.BackupsRoot(), "claudecode")
	// A stray file that matches the prefix but has a malformed timestamp
	// (retention would surface it in E1-S5; ListBackups quietly ignores it).
	if err := os.WriteFile(filepath.Join(toolDir, "settings.json.bak.NOT-A-TIMESTAMP"),
		[]byte("junk"), 0o600); err != nil {
		t.Fatalf("stray file: %v", err)
	}
	// A file with the wrong basename prefix.
	if err := os.WriteFile(filepath.Join(toolDir, "other.json.bak.20260701T000000Z000000000"),
		[]byte("junk"), 0o600); err != nil {
		t.Fatalf("wrong-prefix file: %v", err)
	}

	got, err := ListBackups(r, "claudecode", "settings.json")
	if err != nil {
		t.Fatalf("ListBackups = %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("ListBackups len = %d; want 1 (%+v)", len(got), got)
	}
}

func TestBackupTimestamp_RoundTrip(t *testing.T) {
	// Direct exercise of format/parse to lock the layout width. If a future
	// refactor breaks the 25-char invariant this test fails before ListBackups
	// starts silently skipping every entry.
	orig := time.Date(2026, 7, 1, 12, 34, 56, 123456789, time.UTC)
	s := formatBackupTimestamp(orig)
	if len(s) != 25 {
		t.Fatalf("formatBackupTimestamp len = %d; want 25 (%q)", len(s), s)
	}
	if s != "20260701T123456Z123456789" {
		t.Fatalf("formatBackupTimestamp = %q; want %q", s, "20260701T123456Z123456789")
	}
	parsed, err := parseBackupTimestamp(s)
	if err != nil {
		t.Fatalf("parseBackupTimestamp = %v", err)
	}
	if !parsed.Equal(orig) {
		t.Fatalf("round-trip mismatch: %v != %v", parsed, orig)
	}
}
