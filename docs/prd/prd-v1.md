---
title: claudecm — Local-First Profile Manager for AI Coding Tool Environments
status: final
created: 2026-06-30
updated: 2026-07-01
direction_lock: docs/decisions/0001-direction-lock.md
module_path: github.com/a2d2-dev/claudecm
schema_version: 1
mirror_of: _bmad-output/project-planning-artifacts/prds/prd-a2d2-dev-2026-06-30/prd.md
authority: docs/decisions/0001-direction-lock.md
---

> **Mirror notice.** This file is a mirror of the canonical PRD at
> `_bmad-output/project-planning-artifacts/prds/prd-a2d2-dev-2026-06-30/prd.md`.
> Both copies must stay in sync. **ADR-0001 (`docs/decisions/0001-direction-lock.md`) is the authority**;
> where any wording here conflicts with ADR-0001, ADR-0001 wins. Any update to this PRD MUST
> land in both files in the same commit.

# PRD: claudecm — Local-First Profile Manager for AI Coding Tool Environments

> **Direction Lock authority.** This PRD complies with `docs/decisions/0001-direction-lock.md` (ADR-0001, 2026-06-30). Where any prior brief, README, or architecture note conflicts with the locked direction, ADR-0001 wins and this PRD inherits that authority. The implementation and public module path are both `github.com/a2d2-dev/claudecm`; `claudecm` is the product name, not a codename.

## 0. Document Purpose

This PRD defines v1 of `claudecm`, an open-source local-first CLI that manages and switches API/provider/environment configurations for AI coding tools. v1 supports exactly **Claude Code** and **Codex CLI**. The functional model references `kubecm` (profile add/list/current/switch/rename/delete loop with a clean CLI surface). The positioning, documentation voice, and adoption story reference `cc-switch` (local-first, OSS, "switch your AI coding env in one command"). All "AES-256 / secure storage" language from prior drafts is removed; v1 stores plaintext YAML with `0600` perms and defers encryption.

This is the second-pass revision. It closes 12 blocking validator findings on safety, correctness, and operability of the write path.

## 1. Vision

AI coding workflows now routinely span at least two CLIs — Claude Code and Codex CLI — each with its own on-disk config format, environment-variable conventions, and activation behavior. Operators who alternate between official accounts, work accounts, relay providers, and cost-optimized setups end up hand-editing JSON, TOML, YAML, or shell rc files. That is error-prone, hard to audit, and hostile to anyone trying to verify what a tool will actually see before kicking off an expensive or sensitive coding session.

`claudecm` exists to make API/provider switching for AI coding tools feel as operationally reliable as `kubectl` context switching feels for clusters. A user defines named **Profiles**, inspects what is active, understands *why* it is active, switches safely, and imports or exports configurations without ever hand-editing tool-specific files.

The v1 wedge is intentionally narrow: become the most trustworthy CLI-first switcher for Claude Code and Codex CLI before expanding scope. The long-term opportunity is to be the neutral, local-first control layer for AI-coding profile management — not another chat client, not a hosted control plane, not a proxy.

## 2. Target User

### 2.1 Jobs To Be Done
- Switch between multiple API/provider configurations for daily coding work without hand-editing config files.
- Reuse named working modes such as `official`, `work`, `cheap`, `relay-a` across both supported tools.
- Verify the currently effective configuration before running requests that may affect billing, reliability, or compliance.
- Import an existing real-world Claude Code or Codex CLI config rather than rebuilding from scratch.
- Export or activate a known-good configuration in a way that fits shell-driven workflows.
- Restore a previously known-good tool config from backup after a regretted switch.
- Adopt a local-first OSS tool without being locked into a proprietary hosted service.
- Adopt `claudecm` without losing the user's existing working Claude Code or Codex CLI setup.

### 2.2 Non-Users (v1)
- Teams looking for centralized cloud administration, tenancy, RBAC, or audit logging.
- Users primarily interested in MCP orchestration, Skills management, or a desktop GUI.
- Users looking for provider proxying, failover routing, or usage/billing analytics as the primary value.
- Users of AI coding tools outside the v1 supported set (Gemini CLI, Cursor, Windsurf, IDE plugins, etc.).

### 2.3 Key User Journeys

- **UJ-1. Switch active profile across both tools.**
  *Persona:* Lin alternates between official Claude usage and a lower-cost Codex-compatible relay.
  *Entry:* Multiple profiles saved locally, terminal open.
  *Path:* `claudecm list` → `claudecm switch relay-a` (pre-apply diff shown, confirmed) → `claudecm current` → `claudecm explain`.
  *Climax:* Lin sees the new active profile and the effective provider, key source, base URL, and model for both Claude Code and Codex.
  *Resolution:* Coding resumes with zero hand edits.

- **UJ-2. Import an existing local tool config into a reusable profile.**
  *Persona:* Mina already has a working Claude Code config and a working Codex CLI config.
  *Entry:* Existing on-disk tool config files.
  *Path:* `claudecm import claude-code --name official-claude --yes` and `claudecm import codex --name official-codex --yes` → review canonicalized profile → save.
  *Climax:* Reusable profile created without manually transcribing credentials or settings.
  *Resolution:* Mina can now switch among imported and newly created profiles from one CLI.

- **UJ-3. Explain / diagnose effective configuration.**
  *Persona:* Hao notices billing or routing that does not match expectations.
  *Entry:* A profile appears active but actual tool behavior is suspect.
  *Path:* `claudecm explain` shows each effective field, the **winning layer**, and the **shadowed layers** in the resolution chain.
  *Climax:* Hao identifies whether the active profile, the on-disk tool config, an environment override, or a built-in default is responsible.
  *Resolution:* Hao updates or switches the profile with confidence.

