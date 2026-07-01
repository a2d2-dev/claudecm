#!/usr/bin/env bash
# lint-osrename.sh — enforces Story E2-S2 AC #5 os.Rename discipline.
#
# Only two files in this repo may call os.Rename on tool-owned paths:
#
#   - internal/storage/atomic.go   (temp+fsync+rename primitive)
#   - internal/writepath/apply.go  (write-path pipeline; may reference
#                                   the primitive in comments and via
#                                   its own rename-on-swap in E2-S3+)
#
# Any other production hit indicates an escape from the locked
# write-path. _test.go files are allowed to os.Rename fixtures.
#
# Exit codes: 0 = clean, 1 = one or more forbidden hits.

set -uo pipefail

# Locate repo root (this script lives at scripts/).
here="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
repo="$(cd -- "$here/.." && pwd)"
cd "$repo"

# grep -rEn returns 1 on no match; capture output ourselves.
hits="$(grep -rEn 'os\.Rename\(' -- 'cmd' 'internal' 2>/dev/null || true)"

# Filter allowed lines.
forbidden="$(printf '%s\n' "$hits" \
  | grep -v '^$' \
  | grep -v 'internal/storage/atomic.go' \
  | grep -v 'internal/writepath/apply.go' \
  | grep -v '_test\.go' \
  || true)"

if [ -n "$forbidden" ]; then
  echo "lint-osrename: forbidden os.Rename call(s) outside the write-path:" >&2
  echo "$forbidden" >&2
  echo >&2
  echo "Only internal/storage/atomic.go and internal/writepath/apply.go" >&2
  echo "may call os.Rename on tool-owned paths (Story E2-S2 AC #5)." >&2
  exit 1
fi

echo "lint-osrename: OK (os.Rename discipline holds)"
