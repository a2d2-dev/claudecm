# claudecm v1 — Epics

> Authority chain: ADR-0001 (`docs/decisions/0001-direction-lock.md`) > PRD v1 (`docs/prd/prd-v1.md`) > Architecture (`docs/architecture.md`). If anything here drifts from those, those win and this file is the bug.

This file is the high-level map of the v1 implementation. Each epic has a goal, acceptance criteria, and the list of story IDs it contains. Story-level detail (user story, AC, test plan, complexity, deps) lives in `docs/plan/stories/E#-S#.md`. Execution order lives in `docs/plan/sprint-plan.md`. Per-story dev-readiness gates live in `docs/plan/readiness-checklist.md`.

No story silently expands v1 scope: no MCP, no cloud, no GUI, no Gemini CLI / Cursor / Windsurf / IDE plugins, no AES / encryption claims, no project-scope Claude Code settings.

---

## E1. Foundations

**Goal.** Land the storage + config substrate everything else builds on: unified Profile schema with `schema_version: 1`, paths/HOME hardening, atomic write primitives, timestamped backup writer, retention pruning + audit log, file lock wrapper, and the `~/.claudecm/` layout (`profiles/`, `state.yaml`, `backups/<tool>/`, `audit.log`).

**Acceptance criteria.**
- Profile YAML matches PRD §4.1 schema (core fields + sparse `tools` overlay) with `schema_version: 1` required on read.
- `internal/storage/paths.go` is the single legal way to construct any absolute path inside the project; enforces NFR-S3/S5.
- Atomic write helper writes to `<file>.tmp.<pid>.<rand>` → fsync → rename; first write uses `O_CREAT|O_EXCL` on the temp file.
- Backup helper produces `<file>.bak.<ISO8601>.<short-uuid>` at `0600`, verifies size matches before continuing.
- Retention prunes oldest-first down to N=10 per `(tool, file)`, writes audit-log entry per prune, ignores non-`.bak.*` files, never deletes the just-written target.
- Lock wrapper uses `gofrs/flock`, 5s default timeout, `--lock-timeout` override.
- `~/.claudecm/` dir mode `0700`, all files `0600`, mode re-asserted on every write.

**Stories.** E1-S1 (L — promoted from M to cover legacy-field migration: `auth_token` / `custom_env` callers ported to the new shape), E1-S2, E1-S3, E1-S4, E1-S5, E1-S6, E1-S7.

---

## E2. WritePath Invariant

**Goal.** Implement `internal/writepath.Apply(plan WritePlan) (ApplyReport, error)` as the single FR-5 pipeline (lock → read → parse → resolve symlink → diff → backup → atomic temp+rename → post-write reparse → auto-rollback → concurrent-edit check → release lock). Unit-tested against a synthetic adapter so that adapters can be slotted in independently.

**Acceptance criteria.**
- Every step from PRD FR-5 / Architecture §4 executes in order; bypassing any of them is a coding-standards violation.
- Unparseable target → abort with no backup, no write (NFR-S1). No fallback rewrites, ever.
- Resolved-target-outside-`$HOME` → refuse (NFR-S2).
- Concurrent-edit (size/mtime/sha256 changed between read and rename) → abort with exit code 2, backup retained (NFR-C2).
- Post-write reparse failure OR drifted owned-key → auto-rollback from backup, surface a named error.
- Synthetic-adapter test matrix exercises each branch (happy, malformed, symlink-out-of-home, concurrent-edit, post-reparse-fail).
- Unit-test coverage on `internal/writepath` ≥ 80%.

**Stories.** E2-S1, E2-S2, E2-S3, E2-S4, E2-S5.

---

## E3. Adapter Interface + Claude Code Adapter

**Goal.** Land the `Adapter` interface (Detect, Files, Import, Plan, Apply, Project) and the Claude Code adapter for user-scope `~/.claude/settings.json` only. Owned-key allowlist declared as a Go `var`. JSON edits via `sjson`/`gjson` for comment-tolerant, order-preserving surgical edits.