- **UJ-4. Anti-regret first-time adoption + restore.**
  *Persona:* A new user with an already-working Claude Code and/or Codex CLI setup is trying `claudecm` for the first time.
  *Entry:* No profiles yet; live, working tool configs on disk.
  *Path:* `claudecm import claude-code` and/or `claudecm import codex` bootstraps profiles from existing on-disk state. The first `claudecm switch` writes via atomic temp-file + rename with a timestamped backup of the prior tool config. If the user regrets the switch, `claudecm restore --tool claude-code --latest` reverts byte-identical from backup.
  *Climax:* The user can switch to a new profile, then `restore` back to the original tool behavior byte-for-byte from the backup.
  *Resolution:* The user adopts `claudecm` without ever experiencing "claudecm clobbered my working setup."

## 3. Glossary
- **Profile** — A named reusable configuration intent (e.g. `work`, `official`, `cheap`, `relay-a`). The primary switching unit. Internally a unified object: **core fields + per-tool overlay map**, tagged with `schema_version: 1`.
- **Core Fields** — Shared profile values that apply across supported tools: `name`, `base_url`, `api_key`, `model`, `notes`, timestamps.
- **Per-tool Overlay** — Optional sparse map `tools: { claude_code?: {...}, codex?: {...} }` allowing tool-specific deviations from the core.
- **Adapter** — The internal layer that renders a profile (core + overlay) into a supported tool's required on-disk file format and/or environment-variable surface.
- **Owned Key** — A configuration key inside a tool's on-disk file that the adapter is contractually responsible for managing on switch. Enumerated per tool in §4.7. Non-owned keys are preserved byte-for-byte where the parser allows.
- **Active Profile** — The profile currently selected by claudecm for future activation, export, and explain operations.
- **Activation** — Applying the active profile so supported tools observe the intended configuration. v1 primary path: direct write to the tool's on-disk config file via the locked write-path invariant (§4.2 FR-5).
- **Effective Configuration** — Final resolved value a tool will see after the resolution chain is applied.
- **Resolution Chain (explain)** — `EnvOverride > on-disk tool config > Profile overlay > Profile core > built-in default`.
- **Import Source** — Existing local tool config used to seed or update a profile.
- **Export Artifact** — Shell `export VAR=...` lines (and/or profile-native YAML) emitted from a profile for env-var-driven workflows.
- **Backup Set** — The retained set of timestamped backups for a given tool-config file. Bounded by retention policy (NFR-R).

## 4. Features

### 4.1 Profile Management
**Description.** claudecm manages named **Profiles** as the central operational unit. A profile is a single unified object combining shared core fields and an optional sparse per-tool overlay map, tagged with a required `schema_version`. Users can create, inspect, update, rename, and delete profiles via explicit CLI operations. Realizes UJ-1, UJ-2, UJ-3, UJ-4.

**Profile schema (v1, locked).**

```yaml
schema_version: 1              # REQUIRED on every stored profile
name: relay-a                  # core
base_url: https://...          # core
api_key: sk-...                # core (plaintext in v1, 0600 file)
model: claude-3-5-sonnet       # core (optional)
notes: "cheap relay"           # core (optional)
created_at: 2026-06-30T...     # core
updated_at: 2026-06-30T...     # core
tools:                         # optional sparse overlay map
  claude_code:                 # optional, sparse
    model: claude-3-5-haiku
  codex:                       # optional, sparse
    base_url: https://...
```

**Functional Requirements.**

#### FR-1: Create and store profiles (`add`)
A user can create and persist a named profile containing the unified core fields plus any optional per-tool overlay data.

