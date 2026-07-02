# claudecm

**One-command switch between your Claude Code and Codex CLI configurations.**

## What it does

- Switches between named "profiles" of Claude Code + Codex configuration.
- Direct-writes to `~/.claude/settings.json` and `~/.codex/{config.toml,auth.json}` — no shims, no wrappers, no proxy.
- Merge-preserves unknown keys: anything claudecm doesn't own stays byte-for-byte where you left it (comments and key ordering preserved where the parser allows).
- Atomic two-phase commit with automatic rollback if any target fails post-write reparse.
- `explain` shows exactly which env var / on-disk value / profile field wins for each key, layer by layer.

## Who it's for

People juggling multiple Anthropic or OpenAI accounts, relay endpoints, or work-vs-personal setups: freelancers with several clients, teams routing through a paid API relay, contributors switching between an org key and a personal key, or anyone testing model behavior across regions or providers. If you've been hand-editing `~/.claude/settings.json` or `~/.codex/config.toml` and lost track of which key was active, this is for you.

## Not for

claudecm does **not** sync your configuration to the cloud, is **not** a proxy or gateway (it never sees a request), does **not** support Gemini CLI / Cursor / Windsurf / other IDE plugins in v1, and does **not** encrypt profiles at rest — they are plaintext YAML at file mode `0600` under `~/.claudecm/` (deferred post-v1 per ADR-0001 and PRD NFR-D1). If you need vault-grade secret storage, wire claudecm's `import`/`export` around your existing secret manager instead.

## Install

```bash
go install github.com/a2d2-dev/claudecm@latest
```

## Quickstart

Target: **≤ 3 minutes** from install to first successful switch (PRD SM-5).

```bash
# 1. Import whatever config you already have on disk into a starting profile.
claudecm import claude-code --name existing --yes
# or:  claudecm import codex --name existing --yes

# 2. Add a second profile for the other account/endpoint.
claudecm add work \
  --base-url https://api.anthropic.com \
  --api-key sk-ant-xxxxxxxx \
  --model claude-opus-4-5

# 3. Switch.
claudecm switch work --yes

# 4. Confirm what's live.
claudecm current

# 5. Ask claudecm to prove where every effective key came from.
claudecm explain work
```

See [docs/quickstart.md](docs/quickstart.md) for a longer walk-through with expected output per step and a "what went wrong?" panel.

## Commands

| Command | What it does |
|---|---|
| `claudecm add <name> [flags]` | Create a profile in the unified schema. |
| `claudecm list` | List every profile with the active one marked. |
| `claudecm current` | Compact per-tool summary of the active profile. |
| `claudecm switch <name>` | Two-phase commit both tool files to the named profile. |
| `claudecm explain <name>` | Full per-tool resolution chain (winning + shadowed layers). |
| `claudecm import claude-code\|codex` | Seed a profile from existing on-disk tool config. |
| `claudecm edit <name>` | Open profile in `$EDITOR`, or use `--set key=value`. |
| `claudecm rename <old> <new>` | Rename a profile. |
| `claudecm delete <name>` | Delete a profile. |
| `claudecm restore` | List backups or revert owned files to a chosen snapshot. |
| `claudecm export [name]` | Emit shell exports or YAML (read-only secondary activation). |
| `claudecm completion <shell>` | Emit a shell completion script. |
| `claudecm version` | Print version, commit, and build date. |

Global flags: `--home <dir>` (override `$HOME` for sandboxed runs), `--yes`, `--dry-run` (write commands).

## Deeper reading

- [`docs/prd/prd-v1.md`](docs/prd/prd-v1.md) — v1 PRD, functional + non-functional requirements, decision log.
- [`docs/architecture.md`](docs/architecture.md) — package layout, write-path invariants, adapter model.
- [`docs/decisions/`](docs/decisions/) — ADRs, starting with ADR-0001 (Direction Lock).

## Contributing

PRs welcome. Two guardrails to know:

- `scripts/lint-aes-claims.sh` runs in CI (story E8-S6, NFR-D1). It scrubs any reintroduction of cryptographic marketing claims in `README.md`, `docs/`, `cmd/`, `internal/`, or `pkg/`. v1 is plaintext at `0600`; if you need to say otherwise, land an ADR first.
- `scripts/lint-project-scope.sh` and `scripts/lint-osrename.sh` enforce ADR-0001 scope and the atomic-rename invariant respectively.

Run `make dev-build` before pushing.

## License

[MIT](LICENSE)
