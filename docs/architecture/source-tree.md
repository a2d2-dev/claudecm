# Source Tree (v1, planned)

> This tree is **planned**, not yet materialized. The architecture PR introduces this layout as the contract for subsequent dev stories; no `.go` files are added by the architecture PR itself. Anything not listed here is not part of v1.

```plaintext
claudecm/
├── cmd/                          # Cobra command definitions (one file per command)
│   ├── root.go                   # root command + global flags (--home, --lock-timeout, --retention, --reveal)
│   ├── add.go                    # FR-1: create profile (--dry-run; activates via commit if requested)
│   ├── list.go                   # FR-3: list profiles (--output json, default redaction, --reveal)
│   ├── current.go                # FR-6: current active profile + per-tool effective summary; reports drift
│   ├── switch.go                 # FR-4: switch active profile (--dry-run, --yes, --strict); routes through commit
│   ├── explain.go                # FR-7: full resolution chain incl. shadowed layers (per NFR-E1 allowlist)
│   ├── import.go                 # FR-8: subcommands `claude-code`, `codex` (--name, --yes, --overwrite, --dry-run)
│   ├── export.go                 # FR-9: emits `export VAR=...`; --format yaml; --redact
│   ├── edit.go                   # FR-2: $EDITOR flow with re-parse on save; --set key=value (repeatable); --dry-run
│   ├── rename.go                 # FR-2: rename (validated against profile-name regex)
│   ├── delete.go                 # FR-2: delete (confirmation; --yes; clears active pointer if needed)
│   ├── restore.go                # FR-12: --list, --latest, --id, --dry-run, --yes; routed through writepath
│   ├── completion.go             # FR-10: bash/zsh/fish/powershell; profile-name completion
│   └── version.go                # FR-11: semver + commit + build date from pkg/version
├── internal/                     # Private packages — not importable by other modules
│   ├── adapter/
│   │   ├── claudecode/           # Adapter for Claude Code
│   │   │   ├── adapter.go        # Implements internal/adapter.Adapter
│   │   │   ├── files.go          # OwnedFile list: ~/.claude/settings.json (user scope only)
│   │   │   ├── allowlist.go      # Frozen owned-key allowlist (env.ANTHROPIC_*, env.CLAUDE_CODE_USE_*)
│   │   │   ├── render.go         # Profile -> owned-key map (pure)
│   │   │   ├── import.go         # bytes -> CoreFromTool + OverlayFromTool (pure)
│   │   │   └── jsoncodec.go      # tidwall/sjson + gjson surgical edits
│   │   └── codex/                # Adapter for Codex CLI
│   │       ├── adapter.go        # Implements internal/adapter.Adapter
│   │       ├── files.go          # OwnedFile list: ~/.codex/config.toml, ~/.codex/auth.json
│   │       ├── allowlist.go      # Frozen owned-key allowlists for both files
│   │       ├── render.go         # Profile -> owned-key map per file (pure)
│   │       ├── import.go         # bytes -> CoreFromTool + OverlayFromTool (pure)
│   │       ├── tomlcodec.go      # pelletier/go-toml/v2 doc-model (comment + order preserving)
│   │       └── authcodec.go      # JSON codec for auth.json
│   ├── config/                   # Profile, CoreConfig, ToolOverlay; schema_version handling
│   │   ├── profile.go            # Profile struct + YAML tags
│   │   ├── core.go               # CoreConfig
│   │   ├── overlay.go            # ToolOverlay + ToolID
│   │   ├── state.go              # State + AppliedFingerprint (LastAppliedPerTool)
│   │   └── schema.go             # SchemaVersion=1 enforcement; reject-missing; refuse->=2
│   ├── storage/                  # Filesystem primitives — only path to disk besides writepath
│   │   ├── paths.go              # ~ expansion, --home override, HOME sanity, symlink resolve, profile-name regex
│   │   ├── atomic.go             # temp+fsync+rename; O_CREAT|O_EXCL on first write
│   │   ├── backup.go             # backup path layout; backup verification
│   │   ├── retention.go          # N=10 prune oldest-first; audit-log emission
│   │   └── lock.go               # gofrs/flock wrapper; lock-order helpers
│   ├── writepath/                # The single FR-5 contract: Apply(WritePlan) (ApplyReport, error)
│   │   ├── apply.go              # lock -> read -> parse -> resolve symlink -> diff -> backup -> atomic write -> reparse -> auto-rollback
│   │   ├── plan.go               # WritePlan struct (owned keys, parsed prior, intended post)
│   │   ├── diff.go               # owned-key-scoped diff used by --dry-run and FR-4 pre-apply
│   │   └── fingerprint.go        # size+mtime+sha256 concurrency fingerprint (NFR-C2)
│   ├── commit/                   # Two-phase cross-file commit (FR-16)
│   │   ├── commit.go             # Stage + Commit; lock-order, rename-order = auth -> config -> settings
│   │   ├── rollback.go           # On phase-2 failure, restore committed targets from FR-5 backups
│   │   └── report.go             # PartialFailure with per-file status (committed/rolled-back/untouched)
│   ├── resolver/                 # Layer chain + EffectiveView (FR-7)
│   │   ├── resolver.go           # EnvOverride > on-disk > overlay > core > default
│   │   ├── view.go               # EffectiveField with WinningLayer + ShadowedLayers
│   │   └── drift.go              # Compare on-disk sha256 vs State.LastAppliedPerTool[tool].SHA256
│   ├── envextract/               # EnvOverride lookups for the resolver (existing package, retained)
│   │   ├── claudecode.go         # NFR-E1 Claude Code allowlist
│   │   └── codex.go              # NFR-E1 Codex CLI allowlist
│   ├── export/                   # Secondary activation: emit shell `export VAR=...` lines
│   │   ├── shell.go              # eval $(claudecm export)
│   │   └── yaml.go               # --format yaml
│   └── ui/                       # Interactive prompts + redaction helpers
│       ├── prompt.go             # survey-backed prompts
│       ├── confirm.go            # FR-4 confirmation prompt
│       └── redact.go             # api_key -> sk-***last4
├── pkg/
│   └── version/                  # semver + commit + build date (set via -ldflags)
│       └── version.go
├── testdata/                     # CI fixture matrix (NFR-T1) — release gate
│   ├── claudecode/
│   │   ├── happy/                # canonical minimal, canonical maximal
│   │   └── edge/                 # missing-file, partial, unknown-keys, BOM, CRLF, comments, symlink, cross-fs
│   └── codex/
│       ├── happy/
│       └── edge/
├── docs/
│   ├── architecture.md
│   ├── architecture/
│   │   ├── coding-standards.md
│   │   ├── source-tree.md
│   │   └── tech-stack.md
│   ├── decisions/
│   │   └── 0001-direction-lock.md
│   └── prd/
│       └── prd-v1.md
├── .github/
│   └── workflows/
│       ├── ci.yaml               # lint + test + fixture matrix
│       └── release.yaml          # goreleaser
├── .goreleaser.yaml
├── .golangci.yaml
├── go.mod                        # module github.com/a2d2-dev/claudecm
├── go.sum
├── main.go                       # only call site for panic; wires logger; defers to cmd.Execute()
├── Makefile
├── README.md
└── LICENSE
```

