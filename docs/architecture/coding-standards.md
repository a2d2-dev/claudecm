# Coding Standards (v1)

> Subordinate to ADR-0001 and PRD v1. The rules below are not stylistic preferences; most map directly to a PRD functional or safety requirement. Violations are bugs.

## Core

- **Language & Runtime:** Go at the version pinned in `go.mod` (currently `1.21`). Do not raise the minimum without a tech-stack update.
- **Formatting:** `gofmt` and `goimports`. Enforced in CI.
- **Linting:** `golangci-lint` with `gocyclo`, `goconst`, `misspell`, `errcheck`, `revive`. Configured via `.golangci.yaml`.
- **Module path:** `github.com/a2d2-dev/claudecm`. All imports use this path. No vendored alternates.

## Naming

| Element | Convention | Example |
|---------|------------|---------|
| Packages | lowercase, single word | `config`, `storage`, `writepath` |
| Types | PascalCase | `Profile`, `WritePlan` |
| Interfaces | PascalCase, ends `-er` when natural | `Adapter`, `Resolver` |
| Functions | PascalCase (public), camelCase (private) | `Apply`, `applyOwnedKeys` |
| Constants | PascalCase | `DefaultLockTimeout` |
| Files | snake_case | `write_path.go`, `atomic.go` |
| Test files | `*_test.go` co-located with source | `apply_test.go` |
| Fixture dirs | `testdata/<tool>/<happy|edge>/<case>/` | `testdata/codex/edge/symlink/` |

## Critical Rules

These rules encode the locked invariants. Each one is testable.

1. **Every tool-config write goes through `internal/writepath.Apply`.** No command may call `os.WriteFile`, `os.Rename`, or open a tool-owned file with write intent directly. This maps to PRD FR-5. CI lint rule: forbid imports of `os.WriteFile` / `ioutil.WriteFile` outside `internal/storage` and `internal/writepath`.

2. **No silent fallback writes.** If a parse fails, refuse the operation; do not "best-effort rewrite", do not strip comments, do not reorder keys to make it work. Maps to PRD NFR-S1 and to the global rule in `~/.claude/CLAUDE.md`: "绝对禁止使用 fallback 模式" — fallback is forbidden unless explicitly requested.

3. **All path construction uses `internal/storage/paths.go`.** `~` expansion, `--home` override, symlink resolution, HOME ownership check, profile-name validation, and outside-`$HOME` refusal all live there. `filepath.Join` over user input outside this package is a violation. Maps to NFR-S3, NFR-S5.

4. **Owned-key allowlists are declared, not inferred.** Each adapter declares its owned-file list and its per-file owned-key allowlist as exported `var`s. The merge-preserve path uses those `var`s as the only source of truth. Adding a key to an allowlist requires a PR that updates both the code `var` and the fixture matrix. Maps to PRD §4.7.

5. **Atomic write or no write.** Writes go: write-to-temp → fsync → `rename`. First write against a missing file uses `O_CREAT|O_EXCL` on the temp file. Direct `os.WriteFile` to a real owned-file path is a violation. Maps to FR-5.

6. **Concurrency fingerprint must be re-checked before rename.** `(size, mtime, sha256(content))` recorded at read time must match at rename time; mismatch → abort + retain backup + exit code 2. Maps to NFR-C2.

7. **Post-write reparse is mandatory.** After `rename`, read the file back and parse it; if parse fails or any owned key is wrong, auto-rollback from the FR-5 step-6 backup. Maps to FR-5 step 8.

8. **`SchemaVersion: 1` is required on every stored profile.** Missing → reject on read. `>= 2` → refuse. There are no silent upgrades or downgrades. Maps to NFR-M1.

9. **Default secret redaction.** `list`, `current`, `explain` redact `api_key` and any field tagged `secret: true` unless `--reveal` is passed. `export` does NOT redact by default (it is built for `eval`) but accepts `--redact` for capture-to-log. Maps to NFR-S8.

10. **Bounded backup retention with audit log.** Retention is N=10 per `(tool, file)`. Every prune writes an entry to `~/.claudecm/audit.log` (mode `0600`). `restore` ignores retention; it operates on whatever backups exist. Maps to NFR-R1/R2/R3.

11. **No `panic` in library code.** `panic` is allowed only in `main()` for unrecoverable startup failures. Every fallible function returns `error`. Wrap with `fmt.Errorf("...: %w", err)` when adding context.

12. **No package-level mutable state.** Pass dependencies explicitly. The single exception is the structured logger configured in `main()`.

13. **Two-phase commit on multi-file writes.** When a single command touches more than one owned file, route through `internal/commit`. Direct sequencing of `writepath.Apply` calls across files is a violation. Maps to FR-16.

14. **No cryptographic claims.** No `crypto/aes`, no encryption-at-rest, no marketing copy mentioning AES or "secure storage" beyond filesystem perms. v1 stores plaintext at `0600`. Maps to ADR-0001 Decision 6 and NFR-D1.

## Testing Mandates

- **Fixture-based golden tests for every adapter.** `Import`, `Plan`, `Apply`, `Project` each have golden tests against `testdata/<tool>/{happy,edge}/`. Adding an adapter without these tests is not a complete change.
- **Coverage gates before v1 cut:** `internal/writepath`, `internal/commit`, `internal/resolver` ≥ 80% line coverage. CI fails below the gate.
- **Concurrent-edit simulation test** must exist and pass for every owned file (`auth.json`, `config.toml`, `settings.json`). Maps to NFR-C2.
- **Two-phase rollback simulation** must exist and pass (Codex commits → Claude Code reparse fails → Codex restored from backup). Maps to FR-16.
- Table-driven tests preferred for input-matrix coverage.

## Error Wrapping

```go
if err := writepath.Apply(plan); err != nil {
    return fmt.Errorf("switch to %s failed on %s: %w", profile.Name, plan.Path, err)
}
```

Errors crossing the CLI boundary carry: the command name, the profile name (if applicable), the target file path (if applicable), and the underlying cause via `%w`. Exit codes:

- `0` — success.
- `1` — user error (invalid name, missing profile, refused confirmation).
- `2` — concurrent-edit conflict (NFR-C2) or lock timeout (NFR-C1). Surface the backup path.
- `3` — internal error (parse failure post-rollback, IO error after retention).

## Logging

- Use a single structured logger configured in `main()`.
- **Never log API keys or auth tokens.** Use the redaction helper from `internal/ui`. Logging a key is a release-blocker bug.
- Error logs include: operation, profile name, target file, underlying error.

## File Permissions

- Profile YAML, state YAML, audit log, backup files: `0600`.
- `~/.claudecm/` and every subdirectory: `0700`.
- Mode is re-asserted on every write, not just on creation.

## What Not To Do (concrete)

```go
// FORBIDDEN: direct write to an owned file
os.WriteFile(claudeSettingsPath, data, 0600)

// FORBIDDEN: silent fallback rewrite on parse failure
parsed, err := json.Unmarshal(buf, &cfg)
if err != nil {
    cfg = defaultCfg // ❌ NFR-S1 violation
}

// FORBIDDEN: profile-name into tool-config path
tomlPath := filepath.Join(codexDir, profile.Name + ".toml") // ❌ NFR-S5 violation

// FORBIDDEN: bypass commit on multi-file switch
writepath.Apply(authPlan)
writepath.Apply(configPlan) // ❌ FR-16 violation: must go through internal/commit
```
