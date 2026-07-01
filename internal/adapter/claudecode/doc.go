// Package claudecode will implement the Claude Code adapter — owning
// ~/.claude/settings.json (user scope only) per PRD §4.7 and
// architecture.md §3.1.
//
// This file is the E3-S1 skeleton: package declaration only. The
// adapter body (Detect / Files / Import / Plan / Apply / Project) plus
// the frozen owned-key allowlist land in E3-S2..E3-S7 under the
// contract declared in internal/adapter/adapter.go.
package claudecode
