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

claudecm does **not** sync your configuration to the cloud, is **not** a proxy or gateway for Claude Code or Codex traffic, does **not** support Gemini CLI / Cursor / Windsurf / other IDE plugins in v1, and does **not** encrypt profiles at rest — they are plaintext YAML at file mode `0600` under `~/.claudecm/` (deferred post-v1 per ADR-0001 and PRD NFR-D1). The only networked onboarding path is `add --from-text ... --ai`, which is explicit per run, requires an interactive terminal to review and confirm the desensitized payload, and sends one locally desensitized parse request using your chosen claudecm profile credentials. If you need vault-grade secret storage, wire claudecm's `import`/`export` around your existing secret manager instead.

## Install

Package-manager installs are available from the next tagged release onward.

macOS with Homebrew:

```bash
brew tap a2d2-dev/tap
brew install claudecm
claudecm version
claudecm --help
```

Windows with Scoop:

```powershell
scoop bucket add a2d2-dev https://github.com/a2d2-dev/homebrew-tap
scoop install claudecm
claudecm version
claudecm --help
```

From source:

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

# Or start from a convenience provider preset and inspect the generated
# fields before saving. Built-in presets include moonshot, deepseek,
# glm, and qwen; secrets still come from you.
claudecm add work --preset moonshot --api-key sk-ant-xxxxxxxx --dry-run
claudecm add work --preset moonshot --api-key sk-ant-xxxxxxxx

# Or paste a provider snippet. This path is local by default and does not
# use the network; --dry-run previews the redacted draft without writing.
claudecm add work --from-text 'ANTHROPIC_BASE_URL=https://api.anthropic.com ANTHROPIC_AUTH_TOKEN=sk-ant-xxxxxxxx' --dry-run

# Optional AI parse is opt-in per invocation and requires an interactive TTY.
# claudecm strips secret-shaped tokens locally, shows the desensitized payload
# for confirmation, then sends one Anthropic-compatible messages request.
claudecm add work --from-text 'messy provider note with sk-ant-xxxxxxxx' --ai --dry-run

# 3. Switch.
claudecm switch work --yes
# In an interactive terminal, bare `claudecm switch` opens a fuzzy
# profile selector. Scripts should keep using `claudecm switch <name> --yes`.

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
| `claudecm switch` | Optional terminal-only fuzzy selector; non-TTY scripts keep the v1 usage error. |
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

## Provider Presets

`claudecm add --preset <name>` expands a built-in convenience template into an ordinary profile. The current catalog is `moonshot`, `deepseek`, `glm`, and `qwen`.

Example dry-run output shows the generated fields with secrets redacted:

```bash
claudecm add work --preset moonshot --api-key sk-ant-xxxxxxxx --dry-run
```

```yaml
core:
  provider: moonshot
  base_url: https://api.moonshot.cn/v1
  api_key: sk-a***xxxx
  model: kimi-k2-0711-preview
tools:
  codex:
    raw:
      model: kimi-k2-0711-preview
      model_provider: moonshot
      model_providers.moonshot.base_url: https://api.moonshot.cn/v1
      model_providers.moonshot.env_key: OPENAI_API_KEY
      model_providers.moonshot.name: Moonshot AI
      model_providers.moonshot.wire_api: chat
```

Presets are convenience templates, not official provider support, certification, endorsement, or compatibility guarantees. Every generated field is overridable with explicit flags or `--set`; provider endpoints and model names can drift, so edit the profile when a provider changes its API.

## Smart add inputs

`claudecm add` can also build drafts from existing local material:

- `--from-env` reads the Claude Code / Codex environment-variable allowlist.
- `--from-file <path>` parses dotenv, shell, JSON, YAML, or TOML config files.
- `--from-text <text>` or `--from-text -` parses pasted text with local heuristics.

These paths are local-first. Without `--ai`, pasted text never leaves the machine. `--ai` is an explicit escalation for `--from-text`: claudecm requires an interactive terminal, runs the local redaction pass first, shows the full desensitized payload for confirmation, keeps captured secrets in-process, sends only the confirmed desensitized text to an Anthropic-compatible Messages endpoint using the active profile's credentials (or `--ai-profile <name>`), then re-injects the secret locally before the normal redacted preview/save path. Non-interactive or piped `--ai` runs refuse before any parse request is sent.

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
