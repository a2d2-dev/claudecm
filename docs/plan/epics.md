# claudecm v1 / v1.1 — Epics

> Authority chain: ADR-0002 (`docs/decisions/0002-v1_1-scope.md`) for v1.1 decisions; otherwise ADR-0001 (`docs/decisions/0001-direction-lock.md`) > PRD v1 (`docs/prd/prd-v1.md`) > Architecture (`docs/architecture.md`). If anything here drifts from those, those win and this file is the bug.

This file is the high-level map of the v1 implementation. Each epic has a goal, acceptance criteria, and the list of story IDs it contains. Story-level detail (user story, AC, test plan, complexity, deps) lives in `docs/plan/stories/E#-S#.md`. Execution order lives in `docs/plan/sprint-plan.md`. Per-story dev-readiness gates live in `docs/plan/readiness-checklist.md`.

No story silently expands v1 scope: no MCP, no cloud, no GUI, no Gemini CLI / Cursor / Windsurf / IDE plugins, no AES / encryption claims, no project-scope Claude Code settings. ADR-0002 explicitly amends v1.1 for provider presets, one optional terminal-only fuzzy `switch` selector, and release/distribution planning.

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

## E10. Provider Preset Templates

**Goal.** Add `claudecm add --preset <name>` as a transparent template path for Anthropic-compatible provider profiles. Built-in presets include `moonshot`, `deepseek`, `glm`, and `qwen` at minimum. Presets pre-fill base URL, model, and provider fields; users supply only secrets; generated fields are displayed and overridable before save. Public docs and CLI copy state that presets are convenience templates and do not imply official provider support.

**Acceptance criteria.**
- A closed built-in preset catalog exists with at least `moonshot`, `deepseek`, `glm`, and `qwen`; each entry declares display name, provider key, base URL, default model, supported tool overlays, and expected secret field names.
- `claudecm add --preset <name>` expands into the normal profile schema (`schema_version: 1`, core fields, sparse tool overlays) and reuses existing `add` validation, redaction, `--dry-run`, overwrite, and prompt behavior.
- Secrets are never embedded in presets. Interactive and non-interactive flows collect or require secrets through existing `add` inputs only.
- Generated preset fields are shown before save, can be overridden, and are visible in `--dry-run` output with secrets redacted by default.
- Preset-backed activation, if requested, still routes through `internal/commit` and `internal/writepath`; no preset code writes Claude Code or Codex files directly.
- Docs and help text explicitly say presets are convenience templates, not official-provider support, certification, endorsement, or compatibility guarantees.

**Stories.** E10-S1, E10-S2, E10-S3, E10-S4.

---

## E11. Interactive Fuzzy Switch

**Goal.** Make bare `claudecm switch` in an interactive TTY open a kubecm-style fuzzy profile selector with active marker and preview, while keeping `claudecm switch <name> --yes` script-stable and preserving existing non-TTY behavior.

**Acceptance criteria.**
- Bare `switch` opens the fuzzy selector only when stdin and stdout are interactive TTYs and no profile name argument is provided.
- Non-TTY behavior is unchanged: bare `switch` in a pipe, redirected shell, CI, or cron exits as it does in v1 and never blocks on an interactive selector.
- Named switching remains stable: `claudecm switch <name> --yes` performs the existing non-interactive path, and `switch <name>` keeps the existing confirmation semantics.
- The selector lists profiles with a clear active marker, supports fuzzy filtering, preserves deterministic ordering before filtering, and lets the user cancel without side effects.
- The preview shows enough redacted profile/effective context to distinguish choices, without revealing secrets unless the existing `--reveal` policy explicitly permits it.
- Selecting a profile enters the same pre-apply diff, confirmation, commit, backup, rollback, and reporting path as named `switch`; the selector is terminal UX only, not a second activation engine or GUI.

**Stories.** E11-S1, E11-S2, E11-S3, E11-S4.

---