**Acceptance criteria.**
- `internal/adapter/claudecode` implements the full `Adapter` interface for `~/.claude/settings.json`.
- Owned-key allowlist matches PRD §4.7 exactly: `env.ANTHROPIC_API_KEY`, `env.ANTHROPIC_BASE_URL`, `env.ANTHROPIC_AUTH_TOKEN`, `env.ANTHROPIC_MODEL`, `env.ANTHROPIC_SMALL_FAST_MODEL`, `env.CLAUDE_CODE_USE_BEDROCK`, `env.CLAUDE_CODE_USE_VERTEX`. Allowlist is a single exported `var`.
- Non-owned keys (`permissions`, `hooks`, `mcpServers`, `model`, `theme`, …) are byte-preserved through merge-preserve.
- Project-scope `.claude/settings.json` and `.claude/settings.local.json` are never read, written, or backed up.
- All writes routed through `internal/writepath.Apply`; the adapter never opens a tool file with write intent.
- Fixture matrix covers happy + edge (BOM, CRLF, comments, symlink, missing file, unknown keys mixed with owned).

**Stories.** E3-S1, E3-S2, E3-S3, E3-S4, E3-S5, E3-S6, E3-S7.

---

## E4. Codex Adapter

**Goal.** Land the Codex adapter for `~/.codex/config.toml` and `~/.codex/auth.json`. TOML parsing via `pelletier/go-toml/v2` document model for comment + key-order preservation (NFR-S7). Per-file ownership; owned-key allowlists declared as Go `var`s.

**Acceptance criteria.**
- `internal/adapter/codex` implements the full `Adapter` interface for both owned files.
- `config.toml` owned-key allowlist matches PRD §4.7: `model`, `model_provider`, `model_providers.<name>.{base_url,wire_api,env_key,name}`.
- `auth.json` owned-key allowlist matches PRD §4.7: `OPENAI_API_KEY` + the frozen Codex auth fields declared in a single exported `var`.
- TOML comments + key order preserved on round-trip; where preservation is impossible, `switch` prints a "comments/order may shift in <file>" warning (NFR-S7).
- All writes routed through `internal/writepath.Apply` per file; cross-file ordering is the job of E7, not this adapter.
- Fixture matrix covers happy + edge (BOM, CRLF, comments, symlink, missing file, mixed indentation, unknown providers).

**Stories.** E4-S1, E4-S2, E4-S3, E4-S4, E4-S5, E4-S6, E4-S7.

---

## E5. Resolver + `explain`

**Goal.** Implement the layered resolver (`EnvOverride > on-disk tool config > Profile overlay > Profile core > built-in default`) and the `explain` command. EnvOverride is per-tool allowlisted (NFR-E1) and powered by the existing `internal/envextract`. `explain` reports the winning layer and every shadowed layer for every effective field.

**Acceptance criteria.**
- `internal/resolver` produces an `EffectiveView` with `WinningLayer`, `Source`, and `ShadowedLayers` per field for each supported tool.
- EnvOverride allowlist matches PRD NFR-E1 exactly; other env vars are ignored (or surfaced only under `explain --all-env` as diagnostic, never shadowing).
- External drift detection: on-disk SHA256 compared to `State.LastAppliedPerTool[tool].SHA256`; mismatch reported as `ExternalDriftDetected = true`.
- `explain` ships and is acceptance-tested **before** the `switch` command (PRD §13 second-pass note 5; switch reuses explain-style rendering for its pre-apply diff).
- Default secret redaction in `explain` per NFR-S8; `--reveal` opts in.
- 100% correctness on the explain fixture matrix (SM-4).

**Stories.** E5-S1, E5-S2, E5-S3, E5-S4, E5-S5.

---

## E6. CLI Surface

**Goal.** Ship the locked v1 command surface (PRD §4.6): `add`, `list`, `current`, `switch`, `explain` (lands in E5), `import (claude-code|codex)`, `export`, `edit`, `rename`, `delete`, `restore`, `completion`, `version`. Default redaction for `api_key` in `list`/`current`/`explain`; `--reveal` opt-in. `--dry-run` on every write command. `--yes` for non-interactive confirmation. Profile-name tab completion.

**Acceptance criteria.**
- Every command in PRD §4.6 exists; nothing outside that list ships.
- `switch` uses direct-write through `internal/commit` (E7) and `internal/writepath` (E2); pre-apply diff via the adapter `Plan` output; requires `--yes` or interactive confirm when diff touches non-owned keys or when `--strict` is set; aborts non-interactively without `--yes`.
- `import` accepts `--name`, `--yes`, `--overwrite`, `--dry-run`; default round-trip fidelity ≥ 95% on the fixture corpus (SM-2).
- `edit` default UX = `$EDITOR` on temp copy with re-parse-on-save; also `--set key=value` (repeatable); `--dry-run` prints unified diff.
- `delete` clears the active-profile pointer when it points at the deleted profile (FR-2).
- `restore` ships with `--list`, `--latest`, `--id`, `--dry-run`, `--yes`; routed through writepath (each restore creates a new backup of the file it overwrites).
- `export` is the **secondary** activation path; does NOT redact by default; supports `--format yaml` and `--redact`.
- Default redaction is on for `list`, `current`, `explain`; `--reveal` flips it and emits a stderr notice.

