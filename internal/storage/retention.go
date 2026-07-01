package storage

// retention.go is the FR-5 backup-retention primitive. Prune enforces the
// NFR-R1 "keep the last N=10 per (tool, file)" policy against the on-disk
// state produced by Backup / ListBackups (E1-S4), and records every removal
// to the append-only audit log at ~/.claudecm/audit.log so operators can
// answer "why did that snapshot disappear?" without spelunking through the
// filesystem.
//
// Design choices (per docs/architecture/coding-standards.md and story E1-S5):
//
//   - Pure primitive over ListBackups. Retention operates on the record slice
//     ListBackups already returns — no independent readdir, no independent
//     path-safety logic. The single source of truth for "what is a backup of
//     (tool, basename)" stays in backup.go; retention.go is a policy layer
//     that trims that list. This mirrors the AtomicWrite / Backup separation:
//     each primitive does exactly one thing and composes cleanly.
//
//   - No package-level mutable state (coding-standards rule 12). The audit
//     write is expressed as a plain function under `//go:build !test`; a
//     companion file under `//go:build test` redeclares it as a var and
//     exports SetAuditAppendForTest so the audit-failure test can inject a
//     synthetic error. Production binaries have no swap point. Pattern
//     mirrors atomic_syncfunc.go and backup_clock.go.
//
//   - Audit before "any further" destruction on failure. If the audit write
//     for the current victim fails, we stop the loop immediately and return
//     the partial record slice plus the wrapped error. Rationale (per story
//     brief): "if we can't audit, we don't destroy evidence" — the loop
//     stops before touching the next backup. The file whose audit failed
//     has already been os.Remove'd (audit happens *after* removal so the
//     "removed-at" timestamp is trustworthy), so it goes into the partial
//     list; anything downstream of it is left intact on disk.
//
//   - Refuse non-regular targets on the remove step. os.Remove will happily
//     unlink a symlink or (with rmdir semantics elsewhere) a dir; we call
//     os.Lstat and require Mode().IsRegular() before removing. ListBackups
//     already skips directories in its enumeration, so this branch is
//     defense-in-depth against a future change to that filter and against
//     symlinks planted under backups/<tool>/. Non-regular hits refuse with
//     a wrapped error and DO NOT emit an audit line (there is nothing to
//     audit — no removal happened).
//
//   - Audit log path escape check. The audit path itself is symlink-escape
//     verified before every append (a symlink at ~/.claudecm/audit.log
//     pointing outside HOME must not be honored — same defense as
//     AtomicWrite's parent-dir EvalSymlinks check).
//
//   - Silent success path. No stderr / fmt.Println. Callers get a
//     ([]PruneRecord, error) pair; a nil error and empty slice mean
//     "nothing to prune". This is a primitive, not a UI.

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// DefaultBackupRetention is the NFR-R1 "keep the last N" bound: ten backups
// per (tool, file) tuple. PruneOptions.Keep <= 0 substitutes this value.
const DefaultBackupRetention = 10

// pruneReasonOverLimit is the only reason code v1 emits. It is a typed
// constant so future reasons (age-based, disk-full, operator-request)
// compose cleanly without changing the audit-line schema.
const pruneReasonOverLimit = "over-limit"

// PruneRecord is the receipt for a single removed backup. One entry per
// audit line; the field order matches the tab-separated on-disk format so
// callers doing a round-trip parse see the same shape.
type PruneRecord struct {
	Tool        string    // matches BackupRecord.Tool
	Basename    string    // matches BackupRecord.Basename
	RemovedPath string    // absolute path that was os.Remove'd
	RemovedAt   time.Time // UTC, captured immediately before the audit append
	Reason      string    // "over-limit" in v1; typed for future extension
}

// PruneOptions carries the per-call retention knob. Keeping it a struct
// leaves room for future overrides (min-age, dry-run) without breaking
// the signature.
type PruneOptions struct {
	// Keep is the retention target for this call. If <= 0, DefaultBackupRetention
	// is used. If Keep >= len(records) the function is a no-op.
	Keep int
}

