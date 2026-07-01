// Package claudecode: drift-detection helpers (E5-S4).
//
// These helpers are private to the adapter and support the read-side
// drift check surfaced through Project's EffectiveView. They are
// intentionally split from project.go so a grep for "drift" lands on
// this file first — the Codex adapter carries a symmetric drift.go.
//
// Read/write policy. sha256Hex is pure over its input bytes.
// loadLastApplied performs a single state.yaml read via
// storage.FileStorage.LoadState; any error (nil resolver, unreadable
// state, missing tool entry) is swallowed and reported as (zero,
// false). Drift is informational only — an unreadable state.yaml must
// never break `current` / `explain`, and it must never trigger a
// write.
//
// recordApplied is the write-side counterpart used by Apply. It loads
// the current state, upserts the LastApplied entry for the given tool,
// and saves back through the same FileStorage. All I/O goes through
// storage.FileStorage so the atomic-write / bootstrap invariants are
// preserved.

package claudecode

import (
	"crypto/sha256"
	"encoding/hex"
	"time"

	"github.com/a2d2-dev/claudecm/internal/config"
	"github.com/a2d2-dev/claudecm/internal/storage"
)

// sha256Hex returns the lowercase hex-encoded SHA-256 digest of data.
// Kept as a one-liner helper so both the drift-detection read path and
// the Apply state-update write path hash identically.
func sha256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// loadLastApplied fetches the LastApplied entry for (tool, filePath)
// from state.yaml. Returns (zero, false) on any error or missing entry
// — callers must treat that as "no prior state," which suppresses
// drift reporting per the E5-S4 AC edge case.
func loadLastApplied(r *storage.Resolver, tool config.ToolID, filePath string) (config.LastApplied, bool) {
	if r == nil {
		return config.LastApplied{}, false
	}
	fs := storage.NewFileStorage(r)
	state, err := fs.LoadState()
	if err != nil || state == nil {
		return config.LastApplied{}, false
	}
	return state.GetLastApplied(tool, filePath)
}

// recordAppliedToState is the write-side counterpart of loadLastApplied.
// Called after a successful writepath.Apply to persist the (path,
// SHA256, timestamp) tuple state.yaml uses for drift comparison on the
// next Project. Returns any error from state load/save so Apply can
// surface a state-persistence failure to the caller (which is what
// distinguishes "the write to the owned file succeeded" from "we
// forgot to update our own bookkeeping").
//
// The state read tolerates a missing state.yaml by falling back to a
// fresh NewState — that matches FileStorage.LoadState's contract and
// keeps a fresh install from erroring out on its first switch.
func recordAppliedToState(r *storage.Resolver, tool config.ToolID, filePath, sha string, appliedAt time.Time) error {
	fs := storage.NewFileStorage(r)
	state, err := fs.LoadState()
	if err != nil {
		return err
	}
	if state == nil {
		state = config.NewState()
	}
	state.RecordApplied(tool, filePath, sha, appliedAt)
	return fs.SaveState(state)
}
