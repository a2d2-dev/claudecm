# ADR-0001: Direction Lock for claudecm v1

- **Status:** Accepted
- **Date:** 2026-06-30
- **Owner:** CEO (LF)
- **Authority:** This memo is the single source of truth for direction. Where it conflicts with `brief.md`, `project-brief.md`, `README.md`, or `architecture.md`, this memo wins. All subsequent BMAD agents (PRD, architect, dev, reviewer) MUST defer to it.

## Context

claudecm is a brand-new, experimental Go CLI. There are no customers, no installed base, and no backwards-compatibility obligations. We have license to make sharp choices and cut scope aggressively. Prior planning artifacts drifted across multiple briefs in two languages and accumulated aspirational features (encryption, cloud sync, GUI, MCP) that do not belong in v1. This ADR re-grounds the project.

## Decision Summary

claudecm is a **local-first profile manager for AI coding tool environments**. Its functional model is borrowed from `kubecm` (profile add/list/switch/current/rename/delete with a clean CLI surface). Its positioning and documentation voice take cues from `cc-switch` (concrete, narrow, useful today). v1 supports exactly two tools: **Claude Code** and **Codex CLI**.

## Locked Decisions

1. **V1 tool scope.** Claude Code and Codex CLI only. Gemini CLI, Cursor, Windsurf, and others are explicitly post-v1.
2. **Unified Profile schema.** One profile object with shared core fields (name, base_url, api_key, model, notes, timestamps) plus a per-tool overlay map `tools: { claude_code?: {...}, codex?: {...} }`. Overlays are optional and sparse.
3. **Command surface v1.** `add`, `list`, `current`, `switch`, `explain`, `import (claude-code|codex)`, `export`, `edit`, `rename`, `delete`, `completion`, `version`. Nothing else ships in v1.
4. **Activation model.**
   - **Primary:** direct write to each tool's on-disk config file. Writes are atomic (temp file + rename), mode `0600`, with a timestamped backup taken before every write, and a merge that preserves unknown keys we do not own.
   - **Secondary:** `export` emits shell `export VAR=...` lines for users who prefer env-var-driven flows.
5. **`explain` resolution chain.** When reporting what a tool will actually see, resolve in this order and surface the winner plus the shadowed layers: `EnvOverride > on-disk tool config > Profile overlay > Profile core > built-in default`.
6. **Storage.** Plaintext YAML under the user's config dir, files `0600`, directory `0700`. Encryption is **deferred post-v1**. Stop marketing "AES-256" or any cryptographic claim in v1 docs, READMEs, or marketing copy.
7. **Go module path.** `github.com/a2d2-dev/claudecm`. All imports, install instructions, and CI pin to this path.
8. **Non-goals for v1.** Cloud sync, team sharing, GUI/TUI, MCP servers, Skills, proxy/failover/load-balancing, IDE plugins, enterprise SSO, RBAC, audit logging. These are not "later in v1" — they are out.
9. **Docs.** One canonical English source-of-truth tree under `docs/`. The duplicate `brief.md` / `project-brief.md` split is killed; surviving content folds into PRD and this ADR. A Chinese mirror MAY appear later but is never authoritative.

## Success Metrics (v1)

- **SM-1 — Time-to-switch:** switching the active profile for a supported tool takes < 2 seconds end-to-end on a warm machine.
- **SM-2 — Import fidelity:** `import claude-code` and `import codex` round-trip an existing real config into a working profile with zero manual edits in ≥ 95% of sampled user configs.
- **SM-3 — Activation safety:** zero reported incidents of lost or corrupted tool config across the v1 beta cohort; every write has a recoverable backup.
- **SM-4 — Explainability:** `claudecm explain` correctly identifies the effective value and the shadowing layer for every field in the resolution chain in 100% of internal test fixtures.

## Risks

- **R1.** Tool vendors change their on-disk config schema; merge-preserve mitigates but does not eliminate breakage.
- **R2.** Plaintext storage is a deliberate v1 trade-off; a security-conscious early adopter may reject the tool until encryption lands.
- **R3.** Scope creep pressure from "just add Gemini" / "just add a TUI" requests during beta.
- **R4.** Two-tool scope makes the value prop thin for users of only one tool; positioning must lean on `explain` and safe activation, not breadth.

## Supersedes

This ADR overrides, in `brief.md` and `project-brief.md`: any tool list beyond Claude Code and Codex; any mention of AES-256 or built-in encryption in v1; any cloud-sync, team-sharing, GUI, MCP, Skills, proxy, or IDE-plugin features positioned as v1; any command not listed in Decision 3; any module path other than `github.com/a2d2-dev/claudecm`; and the dual English/Chinese authoritative-brief structure.
