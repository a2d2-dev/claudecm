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
