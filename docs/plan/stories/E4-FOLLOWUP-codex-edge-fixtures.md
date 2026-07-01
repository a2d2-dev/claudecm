# E4-FOLLOWUP — codex adapter edge fixtures deferred from E4-S7

**Status.** Placeholder — filed from the E4-S7 fixture matrix review.

## Problem

The E4-S7 acceptance list enumerated a superset of case names.
The initial land shipped the ones whose semantics are actually
adapter-level; the rest either shadow existing parser-layer tests
or require adapter policy that does not exist in v1. This stub
tracks the deferred names so they are not silently forgotten.

Deferred case names, grouped by design cluster:

## Cluster A — TOML-parser-layer cases

`edge/bom/`, `edge/crlf/`, `edge/comments/`, `edge/mixed_indent/`.

Design considerations:

- The codex adapter delegates parsing to `internal/codex/toml`'s
  doc-model wrapper. That package's `doc_test.go` already covers
  BOM handling, CRLF handling, comment preservation, and mixed
  tab/space indentation at the parser layer.
- A fixture golden at the adapter layer would either shadow the
  parser unit test (no new signal), or would require an
  adapter-level policy decision that has not been made yet:
    - `bom/` — does Import strip a UTF-8 BOM silently, or refuse
      with `ErrParseFailed`? Symmetric question for Apply.
    - `crlf/` — does Apply preserve CRLF line endings when the
      seed file uses them, or normalize to LF? PRD §4.7 says
      "merge-preserve" but does not name a line-ending policy.
    - `comments/` — the doc-model already preserves comments;
      the open question is whether re-emit is byte-verbatim on
      the comment lines or whitespace-normalized around `=`.
    - `mixed_indent/` — does Apply re-emit with the original
      indentation width, or normalize? PRD is silent.
- Land the policy first (adapter-level ADR or explicit note in
  PRD §4.7 for each of the four), then a fixture that pins it.

## Cluster B — cross-filesystem `separate_fs`

`edge/separate_fs/`.

Design considerations:

- Out of `~/.codex/` scope for v1. The write-path
  (`internal/writepath/apply.go`) refuses cross-filesystem rename
  per NFR-S1 rather than falling back to copy+unlink.
- A meaningful fixture needs two mounts wired at test-runner
  time. Neither the local dev machine nor CI provides that.
- Would want a companion story that either (a) documents the
  refusal as expected behavior and drops a build-tag'd fixture
  that only runs on machines with a second mount, or (b) adds a
  copy+unlink fallback behind a flag and drops the fixture on the
  common path. (a) is smaller and matches the NFR-S1 posture.

## Suggested implementation sequencing

1. Cluster A, per-item: land the policy decision for one of BOM /
   CRLF / comments / mixed_indent, then drop the fixture + golden.
   Prefer BOM first — it is a boolean policy (strip vs refuse) and
   the codebase already has half of the plumbing.
2. Cluster A, remaining: repeat once the first case's shape is
   settled.
3. Cluster B: pair with a `writepath` story that either documents
   the NFR-S1 refusal as final or introduces a scoped fallback.
   Do not land the fixture without the policy.

## Verification hook

Once implemented, drop the case directories into
`internal/adapter/codex/testdata/codex/edge/` with the same layout
as the existing cases (`config.toml`, `auth.json`, `profile.yaml`
+ `expected/` goldens; or `error-only.txt` for refusals),
regenerate with `-update-fixtures`, and verify:

- `TestFixtureGoldensAreValidJSON` passes on the new goldens.
- `runFixtureCase`'s `compareCount` tripwire naturally picks up
  the new stages.
- Coverage stays ≥ 91 % on `internal/adapter/codex`.

## Not in scope

- Any change to the four case types' semantics without a written
  policy note. This stub tracks the *fixture* debt; the *policy*
  debt is upstream in PRD §4.7 / adapter ADRs.
- Any change to `writepath.Apply`'s cross-fs policy.
