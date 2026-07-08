# Quickstart

Target: **≤ 3 minutes** from install to first successful `switch` on a warm machine, no maintainer assistance (PRD SM-5).

## Prerequisites

- Go 1.21+ on `$PATH`.
- Either [Claude Code](https://docs.claude.com/en/docs/claude-code) or [Codex CLI](https://github.com/openai/codex) installed and previously run at least once (so its config file exists).

If neither tool is installed, install one first — claudecm's job is to swap their configs, not to install them.

## 1. Install claudecm

```bash
go install github.com/a2d2-dev/claudecm@latest
```

Expected: `go install` completes silently; `claudecm version` prints a version line.

## 2. Import your current tool config

Pick whichever tool you already have set up. Both commands are non-interactive with `--yes`.

```bash
claudecm import claude-code --name existing --yes
```

or

```bash
claudecm import codex --name existing --yes
```

Expected: `imported profile "existing" from claude-code` (or `codex`). A profile file is now at `~/.claudecm/profiles/existing.yaml` with file mode `0600`.

## 3. Add a second profile

```bash
claudecm add work \
  --base-url https://api.anthropic.com \
  --api-key sk-ant-xxxxxxxx \
  --model claude-opus-4-5
```

Expected: `Profile "work" created.`.

You can also start from a built-in provider preset. Presets are convenience templates, not official provider support, certification, endorsement, or compatibility guarantees. They fill generated fields such as `base_url`, `model`, `provider`, and supported tool overlays; you still supply the secret, and every generated field can be overridden.

```bash
claudecm add work --preset moonshot --api-key sk-ant-xxxxxxxx --dry-run
```

Expected: a redacted profile draft, not a write. The generated fields are visible:

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

Then save it:

```bash
claudecm add work --preset moonshot --api-key sk-ant-xxxxxxxx
```

Available presets: `moonshot`, `deepseek`, `glm`, `qwen`. Use `claudecm add --list-presets` to inspect the current catalog. Endpoint and model names can drift, so override with `--base-url`, `--model`, `--provider`, or `--set` when a provider changes its API.

You can also build a draft from pasted text. This is local by default: `--from-text` runs the local extractor, redacts secrets in `--dry-run`, and does not use the network.

```bash
claudecm add work \
  --from-text 'ANTHROPIC_BASE_URL=https://api.anthropic.com ANTHROPIC_AUTH_TOKEN=sk-ant-xxxxxxxx ANTHROPIC_MODEL=claude-opus-4-5' \
  --dry-run
```

Use `--from-text -` to read stdin:

```bash
cat provider-snippet.txt | claudecm add work --from-text - --dry-run
```

If the local extractor is not enough, `--ai` is opt-in per invocation. claudecm strips secret-shaped tokens locally, keeps captured secrets in-process, and sends only the desensitized text in one Anthropic-compatible Messages request using the active profile's credentials, or `--ai-profile <name>` if you choose another credential-lending profile. Interactive runs show the exact desensitized payload before sending.

```bash
claudecm add work --from-text 'messy provider note with sk-ant-xxxxxxxx' --ai --dry-run
```

> **Name rules.** Profile names must match `^[a-z0-9][a-z0-9._-]{0,63}$` (NFR-S5). If `claudecm add` fails with a profile-name error, that regex is the reason — no uppercase, no leading dot/dash, ≤ 64 characters.

## 4. Switch to the second profile

```bash
claudecm switch work --yes
```

Expected: a pre-apply diff summary, followed by `Switched to "work".`. Behind the scenes claudecm has:

1. Locked the target tool files.
2. Backed up the current contents to `~/.claudecm/backups/<tool>/<file>/<timestamp>`.
3. Written both `~/.claude/settings.json` and `~/.codex/config.toml` (and `auth.json` if owned) atomically.
4. Reparsed each written file. If any reparse failed, both files were restored from the pre-Stage in-memory bytes (rollback).

> **First switch.** The first `switch` for each tool creates the first entry in `~/.claudecm/backups/`. `claudecm restore --list` will surface them.

> **Interactive switch.** In a real terminal, bare `claudecm switch` opens an optional fuzzy profile selector with a redacted preview. This is terminal-only convenience UX; scripts, CI, non-TTY stdin/stdout, and `claudecm switch <name> --yes` keep the stable v1 command behavior.

## 5. Verify

```bash
claudecm current
```

Expected: two lines, one per tool, showing the active profile name and the resolved base URL / model.

```bash
claudecm explain work
```

Expected: a per-tool table of every owned key with its winning layer (env / on-disk / profile / default) plus the shadowed layers underneath. Secrets are redacted as `sk-***<last4>` unless you pass `--reveal` (NFR-S8).

## 6. Switch back

```bash
claudecm switch existing --yes
claudecm current
```

Expected: `current` now shows `existing` as active for both tools.

---

## What went wrong?

If any step above misbehaves, in order of usefulness:

1. **`claudecm explain <name>`** — the fastest way to see whether your problem is an env var overriding the profile, a stale on-disk key, or a profile field you didn't intend. Every layer is visible.
2. **`claudecm restore --list`** — every write goes through a backup step (FR-5). This lists timestamped snapshots per owned file. `claudecm restore --file <path> --at <timestamp>` reverts.
3. **`~/.claudecm/audit.log`** — one line per write. Includes which command ran, which files were touched, and the backup timestamp. This is the ground truth when `explain` and memory disagree.

## Notes

- Storage: profiles are plaintext YAML at `~/.claudecm/profiles/*.yaml`, mode `0600`, directory `0700`. No cryptographic protection in v1 (ADR-0001 §Locked Decisions, NFR-D1). If your threat model requires vault-grade storage, wrap `import` / `export` around your existing secret manager.
- Sandboxing: every command accepts `--home <dir>` to redirect `$HOME`. Useful for testing on the same machine without touching your real config.
- Dry-run: `switch`, `add`, `edit`, `import`, `restore` all accept `--dry-run` and print the write plan without touching disk.