## Where Each PRD Requirement Lives

| PRD ref | Package | File(s) |
|---------|---------|---------|
| FR-1 add | `cmd/`, `internal/config`, `internal/storage` | `add.go`, `profile.go`, `atomic.go` |
| FR-2 edit/rename/delete | `cmd/`, `internal/config`, `internal/ui` | `edit.go`, `rename.go`, `delete.go`, `prompt.go` |
| FR-3 list/current | `cmd/`, `internal/resolver`, `internal/ui` | `list.go`, `current.go`, `redact.go` |
| FR-4 switch (+ pre-apply diff) | `cmd/`, `internal/commit`, `internal/writepath` | `switch.go`, `commit.go`, `diff.go` |
| FR-5 locked write-path | `internal/writepath`, `internal/storage` | `apply.go`, `atomic.go`, `backup.go`, `lock.go` |
| FR-6 current | `cmd/`, `internal/resolver` | `current.go`, `resolver.go` |
| FR-7 explain | `cmd/`, `internal/resolver`, `internal/envextract` | `explain.go`, `view.go`, adapters' env files |
| FR-8 import | `cmd/`, adapters | `import.go`, `adapter/<tool>/import.go` |
| FR-9 export | `cmd/`, `internal/export` | `export.go`, `shell.go`, `yaml.go` |
| FR-10 completion | `cmd/` | `completion.go` |
| FR-11 version | `cmd/`, `pkg/version` | `version.go` |
| FR-12 restore | `cmd/`, `internal/writepath`, `internal/storage` | `restore.go`, `apply.go`, `backup.go` |
| FR-15 --dry-run | every write command + `internal/writepath` | `*.go`, `plan.go`, `diff.go` |
| FR-16 two-phase commit | `internal/commit` | `commit.go`, `rollback.go`, `report.go` |
| NFR-C1 file locks | `internal/storage/lock.go` | `lock.go` |
| NFR-C2 concurrent-edit | `internal/writepath/fingerprint.go` | `fingerprint.go`, `apply.go` |
| NFR-C3 first-write | `internal/storage/atomic.go` | `atomic.go` |
| NFR-S1 refuse on malformed | adapters + `internal/writepath` | `apply.go`, codec files |
| NFR-S2 symlink resolution | `internal/storage/paths.go` | `paths.go` |
| NFR-S3 HOME sanity + --home | `internal/storage/paths.go`, `cmd/root.go` | `paths.go`, `root.go` |
| NFR-S5 path traversal | `internal/storage/paths.go`, `internal/config/profile.go` | `paths.go`, `profile.go` |
| NFR-S6 overlay-as-truth | adapters' `render.go` | per adapter |
| NFR-S7 comment/order preserve | `tomlcodec.go`, `jsoncodec.go` | per adapter |
| NFR-S8 secret redaction | `internal/ui/redact.go` | `redact.go` |
| NFR-R1/R2/R3 retention + audit | `internal/storage/retention.go` | `retention.go` |
| NFR-E1 env allowlist | `internal/envextract` | `claudecode.go`, `codex.go` |
| NFR-M1 schema_version | `internal/config/schema.go` | `schema.go` |
| NFR-T1 fixture matrix | `testdata/`, every package | `testdata/<tool>/{happy,edge}/` |

## What's Explicitly Not Here

- No `internal/crypto/` — encryption is deferred post-v1 (ADR-0001 Decision 6).
- No adapter for Gemini CLI / Cursor / Windsurf / IDE plugins — out of v1 scope.
- No MCP / Skills / sync / proxy / cloud subpackages — out of v1 scope.
- No GUI / TUI package — CLI only.
- No `afero` shim — we use the real filesystem behind `internal/storage` and test through `t.TempDir()`.
