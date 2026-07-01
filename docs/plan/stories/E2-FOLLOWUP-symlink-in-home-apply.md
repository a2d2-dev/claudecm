# E2-FOLLOWUP — writepath Apply refuses in-HOME symlinks

**Status.** Placeholder — filed from the E3-S7 fixture matrix review,
CEO decision recorded below.

## Problem

`~/.claude/settings.json` in a dotfiles-repo workflow is typically a
symlink into a git-tracked file inside HOME
(e.g. `~/dotfiles/claude/settings.json`). Import already follows that
symlink correctly via `verifyReadTargetInHome` (parent + leaf resolved,
containment checked against `EvalSymlinks(HOME)`).

Apply does not. `writepath.Apply` calls `storage.Stat` on `plan.Target`,
which refuses non-regular files. A symlink counts as non-regular, so
Apply on a symlinked settings.json errors out even when the resolved
target is a plain file inside HOME. Net effect: users on a dotfiles-
repo workflow can `claudecm import` but cannot `claudecm switch`.

The E3-S7 fixture `testdata/claudecode/edge/symlink-in-home/` proves
this today: it omits `profile.yaml` so the fixture matrix skips
Plan+Apply, and fixtures_test.go carries an inline comment linking
here for the plan.

## Decision (CEO)

`writepath.Apply` should FOLLOW in-HOME symlinks so the dotfiles-repo
workflow is supported:

  1. `filepath.EvalSymlinks(plan.Target)` — resolve the full path.
  2. Re-verify containment: resolved path must still live under
     `filepath.EvalSymlinks(r.Home())`. Out-of-HOME resolution stays
     refused with `ErrOutsideHome` (unchanged policy from Import).
  3. `AtomicWrite` against the RESOLVED path so the temp+fsync+rename
     lands on the real file in the dotfiles repo. The symlink itself
     is not touched.
  4. Backup and post-write reparse operate on the resolved path.

Symmetric with the read side: Import follows in-HOME, refuses
out-of-HOME. The two paths should not diverge on symlink policy.

## Out of scope

  - Out-of-HOME symlinks stay refused with `ErrOutsideHome`. If a
    user's dotfiles live outside HOME they must set up an in-HOME
    symlink chain that ends inside HOME, or move the file.
  - Chased-symlink chains longer than one hop: `EvalSymlinks`
    already resolves the whole chain, so this is handled for free.
  - Any change to `storage.Stat` — this is a writepath policy fix,
    not a lower-level primitive change.

## Sequencing

Not a blocker for E3-S7 (the fixture matrix ships the symlink case as
Import+Project only). Should land BEFORE E5 (explain surfaces symlink
state to users) and BEFORE E7 (commit path expects Apply to succeed on
dotfiles-repo layouts). Part of the "writepath symlink policy"
hardening cluster.

## Verification hook

Once implemented, drop `profile.yaml` into
`testdata/claudecode/edge/symlink-in-home/`, regenerate goldens with
`-update-fixtures`, and remove the "Apply is currently refused"
comment from that case's block in fixtures_test.go. The fixture's
compareCount tripwire picks up the new after_apply.json / diff.json
stages automatically. Add a companion `edge/symlink-out-of-home-apply/`
fixture (or extend the existing symlink-out-of-home case with a
`profile.yaml`) to pin the "out-of-HOME symlink is still refused"
half of the policy.
