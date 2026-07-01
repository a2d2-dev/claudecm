// Package claudecode: empty-file policy helper.
//
// treatAsEmpty is the single source of truth for the "is this settings.json
// effectively empty?" question, shared by Import (read side, import.go) and
// Plan.Transform (write side, plan.go / renderSettings). PRD §4.7 requires the
// two paths to agree — a round-trip Import → Plan → Apply on a fresh install
// where Claude Code has written a whitespace-only settings.json must NOT
// diverge between "Import ErrParseFailed" and "Plan silently normalizes to
// {}".
//
// Prior to this helper the two paths used different predicates:
//
//   - import.go: len(data) == 0   (strict zero-byte)
//   - plan.go:   bytes.TrimSpace(data) empty  (whitespace-tolerant)
//
// which made a file containing "   \n" a parse failure on Import and a happy
// merge target on Plan. Consolidated here so any future policy change
// (e.g. accepting UTF-8 BOM) only edits one line.
package claudecode

import "bytes"

// treatAsEmpty reports whether the given settings.json byte payload should be
// interpreted as the empty JSON object `{}`. Returns true for:
//
//   - a literal zero-byte file (Claude Code's first-launch shape), and
//   - a file whose ONLY content is JSON-insignificant whitespace (space, tab,
//     newline, carriage return) — a hand-edited file where the user cleared
//     the object body but left a trailing newline is not semantically distinct
//     from a zero-byte file on the wire.
//
// Anything else — including a legal `{}` on disk — returns false; the caller
// then routes the bytes through the normal JSON parser (Import) or sjson
// renderer (Plan). This is a READ-SIDE POLICY: no bytes are ever written by
// this function; the "no silent fallback rewrite" rule (NFR-S1) is about
// writing malformed input as `{}`, which is a different question.
func treatAsEmpty(data []byte) bool {
	if len(data) == 0 {
		return true
	}
	return len(bytes.TrimSpace(data)) == 0
}