// Prune enforces the retention policy for a single (tool, basename) pair.
//
// Steps:
//  1. Validate resolver + path segments.
//  2. Resolve the effective Keep bound (opts.Keep or DefaultBackupRetention).
//  3. ListBackups(tool, basename) — this already applies path-safety and the
//     symlink-escape check on the tool dir. Newest-first ordering is inherited.
//  4. If len(records) <= keep, return (nil, nil) without opening the audit
//     log — no I/O when there is nothing to do.
//  5. Otherwise, for each victim (records[keep:]):
//     a. Lstat the backup path; refuse if it is not a regular file (no audit
//     line — nothing was destroyed).
//     b. os.Remove the file.
//     c. Append one audit line to ~/.claudecm/audit.log. Audit-log path is
//     symlink-escape checked before the append, and the audit-log parent
//     dir is EnsureDir'd (mode 0700) if missing. If the audit write fails,
//     stop the loop and return the partial record slice + wrapped error.
//
// On any per-victim error the loop stops and Prune returns
// (partial records, wrapped error) — no fallback writes, no continue-past-error.
func Prune(r *Resolver, tool, basename string, opts PruneOptions) ([]PruneRecord, error) {
	if r == nil {
		return nil, errors.New("prune: resolver is nil")
	}
	if err := validatePathSegment(tool, "tool"); err != nil {
		return nil, fmt.Errorf("prune: %w", err)
	}
	if err := validatePathSegment(basename, "basename"); err != nil {
		return nil, fmt.Errorf("prune: %w", err)
	}

	keep := opts.Keep
	if keep <= 0 {
		keep = DefaultBackupRetention
	}

	records, err := ListBackups(r, tool, basename)
	if err != nil {
		return nil, fmt.Errorf("prune: %w", err)
	}
	if len(records) <= keep {
		// Nothing to prune. Deliberately do not open the audit log — story
		// spec: "only append when at least one removal occurs".
		return nil, nil
	}

	// Newest-first from ListBackups: keep the first `keep`, remove the rest.
	victims := records[keep:]

	auditPath := r.AuditLogPath()
	// Defense in depth: audit log parent (~/.claudecm/) must resolve under HOME.
	// If a symlink at ConfigDir points outside HOME we refuse. This is
	// checkUnderHome — the same EvalSymlinks + Rel dance AtomicWrite uses.
	auditParent := filepath.Dir(auditPath)
	if err := EnsureDir(r, auditParent); err != nil {
		return nil, fmt.Errorf("prune: %w", err)
	}
	// Once EnsureDir succeeded the parent exists; now re-check the audit
	// path itself. Two distinct escape modes to catch:
	//   (a) the parent is fine but audit.log is a symlink pointing outside
	//       HOME (checkUnderHome catches this because it EvalSymlinks the
	//       full path).
	//   (b) audit.log does not exist yet — allowed; the append will create it.
	if _, err := os.Lstat(auditPath); err == nil {
		if _, err := checkUnderHome(r, auditPath); err != nil {
			return nil, fmt.Errorf("prune: audit log %q: %w", auditPath, err)
		}
	} else if !os.IsNotExist(err) {
		return nil, fmt.Errorf("prune: lstat audit %q: %w", auditPath, err)
	}

	removed := make([]PruneRecord, 0, len(victims))
	for _, victim := range victims {
		info, err := os.Lstat(victim.BackupPath)
		if err != nil {
			if os.IsNotExist(err) {
				// Race: someone deleted the file between ListBackups and
				// Prune. Skip silently — no audit line, no error. This is
				// benign under retention semantics (we wanted it gone).
				continue
			}
			return removed, fmt.Errorf("prune: lstat %q: %w", victim.BackupPath, err)
		}
		if !info.Mode().IsRegular() {
			// Refuse to remove non-regular entries (symlinks, dirs, devices).
			// No audit line — nothing was destroyed. Loop stops so the caller
			// sees the error.
			return removed, fmt.Errorf("prune: refuse to remove non-regular entry %q (mode=%s)",
				victim.BackupPath, info.Mode())
		}

		if err := os.Remove(victim.BackupPath); err != nil {
			if os.IsNotExist(err) {
				// Race: a concurrent pruner (or an operator) removed the
				// file between our Lstat and our os.Remove. The victim is
				// already gone, which is exactly the state we wanted, so
				// skip it silently — no audit line (nothing was destroyed
				// by *us*, and the earlier remover is responsible for
				// their own audit trail if they had one).
				continue
			}
			return removed, fmt.Errorf("prune: remove %q: %w", victim.BackupPath, err)
		}

		now := time.Now().UTC()
		pr := PruneRecord{
			Tool:        tool,
			Basename:    basename,
			RemovedPath: victim.BackupPath,
			RemovedAt:   now,
			Reason:      pruneReasonOverLimit,
		}
		line := formatAuditLine(pr)
		if err := auditAppend(auditPath, []byte(line)); err != nil {
			// Story spec: "if we can't audit, we don't destroy evidence".
			// The current victim is already removed (we os.Remove'd it above
			// so its RemovedAt timestamp is trustworthy) — record it in the
			// partial list, then stop before touching any further backup.
			removed = append(removed, pr)
			return removed, fmt.Errorf("prune: audit append %q: %w", auditPath, err)
		}
		removed = append(removed, pr)
	}
	return removed, nil
}

