#!/usr/bin/env bash
# lint-project-scope.sh — enforces per-tool file-ownership discipline:
# tool-owned config paths are NEVER referenced from production Go code
# outside the owning adapter file(s).
#
# claudecm owns four HOME-rooted paths across two tools:
#
#   Claude Code (user scope):
#     ~/.claude/settings.json
#   Codex CLI:
#     ~/.codex/config.toml
#     ~/.codex/auth.json
#
# PRD §4.7 and architecture.md §3.1 put every other path — including
# the project-scope Claude Code siblings —
#
#   <project>/.claude/settings.json
#   <project>/.claude/settings.local.json
#
# explicitly out of v1 scope. This script is the "grep guard" that
# Story E3-S2 introduced and Story E4-S1 extended to cover Codex's
# two owned files.
#
# Rules
# =====
#
#   R1. The string "settings.local.json" must not appear anywhere in
#       cmd/ or internal/ Go files, period. Not in code, not in
#       comments. Tests are exempt so they can *assert* the absence.
#
#   R2. Any file that references ".claude/settings.json" or the Go
#       two-arg join pattern `".claude", "settings.json"` must be on
#       the Claude-Code whitelist below. Tests are exempt (they build
#       fixtures under a temp HOME).
#
#   R3. Any file that references ".codex/config.toml", ".codex/auth.json",
#       or the Go two-arg join patterns that produce them must be on
#       the Codex whitelist below. Same rationale as R2: the owning
#       adapter is the sole legal place to name these paths. Tests
#       are exempt.
#
# Exit codes: 0 = clean, 1 = one or more forbidden hits.

set -uo pipefail

here="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
repo="$(cd -- "$here/.." && pwd)"
cd "$repo"

# Whitelist for R2 — files that may legitimately mention the
# HOME-rooted ~/.claude/settings.json path (in code or doc comments).
# Each entry is a repo-relative path; extend deliberately, with
# review, when a new owner emerges.
whitelist=(
  "internal/adapter/claudecode/adapter.go"
  "internal/adapter/claudecode/allowlist.go"
  # plan.go (E3-S4) renders the sjson merge-preserve transform that
  # writes ~/.claude/settings.json under the FR-5 write-path. It is
  # a legitimate owner of the HOME-rooted user-scope path — its
  # package godoc references settings.json to document scope.
  "internal/adapter/claudecode/plan.go"
  "internal/adapter/adapter.go"
  "internal/storage/lock.go"
)

# Whitelist for R3 — files that may legitimately mention Codex's two
# HOME-rooted owned paths (~/.codex/config.toml, ~/.codex/auth.json).
# Kept intentionally separate from R2 so a Claude Code whitelist entry
# does not silently authorise a Codex-path reference and vice-versa.
codex_whitelist=(
  "internal/adapter/codex/adapter.go"
  "internal/adapter/codex/allowlist.go"
  # import.go (E4-S3) reads ~/.codex/config.toml and ~/.codex/auth.json;
  # its package godoc names both paths to document scope.
  "internal/adapter/codex/import.go"
  "internal/adapter/adapter.go"
)

fail=0

# ---------------------------------------------------------------- R1
# settings.local.json is a project-scope-only string. It is never a
# HOME-rooted path (~/.claude/ has no settings.local.json sibling
# claudecm owns), so any mention in prod code is a bug.
r1_hits="$(grep -rEn --include='*.go' 'settings\.local\.json' -- 'cmd' 'internal' 2>/dev/null \
  | grep -v '_test\.go' \
  || true)"

if [ -n "$r1_hits" ]; then
  echo "lint-project-scope: forbidden 'settings.local.json' reference(s):" >&2
  echo "$r1_hits" >&2
  echo >&2
  echo "The project-scope .claude/settings.local.json file is out of" >&2
  echo "v1 scope (PRD §4.7). Production code must never name it." >&2
  echo >&2
  fail=1
fi

# ---------------------------------------------------------------- R2
# Match either the literal path "<something>.claude/settings.json"
# or the Go join pattern that produces it: `".claude", "settings.json"`.
# Both shapes can, in principle, be used to build a project-scope
# path if the caller supplies a non-HOME anchor.
r2_hits="$(grep -rEn --include='*.go' -e '\.claude/settings\.json' -e '"\.claude", *"settings\.json"' -- 'cmd' 'internal' 2>/dev/null \
  | grep -v '_test\.go' \
  || true)"

if [ -n "$r2_hits" ]; then
  # Filter out whitelisted files.
  bad=""
  while IFS= read -r line; do
    [ -z "$line" ] && continue
    # grep -n output shape: <path>:<lineno>:<content>
    path="${line%%:*}"
    keep=1
    for w in "${whitelist[@]}"; do
      if [ "$path" = "$w" ]; then
        keep=0
        break
      fi
    done
    if [ "$keep" -eq 1 ]; then
      bad+="${line}"$'\n'
    fi
  done <<< "$r2_hits"

  if [ -n "$bad" ]; then
    echo "lint-project-scope: reference(s) to .claude/settings.json outside the owning file(s):" >&2
    printf '%s' "$bad" >&2
    echo >&2
    echo "Only the following files may build or document" >&2
    echo "~/.claude/settings.json (user-scope, HOME-rooted):" >&2
    for w in "${whitelist[@]}"; do
      echo "  - $w" >&2
    done
    echo >&2
    echo "If a new owner needs to be added, extend the whitelist in" >&2
    echo "scripts/lint-project-scope.sh with a review comment." >&2
    echo >&2
    fail=1
  fi
fi

# ---------------------------------------------------------------- R3
# Match either the literal paths ".codex/config.toml" / ".codex/auth.json"
# or the Go join patterns that produce them:
#   `".codex", "config.toml"` / `".codex", "auth.json"`.
# Symmetric with R2 but scoped to the Codex adapter's two owned files.
r3_hits="$(grep -rEn --include='*.go' \
    -e '\.codex/config\.toml' \
    -e '\.codex/auth\.json' \
    -e '"\.codex", *"config\.toml"' \
    -e '"\.codex", *"auth\.json"' \
    -- 'cmd' 'internal' 2>/dev/null \
  | grep -v '_test\.go' \
  || true)"

if [ -n "$r3_hits" ]; then
  bad=""
  while IFS= read -r line; do
    [ -z "$line" ] && continue
    path="${line%%:*}"
    keep=1
    for w in "${codex_whitelist[@]}"; do
      if [ "$path" = "$w" ]; then
        keep=0
        break
      fi
    done
    if [ "$keep" -eq 1 ]; then
      bad+="${line}"$'\n'
    fi
  done <<< "$r3_hits"

  if [ -n "$bad" ]; then
    echo "lint-project-scope: reference(s) to Codex-owned paths outside the owning file(s):" >&2
    printf '%s' "$bad" >&2
    echo >&2
    echo "Only the following files may build or document" >&2
    echo "~/.codex/config.toml and ~/.codex/auth.json (HOME-rooted):" >&2
    for w in "${codex_whitelist[@]}"; do
      echo "  - $w" >&2
    done
    echo >&2
    echo "If a new owner needs to be added, extend codex_whitelist in" >&2
    echo "scripts/lint-project-scope.sh with a review comment." >&2
    echo >&2
    fail=1
  fi
fi

if [ "$fail" -ne 0 ]; then
  exit 1
fi

echo "lint-project-scope: OK (no out-of-scope tool-owned paths in production code)"
