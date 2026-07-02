#!/usr/bin/env bash
# check-coverage.sh — enforces Story E8-S2 per-package coverage floors.
#
# Parses a Go coverprofile (`go test -coverprofile=coverage.out`) and
# aggregates statement coverage per package, then fails if any of the
# three locked packages fall below their v1 floor:
#
#   - internal/writepath  >= 80%
#   - internal/commit     >= 80%
#   - internal/resolver   >= 80%
#
# The three packages carry the FR-5 write-path (writepath), the two-phase
# multi-file orchestrator (commit), and the layer-chain resolver
# (resolver). Together they form the "silent regression must be
# impossible" surface described in PRD NFR-T1 / SM-3 and Architecture §11.
#
# All other packages are printed for visibility but are not gated.
#
# Usage:  bash scripts/check-coverage.sh coverage.out
#
# Exit codes:
#   0  every gated package meets its floor
#   1  at least one gated package is under floor, or usage error

set -uo pipefail

# ------------------------------------------------------------ config
# Gated packages: repo-relative directories. The awk script maps every
# file line back to a directory by stripping /<filename>.go, so keep
# these as directory suffixes matching the coverprofile output.
gated_pkgs=(
  "internal/writepath"
  "internal/commit"
  "internal/resolver"
)
floor=80

# ------------------------------------------------------------ inputs
if [ "$#" -ne 1 ]; then
  echo "usage: $0 <coverprofile>" >&2
  exit 1
fi
profile="$1"

if [ ! -f "$profile" ]; then
  echo "check-coverage: coverprofile not found: $profile" >&2
  exit 1
fi

# ------------------------------------------------------------ parse
# coverprofile line shape: <import-path>/<file>.go:<L>.<C>,<L>.<C> <numStmts> <count>
#
# Aggregate per package (drop /<file>.go from the path). Print two
# columns: "<pkg>\t<percent>" sorted by package. All arithmetic uses
# awk float ops; % is formatted to one decimal place.
per_pkg="$(awk '
  NR == 1 { next }                       # skip "mode: set"
  {
    # split path from position:  path.go:pos ...
    n = index($1, ":")
    if (n == 0) next
    file = substr($1, 1, n - 1)
    # strip trailing /<name>.go
    m = match(file, /\/[^/]+\.go$/)
    if (m == 0) next
    pkg = substr(file, 1, m - 1)
    stmts = $2 + 0
    cnt   = $3 + 0
    total[pkg] += stmts
    if (cnt > 0) covered[pkg] += stmts
  }
  END {
    for (p in total) {
      if (total[p] == 0) {
        pct = 0
      } else {
        pct = (covered[p] * 100.0) / total[p]
      }
      printf("%s\t%.1f\n", p, pct)
    }
  }
' "$profile" | sort)"

if [ -z "$per_pkg" ]; then
  echo "check-coverage: no coverage entries parsed from $profile" >&2
  exit 1
fi

# ------------------------------------------------------------ print
echo "check-coverage: per-package statement coverage"
echo "----------------------------------------------"
printf '%s\n' "$per_pkg" | awk -F'\t' '{ printf("  %-70s %6s%%\n", $1, $2) }'
echo "----------------------------------------------"

# ------------------------------------------------------------ gate
fail=0
for want in "${gated_pkgs[@]}"; do
  # Find the row whose package ends with the gated suffix.
  row="$(printf '%s\n' "$per_pkg" | awk -F'\t' -v suf="/$want" '
    { if (index($1, suf) == length($1) - length(suf) + 1) { print; exit } }
  ')"
  if [ -z "$row" ]; then
    echo "check-coverage: FAIL — no coverage entry for gated package $want" >&2
    fail=1
    continue
  fi
  pct="$(printf '%s' "$row" | awk -F'\t' '{ print $2 }')"
  # Compare using awk to avoid bash float pitfalls.
  under="$(awk -v p="$pct" -v f="$floor" 'BEGIN { print (p + 0 < f + 0) ? 1 : 0 }')"
  if [ "$under" = "1" ]; then
    echo "check-coverage: FAIL — $want coverage $pct% is below floor ${floor}%" >&2
    fail=1
  else
    echo "check-coverage: OK   — $want coverage $pct% (floor ${floor}%)"
  fi
done

if [ "$fail" -ne 0 ]; then
  echo >&2
  echo "One or more gated packages fell below the ${floor}% floor. See" >&2
  echo "PRD NFR-T1 / SM-3 and Architecture §11 for the release-gate contract." >&2
  exit 1
fi

echo "check-coverage: OK (all gated packages meet the ${floor}% floor)"
