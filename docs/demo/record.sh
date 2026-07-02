#!/usr/bin/env bash
# record.sh — regenerate the 60-second asciinema cast for the claudecm README.
#
# Story E9-S3. Produces docs/demo/claudecm-demo.cast showing:
#   1. `claudecm explain` with the default redaction (sk-***last4) visible.
#   2. `claudecm switch --dry-run` printing the pre-apply diff.
#
# The cast is fully sandboxed: --home /tmp/claudecm-demo means no real
# credentials, no real ~/.claude / ~/.codex writes. Rerun any time; the
# script is idempotent (it wipes the sandbox before recording).
#
# Requirements (host machine):
#   - asciinema        https://docs.asciinema.org
#   - claudecm         (on $PATH; `go install github.com/a2d2-dev/claudecm@latest`)
#   - a real TTY (do NOT run under `nohup`, `tmux new-session -d`, etc.)
#
# Usage:
#   bash docs/demo/record.sh
#
# Then commit docs/demo/claudecm-demo.cast.

set -euo pipefail

SANDBOX="/tmp/claudecm-demo"
CAST_OUT="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)/claudecm-demo.cast"

command -v asciinema >/dev/null 2>&1 || {
  echo "record.sh: asciinema not on PATH; install from https://docs.asciinema.org" >&2
  exit 1
}
command -v claudecm >/dev/null 2>&1 || {
  echo "record.sh: claudecm not on PATH; run 'go install github.com/a2d2-dev/claudecm@latest'" >&2
  exit 1
}

# Clean sandbox so the cast starts from a known state.
rm -rf "$SANDBOX"
mkdir -p "$SANDBOX/.claude" "$SANDBOX/.codex" "$SANDBOX/.claudecm"

# Seed a Claude Code settings file so `import claude-code` has something to
# read. Values are obviously synthetic — the redaction rule (NFR-S8) will
# render the token as sk-***xxxx in the cast.
cat > "$SANDBOX/.claude/settings.json" <<'JSON'
{
  "env": {
    "ANTHROPIC_BASE_URL": "https://api.anthropic.com",
    "ANTHROPIC_AUTH_TOKEN": "sk-ant-demo0000000000demo",
    "ANTHROPIC_MODEL": "claude-sonnet-4-5"
  }
}
JSON

# Seed a Codex config so both tools have something to diff against.
cat > "$SANDBOX/.codex/config.toml" <<'TOML'
model = "gpt-5"

[model_providers.openai]
name = "openai"
base_url = "https://api.openai.com/v1"
TOML

cat > "$SANDBOX/.codex/auth.json" <<'JSON'
{"OPENAI_API_KEY": "sk-openai-demo0000demo"}
JSON

# Small helper: type-out delay so the cast reads at a human pace.
run() {
  echo "\$ $*"
  "$@"
  echo
}

# What the cast will exercise (~60s wall-clock at ~2s/step):
#   1. Import both tools into starting profiles.
#   2. Add a second profile.
#   3. `explain` on the second profile — shows redacted sk-***last4.
#   4. `switch --dry-run` — shows the pre-apply diff, writes nothing.
#
# `asciinema rec` records the terminal until the wrapped script exits.
asciinema rec \
  --overwrite \
  --title "claudecm: explain + switch --dry-run (sandboxed)" \
  --command "bash -c '
    set -e
    export CLAUDECM_HOME=$SANDBOX
    run() { echo \"\\\$ \$*\"; \"\$@\"; echo; sleep 1; }
    run claudecm --home $SANDBOX import claude-code --name existing --yes
    run claudecm --home $SANDBOX import codex        --name existing --yes
    run claudecm --home $SANDBOX add work --base-url https://api.anthropic.com --api-key sk-ant-work00000000work --model claude-opus-4-5
    run claudecm --home $SANDBOX explain work
    run claudecm --home $SANDBOX switch  work --dry-run
    sleep 1
  '" \
  "$CAST_OUT"

echo
echo "record.sh: wrote $CAST_OUT"
echo "record.sh: commit the .cast file and update the README embed."
