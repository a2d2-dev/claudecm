# claudecm demo cast

Story E9-S3 asks for an asciinema cast, embedded in the top-level README,
showing `explain` resolving an effective config and `switch --dry-run`
printing the pre-apply diff — so a prospective user can see the tool in
motion before installing it.

## What lives here

- `record.sh` — one-command regeneration script. Runs a fully sandboxed
  demo (`--home /tmp/claudecm-demo`, synthetic credentials) and writes
  `claudecm-demo.cast` next to itself.
- `claudecm-demo.cast` — the asciinema recording embedded in the README
  (produced by `record.sh`; not committed until a maintainer with a real
  terminal + asciinema installed runs the script).

## How to regenerate

```bash
# On a machine with a real TTY and asciinema installed:
bash docs/demo/record.sh
git add docs/demo/claudecm-demo.cast
git commit -m "docs(demo): regenerate cast"
```

## What the cast must show

1. `claudecm import claude-code` and `claudecm import codex` seeding
   starting profiles from the sandboxed on-disk config.
2. `claudecm add work ...` creating a second profile.
3. `claudecm explain work` — full per-tool resolution chain, with the
   sample API token rendered as `sk-***xxxx` (NFR-S8 default redaction).
4. `claudecm switch work --dry-run` — pre-apply diff, no disk writes.

Total runtime: ~60 s at ~2 s/step so the cast reads at a human pace.

## Non-goals for the cast

- No real credentials, ever. The sandbox uses obviously synthetic tokens.
- No `--reveal`. The redaction is the point.
- No two-phase failure demo — that's a separate cast if we want it.

## Why the `.cast` file is not committed in this PR

Asciinema requires an interactive terminal at record time. The
implementation environment for this story was headless, so the script
is committed and the `.cast` output is deferred to a maintainer with
asciinema access. See the Implementation Note in
`docs/plan/stories/E9-S3.md`.

## CI guardrail

`scripts/lint-aes-claims.sh` also greps the demo cast (once it lands
under `docs/`) for cryptographic marketing claims and for exposed
`sk-` tokens outside the intended redaction. Any real key that leaks
into a cast will fail CI.
