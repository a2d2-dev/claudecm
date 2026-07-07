#!/usr/bin/env bash
set -euo pipefail

fail() {
  echo "release-smoke: $*" >&2
  exit 1
}

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
bin_path="${repo_root}/bin/claudecm-smoke"
version="${VERSION:-v0.0.0-smoke}"
commit="${COMMIT:-smoke}"
date="${BUILD_DATE:-2026-01-01T00:00:00Z}"
version_pkg="github.com/a2d2-dev/claudecm/pkg/version"

home_dir="$(mktemp -d)"
cleanup() {
  rm -rf "${home_dir}"
  rm -f "${bin_path}"
}
trap cleanup EXIT

mkdir -p "${repo_root}/bin" "${home_dir}/.claude" "${home_dir}/.codex"
printf '{}\n' > "${home_dir}/.claude/settings.json"
printf 'model = "old-model"\n' > "${home_dir}/.codex/config.toml"

(
  cd "${repo_root}"
  CGO_ENABLED=0 go build \
    -ldflags "-X ${version_pkg}.Version=${version} -X ${version_pkg}.Commit=${commit} -X ${version_pkg}.Date=${date}" \
    -o "${bin_path}" .
)

run_cli() {
  HOME="${home_dir}" "${bin_path}" --home "${home_dir}" "$@"
}

version_out="$(run_cli version)"
[[ "${version_out}" == *"claudecm ${version}"* ]] || fail "version output missing ${version}: ${version_out}"
[[ "${version_out}" == *"commit: ${commit}"* ]] || fail "version output missing commit ${commit}: ${version_out}"
[[ "${version_out}" == *"built: ${date}"* ]] || fail "version output missing build date ${date}: ${version_out}"

completion_out="$(run_cli completion bash)"
[[ "${completion_out}" == *"complete -o default -F __start_claudecm claudecm"* ]] || fail "bash completion output did not contain claudecm registration"

add_out="$(run_cli add work \
  --base-url https://api.example.com \
  --api-key sk-smoke-1234567890 \
  --model smoke-model)"
[[ "${add_out}" == 'Profile "work" created.' ]] || fail "add output mismatch: ${add_out}"

list_out="$(run_cli list)"
[[ "${list_out}" == *"work"* ]] || fail "list output missing work profile: ${list_out}"

switch_out="$(run_cli switch work --yes)"
[[ "${switch_out}" == *'Switched to "work".'* ]] || fail "switch output missing success line: ${switch_out}"

current_out="$(run_cli current)"
[[ "${current_out}" == *"Profile: work"* ]] || fail "current output missing active profile: ${current_out}"
[[ "${current_out}" == *"smoke-model"* ]] || fail "current output missing smoke model: ${current_out}"

echo "release-smoke: passed"
