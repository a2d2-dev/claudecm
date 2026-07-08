# ADR-0003: Smart `add` Onboarding Scope (env / file / paste, optional AI-assisted parse)

- **Status:** Proposed (awaiting CEO/LF sign-off)
- **Date:** 2026-07-08
- **Owner:** CEO (LF)
- **Authority:** This memo governs the "smart api-key onboarding" scope (epic E13) only. Where it conflicts with `docs/decisions/0001-direction-lock.md` or `docs/decisions/0002-v1_1-scope.md`, this memo wins **for E13 only**. ADR-0001 and ADR-0002 remain the authority everywhere else.

## Context

`claudecm add` today is a declarative, flag-driven profile creator (E6-S3): every field
(`--base-url`, `--api-key`, `--model`, …) must be typed explicitly. That is script-perfect but
high-friction for the most common human task — "I just got a provider's onboarding blurb / an
`export` snippet / a config file, make me a profile from it." LF asked for three smarter entry
points:

1. **From environment variables** — read the already-exported `ANTHROPIC_*` (and Codex) env vars
   and materialize a profile. Zero network.
2. **From a config file** — point at a `.env`, shell `export` snippet, JSON/YAML/TOML config, or
   another tool's config file and parse it into a profile draft. Zero network.
3. **From pasted free text** — paste an arbitrary blob (a provider's docs snippet, a chat log, a
   half-formatted `export`), and have claudecm figure out `base_url` / `api_key` / `model`.

Item 3 has two admissible strengths. A **local heuristic** pass (regex/structure extraction) needs
no network and is fully within the local-first lock. When the blob is too messy for heuristics, LF
wants an **opt-in AI-assisted** escalation whose defining constraint is: **the secret is stripped
locally first and never leaves the machine.** Only the desensitized (secret-free) text is sent to an
LLM, which returns a structured profile; the captured secret is re-injected locally.

The AI escalation is the only part of E13 that makes an outbound network request, so it is the only
part that touches ADR-0001 Decision 8 ("no cloud") / the README's "never sees a request" language.
This ADR draws the line precisely.

## Decision Summary

Epic **E13 — Smart `add` Onboarding** adds three input adapters to `add`, layered so the
zero-network paths are the default and the network path is explicit opt-in:

1. **`add --from-env`** — build a profile draft from the Claude Code / Codex environment-variable
   allowlist. Local only.
2. **`add --from-file <path>`** — auto-detect format (`.env` / shell `export` / JSON / YAML / TOML)
   and parse into a profile draft. Local only.
3. **`add --from-text` / `--from-text -` (stdin)** — run the shared **local redaction + heuristic
   extractor** over pasted text and produce a profile draft. Local only.
4. **`add --ai`** — escalation, **off by default**. Uses the shared extractor to strip and hold
   secrets locally, requires an interactive TTY to show and confirm the full desensitized payload,
   sends only the confirmed desensitized text to an LLM, receives a structured profile, re-injects
   the held secret locally, then routes through the normal `add` preview/validation. This is the
   only E13 path that uses the network.

All four paths converge on the existing `add` pipeline: they only produce a `config.Profile` draft,
which is then subject to the same `--dry-run`, redaction (NFR-S8), name validation (NFR-S5),
overwrite guard, and `SaveProfile` invariants. No E13 path writes a Claude Code or Codex tool file
directly, and no path auto-activates the new profile (activation stays `switch`).

## Amendments to Prior Scope

1. **ADR-0001 Decision 8 ("no cloud") is narrowly amended for E13's `--ai` path only.** claudecm
   MAY make a single outbound LLM request to parse desensitized text into a profile draft, strictly
   when the user opts in via `--ai` and confirms the desensitized payload in an interactive TTY.
   This does not admit cloud sync, telemetry, a proxy, a gateway, remote credential validation, or
   any always-on network behavior. Every non-`--ai` path, every non-interactive `--ai` invocation,
   and the default of every path remains zero-network.
2. **README "never sees a request" is refined, not revoked.** The tool still never sees, proxies, or
   routes a *tool* request (Claude Code / Codex traffic). The `--ai` path issues its own,
   user-initiated, secret-free parse request and nothing else. Docs MUST state this distinction
   plainly.
