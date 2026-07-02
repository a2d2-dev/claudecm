# Adapter fixture corpora (Story E8-S1 gate)

This tree pins the **release-gate fixture corpora** for every adapter that
`claudecm` v1 ships (Claude Code, Codex CLI). Every case named here backs a
golden-file test in the owning adapter package; the coverage floor on
`internal/writepath`, `internal/commit`, and `internal/resolver` (E8-S2)
depends on these cases catching regressions in the FR-5 write-path and the
tool-owned key allowlists.

The actual fixture files do **not** live under a repo-root `testdata/`
directory. Go's test runner treats every `testdata/` directory as
sibling-scoped: `internal/adapter/claudecode/testdata/` is loaded by the
`claudecode` package tests, `internal/adapter/codex/testdata/` by the `codex`
package tests. Storing them next to the package under test is what makes
the goldens hermetic (no import path from one adapter test into another's
fixture set). This README documents the layout that already exists on
disk under those two adapter-scoped `testdata/` roots. Story E8-S1 was
originally worded as if the corpus lived at the repo root; the actual
placement under `internal/adapter/*/testdata/` is the right interpretation
and is what the fixture-matrix stories E3-S7 and E4-S7 produced.

---

## Layout

```
internal/adapter/
  claudecode/
    testdata/claudecode/
      happy/<case>/                # e.g. minimal, kitchen-sink, unicode-fields
      edge/<case>/                 # e.g. bom, comments, empty, missing,
                                   # typed-primitives, unknown-keys-preserved,
                                   # whitespace-only, symlink-in-home,
                                   # symlink-out-of-home
  codex/
    testdata/codex/
      happy/<case>/                # e.g. minimal, kitchen-sink,
                                   # anthropic-provider, openai-provider
      edge/<case>/                 # e.g. auth-only, config-only,
                                   # malformed-auth, malformed-config,
                                   # empty-files, whitespace-only,
                                   # unknown-keys-preserved, missing,
                                   # symlink-out-of-home,
                                   # symlink-out-of-home-auth
```

The `happy/` split holds cases the adapter must round-trip losslessly;
the `edge/` split holds cases the adapter must handle without silent data
loss (comment preservation, BOM, CRLF, symlinks, unknown keys, malformed
inputs, missing files, etc.). Case directory names are the primary
edge-characteristic label — `bom`, `whitespace-only`,
`symlink-out-of-home` — so no separate `assertions.yaml` file is required
per case; the name carries the assertion in the same way sub-tests do
in the corresponding `_test.go`.

## Files per case

Every case under either adapter contains two kinds of files:

**Inputs** — what the tool would find on disk before `claudecm` acts:

| Adapter    | Inputs                                                    |
| ---------- | --------------------------------------------------------- |
| claudecode | `settings.json` — Claude Code's user-scope config         |
| codex      | `config.toml`, `auth.json` — Codex CLI's two owned files  |

Plus, for both adapters, an in-repo `profile.yaml` that seeds the unified
schema the adapter is asked to apply.

**Expected goldens** — what the adapter must produce, under
`<case>/expected/`:

| File                                       | Meaning                                              |
| ------------------------------------------ | ---------------------------------------------------- |
| `import.json`                              | `Import()` output (unified `Profile` JSON)           |
| `project.json`                             | `Project()` output (unified `Project` snapshot)     |
| `plans.json` (or `diff.json`)              | `Plan()` output — WritePlan slice, ready for FR-5   |
| `after_apply.json` / `after_apply_*.{ext}` | Post-`Apply` on-disk file contents (per target)     |

The exact set varies by adapter (Claude Code writes one JSON file so its
after-apply golden is a single `after_apply.json`; Codex writes two so it
has separate `after_apply_auth.json` and `after_apply_config.toml`
goldens), but the four semantic slots — import, project, plan,
after-apply — are always present for happy cases and for edge cases that
survive the round-trip.

## How to add a fixture

1. Pick the adapter and split (`happy/` or `edge/`).
2. Create the case directory under
   `internal/adapter/<adapter>/testdata/<adapter>/<split>/<case>/`.
3. Seed the input files (see the table above for which files each
   adapter reads). For edge cases, mangle the input the way the case
   name says (e.g. `bom/settings.json` starts with a UTF-8 BOM).
4. Author the `profile.yaml` that describes the switch the adapter must
   apply.
5. Regenerate goldens by running the fixture tests with the update flag:

   ```bash
   go test ./internal/adapter/<adapter>/ -run TestFixtures -update-fixtures
   ```

   Both adapters implement the `-update-fixtures` flag on the fixture
   test (`internal/adapter/claudecode/fixtures_test.go`,
   `internal/adapter/codex/fixtures_test.go`).
6. **Review the regenerated goldens by hand.** The point of the corpus
   is that a change in golden content is a signal, not a rubber stamp;
   PR review must include reading the diff on every touched golden.
7. Run the full test suite (`go test ./...`) to confirm the case wires
   into the existing fixture matrix without breaking siblings.

## How to update an existing golden

Any change to a golden file MUST be justified by either:

- a paired PRD/architecture edit — if the change alters the semantics of
  an owned-key surface (adds a key to `internal/adapter/<adapter>/
  allowlist.go`, changes the merge-preserve contract, adjusts the
  read-what-you-wrote reparse expectations); this is
  **coding-standards rule 4** and is not optional. Any owned-key
  change requires the paired PRD edit in the same PR.
- a paired code change — if the change fixes a bug in the adapter or the
  write-path. In this case the diff on the golden must be the natural
  consequence of the code change and must be described in the commit
  message.

Procedure:

1. Make the code or PRD edit that motivates the golden update.
2. Re-run `go test ./internal/adapter/<adapter>/ -run TestFixtures -update-fixtures`.
3. Inspect the golden diff. Reject anything you did not expect — a
   surprise golden diff is a real regression, not a rubber stamp.
4. Commit the code/PRD edit and the golden update in the same commit.

## Guard rails

- **No new fixtures without a `happy/` or `edge/` label.** Anything else
  fails the layout convention and is treated as a bug in the PR.
- **No sibling coupling.** A fixture under one adapter's `testdata/`
  must never be `os.Open`ed by the other adapter's tests; that would
  break Go's per-package `testdata/` isolation and is a red flag in
  review.
- **No AES / secure-storage / encryption-at-rest claims in fixture
  contents.** Story E8-S6 enforces this at CI lint level; fixtures
  are grep'd along with the rest of `docs/` and `internal/`.
- **No cross-tool leakage.** A `claudecode` case must not name a
  `~/.codex/*` path, and vice-versa. The project-scope lint
  (`scripts/lint-project-scope.sh`) covers production code; reviewers
  are responsible for enforcing the same discipline in fixture inputs
  and goldens.

## Cross-references

- PRD NFR-T1 (release-gate fixture corpus).
- Architecture §11 (test surface for adapters).
- Coding standards rule 4 (paired PRD edit for any owned-key change).
- Coding standards rule 14 (no cryptographic claims — enforced against
  fixtures by `scripts/lint-aes-claims.sh`).
- Stories E3-S7 and E4-S7 (Claude Code and Codex fixture matrices —
  the sources of the actual case rows).
- Story E8-S2 (per-package coverage floor — the reason these fixtures
  cannot be reduced without a paired coverage bump elsewhere).
