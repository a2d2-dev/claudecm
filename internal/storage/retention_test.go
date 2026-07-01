package storage

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
)

// retentionHome is the retention-test analog of backupHome / atomicHome.
// It builds a Resolver rooted at t.TempDir with ConfigDir ensured so
// Prune can EnsureDir the audit-log parent without hitting a missing HOME.
func retentionHome(t *testing.T) (*Resolver, string) {
	t.Helper()
	home := t.TempDir()
	r := mustResolver(t, home)
	if err := r.EnsureConfigDir(); err != nil {
		t.Fatalf("EnsureConfigDir: %v", err)
	}
	return r, home
}

// seedBackups drops n synthetic backup files under `~/.claudecm/backups/<tool>/`
// with the canonical `<basename>.bak.<ts>` layout. Timestamps are strictly
// increasing (base + i*second) so the seeded files sort chronologically.
// The seam-free variant used by non-clock-injecting tests: we write the
// files directly rather than call Backup, because Backup would fsync and
// hash 15 fixture writes for every test and slow the suite down without
// exercising anything new — Backup is already covered by backup_test.go.
func seedBackups(t *testing.T, r *Resolver, tool, basename string, n int) []string {
	t.Helper()
	base := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	toolDir := filepath.Join(r.BackupsRoot(), tool)
	if err := EnsureDir(r, toolDir); err != nil {
		t.Fatalf("EnsureDir %s: %v", toolDir, err)
	}
	paths := make([]string, 0, n)
	for i := 0; i < n; i++ {
		ts := formatBackupTimestamp(base.Add(time.Duration(i) * time.Second))
		p := filepath.Join(toolDir, basename+".bak."+ts)
		if err := os.WriteFile(p, []byte(fmt.Sprintf("backup-%03d", i)), 0o600); err != nil {
			t.Fatalf("write %s: %v", p, err)
		}
		paths = append(paths, p)
	}
	// Return newest-first to match ListBackups ordering — the last-written
	// file (highest timestamp) is index 0 in the returned slice, so tests
	// can index the "should-be-kept" prefix with [0:keep] and the
	// "should-be-pruned" suffix with [keep:].
	sort.Sort(sort.Reverse(sort.StringSlice(paths)))
	return paths
}

// readAuditLines returns each newline-terminated line of the audit log,
// stripped of the trailing "\n". If the file does not exist the function
// returns (nil, false) so tests can assert both "no log" and "empty log"
// as distinct cases.
func readAuditLines(t *testing.T, r *Resolver) ([]string, bool) {
	t.Helper()
	b, err := os.ReadFile(r.AuditLogPath())
	if err != nil {
		if os.IsNotExist(err) {
			return nil, false
		}
		t.Fatalf("readfile audit: %v", err)
	}
	if len(b) == 0 {
		return nil, true
	}
	trimmed := strings.TrimRight(string(b), "\n")
	if trimmed == "" {
		return nil, true
	}
	return strings.Split(trimmed, "\n"), true
}

