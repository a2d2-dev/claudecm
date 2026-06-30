# Per-Story "Ready-for-Dev" Checklist

> Every story under `docs/plan/stories/` must satisfy every box below before dev claims it. Mirror this checklist at the bottom of each story file; don't re-derive it.

## Mandatory gates (all must check)

- [ ] **PRD/architecture refs are correct.** Story names at least one PRD FR/NFR and at least one architecture section. Wrong refs → re-author, don't dev.
- [ ] **Acceptance criteria are testable.** Each AC is a Given/When/Then or equivalent observable statement. No "should feel good" criteria.
- [ ] **≥ 1 happy + ≥ 1 edge-case test row.** Edge case is concrete (e.g., "CRLF line endings", "symlink resolving outside `$HOME`", "missing file"). "Test failure cases" alone does not count.
- [ ] **"No fallback writes" reminder is present** in the story body or its test plan. Maps to NFR-S1 and `~/.claude/CLAUDE.md` rule "绝对禁止使用 fallback 模式". Stories that write to disk must explicitly call out the refuse-on-malformed behavior.
- [ ] **Scope is v1-only.** No story may quietly add: MCP, cloud sync, GUI/TUI, Gemini CLI / Cursor / Windsurf / IDE plugins, AES / encryption, project-scope `.claude/settings*.json`, RBAC/SSO/audit dashboards, proxy/failover. Out-of-scope creep is a re-author, not a dev fix.
- [ ] **Dependencies satisfied.** Every story listed in `blockedBy` is `completed` per `docs/plan/sprint-plan.md`. Stories touching multi-file writes must wait on E7. Stories touching `switch`'s pre-apply diff must wait on E5-S5.
- [ ] **Complexity estimate is set** (S / M / L). Stories above L are split before dev claim.
- [ ] **Owned-key allowlist references are exact.** Stories under E3/E4 cite the PRD §4.7 keys verbatim. Adding a key requires a paired PRD edit, not a story edit.
- [ ] **Write-path routing rule honored.** Any story that writes to a tool-owned file routes through `internal/writepath.Apply` (and through `internal/commit` for multi-file writes). Direct `os.WriteFile` / `os.Rename` on owned files is a coding-standards violation.
- [ ] **Default redaction not regressed.** Stories touching `list` / `current` / `explain` output keep `api_key` redacted unless `--reveal` is passed (NFR-S8).
- [ ] **`--dry-run` honored.** Stories landing or touching write commands accept `--dry-run` per FR-15.

## Per-story claim ritual

1. Read the story file under `docs/plan/stories/E#-S#.md`.
2. Re-read the cited PRD FR/NFR sections and architecture section.
3. Confirm every box above is checked. If not, edit the story; do not start coding around a gap.
4. Update `TaskList` (set the story task `in_progress`, owner = your agent name).
5. Branch off `main` with `feat/E#-S#-<slug>`. One story = one PR.
6. PR description links the story file and lists which ACs the PR closes.

## What disqualifies a story from being "ready"

- Cites a PRD FR/NFR that does not exist.
- AC contains "TBD", "tbd", "later", or "post-v1".
- Adds a tool, a command, or a config layer not present in PRD §4.6 / §4.7 / §6.
- Writes to a tool-owned file without routing through `internal/writepath.Apply`.
- Reduces test coverage below the gate set by NFR-T1 (≥ 80% on `writepath` / `commit` / `resolver`).
- Re-introduces AES / "secure storage" language or a crypto dependency.