**Stories.** E6-S1, E6-S2, E6-S3, E6-S4, E6-S5, E6-S6, E6-S7, E6-S8, E6-S9, E6-S10 (`cmd/list` — added 2026-07-01 per readiness audit, fills the FR-3 listing gap).

---

## E7. Two-Phase Commit + Rollback

**Goal.** Land `internal/commit` so any command touching multiple owned files routes through a two-phase commit. Order: `auth.json` → `config.toml` → `settings.json`. Phase 1 stages temp files via FR-5 steps 1–7 with deferred rename; phase 2 renames in order and post-write-reparses; on any failure, rename-restore already-committed targets from their FR-5 step-6 backups and return a structured `PartialFailure`.

**Acceptance criteria.**
- `commit.Stage` / `commit.Commit` are the only legal sequencer of multi-file writes.
- Lock acquisition order matches commit order; release is reverse (avoids lock-order inversion, NFR-C1).
- Any phase-2 failure rolls back already-committed targets from their backups; result enumerates `committed | rolled-back | untouched` per file + backup paths.
- Direct sequencing of `writepath.Apply` across files is a coding-standards violation (caught in CI lint).
- Simulation test: Codex commits then Claude Code reparse fails → Codex restored from backup, `PartialFailure` surfaces clean status.

**Stories.** E7-S1, E7-S2, E7-S3, E7-S4, E7-S5.

---

## E8. Testing & Release Gates

**Goal.** Make NFR-T1 enforceable: pinned `testdata/` corpus per tool, golden tests per adapter, coverage thresholds, CI round-trip smoke, concurrent-edit + two-phase-rollback simulations. AES-claim scrub gate is referenced as **closed** via PR #3 (NFR-D1).

**Acceptance criteria.**
- `testdata/{claudecode,codex}/{happy,edge}/` corpus exists and is pinned in repo.
- Edge cases per NFR-T1: missing file, partial file, unknown-keys-mixed-with-owned, UTF-8 BOM, CRLF, mixed indentation, comments, symlinked target, target on separate filesystem.
- Coverage on `internal/writepath`, `internal/commit`, `internal/resolver` is ≥ 80% line coverage; CI fails below the gate.
- CI smoke: `import → switch → export → explain` round-trips with FR-5 invariants preserved and owned keys byte-identical.
- Concurrent-edit simulation: mtime/size mutation between FR-5 step 2 and step 7 triggers NFR-C2 abort + backup retained.
- Two-phase-failure simulation: Claude Code post-write-reparse failure after Codex commit triggers FR-16 rollback.
- NFR-D1 (AES/encryption claim scrub) marked closed by reference to PR #3 in the release-gate checklist.

**Stories.** E8-S1, E8-S2, E8-S3, E8-S4, E8-S5, E8-S6.

---

## E9. Docs & Polish

**Goal.** README rewrite in the cc-switch voice (concrete, narrow, useful today). Quickstart sized to hit SM-5 (first switch ≤ 3 min for a user with a working Claude Code or Codex install). `explain` + `switch --dry-run` demo asciinema. Decision-log update reflecting the implementation cut.

**Acceptance criteria.**
- README is rewritten to the locked scope (Claude Code + Codex CLI only, plaintext YAML at `0600`, primary activation = direct write, secondary = `export`). No AES, no MCP, no GUI, no Gemini.
- Quickstart walks: install → `import claude-code` (or `import codex`) → `switch` → `current` → `explain`. Timed against SM-5.
- asciinema demo for `explain` and `switch --dry-run` linked from README.
- Decision log (PRD §13) updated with any policy choices made during implementation.

**Stories.** E9-S1, E9-S2, E9-S3, E9-S4.

---

## Story count

- E1: 7 stories
- E2: 5 stories
- E3: 7 stories
- E4: 7 stories
- E5: 5 stories
- E6: 10 stories
- E7: 5 stories
- E8: 6 stories
- E9: 4 stories
- **Total: 9 epics, 56 stories.**
