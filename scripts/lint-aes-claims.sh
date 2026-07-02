#!/usr/bin/env bash
# lint-aes-claims.sh — enforces Story E8-S6 (NFR-D1) release gate.
#
# v1 stores plaintext YAML/JSON/TOML at 0600 with no cryptographic
# guarantee. Public docs, READMEs, and marketing copy must NOT claim
# AES-256, "secure storage", or "encryption at rest" for v1. This
# script is the CI grep guard that makes that policy non-regressible.
#
# Two checks:
#
#   C1 — grep -REni 'AES-?256|secure storage|encryption at rest'
#        over README.md, docs/, cmd/, internal/, pkg/ and fail on any
#        hit not covered by scripts/lint-aes-claims-allowlist.txt.
#
#   C2 — grep for `import "crypto/aes"` under cmd/, internal/, pkg/
#        and fail on any hit. Introducing a crypto library requires
#        an explicit post-v1 ADR; until then the guard is absolute.
#
# The allowlist file is line-oriented extended-regex; blank/comment
# lines are stripped before use.
#
# Exit codes: 0 = clean, 1 = one or more forbidden claims found.

set -uo pipefail

here="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
repo="$(cd -- "$here/.." && pwd)"
cd "$repo"

allowlist_src="$here/lint-aes-claims-allowlist.txt"
if [ ! -f "$allowlist_src" ]; then
  echo "lint-aes-claims: allowlist not found at $allowlist_src" >&2
  exit 1
fi

# Strip comments + blanks into a runtime allowlist file for grep -f.
allowlist_run="$(mktemp)"
trap 'rm -f "$allowlist_run"' EXIT
grep -Ev '^\s*(#|$)' "$allowlist_src" > "$allowlist_run" || true

fail=0

# ---------------------------------------------------------------- C1
# Search targets: README.md at repo root plus the four doc/code trees.
# Some trees may not exist in all snapshots (e.g. pkg/); pass only
# those that do so grep does not error on missing paths.
targets=()
[ -f README.md ] && targets+=("README.md")
for d in docs cmd internal pkg; do
  [ -d "$d" ] && targets+=("$d")
done

if [ "${#targets[@]}" -eq 0 ]; then
  echo "lint-aes-claims: no search targets found (repo layout unexpected)" >&2
  exit 1
fi

# grep -REni returns 1 on no match; capture output either way.
c1_hits="$(grep -REni 'AES-?256|secure storage|encryption at rest' -- "${targets[@]}" 2>/dev/null || true)"

if [ -n "$c1_hits" ]; then
  # If the allowlist is empty, do not pass -f (grep -f with an empty
  # pattern file matches every line, which would silently swallow
  # every hit).
  if [ -s "$allowlist_run" ]; then
    c1_forbidden="$(printf '%s\n' "$c1_hits" | grep -Evf "$allowlist_run" || true)"
  else
    c1_forbidden="$c1_hits"
  fi

  if [ -n "$c1_forbidden" ]; then
    echo "lint-aes-claims: forbidden cryptographic-claim string(s) found:" >&2
    printf '%s\n' "$c1_forbidden" >&2
    echo >&2
    echo "v1 stores plaintext at 0600 with no cryptographic guarantee." >&2
    echo "Public copy must not claim AES-256, 'secure storage', or" >&2
    echo "'encryption at rest' (PRD NFR-D1, ADR-0001 Decision 6," >&2
    echo "coding-standards rule 14, Story E8-S6). Scrub the claim or," >&2
    echo "if the line legitimately names the string to DENY it (e.g." >&2
    echo "an ADR/PRD §Non-Goals clause), extend the allowlist at:" >&2
    echo "  scripts/lint-aes-claims-allowlist.txt" >&2
    echo >&2
    fail=1
  fi
fi

# ---------------------------------------------------------------- C2
# Any production import of crypto/aes is a policy failure at v1: it
# implies a cryptographic guarantee we explicitly defer post-v1.
# Tests are exempt only to the extent they might reference the string
# in a scrub-guard test itself; keep the check strict and rely on the
# ADR process to lift it.
c2_targets=()
for d in cmd internal pkg; do
  [ -d "$d" ] && c2_targets+=("$d")
done

if [ "${#c2_targets[@]}" -gt 0 ]; then
  c2_hits="$(grep -REn --include='*.go' 'crypto/aes' -- "${c2_targets[@]}" 2>/dev/null || true)"
  if [ -n "$c2_hits" ]; then
    echo "lint-aes-claims: forbidden crypto/aes reference(s) in production Go code:" >&2
    printf '%s\n' "$c2_hits" >&2
    echo >&2
    echo "Introducing a crypto library requires an explicit post-v1" >&2
    echo "ADR that lifts NFR-D1. Until then the guard is absolute." >&2
    echo >&2
    fail=1
  fi
fi

if [ "$fail" -ne 0 ]; then
  exit 1
fi

echo "lint-aes-claims: OK (no forbidden cryptographic claims)"