// PruneAll walks the backups root, discovers every (tool, basename) pair,
// and applies Prune to each. Convenience wrapper; not auto-invoked anywhere.
//
// Discovery rules:
//   - Only directories under BackupsRoot are considered tool dirs; foreign
//     files are ignored.
//   - Only entries under a tool dir whose name contains ".bak." are considered
//     candidate backups; the basename portion is the substring before the
//     first ".bak." occurrence. Iteration order is byte-sorted (deterministic
//     across runs) so the audit log ordering is stable.
//   - Duplicate basenames within a tool dir are dedup'd — Prune is called
//     once per unique (tool, basename).
//
// On the first per-pair Prune error the walk stops and returns the partial
// aggregate record slice + wrapped error, symmetric with Prune's own
// stop-on-error semantics.
func PruneAll(r *Resolver, opts PruneOptions) ([]PruneRecord, error) {
	if r == nil {
		return nil, errors.New("prune all: resolver is nil")
	}
	root := r.BackupsRoot()
	entries, err := os.ReadDir(root)
	if err != nil {
		if os.IsNotExist(err) {
			// No backups root yet: nothing to do. Do NOT auto-create it —
			// PruneAll is read-mostly except when it finds work.
			return nil, nil
		}
		return nil, fmt.Errorf("prune all: readdir %q: %w", root, err)
	}

	// Stable ordering: tool dirs byte-sorted, basenames byte-sorted. os.ReadDir
	// is already sorted on most platforms but we do not depend on that.
	toolNames := make([]string, 0, len(entries))
	for _, ent := range entries {
		if !ent.IsDir() {
			continue
		}
		toolNames = append(toolNames, ent.Name())
	}
	sort.Strings(toolNames)

	var all []PruneRecord
	for _, tool := range toolNames {
		if err := validatePathSegment(tool, "tool"); err != nil {
			// A tool dir whose name would be refused by BackupPath cannot be
			// operated on safely. Surface the refusal rather than silently
			// skipping — an operator planted this dir and should know it is
			// invisible to retention.
			return all, fmt.Errorf("prune all: %w", err)
		}
		toolDir := filepath.Join(root, tool)
		subs, err := os.ReadDir(toolDir)
		if err != nil {
			return all, fmt.Errorf("prune all: readdir %q: %w", toolDir, err)
		}
		names := make([]string, 0, len(subs))
		for _, sub := range subs {
			names = append(names, sub.Name())
		}
		sort.Strings(names)

		seen := make(map[string]struct{}, len(names))
		for _, name := range names {
			idx := strings.Index(name, ".bak.")
			if idx <= 0 {
				continue
			}
			basename := name[:idx]
			if _, dup := seen[basename]; dup {
				continue
			}
			seen[basename] = struct{}{}
			// validatePathSegment on basename before handing it to Prune. A
			// foreign file whose derived basename would fail Prune's guard
			// is ignored; retention should not fail hard because someone
			// dropped an oddly-named file in backups/<tool>/.
			if err := validatePathSegment(basename, "basename"); err != nil {
				continue
			}
			recs, err := Prune(r, tool, basename, opts)
			all = append(all, recs...)
			if err != nil {
				return all, err
			}
		}
	}
	return all, nil
}

// formatAuditLine renders one audit-log entry per the schema documented in
// docs/architecture/coding-standards.md rule 10:
//
//	<RFC3339Nano UTC timestamp>\t<tool>\t<basename>\t<removed-path>\t<reason>\n
//
// Fields are tab-separated so the format is trivially splittable back into
// a PruneRecord for tests. RemovedAt is rendered as RFC3339Nano so the
// original nanosecond resolution survives a round-trip through time.Parse.
func formatAuditLine(pr PruneRecord) string {
	return fmt.Sprintf("%s\t%s\t%s\t%s\t%s\n",
		pr.RemovedAt.UTC().Format(time.RFC3339Nano),
		pr.Tool, pr.Basename, pr.RemovedPath, pr.Reason)
}
