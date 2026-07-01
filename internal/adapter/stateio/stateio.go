// Package stateio consolidates the read/write helpers every adapter
// needs to keep State.LastAppliedPerTool honest for external drift
// detection (E5-S4).
//
// Before this package existed, claudecode/drift.go and codex/drift.go
// each carried byte-identical sha256Hex + loadLastApplied +
// recordAppliedToState triplets — a hand-copy pattern the PR #40 review
// (F6) flagged as future-drift bait. Every future policy tweak (a new
// hash algorithm, a state-lock retry knob, a lastApplied-anchoring rule)
// would have had to land twice, in lock-step. That is now a single site.
//
// State-write ordering (F1)
// =========================
//
// RecordApplied performs a read-modify-write against ~/.claudecm/state.yaml.
// Two adapter.Apply calls that fire concurrently (Codex's two-file plan
// runs auth.json and config.toml through the same state file; E7's
// commit orchestrator will fan out both tools in parallel) can race:
//
//	adapter A: LoadState → mutate in-memory copy
//	adapter B: LoadState → mutate in-memory copy   (does not see A yet)
//	adapter A: SaveState                            (persists A's copy)
//	adapter B: SaveState                            (overwrites A's entry)
//
// The window is small but not zero, and the failure mode is silent state
// corruption: one of the two Apply calls' LastApplied entries disappears,
// and the next Project reports a false drift on that owned file. F1 in
// the E5-S4 review was exactly this pattern.
//
// RecordApplied fixes it by taking a storage.WithLock on state.yaml
// itself for the load → mutate → save critical section. Any second
// caller blocks in Acquire until the first Release fires; every entry
// survives regardless of concurrency.
//
// The lock target is HOME-relative (".claudecm/state.yaml") because
// storage.Acquire refuses absolute paths (see storage/lock.go). The
// sidecar file lives next to state.yaml inside the ~/.claudecm/ dir
// created by storage.Bootstrap; RecordApplied does not create HOME
// layout itself — the shared adapter contract is "Bootstrap has run"
// (the underlying FileStorage.SaveState asserts it).
//
// Drift-check contract
// ====================
//
// LoadLastApplied is READ-ONLY informational — it never writes. Errors
// bubble up so callers can decide policy, but the DriftForFile
// convenience swallows them (an unreadable state.yaml must never turn
// into a drift alarm and must never break `current` / `explain`).
package stateio

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/a2d2-dev/claudecm/internal/config"
	"github.com/a2d2-dev/claudecm/internal/storage"
)

// stateLockTimeout is the flock timeout for the state.yaml
// read-modify-write critical section. Kept short so a stuck adapter
// surfaces as ErrLockTimeout instead of hanging Apply indefinitely.
// The state-file write itself is a few KB of YAML; the practical hold
// time is sub-millisecond, so 5 seconds is generous.
const stateLockTimeout = 5 * time.Second

// stateLockRelTarget is the HOME-relative path to state.yaml used as
// the flock target. storage.Acquire refuses absolute paths. The literal
// mirrors storage.ConfigDirName / storage.StateFileName; kept as a
// package-level string so any future rename lands in one spot.
var stateLockRelTarget = filepath.Join(storage.ConfigDirName, storage.StateFileName)

