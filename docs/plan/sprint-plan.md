# claudecm v1 — Sprint Plan (Implementation Order)

> Source of truth for the order in which stories enter dev. Story content lives in `docs/plan/stories/`. Epic-level rationale lives in `docs/plan/epics.md`.

## Ordering rules (locked)

1. **E1 (Foundations) lands before everything.** Profile schema + storage substrate is the bedrock.
2. **E2 (WritePath) must complete before E3/E4** — adapters cannot Apply without the write contract.
3. **E5 explain story ships before E6 switch story** (PRD §13 second-pass note 5: switch's pre-apply diff reuses explain-style rendering).
4. **E7 (Two-phase commit) lands before any multi-file `switch` story** — no story may sequence multiple `writepath.Apply` calls directly.
5. **E8 fixture matrix for each adapter lands together with that adapter's Apply story** — adding an adapter without its fixture is not a complete change.
6. **E9 README rewrite runs in parallel with E5/E6** — not on the blocking path.

## Numbered implementation queue

1. **E1-S1** — Unified Profile schema (core + sparse overlay) with `schema_version: 1`
2. **E1-S2** — `internal/storage/paths.go`: `~` expand, `--home`, HOME sanity, profile-name regex, outside-`$HOME` refuse
3. **E1-S3** — `internal/storage/atomic.go`: temp+fsync+rename, `O_CREAT|O_EXCL` first-write
4. **E1-S4** — `internal/storage/backup.go`: `<file>.bak.<ISO8601>.<short-uuid>` with size verification
5. **E1-S5** — `internal/storage/retention.go`: N=10 prune + `~/.claudecm/audit.log`
6. **E1-S6** — `internal/storage/lock.go`: `gofrs/flock` wrapper + `--lock-timeout`
7. **E1-S7** — `~/.claudecm/` layout migration (`profiles/`, `state.yaml`, `backups/<tool>/`, `audit.log`)
8. **E2-S1** — `WritePlan`, `ApplyReport`, `OwnedFile`, `KeyPath` types
9. **E2-S2** — `writepath.Apply` core pipeline (lock → read → parse-via-adapter → symlink resolve → diff → backup → atomic rename)
10. **E2-S3** — Post-write reparse + auto-rollback from FR-5 step-6 backup
11. **E2-S4** — Concurrent-edit detection (`size + mtime + sha256` fingerprint, exit code 2)
12. **E2-S5** — Synthetic-adapter unit test matrix (≥ 80% coverage on `internal/writepath`)
13. **E3-S1** — `Adapter` interface contract + `internal/adapter/` skeleton
14. **E3-S2** — `claudecode`: `Detect` + `Files` + owned-key allowlist as Go `var`
15. **E3-S3** — `claudecode`: `Import` (read `~/.claude/settings.json`, project into Core + Overlay)
16. **E3-S4** — `claudecode`: `Plan` + render via `sjson`/`gjson` (preserve order + non-owned bytes)
17. **E3-S5** — `claudecode`: `Apply` (calls `writepath.Apply`)
18. **E3-S6** — `claudecode`: `Project` → `EffectiveView` (moved ahead of E3-S7 2026-07-01 per readiness audit: `Project` must exist before the fixture matrix can verify effective-view rendering against it)
19. **E3-S7** — `claudecode` fixture matrix (lands with E3-S5 per rule 5: happy + edge: BOM, CRLF, comments, symlink, missing, unknown-keys-mixed)
20. **E4-S1** — `codex`: `Detect` + `Files` (both `config.toml` + `auth.json`) + per-file owned-key allowlist `var`s
21. **E4-S2** — TOML parser plumbing via `pelletier/go-toml/v2` doc model (comment + order preservation)
22. **E4-S3** — `codex`: `Import` (read `config.toml` + `auth.json`)
23. **E4-S4** — `codex`: `Plan` + render for both files
24. **E4-S5** — `codex`: `Apply` (per-file; cross-file ordering deferred to E7)
25. **E4-S6** — `codex`: `Project` → `EffectiveView` (moved ahead of E4-S7 2026-07-01 per readiness audit, mirrors the E3-S6/S7 fix)
26. **E4-S7** — `codex` fixture matrix (lands with E4-S5 per rule 5)
27. **E5-S1** — Resolver types: `EffectiveView`, `EffectiveField`, `ShadowEntry`, `Layer`
28. **E5-S2** — `resolver.Resolve` layered chain implementation
29. **E5-S3** — EnvOverride per-tool allowlist (NFR-E1) + `envextract` wiring
30. **E5-S4** — External-drift detection (`State.LastAppliedPerTool[tool].SHA256` cross-check)
31. **E5-S5** — `cmd/explain` (winning + shadowed layers, redaction, `--reveal`, JSON output) **← must precede E6-S2**
32. **E7-S1** — `internal/commit`: `Commit` interface + `StagedTxn` types
33. **E7-S2** — `commit.Stage` (phase 1: FR-5 steps 1–7 with deferred rename)
34. **E7-S3** — `commit.Commit` (phase 2: ordered rename `auth.json → config.toml → settings.json` + post-write reparse)
35. **E7-S4** — Rollback path: rename FR-5 step-6 backups over already-committed targets
36. **E7-S5** — `PartialFailure` structured error + per-file `committed | rolled-back | untouched` enumeration
37. **E6-S1** — `cmd/current` (active profile + per-tool effective summary + drift warning)
37a. **E6-S10** — `cmd/list` (all profiles + active marker + default redaction + `--reveal` + `--output json`; refuses on corrupt store) (inserted 2026-07-01 per readiness audit — slots alongside `current` because both are read-only inventory commands; depends only on E1-S1 + E1-S7)
38. **E6-S2** — `cmd/switch` refactor: pre-apply diff via `Plan`, `--dry-run`, `--yes`, routed through `commit.Commit` **← needs E5-S5 + E7 complete**
39. **E6-S3** — `cmd/add` refactor to unified schema + `--dry-run`
40. **E6-S4** — `cmd/import {claude-code|codex}` with `--name`, `--yes`, `--overwrite`, `--dry-run`
41. **E6-S5** — `cmd/edit`: `$EDITOR` default + `--set key=value` + `--dry-run`
42. **E6-S6** — `cmd/rename` + `cmd/delete` (`--yes`, clear active pointer)
43. **E6-S7** — `cmd/restore`: `--list`, `--latest`, `--id`, `--dry-run`, `--yes`
44. **E6-S8** — `cmd/export` refactor (secondary path; `--format yaml`, `--redact`)
45. **E6-S9** — `cmd/completion` + `cmd/version` + global `--reveal` / `--home` / `--lock-timeout` / `--retention`
46. **E8-S1** — `testdata/` corpus scaffolding (lands incrementally with E3-S7 / E4-S7; this story tracks the gate)
47. **E8-S2** — CI workflow + ≥ 80% coverage gate on `internal/writepath`, `internal/commit`, `internal/resolver`
48. **E8-S3** — CI round-trip smoke: `import → switch → export → explain` byte-identical on owned-key scope
49. **E8-S4** — Concurrent-edit simulation test per owned file (NFR-C2)
50. **E8-S5** — Two-phase rollback simulation test (FR-16)
51. **E8-S6** — AES-claim scrub release gate marker (NFR-D1) — referenced **closed via PR #3**; CI grep guard for "AES" / "secure storage" added
52. **E9-S1** — README rewrite (cc-switch voice) **← runs in parallel with E5/E6, not blocking**
53. **E9-S2** — Quickstart for SM-5 ≤ 3 min
54. **E9-S3** — `explain` + `switch --dry-run` asciinema demo
55. **E9-S4** — Decision-log update (PRD §13)

## Parallelism notes

- E9-S1 (README rewrite) starts as soon as E5-S5 (`explain`) ships and runs alongside E6.
- E8-S2/S3/S4/S5 land after the code they verify; E8-S6 is a CI guard and can land any time.
- Within E1, stories can land in two parallel tracks: storage primitives (S2–S6) and schema (S1, S7), but S1 must precede any commands that read profiles.

## BLOCKED at queue creation

None. Every story above has a path to dev-ready once its `blockedBy` deps clear. Re-evaluate after each sprint.
