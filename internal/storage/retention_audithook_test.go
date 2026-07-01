//go:build test

package storage

import (
	"errors"
	"os"
	"strings"
	"testing"
)

// TestPrune_AuditWriteFailureAbortsFurtherRemovals exercises the story-spec
// "audit-log write failure aborts further removals" rule. We inject an
// auditAppend that returns nil for the first call and an error for every
// subsequent call, then run Prune against a seed of 15 backups with Keep=10
// (5 victims). We expect:
//
//   - The first victim is removed AND its audit line is written.
//   - The second victim is removed (removal precedes audit per the "audit
//     after remove so RemovedAt is trustworthy" rule), then the audit for
//     it fails, so Prune returns the partial slice [victim1, victim2]
//     plus a wrapped error carrying the sentinel.
//   - Victims 3, 4, 5 remain on disk.
//
// Compiled only with `-tags=test`; the seam is not present in production
// builds (coding-standards rule 12).
func TestPrune_AuditWriteFailureAbortsFurtherRemovals(t *testing.T) {
	r, _ := retentionHome(t)
	all := seedBackups(t, r, "claudecode", "settings.json", 15)

	sentinel := errors.New("synthetic audit-log write failure")
	prod := auditAppend
	calls := 0
	restore := SetAuditAppendForTest(func(path string, line []byte) error {
		calls++
		if calls == 1 {
			// Let the first append succeed via the production wiring so the
			// on-disk audit log has one legitimate line we can assert on.
			return prod(path, line)
		}
		return sentinel
	})
	defer restore()

	got, err := Prune(r, "claudecode", "settings.json", PruneOptions{Keep: 10})
	if err == nil {
		t.Fatalf("Prune under audit-fail hook = %+v, nil; want wrapped error", got)
	}
	if !errors.Is(err, sentinel) {
		t.Fatalf("Prune error chain = %v; want to contain sentinel %v", err, sentinel)
	}
	if !strings.Contains(err.Error(), "audit append") {
		t.Fatalf("Prune error message = %q; want 'audit append' context", err.Error())
	}

	// Partial list contains two records: victim 1 (audited) + victim 2
	// (removed but audit failed). The victims are the tail of the seed's
	// newest-first slice, so all[10] is victim 1 and all[11] is victim 2.
	if len(got) != 2 {
		t.Fatalf("Prune under audit-fail returned %d records; want 2 (got=%+v)", len(got), got)
	}
	if got[0].RemovedPath != all[10] {
		t.Errorf("record 0 RemovedPath = %q; want %q (victim 1)", got[0].RemovedPath, all[10])
	}
	if got[1].RemovedPath != all[11] {
		t.Errorf("record 1 RemovedPath = %q; want %q (victim 2)", got[1].RemovedPath, all[11])
	}

	// Both audited-or-removed victims are gone from disk.
	for i, want := range []string{all[10], all[11]} {
		if _, err := os.Lstat(want); !os.IsNotExist(err) {
			t.Errorf("victim %d %q still present after Prune: err=%v", i+1, want, err)
		}
	}
	// Victims 3, 4, 5 remain — the loop stopped before removing them.
	for i, want := range []string{all[12], all[13], all[14]} {
		if _, err := os.Lstat(want); err != nil {
			t.Errorf("victim %d %q missing (should have survived audit-fail abort): %v", i+3, want, err)
		}
	}

	// Audit log has exactly one line — the legitimate first append.
	lines, ok := readAuditLines(t, r)
	if !ok {
		t.Fatalf("audit log not created despite first successful append")
	}
	if len(lines) != 1 {
		t.Fatalf("audit lines = %d; want 1 (first append succeeded, second failed)", len(lines))
	}
	// Sanity: the surviving line references victim 1.
	if !strings.Contains(lines[0], all[10]) {
		t.Fatalf("audit line does not reference victim 1 %q: %q", all[10], lines[0])
	}
}
