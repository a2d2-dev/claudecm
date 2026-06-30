# Tech Stack (v1)

> Subordinate to ADR-0001 and PRD v1. Every entry here is justified by a concrete requirement; rows that were aspirational under prior drafts (encryption, cloud, GUI) are removed because the matching features are out of v1 scope.

## Core Technologies

| Category | Technology | Version | Purpose | PRD ref |
|----------|------------|---------|---------|---------|
| Language | Go | as pinned in `go.mod` (currently 1.21) | Primary language | — |
| Module path | `github.com/a2d2-dev/claudecm` | n/a | All imports, install, CI | ADR-0001 Dec. 7 |
| CLI framework | `github.com/spf13/cobra` | v1.8.0 | Command parsing, tab completion | FR-1..FR-12, FR-10 |
| Interactive prompts | `github.com/AlecAivazis/survey/v2` | v2.3.7 | $EDITOR fallback prompts, switch confirmation, delete confirmation | FR-2, FR-4 |
| Profile/state format | `gopkg.in/yaml.v3` | v3.0.1 | Profile + state YAML | FR-1, NFR-M1 |
| TOML (Codex `config.toml`) | `github.com/pelletier/go-toml/v2` | latest stable | Comment + key-order preserving parser; doc-model API for surgical edits | NFR-S7, §4.7 Codex allowlist |
| JSON (Claude Code `settings.json`) | `github.com/tidwall/sjson` + `github.com/tidwall/gjson` | latest stable | Surgical, order-preserving JSON edits on owned keys only; stdlib `encoding/json` reorders + drops formatting | NFR-S7, §4.7 Claude Code allowlist |
| File locks | `github.com/gofrs/flock` | latest stable | Advisory `flock(LOCK_EX)` per owned file | NFR-C1 |
| Test framework | `testing` (stdlib) + `github.com/stretchr/testify` | v1.9.0 | Unit + integration | NFR-T1 |
| Build | `go build` (stdlib) | n/a | Single static binary | — |
| Release | goreleaser | 1.24.x | Multi-arch tarballs + checksums + Homebrew tap | — |
| CI | GitHub Actions | n/a | Lint + test + fixture-matrix gate | NFR-T1 |
| Linter | golangci-lint | 1.55.x | Style + bugs (`gocyclo`, `goconst`, `errcheck`, `revive`, `misspell`) | Coding standards |
| Vuln scan | `govulncheck` | latest | Dependency CVE check on every PR | — |

## go.mod (target end-state for v1)

```go
require (
    github.com/spf13/cobra v1.8.0
    github.com/AlecAivazis/survey/v2 v2.3.7
    gopkg.in/yaml.v3 v3.0.1
    github.com/pelletier/go-toml/v2 vX.Y.Z      // added in adapter/codex story
    github.com/tidwall/sjson vX.Y.Z             // added in adapter/claudecode story
    github.com/tidwall/gjson vX.Y.Z             // transitive of sjson; explicit pin
    github.com/gofrs/flock vX.Y.Z               // added in internal/storage story
    github.com/stretchr/testify v1.9.0          // test only
)
```

Versions for the four new dependencies are picked at PR time on the adapter / storage stories; the contract is that they are released stable versions with no open CVEs.

## What Was Removed and Why

| Removed | Reason |
|---------|--------|
| `crypto/aes`, any encryption library | Encryption is deferred post-v1 (ADR-0001 Dec. 6, PRD §5, NFR-D1). No cryptographic claims anywhere. |
| `github.com/spf13/afero` | We don't need an in-memory FS shim; tests use `t.TempDir()` and the real `internal/storage` paths. |
| `log/slog` row | We use stdlib `log/slog` (it ships with Go); it's not a separate dependency and does not warrant a row. |
| "Provider: GitHub Gist / S3 / self-hosted" post-MVP cloud row | Cloud sync is out of v1 and not committed to in v2 — no architecture hooks reserved. |
| "Survey/Bubbletea" TUI row | TUI is out of v1; Survey covers the small interactive prompts we need. |

## Cross-Platform Notes

- **Symlink resolution** uses stdlib `filepath.EvalSymlinks`; resolved targets outside `$HOME`/`--home` are refused (NFR-S2). Windows symlink semantics are surfaced as warnings, not errors, in v1.
- **Atomic rename** uses `os.Rename`, which is atomic on POSIX same-filesystem renames. The temp file is created in the same directory as the resolved target to guarantee that.
- **File locks** use `gofrs/flock` which abstracts `flock(2)` (POSIX) and `LockFileEx` (Windows).
- **File modes** (`0600`/`0700`) are best-effort on Windows; we still re-assert them but document that they are not strict ACL controls there.

## Choice Justifications (architect decisions, recorded for traceability)

- **TOML = pelletier/go-toml/v2.** The PRD explicitly suggests this library by name in NFR-S7. Its doc-model API preserves comments and key order, which is required to honor non-owned keys and inline comments in `~/.codex/config.toml`.
- **JSON = tidwall/sjson + gjson.** `encoding/json` does not preserve key order or whitespace; `sjson` does surgical edits without disturbing surrounding structure. This is the minimum viable way to satisfy "preserve key order and support JSONC where the tool accepts it" for `~/.claude/settings.json` while only mutating the owned-key allowlist.
- **No second YAML library.** Profile + state YAML stays on `yaml.v3`; we don't bring `goccy/go-yaml` or similar.
- **No `viper`.** Cobra alone is sufficient for the v1 flag surface; `viper` adds config-discovery behavior we explicitly don't want (we have one config root and one state file, full stop).