func TestPrune_HappyRemovesOldestAndKeepsN(t *testing.T) {
	r, _ := retentionHome(t)
	all := seedBackups(t, r, "claudecode", "settings.json", 15)

	// all is newest-first. Prune with Keep=10 should remove the oldest 5,
	// which are the LAST 5 in the newest-first slice.
	expectRemoved := append([]string{}, all[10:]...)
	expectKept := append([]string{}, all[:10]...)

	got, err := Prune(r, "claudecode", "settings.json", PruneOptions{Keep: 10})
	if err != nil {
		t.Fatalf("Prune = %v", err)
	}
	if len(got) != 5 {
		t.Fatalf("Prune removed %d records; want 5 (%+v)", len(got), got)
	}
	for _, rec := range got {
		if rec.Tool != "claudecode" {
			t.Errorf("PruneRecord.Tool = %q; want claudecode", rec.Tool)
		}
		if rec.Basename != "settings.json" {
			t.Errorf("PruneRecord.Basename = %q; want settings.json", rec.Basename)
		}
		if rec.Reason != "over-limit" {
			t.Errorf("PruneRecord.Reason = %q; want over-limit", rec.Reason)
		}
		if rec.RemovedAt.IsZero() || rec.RemovedAt.Location() != time.UTC {
			t.Errorf("PruneRecord.RemovedAt = %v; want non-zero UTC", rec.RemovedAt)
		}
	}
	// Kept files still on disk; removed files gone.
	for _, p := range expectKept {
		if _, err := os.Lstat(p); err != nil {
			t.Errorf("kept file %q missing after Prune: %v", p, err)
		}
	}
	for _, p := range expectRemoved {
		if _, err := os.Lstat(p); !os.IsNotExist(err) {
			t.Errorf("removed file %q still present after Prune: err=%v", p, err)
		}
	}

	// Audit log: 5 lines, mode 0600, RFC3339Nano parses, tab-separated.
	info, err := os.Lstat(r.AuditLogPath())
	if err != nil {
		t.Fatalf("lstat audit: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("audit mode = %o; want 0600", perm)
	}
	lines, ok := readAuditLines(t, r)
	if !ok {
		t.Fatalf("audit log not created after 5 removals")
	}
	if len(lines) != 5 {
		t.Fatalf("audit line count = %d; want 5", len(lines))
	}
	for i, line := range lines {
		fields := strings.Split(line, "\t")
		if len(fields) != 5 {
			t.Fatalf("audit line %d fields = %d; want 5 (line=%q)", i, len(fields), line)
		}
		ts, err := time.Parse(time.RFC3339Nano, fields[0])
		if err != nil {
			t.Fatalf("audit line %d timestamp parse: %v", i, err)
		}
		if ts.Location() != time.UTC {
			t.Errorf("audit line %d timestamp not UTC: %v", i, ts)
		}
		if fields[1] != "claudecode" {
			t.Errorf("audit line %d tool = %q; want claudecode", i, fields[1])
		}
		if fields[2] != "settings.json" {
			t.Errorf("audit line %d basename = %q; want settings.json", i, fields[2])
		}
		if fields[4] != "over-limit" {
			t.Errorf("audit line %d reason = %q; want over-limit", i, fields[4])
		}
	}
}

func TestPrune_KeepZeroUsesDefault(t *testing.T) {
	r, _ := retentionHome(t)
	seedBackups(t, r, "codex", "config.toml", 12)

	got, err := Prune(r, "codex", "config.toml", PruneOptions{Keep: 0})
	if err != nil {
		t.Fatalf("Prune keep=0 = %v", err)
	}
	// Keep=0 → default (10). 12 - 10 = 2 removed.
	if len(got) != 2 {
		t.Fatalf("Prune keep=0 removed %d; want 2 (default N=10)", len(got))
	}
	// Belt-and-braces: on-disk file count matches DefaultBackupRetention.
	remaining, err := ListBackups(r, "codex", "config.toml")
	if err != nil {
		t.Fatalf("ListBackups: %v", err)
	}
	if len(remaining) != DefaultBackupRetention {
		t.Fatalf("remaining = %d; want %d", len(remaining), DefaultBackupRetention)
	}
}

func TestPrune_UnderLimitIsNoOpNoAuditLogCreated(t *testing.T) {
	// Story-spec: "only append when at least one removal occurs. If nothing
	// is removed, do not open/create the audit log. Prefer minimal I/O."
	r, _ := retentionHome(t)
	seedBackups(t, r, "claudecode", "settings.json", 3)

	got, err := Prune(r, "claudecode", "settings.json", PruneOptions{Keep: 10})
	if err != nil {
		t.Fatalf("Prune under-limit = %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("Prune under-limit removed %d; want 0", len(got))
	}
	if _, err := os.Lstat(r.AuditLogPath()); !os.IsNotExist(err) {
		t.Fatalf("audit log created despite no removals: err=%v", err)
	}
}

func TestPrune_EmptyToolDirIsNoOp(t *testing.T) {
	r, _ := retentionHome(t)
	// Tool dir does not exist — ListBackups returns (nil, nil).
	got, err := Prune(r, "claudecode", "settings.json", PruneOptions{Keep: 10})
	if err != nil {
		t.Fatalf("Prune empty = %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("Prune empty removed %d; want 0", len(got))
	}
	if _, err := os.Lstat(r.AuditLogPath()); !os.IsNotExist(err) {
		t.Fatalf("audit log created despite no tool dir: err=%v", err)
	}
}

func TestPruneAll_MultipleToolsFanOut(t *testing.T) {
	r, _ := retentionHome(t)
	// Three tools, 12 backups each. Keep=10 → 2 removed per tool = 6 total.
	tools := []string{"claudecode", "codex", "gemini"}
	for _, tool := range tools {
		seedBackups(t, r, tool, "settings.json", 12)
	}

	got, err := PruneAll(r, PruneOptions{Keep: 10})
	if err != nil {
		t.Fatalf("PruneAll = %v", err)
	}
	if len(got) != 6 {
		t.Fatalf("PruneAll removed %d; want 6", len(got))
	}

	// Each tool retains exactly N=10.
	for _, tool := range tools {
		recs, err := ListBackups(r, tool, "settings.json")
		if err != nil {
			t.Fatalf("ListBackups %s: %v", tool, err)
		}
		if len(recs) != 10 {
			t.Fatalf("tool %s post-PruneAll count = %d; want 10", tool, len(recs))
		}
	}

	lines, ok := readAuditLines(t, r)
	if !ok {
		t.Fatalf("audit log not created")
	}
	if len(lines) != 6 {
		t.Fatalf("audit lines = %d; want 6", len(lines))
	}
	// Each tool must appear in the audit log exactly twice.
	counts := map[string]int{}
	for _, line := range lines {
		fields := strings.Split(line, "\t")
		if len(fields) != 5 {
			t.Fatalf("audit line fields = %d; want 5 (%q)", len(fields), line)
		}
		counts[fields[1]]++
	}
	for _, tool := range tools {
		if counts[tool] != 2 {
			t.Errorf("audit lines for tool %s = %d; want 2 (counts=%v)", tool, counts[tool], counts)
		}
	}
}

func TestPrune_PathSafetyRefusals(t *testing.T) {
	r, _ := retentionHome(t)
	cases := []struct {
		name, tool, basename string
	}{
		{"tool traversal", "../evil", "settings.json"},
		{"tool with slash", "claudecode/..", "settings.json"},
		{"tool empty", "", "settings.json"},
		{"tool with NUL", "claudecode\x00", "settings.json"},
		{"basename traversal", "claudecode", ".."},
		{"basename with slash", "claudecode", "/etc/passwd"},
		{"basename empty", "claudecode", ""},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			got, err := Prune(r, tc.tool, tc.basename, PruneOptions{Keep: 10})
			if err == nil {
				t.Fatalf("Prune(%q,%q) = %+v, nil; want error", tc.tool, tc.basename, got)
			}
			if got != nil {
				t.Errorf("Prune(%q,%q) returned records %+v; want nil", tc.tool, tc.basename, got)
			}
		})
	}
}

func TestPrune_RefusesSymlinkedToolDirOutsideHome(t *testing.T) {
	// Mirrors backup_test.go's TestListBackups_RefusesSymlinkedToolDirOutsideHome.
	// ListBackups already surfaces ErrOutsideHome; Prune must propagate it.
	r, _ := retentionHome(t)
	outside := t.TempDir()
	planted := filepath.Join(outside, "settings.json.bak.20260701T000000Z000000000")
	if err := os.WriteFile(planted, []byte("leaked"), 0o600); err != nil {
		t.Fatalf("writefile planted: %v", err)
	}
	backupsRoot := r.BackupsRoot()
	if err := os.MkdirAll(backupsRoot, 0o700); err != nil {
		t.Fatalf("mkdir backups root: %v", err)
	}
	link := filepath.Join(backupsRoot, "claudecode")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	got, err := Prune(r, "claudecode", "settings.json", PruneOptions{Keep: 1})
	if !errors.Is(err, ErrOutsideHome) {
		t.Fatalf("Prune symlinked tool dir = %v; want ErrOutsideHome", err)
	}
	if got != nil {
		t.Errorf("Prune symlinked tool dir returned records %+v; want nil", got)
	}
	// Planted file untouched (proof we never reached the enumeration).
	if _, err := os.Lstat(planted); err != nil {
		t.Errorf("planted file %q disturbed after refused Prune: %v", planted, err)
	}
}

func TestPrune_RefusesToRemoveNonFileEntries(t *testing.T) {
	// Plant a subdir whose name matches the "<basename>.bak.<ts>" shape.
	// ListBackups already skips directory entries (see backup.go line ~264),
	// so the directory should NOT appear in the record list; Prune should
	// therefore not touch it, and should NOT emit an audit line for it.
	// We seed enough regular backups alongside so Prune has real work.
	r, _ := retentionHome(t)
	seedBackups(t, r, "claudecode", "settings.json", 12)

	toolDir := filepath.Join(r.BackupsRoot(), "claudecode")
	plantedDir := filepath.Join(toolDir, "settings.json.bak.20260701T000000Z000000001")
	if err := os.MkdirAll(plantedDir, 0o700); err != nil {
		t.Fatalf("mkdir planted dir: %v", err)
	}

	got, err := Prune(r, "claudecode", "settings.json", PruneOptions{Keep: 10})
	if err != nil {
		t.Fatalf("Prune with planted dir = %v", err)
	}
	// 12 seeded regular files, Keep=10 → 2 removed. Planted dir untouched.
	if len(got) != 2 {
		t.Fatalf("Prune removed %d; want 2", len(got))
	}
	info, err := os.Lstat(plantedDir)
	if err != nil {
		t.Fatalf("planted dir vanished: %v", err)
	}
	if !info.IsDir() {
		t.Fatalf("planted entry mode = %s; want dir", info.Mode())
	}

	// Audit log has exactly 2 lines (no line for the planted dir).
	lines, ok := readAuditLines(t, r)
	if !ok {
		t.Fatalf("audit log not created")
	}
	if len(lines) != 2 {
		t.Fatalf("audit lines = %d; want 2", len(lines))
	}
	for _, line := range lines {
		if strings.Contains(line, plantedDir) {
			t.Fatalf("audit line references planted dir %q: %q", plantedDir, line)
		}
	}
}

func TestPrune_RefusesSymlinkedAuditLogOutsideHome(t *testing.T) {
	// A symlink at ~/.claudecm/audit.log pointing outside HOME must be
	// refused before any removal happens. The lexical guard would accept
	// the path itself (it's under HOME); checkUnderHome's EvalSymlinks +
	// Rel is what catches this.
	r, _ := retentionHome(t)
	seedBackups(t, r, "claudecode", "settings.json", 15)

	outside := t.TempDir()
	victim := filepath.Join(outside, "captured-audit.log")
	// Pre-create the file so the symlink target resolves.
	if err := os.WriteFile(victim, nil, 0o600); err != nil {
		t.Fatalf("writefile victim: %v", err)
	}
	if err := os.Symlink(victim, r.AuditLogPath()); err != nil {
		t.Fatalf("symlink audit: %v", err)
	}

	got, err := Prune(r, "claudecode", "settings.json", PruneOptions{Keep: 10})
	if !errors.Is(err, ErrOutsideHome) {
		t.Fatalf("Prune outside-symlink audit = %v; want ErrOutsideHome", err)
	}
	if len(got) != 0 {
		t.Fatalf("Prune returned %d records; want 0 before any removal", len(got))
	}
	// No backup file may have been removed.
	remaining, err := ListBackups(r, "claudecode", "settings.json")
	if err != nil {
		t.Fatalf("ListBackups post-refusal: %v", err)
	}
	if len(remaining) != 15 {
		t.Fatalf("ListBackups after refused Prune = %d; want 15", len(remaining))
	}
	// Victim file untouched (proof the append never fired).
	got2, err := os.ReadFile(victim)
	if err != nil {
		t.Fatalf("readfile victim: %v", err)
	}
	if len(got2) != 0 {
		t.Fatalf("victim file received bytes despite refusal: %q", got2)
	}
}

func TestPrune_AuditFormatRoundTrip(t *testing.T) {
	r, _ := retentionHome(t)
	all := seedBackups(t, r, "claudecode", "settings.json", 11)
	// Prune with Keep=10 → 1 removed. The lone line lets us round-trip
	// timestamp + fields without disambiguating multiple entries.
	got, err := Prune(r, "claudecode", "settings.json", PruneOptions{Keep: 10})
	if err != nil {
		t.Fatalf("Prune = %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("Prune removed %d; want 1", len(got))
	}
	lines, ok := readAuditLines(t, r)
	if !ok || len(lines) != 1 {
		t.Fatalf("audit lines ok=%v len=%d; want 1", ok, len(lines))
	}
	fields := strings.Split(lines[0], "\t")
	if len(fields) != 5 {
		t.Fatalf("field count = %d; want 5 (%q)", len(fields), lines[0])
	}
	ts, err := time.Parse(time.RFC3339Nano, fields[0])
	if err != nil {
		t.Fatalf("timestamp parse: %v", err)
	}
	if !ts.Equal(got[0].RemovedAt.UTC()) {
		t.Fatalf("audit timestamp = %v; want %v (from PruneRecord)", ts, got[0].RemovedAt.UTC())
	}
	if fields[1] != "claudecode" || fields[2] != "settings.json" {
		t.Fatalf("audit identity fields = %q/%q; want claudecode/settings.json", fields[1], fields[2])
	}
	if fields[3] != got[0].RemovedPath {
		t.Fatalf("audit removed-path = %q; want %q", fields[3], got[0].RemovedPath)
	}
	// The removed file is the oldest — the last entry in seed's newest-first slice.
	if fields[3] != all[10] {
		t.Fatalf("audit removed-path = %q; want oldest seed %q", fields[3], all[10])
	}
	if fields[4] != "over-limit" {
		t.Fatalf("audit reason = %q; want over-limit", fields[4])
	}
}