*Consequences (testable):*
- Profiles persist as plaintext YAML under the user's config dir (`0700` dir, `0600` file).
- Every stored profile carries `schema_version: 1`. Profiles missing `schema_version` are rejected on read.
- The external model exposed in docs and CLI is the unified profile (core + overlay), not separate per-tool profile types.
- Per-tool fields appear only as adapter-layer overlays under `tools.<tool>` and never replace core.
- Invalid or incomplete required fields are rejected with actionable CLI feedback.
- Existing profile names cannot be silently overwritten.
- Profile names are validated against a strict allowlist (`[A-Za-z0-9._-]{1,64}`); `..`, `/`, `\`, and absolute paths are rejected (path-traversal hardening — see NFR-S5).
- When `add` performs any tool-config write (e.g., the new profile is also made active), the locked write-path invariant FR-5 and `--dry-run` (FR-15) apply.

#### FR-2: Edit, rename, delete profiles (`edit`, `rename`, `delete`)
A user can update, rename, or delete an existing profile through explicit CLI operations.

*Consequences (testable):*
- `edit` and `rename` modify values without recreating the profile.
- `edit` default UX is to launch `$EDITOR` (fallback `vi`) on a temp copy of the profile YAML. On save, claudecm re-parses; invalid YAML or invalid schema rejects the edit and preserves the original file.
- `edit` also accepts a non-interactive `--set key=value` flag (repeatable) for scripted use. `--set` writes are validated identically.
- `edit` supports `--dry-run` (prints the planned change as a unified diff without writing).
- `rename` validates the new name against the same allowlist as FR-1.
- `delete` requires explicit confirmation in interactive mode and accepts `--yes` for non-interactive use.
- After `delete`, the system never ends in a corrupted state where the active-profile pointer references a missing profile; it is cleared or the user is prompted.

#### FR-3: List and inspect profiles (`list`, `current`)
A user can list available profiles and inspect which one is active.

*Consequences (testable):*
- `list` clearly marks the active profile.
- `list` provides human-readable output and a machine-readable JSON output (`--output json`) suitable for shell pipelines.
- `current` shows active profile name plus the minimum effective values needed to confirm the selection.
- `list`, `current`, and `explain` redact `api_key` and any field tagged `secret: true` by default (display as `sk-***last4`). `--reveal` opts in to plaintext display. See NFR-S8.

### 4.2 Switching and Activation
**Description.** kubecm-style switching loop: choose a target profile, activate it, confirm the result. Activation is a first-class product action — never a side effect of an editor or shell hack. Realizes UJ-1, UJ-4.

**Functional Requirements.**

#### FR-4: Switch active profile (`switch`)
A user can switch the active profile by name argument or via interactive selection.

*Consequences (testable):*
- Direct switching by profile name is supported (with shell tab-completion).
- Interactive selection is supported when no name is provided.
- Before any write, `switch` prints an **explain-style pre-apply diff** for every tool-config file it will touch, showing per-key old → new for owned keys, preserved keys, and any keys the profile does not own that would change due to overlay/render.
- `switch` requires explicit confirmation (interactive `y/N` prompt or `--yes`) when the diff touches keys claudecm does not own, OR when `--strict` is set. In non-interactive contexts without `--yes`, `switch` aborts.
- `switch` supports `--dry-run`: computes and prints the same diff but performs no writes and no backups.
- The newly active profile is reported immediately after switching, alongside the path of every backup created.
- `switch` MUST use the locked write-path invariant (FR-5) for every tool-config file it touches.

#### FR-5: Locked write-path invariant (single source of truth for all on-disk writes)
**Every** command that mutates a tool-config file on disk — `switch`, `import` (when it overwrites), `edit` (when scoped to a tool file), `restore`, and any future write path — MUST go through this exact pipeline. No command may bypass it.

The pipeline, in order:

1. **Acquire exclusive file lock** (advisory `flock` LOCK_EX with timeout; see NFR-C1).
2. **Read** the current tool-config file from disk. Record size and mtime.
3. **Parse** with a format-aware parser that preserves comments and key order where the format permits (JSON-with-comments / TOML). Refuse-on-malformed: if the file is unparseable, abort with a clear error and do NOT write. No fallback rewrite.
4. **Resolve symlinks** on the target path. If the resolved target is outside `$HOME` (or outside `--home`, see NFR-S3), abort with a clear error. Otherwise back up and write through to the resolved target. See NFR-S2.
5. **Compute diff** against the rendered profile, restricted to OWNED-KEY scope (§4.7); produce the pre-apply diff used by FR-4 and `--dry-run`.
6. **Timestamped backup** of the current file to `<file>.bak.<ISO8601>.<short-uuid>`, mode `0600`. The backup is the original bytes, before any transform. Backup creation is verified (size matches) before proceeding.
7. **Atomic write** of the new content: write to `<file>.tmp.<pid>.<rand>` in the same directory, fsync, `rename()` over the target. First write against a missing file uses `O_EXCL | O_CREAT` semantics; if the file appears between read and write, abort with concurrent-edit error.
8. **POST-WRITE REPARSE**: read the file back from disk and parse it. If parse fails, OR if owned keys do not equal the intended values, **AUTO-ROLLBACK**: restore the file from the backup created in step 6, mark backup as primary, surface a clear error.
9. **Concurrent-edit check**: between step 2 and step 7, if size or mtime of the original file changed (or if its content hash now differs), abort, do NOT overwrite, retain the backup, and surface a clear error pointing at the backup path. See NFR-C2.
10. **Release file lock.**

*Consequences (testable):*
- A crash, signal, or panic at any step leaves either the original file intact OR a recoverable backup at a known path. No partial/truncated config is ever observable.
- Mode of the written file is `0600`; containing directory is `0700`.
- Adapters exist for **Claude Code** and **Codex CLI**, both routed through this pipeline.
- Export-driven activation (`export`) is supported as a **secondary** workflow path and does not write to tool files; it bypasses FR-5 because it does not mutate tool files.
- Two-tool writes are coordinated as a two-phase commit (FR-16).

#### FR-16: Cross-file two-phase write with rollback
When a single `switch` touches more than one tool file (Claude Code settings.json AND Codex config.toml, possibly also Codex auth.json), claudecm coordinates the writes as a two-phase commit.

*Consequences (testable):*
- Phase 1 (prepare): for every target file, perform steps 1-7 of FR-5 against staged temp files but defer the final `rename`.
- Phase 2 (commit): perform the `rename` for each target file in a defined order: (a) Codex auth.json, (b) Codex config.toml, (c) Claude Code settings.json.
- If any phase-2 rename fails OR any post-write reparse (FR-5 step 8) fails, claudecm restores already-committed targets from their FR-5 step-6 backups and surfaces a single structured "partial failure" error listing per-file status (`committed`, `rolled-back`, `untouched`) and the backup paths.
- Locks across files are acquired in the order above and released in reverse order to avoid lock-order inversion.

### 4.3 Effective Configuration Visibility
**Description.** Switching alone is not enough; v1 must explain the effective result. This is claudecm's headline differentiator versus ad-hoc env/file editing. Realizes UJ-1, UJ-3.

**Functional Requirements.**

#### FR-6: Show current active profile (`current`)
A user can view the active profile name and the core effective values it implies.

*Consequences (testable):*
- `current` shows the active profile name.
- `current` shows effective provider/base URL/model context for each supported tool.
- Output is concise enough for frequent terminal use and offers a JSON variant.
- Default secret-redaction per FR-3 applies.

#### FR-7: Explain effective configuration and precedence (`explain`)
A user can inspect how the effective configuration was derived for each supported tool.

*Consequences (testable):*
- `explain` reports, for each effective field per tool, the resolution chain order:
  **`EnvOverride > on-disk tool config > Profile overlay > Profile core > built-in default`**.
- For every effective field, `explain` labels the **winning layer** and lists the **shadowed layers** with their source paths (e.g. file path, env var name, profile YAML key).
- `explain` distinguishes between intended profile values and final effective values.
- `explain` does not pretend to discover runtime truth from unrelated OS state outside claudecm's observable surface.
- Default secret-redaction per FR-3 applies; `--reveal` opts in.
- The `EnvOverride` layer enumerates only the variables in NFR-E1 (per-tool env allowlist).

### 4.4 Import and Export
**Description.** Low-friction adoption from existing setups (UJ-2, UJ-4) and easy movement across local workflows. Import canonicalizes real on-disk tool configs into profiles; export emits shell-ready output for users who prefer env-var-driven flows.

**Functional Requirements.**

#### FR-8: Import existing tool config (`import claude-code`, `import codex`)
A user can import existing local config from supported tools into profiles.

*Consequences (testable):*
- `import claude-code` reads the Claude Code on-disk config (NFR-O1) and produces a canonicalized profile.
- `import codex` reads the Codex CLI on-disk config (NFR-O2) and produces a canonicalized profile, including credential material from `auth.json` where present.
- Import prioritizes faithful recovery of user-meaningful configuration where possible.
- Round-trip (`import` → `switch` activation) is **byte-identical for OWNED-KEY scope** in the resulting on-disk tool config (Success Metric SM-3 helper; SM-2 the headline).
- v1 does not promise lossless preservation of every private tool-specific source field that falls outside the unified profile model and overlay contract; unknown keys in the on-disk file are preserved on activation via merge-preserve where the parser supports it, and surfaced as "preserved verbatim" or "lost (parser limitation)" in `switch` output.
- Imported profiles are presented for review and explicit naming before being finalized in interactive mode.
- `import` supports non-interactive use via `--name <profile-name> --yes`. With `--yes`, no interactive review is required; with `--name` collision, `import` refuses unless `--overwrite` is also passed.
- `import` supports `--dry-run` (prints what the profile would be without writing).
- Stored profiles carry `schema_version: 1`.

#### FR-9: Export profile in reusable form (`export`)
A user can export profile data for shell-driven or file-driven workflows.

*Consequences (testable):*
- `export` emits shell `export VAR=...` lines suitable for `eval $(claudecm export)`.
- `export` can also emit the profile-native YAML form via `--format yaml`.
- Export output is consistent with `current` and `explain` for the same profile.
- `export` does NOT redact secrets by default (it is intended for `eval` use); a `--redact` flag is provided for capture-to-log scenarios.

### 4.5 CLI Ergonomics
**Description.** Commands must be discoverable, scriptable, and tab-completable.

**Functional Requirements.**

#### FR-10: Shell completion (`completion`)
`claudecm completion [bash|zsh|fish|powershell]` emits a completion script; profile names tab-complete for `switch`, `delete`, `edit`, `rename`, `export`, `restore`.

#### FR-11: Version reporting (`version`)
`claudecm version` reports semver, commit, and build date.

### 4.6 Locked Command Surface (v1)
The complete v1 command surface — nothing else ships in v1:

`add`, `list`, `current`, `switch`, `explain`, `import (claude-code|codex)`, `export`, `edit`, `rename`, `delete`, `restore`, `completion`, `version`.

#### FR-12: Restore previous tool config from backup (`restore`)
A user can list and revert to any backup created by FR-5.

*Consequences (testable):*
- `restore --tool <claude-code|codex> --list` enumerates the retained backup set (NFR-R1) for that tool's owned files, newest first, with timestamp, size, and provenance (which command created it).
- `restore --tool <claude-code|codex> --latest` restores the most recent backup of every file claudecm owns for that tool, using the FR-5 pipeline in reverse (lock → backup current → atomic rename of selected backup over target → post-write reparse → auto-rollback on parse fail).
- `restore --tool <claude-code|codex> --id <backup-id>` restores a specific backup by ID/timestamp.
- `restore` supports `--dry-run`, `--yes`, and prints the same explain-style pre-apply diff as `switch`.
- `restore` is itself routed through the FR-5 invariant (each restore creates a new backup of the file it is about to overwrite).
- `restore` never deletes a backup; pruning is governed by retention policy (NFR-R1).

#### FR-15: `--dry-run` on every write command
Every command that may mutate state (profile store or tool config) MUST accept `--dry-run`. With `--dry-run`, the command performs all reads, parsing, and diff computation, prints the planned change, and exits without taking the file lock and without writing. Commands covered: `add` (when it writes a profile or activates), `import`, `edit`, `switch`, `restore`, `delete`, `rename`. `list`, `current`, `explain`, `export`, `completion`, `version` are read-only and `--dry-run` is a no-op accepted for uniform scripting.

### 4.7 Per-Tool File Ownership and Owned-Key Allowlist

claudecm operates only on files and keys it claims to own. Everything else is preserved verbatim where the parser allows, or reported as "lost (parser limitation)" loudly in `switch` output.

#### Claude Code
- **Owned files:** `~/.claude/settings.json` (user scope). The project-scope `.claude/settings.json` and `.claude/settings.local.json` are out of scope for v1 — claudecm does not read, write, or back them up.
- **Owned-key allowlist (Claude Code adapter, v1):**
  - `env.ANTHROPIC_API_KEY`
  - `env.ANTHROPIC_BASE_URL`
  - `env.ANTHROPIC_AUTH_TOKEN`
  - `env.ANTHROPIC_MODEL`
  - `env.ANTHROPIC_SMALL_FAST_MODEL`
  - `env.CLAUDE_CODE_*` keys explicitly enumerated by the adapter (initial set: `CLAUDE_CODE_USE_BEDROCK`, `CLAUDE_CODE_USE_VERTEX`).
- All other keys (including `permissions`, `hooks`, `mcpServers`, `model`, `theme`, etc.) are **preserved byte-for-byte** through merge-preserve. The adapter MUST NOT touch them.
- Architecture is required to publish the exact, frozen allowlist in code as a single exported list and to gate it with the CI fixture matrix (NFR-T1).

#### Codex CLI
- **Owned files:** `~/.codex/config.toml` and `~/.codex/auth.json`.
- **Owned-key allowlist for `config.toml` (Codex adapter, v1):**
  - `model`
  - `model_provider`
  - `model_providers.<name>.base_url`
  - `model_providers.<name>.wire_api`
  - `model_providers.<name>.env_key`
  - `model_providers.<name>.name`
- **Owned-key allowlist for `auth.json` (Codex adapter, v1):**
  - `OPENAI_API_KEY`
  - any top-level field used by Codex CLI for current-user auth state (architecture freezes the exact list).
- All other keys are preserved verbatim through merge-preserve. The adapter MUST NOT touch them.

#### Adapter contract
Every adapter MUST declare, in code:
1. The exact list of files it owns.
2. The exact owned-key allowlist per file.
3. A pure function `Render(profile) -> (owned-key map, format-aware bytes)`.
4. A pure function `Extract(bytes) -> profile candidate` for `import`.
Both functions MUST be exercised by the CI fixture matrix (NFR-T1) on every PR.

## 5. Non-Goals (Explicit, v1)
- **Cloud sync** — local-first only.
- **Team sharing / centralized admin / RBAC / SSO / audit logging** — out.
- **GUI / TUI** — out. CLI only.
- **MCP server management** — out.
- **Skills management** — out.
- **Provider proxy, failover, traffic routing, load balancing** — out.
- **IDE plugins** — out.
- **Usage / billing / cost dashboards** — out.
- **Project-scope Claude Code settings files** (`.claude/settings.json`, `.claude/settings.local.json`) — out for v1; user-scope only.
- **Support for tools beyond Claude Code and Codex CLI** (Gemini CLI, Cursor, Windsurf, etc.) — out for v1.
- **Built-in encryption / "AES-256" / "secure storage" marketing claims** — **deferred post-v1**. v1 stores plaintext YAML at `0600`. Public-facing copy must not claim cryptographic protection.

These are not "later in v1" — they are out.

## 6. MVP Scope

### 6.1 In Scope
- CLI-first workflow under module path `github.com/a2d2-dev/claudecm`.
- Supported tools: **Claude Code** and **Codex CLI**.
- Unified profile schema (core fields + per-tool overlay map + `schema_version: 1`).
- Locked command surface from §4.6 (including `restore`).
- Switching with **primary activation via the FR-5 locked write-path invariant** and the FR-16 two-phase commit across tools.
- Secondary export workflow (`eval $(claudecm export)`).
- `current` and `explain` with the locked resolution chain.
- `import claude-code` and `import codex` with canonicalization into the unified profile, including non-interactive `--name`/`--yes`.
- Plaintext YAML storage, `0700` dir, `0600` files.
- Shell completion for bash/zsh/fish/powershell.
- CI fixture matrix (NFR-T1) as a v1 release gate.
- Doc cleanup of AES/encryption claims across README/brief/project-brief/architecture as a v1 release gate (NFR-D1).
- Onboarding docs sized for the cc-switch / kubecm voice: concrete, narrow, useful today.

### 6.2 Out of Scope for MVP
See §5. In particular: encryption, cloud sync, team features, GUI/TUI, MCP, Skills, proxy/failover, IDE plugins, support for tools beyond Claude Code and Codex CLI, project-scope Claude Code settings files.

## 7. Success Metrics (v1)

Numbers are **restored to ADR-0001** values; SM-5 is new and added per validator request. Any tension between ADR-0001 and the prior PRD draft is resolved in favor of ADR-0001.

**Primary**
- **SM-1 — Time-to-switch < 2s end-to-end.** From `claudecm switch <name>` returning success to a successful `claudecm current` confirming the new effective values across both supported tools takes < 2 seconds on a warm machine. Validates FR-4, FR-5, FR-6.
- **SM-2 — Import fidelity ≥ 95% on sampled real configs.** `import claude-code` and `import codex` round-trip an existing real config into a working profile with zero manual edits in ≥ 95% of sampled user configs in the v1 fixture corpus. Validates FR-8, FR-5.
- **SM-3 — Activation safety (zero corruption).** Across the v1 beta cohort, zero reported incidents of lost or corrupted tool config; every write has a recoverable backup discoverable via `restore --list`. Validates FR-5, FR-12, FR-16.
- **SM-4 — Explainability 100% on fixture set.** `claudecm explain` correctly identifies the effective value and the shadowing layer for every field in the resolution chain in 100% of internal test fixtures. Validates FR-7.
- **SM-5 — README quickstart first-switch ≤ 3 minutes.** A new user following the README quickstart, with an already-working Claude Code or Codex CLI install, can perform their first successful `claudecm switch` (including `add`/`import`) within 3 minutes, without maintainer assistance. Validates FR-1, FR-3, FR-4, FR-5, FR-8.

**Secondary**
- **SM-6 — Trust in `current` / `explain`.** Early adopters report they trust `current` / `explain` enough to use them before expensive or sensitive coding sessions. Validates FR-6, FR-7.

**Counter-metrics (do not optimize)**
- **SM-C1 — Supported tool count in v1.** Breadth beyond Claude Code + Codex CLI is **explicitly not** a v1 goal.
- **SM-C2 — Adjacent management domains (MCP, Skills, sync, proxy).** Feature breadth must not outrun product clarity.

## 8. Open Questions
1. Documentation and launch messaging that crisply communicates scope boundary versus adjacent categories (proxying, cloud control planes, tool orchestration) when public launch materials land. Owner: PM, before v1 public launch.
2. Post-v1 expansion ordering: which AI coding tool comes third after Claude Code and Codex CLI, once the unified-profile abstraction is validated in the field. Owner: PM, post-v1 review.
3. Migration policy for `schema_version: 2+` once a second profile schema lands post-v1. v1 policy: refuse-on-unknown-future-version (see NFR-M1).

> Closed by Direction Lock (ADR-0001, 2026-06-30) and this PRD:
> - Product/module name: **`claudecm`**, module path `github.com/a2d2-dev/claudecm`.
> - Encryption: **deferred post-v1**; plaintext YAML + `0600` for v1.
> - V1 tool scope: **Claude Code + Codex CLI only**.
> - V1 command surface: locked per §4.6 (with `restore`).
> - Authoritative docs structure: single English source-of-truth tree under `docs/`; the `brief.md` / `project-brief.md` split is retired.

## 9. Assumptions Index
- §4.1 / FR-2 — Active-profile pointer is cleared (not silently dangling) when its target is deleted.
- §4.1 / FR-3 — `list` and `current` provide both human-readable and JSON output, redacting secrets by default.
- §4.4 / FR-9 — `export` supports both shell `export VAR=...` lines and profile-native YAML.
- §7 / SM-5 — Quickstart timing is measured against the README walkthrough on a warm machine with an already-installed supported tool.
- §4.7 — `~` resolves to `$HOME` unless `--home` overrides; see NFR-S3.

## 10. Why Now
The AI coding tool ecosystem is fragmenting across multiple CLIs, provider models, and unofficial relay setups, while users increasingly manage their own keys, provider routing, and model selection. There is a clear timing window for a neutral, local-first, OSS configuration switcher that helps developers operate safely across tools without adopting a heavyweight hosted control plane. cc-switch has shown the appetite for narrow, local-first switchers; kubecm has shown the durability of the profile/context loop. claudecm sits at the intersection, scoped sharply to Claude Code + Codex CLI for v1.

## 11. Constraints, Safety, and Non-Functional Requirements

### 11.1 Local-first
v1 functions as a standalone local tool without any managed backend or cloud account.

### 11.2 Transparency
Every applied or generated configuration must be inspectable. `explain` is non-negotiable. The product must never obscure effective state behind opaque automation. `switch` and `restore` MUST print a pre-apply diff (FR-4, FR-12).

### 11.3 Compatibility / Anti-regret (UJ-4)
v1 must minimize migration cost by interoperating with existing Claude Code and Codex CLI on-disk configuration. For each supported tool, v1 must at minimum support: importing the current on-disk config, activating by writing the intended config (FR-5), viewing the current projected result, viewing the `explain` source chain, and restoring from backup (FR-12). Adopting claudecm must not destroy a user's previously working tool setup; switching to a fresh profile and then `restore`ing must be a reversible operation.

### 11.4 Safety NFRs

- **NFR-S1 — Refuse on malformed tool config.** If a target tool-config file is unparseable, claudecm aborts the operation, makes no backup, and exits with a clear error. No fallback writes, ever.
- **NFR-S2 — Symlinked tool-config files.** Symlinks at the target path are followed; backups and writes hit the resolved target. If the resolved target lies outside `$HOME` (or `--home`), claudecm refuses with a clear error pointing at the resolved path.
- **NFR-S3 — HOME redirection sanity.** claudecm derives `$HOME` from the environment, validates it is an existing absolute path owned by the current user, and supports `--home <path>` as an explicit override for test/sandbox scenarios. If `$HOME` is unset and `--home` is not provided, claudecm refuses.
- **NFR-S4 — Refuse on partial profile YAML.** Profile YAML missing required fields (`schema_version`, `name`) is rejected on read; the active-profile pointer is never set to an invalid profile.
- **NFR-S5 — Path-traversal hardening.** Profile names are validated against `[A-Za-z0-9._-]{1,64}` (FR-1) AND used as the sole input to profile-file path construction. The same validation gates any tool-config target path construction derived from profile data, eliminating profile-name → tool-file path-traversal.
- **NFR-S6 — Overlay-as-truth on switch.** **Decision: overlay-as-truth.** On `switch`, owned keys are computed entirely from the new profile (core + overlay). If the new profile omits an owned key that was previously set, the adapter resets that owned key to its built-in default (or removes it where the tool treats absence as "use default"). Non-owned keys remain untouched via merge-preserve. This is the only switch semantics; "merge-preserve owned keys from the prior profile" is explicitly not supported. Surfaced in `switch` pre-apply diff as "key reset to default".
- **NFR-S7 — Comment & ordering preservation.** TOML adapters MUST use a parser that preserves comments and key order (e.g., `pelletier/go-toml/v2` document model). JSON adapters MUST use a parser that preserves key order and supports comments where the tool accepts JSONC. Where preservation is impossible for a given key (e.g., re-ordering forced by parser limitations), `switch` MUST print a "comments/order may shift in <file>" warning before writing. Loss is never silent.
- **NFR-S8 — Default secret redaction.** `list`, `current`, and `explain` redact `api_key` and any field tagged `secret: true` by default (display as `sk-***last4` or `***`). `--reveal` opts in to plaintext display and emits a stderr notice. `export` does NOT redact by default (it is intended for `eval`), but supports `--redact` for capture-to-log scenarios.

### 11.5 Concurrency NFRs

- **NFR-C1 — File locks.** Each tool-config file write acquires an advisory `flock(LOCK_EX)` on the target path with a 5-second timeout (configurable via `--lock-timeout`). On timeout, the operation aborts with a clear "another claudecm appears to be writing this file" error. The Codex `auth.json`, Codex `config.toml`, and Claude Code `settings.json` each have their own lock; lock acquisition order is `auth.json → config.toml → settings.json` and release is reverse.
- **NFR-C2 — Concurrent-edit detection.** Between the FR-5 read and the FR-5 rename, claudecm records the original file's size + mtime + content hash. If any of these changed at write time, the write is aborted, the backup is retained, the user is told which backup to inspect, and exit code 2 is returned.
- **NFR-C3 — First-write semantics.** When the tool-config file does not yet exist, the write uses `open(... O_CREAT | O_EXCL ...)` on the temp file and a `rename()` against the missing target. If the file appears between FR-5 steps 2 and 7, the write is aborted as a concurrent-edit conflict.

### 11.6 Backup Retention NFRs

- **NFR-R1 — Retention.** Per tool-config file, claudecm retains the last **N=10** backups. Older backups are pruned at the end of a successful write, oldest-first. Pruning writes an audit log entry to `~/.config/claudecm/audit.log` (mode `0600`) recording deleted backup paths and timestamps. `--retention <int>` overrides the default per-invocation.
- **NFR-R2 — Pruning never deletes the file claudecm just wrote.** Pruning only operates on files matching `<owned-file>.bak.*`.
- **NFR-R3 — `restore` ignores retention.** `restore --list` shows whatever backups are present; if N=10 is reduced post-hoc, no backups are deleted retroactively.

### 11.7 EnvOverride NFRs

- **NFR-E1 — Enumerated env-var allowlist per tool.** The `EnvOverride` layer of the resolution chain considers ONLY the following variables:
  - **Claude Code:** `ANTHROPIC_API_KEY`, `ANTHROPIC_AUTH_TOKEN`, `ANTHROPIC_BASE_URL`, `ANTHROPIC_MODEL`, `ANTHROPIC_SMALL_FAST_MODEL`, `CLAUDE_CODE_USE_BEDROCK`, `CLAUDE_CODE_USE_VERTEX`.
  - **Codex CLI:** `OPENAI_API_KEY`, `OPENAI_BASE_URL`, `CODEX_HOME`, `CODEX_MODEL`, `CODEX_MODEL_PROVIDER`.
- Other env vars do not participate in `explain`'s `EnvOverride` layer (they may still be displayed by `explain --all-env` for diagnostic purposes, but they do not shadow on-disk config).
- The allowlist is published in code as a single exported list per adapter and exercised by the CI fixture matrix (NFR-T1).

### 11.8 Schema/Migration NFRs

- **NFR-M1 — Schema versioning.** Every stored profile carries `schema_version: 1`. On read, claudecm:
  - Accepts `schema_version: 1`.
  - Rejects missing `schema_version` with a clear error.
  - Refuses to operate on `schema_version >= 2` with a clear "this profile was written by a newer claudecm; upgrade or remove" error. Never silently misreads.
- **NFR-M2 — Migration policy.** Post-v1 schema bumps land with explicit, opt-in migration commands. v1 contains no implicit migrations.

### 11.9 Testing NFRs

- **NFR-T1 — CI fixture matrix (v1 acceptance gate).** A pinned `testdata/` corpus per tool MUST exist and be exercised on every PR:
  - **Happy paths:** canonical minimal config, canonical maximal config.
  - **Edge cases:** missing file, partial file, unknown keys mixed with owned keys, BOM (UTF-8 BOM), CRLF line endings, mixed indentation, comments in TOML/JSONC, symlinked target, target on a separate filesystem.
  - **Round-trip:** for every fixture, `import → switch → export → explain` round-trips with FR-5 invariants preserved and owned keys byte-identical.
  - **Concurrent-edit:** simulated mtime/size change between read and write triggers NFR-C2 abort and retains backup.
  - **Two-phase failure:** simulated failure of Claude Code write after Codex commit triggers FR-16 rollback.
  - The fixture matrix passing is a v1 release gate. SM-2 and SM-4 are measured against this matrix.

### 11.10 Documentation NFRs

- **NFR-D1 — AES/encryption claim scrub (v1 release gate).** A separate doc-cleanup PR (tracked outside this PRD) MUST remove every AES-256, "secure storage", and cryptographic-protection claim from `README.md`, `brief.md`, `project-brief.md`, and `architecture.md` before v1 ships. v1 cannot ship while any such claim remains in the canonical docs tree.

### 11.11 Storage and Security Posture (unchanged from prior draft, restated)
Profiles are stored as plaintext YAML in the user's config directory, files `0600`, directory `0700`. **Encryption is deferred post-v1.** Public docs, README, and marketing copy must not claim AES-256, "secure storage" beyond filesystem perms, or any cryptographic guarantee in v1.

### 11.12 Module Path and Naming
The Go module path is `github.com/a2d2-dev/claudecm`. All imports, install instructions, and CI pin to this path. `claudecm` is the product name as well as the binary name.

## 12. Developer Product Considerations

### Public Surface
The primary public surface in v1 is the locked CLI command set (§4.6) and the unified profile model (§4.1). The owned-key allowlists (§4.7) are part of the public contract.

### Versioning and Deprecation
v1 prefers simple, explicit commands and stable profile semantics, because future multi-tool growth will depend on users trusting that saved profiles remain understandable and migratable. The profile schema is versioned via `schema_version: 1`.

### Performance Budget
- `list`, `current`, `switch` on warm cache: effectively instantaneous (< 200 ms wall clock for `list`/`current`; switch end-to-end < 2 s per SM-1).
- `add`, `edit`, `rename`, `delete`: bound by interactive prompts, not I/O.
- `import`, `explain`: may do slightly more work but must remain terminal-friendly and fully local.
- End-to-end `switch` + verify across both tools: < 2 s (SM-1).

## 13. Decision Log

- **2026-06-30 — Direction Lock applied** per `docs/decisions/0001-direction-lock.md` (ADR-0001).
  - V1 tool scope reduced to **Claude Code + Codex CLI** only.
  - Profile schema locked: **unified core fields + per-tool overlay map** `tools: { claude_code?, codex? }`.
  - Command surface locked.
  - Activation primary path locked to direct write with atomic temp+rename, `0600`, timestamped backup-before-write, merge-preserve of unknown keys; `export` retained as secondary path.
  - `explain` resolution chain locked.
  - Storage locked to plaintext YAML, `0600` files, `0700` dir; encryption deferred post-v1.
  - Module path and product name locked to `github.com/a2d2-dev/claudecm`.

- **2026-07-01 — Second-pass PRD revision** closing 12 blocking validator findings:
  1. **`restore` added** to the v1 command surface (FR-12). UJ-4 expanded to cover restore-after-regret.
  2. **FR-5 rewritten** as the full single write-path invariant: lock → read → parse → resolve symlink → diff → backup → atomic temp+rename → post-write reparse → auto-rollback on parse failure. Every write command in §4.6 is required to route through it.
  3. **Owned-key allowlist (§4.7) added** per tool: explicit, enumerated, frozen, and gated by the CI fixture matrix (NFR-T1). Unknown/unowned keys are preserved byte-for-byte where the parser allows.
  4. **`--dry-run` added** to every write command (FR-15). `switch` requires `--yes` or interactive confirm when the diff touches keys claudecm does not own (FR-4).
  5. **`switch` pre-apply diff** required before any write (FR-4). **Story-ordering note:** `explain` (FR-7) MUST ship and be acceptance-tested BEFORE `switch` (FR-4, FR-5) lands, because `switch` reuses `explain`-style rendering for its pre-apply diff.
  6. **Concurrency** policy added (NFR-C1, NFR-C2, NFR-C3): file locks via `flock`, post-write reparse + size/mtime + content-hash check, abort on concurrent-edit and retain backup as primary, first-write `O_EXCL` semantics.
  7. **Cross-file two-phase write with rollback** added as FR-16: if Codex commits and Claude Code fails (or vice versa), already-committed targets are restored from their FR-5 backups; partial failure is reported cleanly.
  8. **Per-tool file ownership** enumerated (§4.7): Claude Code = user-scope `~/.claude/settings.json`; Codex = `~/.codex/config.toml` and `~/.codex/auth.json`. Project-scope Claude Code settings explicitly out for v1.
  9. **Profile schema** now requires `schema_version: 1` (FR-1, NFR-M1). Migration policy: refuse on unknown future versions; never silently misread.
  10. **CI fixture matrix** (NFR-T1) added as a v1 acceptance gate: pinned `testdata/`, happy + edge cases (missing file, partial file, unknown keys, BOM, CRLF, comments, symlinks), round-trip `import → switch → export → explain`, concurrent-edit simulation, two-phase failure simulation.
  11. **Success metrics reconciled with ADR-0001.** SM-1 restored to **< 2s end-to-end**, SM-2 restored to **≥ 95% import round-trip on sampled configs**, SM-3 kept as activation safety (zero corruption), SM-4 restored to **100% explain fixture correctness**. SM-5 added for **README quickstart ≤ 3 min** per validator request. The prior draft's deviations from ADR-0001 are reverted, not preserved.
  12. **Policy NFRs added:**
      - Backup retention N=10 per file with audit log (NFR-R1, NFR-R2, NFR-R3).
      - Symlinks: follow + resolved target inside `$HOME` or `--home` (NFR-S2).
      - `--home` flag + `$HOME` sanity check (NFR-S3).
      - Refuse-on-malformed tool config (NFR-S1).
      - Profile-name path-traversal hardening also gates target path construction (NFR-S5).
      - TOML/JSON comment & ordering preservation with loud warning when lost (NFR-S7).
      - **Overlay-as-truth on switch** chosen and documented (NFR-S6) — owned keys reset to default when new profile omits them.
      - EnvOverride env-var allowlist enumerated per tool (NFR-E1).
      - Default secret redaction in `list`/`current`/`explain` unless `--reveal` (NFR-S8).
      - `edit` UX: default `$EDITOR` launcher with validation on save; `--set key=value` for scriptable use (FR-2).
      - Non-interactive `import --name --yes --overwrite` (FR-8).
      - Doc cleanup of AES/encryption claims tracked as a v1 release-gate item (NFR-D1).

- **2026-07-01 — Policy choices made by PM without ADR-0001 backing** (recorded here for traceability):
  - **Overlay-as-truth on switch** (NFR-S6). Rationale: matches kubecm context-switch mental model and produces predictable, testable diffs; "merge-with-prior-owned-keys" would silently carry state across switches.
  - **`edit` defaults to `$EDITOR` with re-parse validation** and adds `--set` for scripts (FR-2). Rationale: flag-only `--set` is awkward for nested overlays; editor flow is what real users will hit. Scripts still have a path.
  - **Project-scope Claude Code settings files OUT for v1** (§5). Rationale: scope discipline; the v1 surface is the user-scope file, which is what `cc-switch`-class workflows actually touch.
  - **N=10 backups per file** as the retention default (NFR-R1). Rationale: bounded disk use, enough history for "two switches ago" recovery, overrideable per invocation.