## E12. Release & Distribution

**Goal.** Plan the v1.0.0 and v1.1 distribution path: GoReleaser config, GitHub release workflow, version injection via ldflags, tag `v1.0.0`, then Homebrew tap and Scoop manifest. The actual `v1.0.0` tagging is owned by a separate release-engineering effort, but the stories document the full plan and acceptance surface.

**Acceptance criteria.**
- Version metadata is injected via ldflags and visible through `claudecm version`, including version, commit, date, and dirty/build metadata where available.
- GoReleaser packaging plan covers supported OS/arch archives, checksums, SBOM/provenance decision, changelog generation, and dry-run validation.
- GitHub release workflow plan defines tag-triggered release, required secrets, permissions, artifact upload, rollback/retry notes, and release-note expectations.
- The `v1.0.0` tag sequence is documented end-to-end while clearly noting that the tag operation itself is handled by the separate release-engineering effort.
- Homebrew tap plan covers formula ownership, URL/checksum updates, install smoke, upgrade smoke, and rollback.
- Scoop manifest plan covers bucket location, manifest fields, checksum update, install smoke, upgrade smoke, and rollback.
- No release story changes product scope: no new commands beyond existing `version`, no cloud service, no telemetry, and no package-manager auto-update daemon.

**Stories.** E12-S1, E12-S2, E12-S3, E12-S4, E12-S5.

---

## E13. Smart `add` Onboarding

> Authority: ADR-0003 (`docs/decisions/0003-smart-add-scope.md`) governs this epic. It narrowly amends ADR-0001 Decision 8 for the opt-in `--ai` parse path only; everything else stays zero-network and local-first.

**Goal.** Collapse the three highest-friction onboarding flows into `add`: build a profile draft from environment variables, from a config file, or from pasted free text. Pasted text is parsed by a local redaction+heuristic extractor by default; an explicit `--ai` escalation strips secrets locally, sends only desensitized text to an LLM (borrowing the active profile's credentials), and re-injects the secret locally. Every path ends in the existing `add` preview/validation/save pipeline and never auto-activates or writes a tool file directly.

**Acceptance criteria.**
- `add --from-env` materializes a draft from the Claude Code / Codex env-var allowlist; zero network; missing required fields (no key) refused with a clear message.
- `add --from-file <path>` auto-detects `.env` / shell `export` / JSON / YAML / TOML and parses into a draft; unreadable/unrecognized input is refused (NFR-S1), never half-populated; zero network.
- A local redaction+heuristic extractor library exists (`internal/blobparse` or equivalent): given arbitrary text it returns extracted core fields **plus** a desensitized copy in which no secret-shaped token survives; "strip on doubt" — a candidate secret that cannot be safely placeholdered is removed, never passed through.
- `add --from-text <text>` / `add --from-text -` (stdin) runs the local extractor and produces a draft; zero network.
- `add --ai` is off by default and requires the flag per invocation. It runs the extractor, holds secrets locally, sends only the desensitized text to the LLM (active profile by default, `--ai-profile <name>` override), maps the structured response into the profile schema, re-injects the held secret locally, and refuses if the LLM output does not conform. Interactive runs name the credential-lending profile and show the desensitized payload before the call.
- No E13 path probes a remote provider to validate a key/model; no secret is logged or persisted by the parse call; all drafts save through `storage.SaveProfile` and any activation still routes through `internal/commit` + `internal/writepath`.
- Secrets are redacted by default in every preview (`--dry-run` / prompt) per NFR-S8; profile-name validation (NFR-S5) and overwrite guard are unchanged.
- Docs and `--help` state the local-first default and that only the opt-in `--ai` path makes a (secret-free) network request.

**Stories.** E13-S1, E13-S2, E13-S3, E13-S4, E13-S5.

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
- E10: 4 stories
- E11: 4 stories
- E12: 5 stories
- E13: 5 stories
- **Total: 13 epics, 74 stories.**