// Sha256Hex returns the lowercase hex-encoded SHA-256 digest of data.
// Kept in one place so every adapter that hashes a file for drift or
// state anchoring uses the same algorithm and the same encoding —
// mismatches would silently manifest as false drift alarms.
func Sha256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// LoadLastApplied fetches the LastApplied entry for (tool, filePath)
// from state.yaml. Signature:
//
//		(entry, present, err)
//
//	  - (zero, false, nil) when the state file is present but carries no
//	    entry for this (tool, path) pair, OR when the resolver is nil.
//	  - (entry, true, nil) on a hit.
//	  - (zero, false, err) on an I/O or parse error from state load.
//
// A nil resolver is treated as "no state" rather than panicking so an
// early Project call from a partially-constructed environment fails
// gracefully — the read-side contract is informational.
//
// Unlike the pre-F6 adapter-local helpers (which swallowed the error and
// returned a two-tuple), this signature surfaces the error so callers
// that DO care (a future `cmd/state doctor`) can differentiate "no prior
// state" from "state file corrupt". The DriftForFile convenience below
// swallows the error to preserve the informational contract for the
// read path most adapters actually use.
func LoadLastApplied(r *storage.Resolver, tool config.ToolID, filePath string) (config.LastApplied, bool, error) {
	if r == nil {
		return config.LastApplied{}, false, nil
	}
	fs := storage.NewFileStorage(r)
	state, err := fs.LoadState()
	if err != nil {
		return config.LastApplied{}, false, err
	}
	// LoadState returns a fresh config.NewState() on ENOENT and a
	// non-nil *State on success; a nil state alongside a nil error is
	// impossible per its contract, so we do not carry a defensive
	// state == nil guard here (it would only exist as an uncovered
	// dead branch).
	entry, ok := state.GetLastApplied(tool, filePath)
	return entry, ok, nil
}

// RecordApplied persists a (path, SHA256, appliedAt) tuple into
// State.LastAppliedPerTool[tool][filePath], serialised through an
// exclusive flock on ~/.claudecm/state.yaml so concurrent Apply calls
// from different adapters (or from Codex's two-file plan) do not race
// on the read-modify-write. See file godoc "State-write ordering (F1)".
//
// A missing state.yaml is tolerated by falling back to a fresh
// NewState — that matches storage.FileStorage.LoadState's contract and
// keeps a fresh install from erroring out on its first switch.
//
// Errors from Acquire, LoadState, or SaveState bubble up so an operator
// sees "write succeeded but state.yaml update failed" as a distinct
// condition. Silently swallowing would leave the drift detector in a
// permanent false-positive state after the next external edit.
func RecordApplied(r *storage.Resolver, tool config.ToolID, filePath, sha256 string, appliedAt time.Time) error {
	if r == nil {
		return errors.New("stateio: RecordApplied: resolver is nil")
	}
	fs := storage.NewFileStorage(r)
	return storage.WithLock(r, stateLockRelTarget, storage.LockOptions{Timeout: stateLockTimeout}, func() error {
		state, err := fs.LoadState()
		if err != nil {
			return fmt.Errorf("stateio: load state: %w", err)
		}
		// LoadState returns config.NewState() on a missing file, so
		// state is never nil when err is nil. No defensive guard here.
		state.RecordApplied(tool, filePath, sha256, appliedAt)
		if err := fs.SaveState(state); err != nil {
			return fmt.Errorf("stateio: save state: %w", err)
		}
		return nil
	})
}

// DriftForFile checks a single owned file for external drift. Returns
// true iff (a) state.yaml records a prior Apply for this (tool, path)
// AND (b) the file is present on disk AND (c) the current on-disk
// SHA256 differs from the recorded SHA256.
//
// Absent file or absent state entry → false (the E5-S4 AC "no prior
// state → no drift report" edge case). Any read error is swallowed and
// reported as false: drift is informational, and a broken state.yaml
// or a transiently-unreadable owned file must never surface as a drift
// alarm.
//
// Both adapter Project methods share this helper so the drift test
// battery only has to cover one implementation.
func DriftForFile(r *storage.Resolver, tool config.ToolID, filePath string) bool {
	last, ok, err := LoadLastApplied(r, tool, filePath)
	if err != nil || !ok {
		return false
	}
	raw, err := os.ReadFile(filePath)
	if err != nil {
		// A file that was present at projection time but is unreadable
		// here (dangling symlink, permission race, unlinked between the
		// layer-chain read and this hash) is NOT drift — the layer
		// chain already handled the presence bit. Any error → false.
		return false
	}
	return Sha256Hex(raw) != last.SHA256
}
