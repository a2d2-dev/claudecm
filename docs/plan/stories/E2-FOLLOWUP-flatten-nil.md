# E2-FOLLOWUP — writepath Flatten(nil) surfaces `""` key on empty current

**Status.** Placeholder — filed from the E3-S7 fixture matrix review.

## Problem

When `~/.claude/settings.json` is zero-byte or whitespace-only, the
Claude Code adapter's Import treats it as empty and returns a zero
`(Core, Overlay)`. That path is well-covered.

The write side is not symmetric. `writepath.Apply` calls
`Flatten(nil)` on the current side to build the ownership set for
the TouchesUnowned guard. The flatten helper currently yields a
single entry `map["": nil]{}` for a nil root, and `""` is not in
the adapter's OwnedKeys allowlist. So even a no-op Apply against a
zero-byte current is refused with a TouchesUnowned error — an
adapter cannot Plan+Apply on a fresh install shape.

Symptom captured by the fixture matrix:

  - `testdata/claudecode/edge/empty/`
  - `testdata/claudecode/edge/whitespace-only/`

Both omit `profile.yaml` on purpose so the fixture harness skips
Plan+Apply. If either dropped a `profile.yaml` in today's code the
fixture would fail at the TouchesUnowned guard, not on any real
adapter regression.

## Fix direction

Two options; both need CEO sign-off before implementation:

  1. Special-case an empty-string flatten key in writepath so it is
     ignored when the current side is empty (Flatten(nil) has no
     meaningful "keys touched" set).
  2. Special-case an empty current *upstream* of Flatten — treat a
     zero-byte or whitespace-only current as `{}` inside Apply, the
     same way Import already does via `treatAsEmpty`.

Option 2 mirrors the read-side policy from `emptycheck.go` and is
the smaller surface change. Option 1 is a writepath-only fix but
leaks read-side policy into the generic write-path helper.

## Sequencing

Not a blocker for E3-S7 (the fixture matrix ships without it by
declining to seed a profile.yaml for the empty cases). Should land
before any future story wants to exercise `Plan → Apply` against
an empty starting settings.json — e.g. a first-launch smoke test
or the eventual `cmd/switch` end-to-end. Land before E5 (explain)
so the guard-refusal path is never surfaced to end users on their
first launch.

## Verification hook

Once fixed, drop a `profile.yaml` into `edge/empty/` and
`edge/whitespace-only/` and regenerate the goldens with
`-update-fixtures`. Expected: `after_apply.json` shows the
adapter's canonical empty-object write, `diff.json` shows a set of
top-level env writes, and the fixture test's compareCount tripwire
picks up the new stages automatically.
