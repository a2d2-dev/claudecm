package config

import "time"

// State tracks the currently active configuration profile and, per
// (tool, file), the SHA256 of the last successfully activated on-disk
// bytes. The SHA256 slot powers external drift detection: Project
// re-hashes each owned file at read time and compares against the
// recorded SHA256, so hand-edits outside claudecm surface as a
// warning (architecture §6.2, PRD FR-6/FR-7, story E5-S4).
//
// Schema-evolution rule. LastAppliedPerTool was added post-v1-bootstrap.
// A state.yaml that predates the field decodes into a nil map — that is
// normal absence, not a migration error. Nil-map reads are safe (Go's
// zero-value map semantics); writes go through RecordApplied, which
// lazily allocates the map on the first upsert. NO fallback rewrite of
// old state.yaml on load: we do not silently upgrade files (see
// CLAUDE.md "no fallback").
type State struct {
	// Version is the state file format version (for future compatibility)
	Version string `yaml:"version"`

	// CurrentProfile is the name of the active profile
	CurrentProfile string `yaml:"current_profile"`

	// LastSwitched is when the active profile was set
	LastSwitched time.Time `yaml:"last_switched"`

	// LastAppliedPerTool records, per (ToolID, absolute file path), the
	// LastApplied entry from the most recent successful Apply that
	// wrote that file. Consumed by Adapter.Project's drift check
	// (E5-S4).
	//
	// The inner map is keyed by absolute file path because a single
	// tool can own multiple files (each tracked independently). Each
	// adapter contributes an entry per owned file after a successful
	// Apply.
	//
	// Absent entry (outer key missing, inner key missing, or the entire
	// map nil) = no prior applied state for that (tool, file) → Project
	// MUST NOT report drift (the E5-S4 AC "no prior state → no drift
	// report" edge case). Present entry with matching SHA256 = clean;
	// mismatch = external drift, surfaced informationally in
	// EffectiveView.
	LastAppliedPerTool map[ToolID]map[string]LastApplied `yaml:"last_applied_per_tool,omitempty"`
}

// LastApplied captures the (path, SHA256, when) tuple claudecm records
// after a successful writepath.Apply. All three fields are required for
// drift detection: FilePath is redundant with the map key but kept for
// self-describing round-trips through state.yaml; SHA256 is the
// byte-identity anchor; AppliedAt is diagnostic (surfaced by
// cmd/explain to answer "when did we last touch this file?").
type LastApplied struct {
	// FilePath is the absolute on-disk path claudecm wrote. Constructed
	// only through internal/storage/paths.go (coding-standards rule 3).
	// Redundant with the map key it lives under, but keeping it here
	// means an individual LastApplied value is self-describing when
	// passed by value.
	FilePath string `yaml:"file_path"`

	// SHA256 is the hex-encoded SHA-256 digest of the exact bytes
	// writepath.Apply committed to disk (post-rename). The digest is
	// computed by the caller (Apply integration) from the same bytes
	// the write-path emitted, so drift check has a stable anchor even
	// if the file's mtime changes for benign reasons (e.g. rsync).
	SHA256 string `yaml:"sha256"`

	// AppliedAt is when the successful Apply completed. Diagnostic only;
	// drift detection depends on SHA256 alone.
	AppliedAt time.Time `yaml:"applied_at"`
}

// NewState creates a new State with default values
func NewState() *State {
	return &State{
		Version:      "1.0",
		LastSwitched: time.Now(),
	}
}

// SetCurrentProfile updates the current profile and timestamp
func (s *State) SetCurrentProfile(profileName string) {
	s.CurrentProfile = profileName
	s.LastSwitched = time.Now()
}

// HasActiveProfile returns true if there is an active profile set
func (s *State) HasActiveProfile() bool {
	return s.CurrentProfile != ""
}

// RecordApplied upserts the last-applied record for (tool, filePath).
// It lazily allocates the outer map (and its per-tool inner map) on
// the first call so callers never have to nil-check before writing.
// Mutates the receiver.
//
// Idempotency. Subsequent calls with the same (tool, filePath)
// overwrite the prior entry — this is the intended semantic: "the
// LAST successful Apply is what Project compares against." A stale
// record from a previous Apply that has since been superseded would
// produce a false-positive drift.
func (s *State) RecordApplied(tool ToolID, filePath, sha256 string, appliedAt time.Time) {
	if s.LastAppliedPerTool == nil {
		s.LastAppliedPerTool = make(map[ToolID]map[string]LastApplied)
	}
	inner, ok := s.LastAppliedPerTool[tool]
	if !ok || inner == nil {
		inner = make(map[string]LastApplied)
		s.LastAppliedPerTool[tool] = inner
	}
	inner[filePath] = LastApplied{
		FilePath:  filePath,
		SHA256:    sha256,
		AppliedAt: appliedAt,
	}
}

// GetLastApplied returns the recorded LastApplied entry for the
// (tool, filePath) pair. The bool is false when no entry exists — the
// "no prior applied state" case Project uses to suppress drift
// warnings. A nil LastAppliedPerTool map (or nil inner map) is treated
// as "no entries" (Go's zero-value map read contract makes this safe).
func (s *State) GetLastApplied(tool ToolID, filePath string) (LastApplied, bool) {
	if s.LastAppliedPerTool == nil {
		return LastApplied{}, false
	}
	inner, ok := s.LastAppliedPerTool[tool]
	if !ok || inner == nil {
		return LastApplied{}, false
	}
	entry, ok := inner[filePath]
	return entry, ok
}