3. **No new command.** E13 adds flags/inputs to the existing `add` command (ADR-0001 Decision 3).
   It does not introduce a new top-level command.

## Locked Decisions

1. **Secrets never transit the network.** The `--ai` path MUST run the local redaction pass first,
   MUST hold captured secrets in-process, and MUST send only text from which secret-shaped tokens
   have been removed/placeholdered. Re-injection of the real secret happens locally after the LLM
   responds. A verifiable redaction step (secret token → placeholder) is an acceptance gate, not a
   nicety. If redaction cannot be established for a candidate secret, that token is stripped
   entirely rather than risk transit (no fallback that leaks). `--ai` MUST require interactive
   confirmation of the full desensitized payload before the parse request; non-interactive or piped
   invocations refuse before sending.
2. **AI is opt-in and off by default.** Absent `--ai`, `add --from-text` uses only the local
   heuristic and never touches the network. `--ai` must be typed explicitly per invocation; there is
   no persisted "always use AI" mode in E13.
3. **LLM credential source.** `--ai` reuses the currently-active claudecm profile's endpoint and
   key by default (dogfooding; no new secret to configure). `--ai-profile <name>` overrides which
   profile's credentials are used for the parse call. The parse call never persists or logs the
   credential it borrows.
4. **No remote validation.** No E13 path probes a provider endpoint to "verify" a key or model.
   Drafts are produced from local + (optionally) LLM-structured data only; the user still confirms
   via `--dry-run` / prompt before `SaveProfile`.
5. **No write-path bypass.** Every profile produced by E13 is saved through the same
   `storage.SaveProfile` used by `add`/`import`, and any later activation still routes through
   `internal/commit` + `internal/writepath`. E13 creates no side write path.
6. **Refuse, don't guess.** Consistent with NFR-S1 / CLAUDE.md no-fallback-writes: when a source
   cannot be parsed into a valid draft (unreadable file, unrecognized format, LLM returns
   non-conforming output), the command refuses with a clear error and writes nothing. It does not
   silently emit a half-populated profile.

## Explicit Non-Goals

- No clipboard integration (paste is via argument or stdin `-`); OS clipboard access is deferred.
- No batch/multi-profile import from one blob; one draft per invocation.
- No provider auto-detection beyond field extraction (we do not map a base URL to an official
  provider identity or claim support).
- No persisted AI configuration, no streaming UI, no model selection surface beyond the borrowed
  profile's model.

## Rationale

The three entry points collapse the most common onboarding frictions into one command while keeping
claudecm's core promise intact: profiles stay explicit, local, inspectable YAML, and the user owns
the secret. Layering zero-network paths as the default with a clearly-fenced, secret-free, opt-in AI
escalation lets us say "smart" without becoming "a thing that phones home." The secret-never-transits
rule is what makes the AI path defensible against ADR-0001's spirit even while amending its letter.

## Risks

- **R1. Redaction miss → secret leak on the `--ai` path.** This is the load-bearing risk.
  Mitigation: redaction is a tested gate with a conservative "strip on doubt" rule; `--ai` shows the
  exact desensitized payload before sending and requires interactive confirmation; non-interactive
  runs refuse; unit tests assert no secret-shaped token survives into the outbound request.
- **R2. Heuristic false extraction** (wrong field grabbed). Mitigation: every path ends in
  `--dry-run`/preview with redacted secrets; nothing is saved without confirmation.
- **R3. Scope creep toward "AI everywhere."** Mitigation: `--ai` is per-invocation, single-request,
  parse-only, no persistence; this ADR forbids always-on modes.
- **R4. Borrowed-credential surprise** (user didn't expect their active key to be used for a parse
  call). Mitigation: interactive confirmation names the profile whose credentials will be used
  before the call; `--ai-profile` makes it explicit.

## Supersedes

For E13 only: ADR-0001 Decision 8's blanket no-network stance as applied to the single opt-in
`--ai` parse request, and the README's "never sees a request" phrasing as applied to that same
user-initiated, secret-free call. Everything else in ADR-0001 and ADR-0002 stands.
