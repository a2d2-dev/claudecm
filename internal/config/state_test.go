// state_test.go — E5-S4 tests for State's LastAppliedPerTool schema.
//
// The tests cover the four contract points RecordApplied + GetLastApplied
// promise (lazy allocation, upsert-overwrite, missing→false getter,
// YAML marshal round-trip). They deliberately do NOT touch storage —
// state.yaml I/O is exercised through the FileStorage tests in the
// storage package. Keeping this file purely in-memory means a
// state-schema regression fails here first without pulling in the full
// bootstrap dance.

package config

import (
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

func TestState_RecordAppliedNewEntry(t *testing.T) {
	s := NewState()
	// Guard: the fresh State has a nil LastAppliedPerTool. RecordApplied
	// must lazily allocate; callers do not pre-initialise.
	if s.LastAppliedPerTool != nil {
		t.Fatalf("NewState().LastAppliedPerTool = %v, want nil", s.LastAppliedPerTool)
	}
	when := time.Date(2026, 6, 30, 12, 0, 0, 0, time.UTC)
	s.RecordApplied(ToolClaudeCode, "/home/u/.claude/settings.json", "deadbeef", when)

	got, ok := s.GetLastApplied(ToolClaudeCode, "/home/u/.claude/settings.json")
	if !ok {
		t.Fatalf("GetLastApplied after RecordApplied: ok=false, want true")
	}
	if got.FilePath != "/home/u/.claude/settings.json" {
		t.Errorf("FilePath = %q, want /home/u/.claude/settings.json", got.FilePath)
	}
	if got.SHA256 != "deadbeef" {
		t.Errorf("SHA256 = %q, want deadbeef", got.SHA256)
	}
	if !got.AppliedAt.Equal(when) {
		t.Errorf("AppliedAt = %v, want %v", got.AppliedAt, when)
	}
}

func TestState_RecordAppliedOverwrites(t *testing.T) {
	s := NewState()
	t0 := time.Date(2026, 6, 30, 12, 0, 0, 0, time.UTC)
	t1 := t0.Add(time.Hour)
	s.RecordApplied(ToolClaudeCode, "/home/u/.claude/settings.json", "aaa", t0)
	s.RecordApplied(ToolClaudeCode, "/home/u/.claude/settings.json", "bbb", t1)

	got, ok := s.GetLastApplied(ToolClaudeCode, "/home/u/.claude/settings.json")
	if !ok {
		t.Fatalf("GetLastApplied after two RecordApplied calls: ok=false")
	}
	if got.SHA256 != "bbb" {
		t.Errorf("SHA256 = %q, want bbb (second call must overwrite)", got.SHA256)
	}
	if !got.AppliedAt.Equal(t1) {
		t.Errorf("AppliedAt = %v, want %v (second call must overwrite)", got.AppliedAt, t1)
	}
}

func TestState_RecordAppliedMultipleFilesSameToolIndependent(t *testing.T) {
	// The Codex two-file case: auth.json and config.toml under the same
	// ToolCodex outer key must not collide.
	s := NewState()
	when := time.Now()
	s.RecordApplied(ToolCodex, "/home/u/.codex/auth.json", "auth-sha", when)
	s.RecordApplied(ToolCodex, "/home/u/.codex/config.toml", "cfg-sha", when)

	if got, ok := s.GetLastApplied(ToolCodex, "/home/u/.codex/auth.json"); !ok || got.SHA256 != "auth-sha" {
		t.Errorf("auth.json entry not preserved: ok=%v sha=%q", ok, got.SHA256)
	}
	if got, ok := s.GetLastApplied(ToolCodex, "/home/u/.codex/config.toml"); !ok || got.SHA256 != "cfg-sha" {
		t.Errorf("config.toml entry not preserved: ok=%v sha=%q", ok, got.SHA256)
	}
}

func TestState_GetLastAppliedMissing(t *testing.T) {
	// Nil map → getter must return (zero, false), not panic. Absent
	// tool AND absent file both surface as false so callers can treat
	// "no prior applied state" uniformly.
	s := NewState()
	if _, ok := s.GetLastApplied(ToolClaudeCode, "/anywhere"); ok {
		t.Errorf("GetLastApplied on empty state = ok=true, want false")
	}
	s.RecordApplied(ToolClaudeCode, "/a", "sha", time.Now())
	if _, ok := s.GetLastApplied(ToolCodex, "/a"); ok {
		t.Errorf("GetLastApplied wrong tool = ok=true, want false")
	}
	if _, ok := s.GetLastApplied(ToolClaudeCode, "/b"); ok {
		t.Errorf("GetLastApplied wrong path = ok=true, want false")
	}
}

func TestState_MarshalRoundTrip(t *testing.T) {
	s := NewState()
	when := time.Date(2026, 6, 30, 12, 0, 0, 0, time.UTC)
	s.RecordApplied(ToolClaudeCode, "/home/u/.claude/settings.json", "cc-sha", when)
	s.RecordApplied(ToolCodex, "/home/u/.codex/auth.json", "auth-sha", when)
	s.RecordApplied(ToolCodex, "/home/u/.codex/config.toml", "cfg-sha", when)

	buf, err := yaml.Marshal(s)
	if err != nil {
		t.Fatalf("yaml.Marshal: %v", err)
	}
	var decoded State
	if err := yaml.Unmarshal(buf, &decoded); err != nil {
		t.Fatalf("yaml.Unmarshal: %v\nbuf:\n%s", err, buf)
	}
	for _, want := range []struct {
		tool ToolID
		file string
		sha  string
	}{
		{ToolClaudeCode, "/home/u/.claude/settings.json", "cc-sha"},
		{ToolCodex, "/home/u/.codex/auth.json", "auth-sha"},
		{ToolCodex, "/home/u/.codex/config.toml", "cfg-sha"},
	} {
		got, ok := decoded.GetLastApplied(want.tool, want.file)
		if !ok {
			t.Errorf("round-trip: missing entry for (%s, %s)", want.tool, want.file)
			continue
		}
		if got.SHA256 != want.sha {
			t.Errorf("round-trip: SHA256 for (%s, %s) = %q, want %q", want.tool, want.file, got.SHA256, want.sha)
		}
		if got.FilePath != want.file {
			t.Errorf("round-trip: FilePath for (%s, %s) = %q, want %q", want.tool, want.file, got.FilePath, want.file)
		}
	}
}

func TestState_LegacyStateYAMLDecodesToNilMap(t *testing.T) {
	// Schema-evolution guard: a state.yaml written by an older claudecm
	// (before LastAppliedPerTool existed) must decode into a State with
	// a nil map. Callers treat nil as "no prior applied state" and
	// suppress drift reports — this is the E5-S4 AC edge case.
	legacy := []byte("version: \"1.0\"\ncurrent_profile: alpha\nlast_switched: 2026-01-01T00:00:00Z\n")
	var s State
	if err := yaml.Unmarshal(legacy, &s); err != nil {
		t.Fatalf("yaml.Unmarshal legacy state: %v", err)
	}
	if s.CurrentProfile != "alpha" {
		t.Errorf("CurrentProfile = %q, want alpha", s.CurrentProfile)
	}
	if s.LastAppliedPerTool != nil {
		t.Errorf("LastAppliedPerTool = %v, want nil", s.LastAppliedPerTool)
	}
	// Getter must still return false without panicking.
	if _, ok := s.GetLastApplied(ToolClaudeCode, "/anywhere"); ok {
		t.Errorf("GetLastApplied on legacy state = ok=true, want false")
	}
	// And a RecordApplied on a legacy-decoded State must lazy-allocate.
	s.RecordApplied(ToolClaudeCode, "/a", "sha", time.Now())
	if s.LastAppliedPerTool == nil {
		t.Errorf("LastAppliedPerTool nil after RecordApplied; lazy alloc broken")
	}
}
