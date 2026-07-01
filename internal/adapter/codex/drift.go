// Package codex: drift-detection helpers (E5-S4).
//
// These helpers are private to the adapter and support the read-side
// drift check surfaced through Project's EffectiveView plus the
// write-side state update triggered by Apply. Split from project.go /
// adapter.go so a grep for "drift" lands here first — the Claude Code
// adapter carries a symmetric drift.go.
//
// Two-file scope. Unlike the Claude Code adapter (single owned file),
// Codex owns TWO owned files (auth.json + config.toml) and each can
// drift independently. The drift check is invoked once per file and
// each Apply call records state for its own file only, so a full
// switch that touches both files leaves State with two entries under
// the ToolCodex outer key — one per file path.
//
// Read/write policy. sha256Hex is pure over its input bytes.
// loadLastApplied performs a single state.yaml read via
// storage.FileStorage.LoadState per call; any error (nil resolver,
// unreadable state, missing entry) is swallowed and reported as
// (zero, false). Drift is informational only — an unreadable
// state.yaml must never break `current` / `explain`, and it must
// never trigger a write. recordAppliedToState is the write-side
// counterpart used by Apply.

package codex

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
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
// surface a state-persistence failure to the caller.
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

// driftForFile checks a single owned file for external drift. Returns
// true iff (a) the file is present on disk AND (b) state.yaml records
// a prior Apply for this (tool, path) AND (c) the current on-disk
// SHA256 differs from the recorded SHA256.
//
// Absent file or absent state entry → false (the "no prior state → no
// drift report" AC edge case). Read errors are swallowed and reported
// as false, matching the read-only informational contract: a broken
// state.yaml / missing file must not surface as a drift alarm.
func driftForFile(r *storage.Resolver, tool config.ToolID, filePath string) bool {
	last, ok := loadLastApplied(r, tool, filePath)
	if !ok {
		return false
	}
	raw, err := os.ReadFile(filePath)
	if err != nil {
		// A file present-at-projection-time that becomes unreadable
		// between the layer-chain read and this hash is a race we do
		// not want to surface as drift — the layer chain already
		// handled the presence bit.
		if errors.Is(err, os.ErrNotExist) {
			return false
		}
		return false
	}
	return sha256Hex(raw) != last.SHA256
}
