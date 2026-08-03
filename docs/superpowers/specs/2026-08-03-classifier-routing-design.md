# Classifier routing to Anthropic — design

Date: 2026-08-03 · Status: draft for review

## Problem

In `defaultMode: auto`, Claude Code gates every tool call through a **permission
classifier**: a small, fixed-purpose call that decides whether a proposed tool
invocation is allowed. That call currently routes to the same DeepSeek backend
as the main loop. The result: **every tool call's context in auto mode is sent
to a provider with no trust relationship** (DeepSeek via Nous/OpenRouter).

DeepSeek is a China-hosted model. The classifier is the security gate of the
whole auto-mode experience — it sees the intent of every tool call before
anything else. Routing that gate to a model with no trust relationship is the
wrong place for the security boundary to live. It should sit in a trusted
boundary: the Anthropic subscription the user already pays for.

## Architecture

The shared proxy (`src/proxy.py`) already rewrites request bodies per profile
and relays them upstream. A classifier request is **already a valid Anthropic
Messages request** — small `max_tokens`, thinking disabled, tools array, a
single user message. It only needs to be pointed at the right upstream with the
right credential. No child spawn, no transcription, no temp files.

So the classifier path is a **relay swap**, not a rewrite:

```
claude (auto mode) → proxy → sees classifier signature
                                │
                                ├─ route=anthropic (default)
                                │     └─ rewrite model → Anthropic id
                                │          forward to api.anthropic.com/v1/messages
                                │          Authorization: Bearer <keychain token>
                                │          → fail open to ds4 on any error
                                │
                                └─ route=ds4 (opt-out) → today's exact path
```

### Detecting the classifier

The classifier arrives with a consistent, distinct signature (measured 216× in
the live proxy log, `~/.claude-ds4-proxy.log`):

| field | classifier value | main loop |
|---|---|---|
| `model` | `ds4-high` | `ds4-xhigh` |
| `max_tokens` | `2112` (fixed) | `32000` |
| `thinking` | `{"type": "disabled"}` | adaptive |

The `ds4-high` tier alone is ambiguous — subagents also run at `ds4-high`
(`CLAUDE_CODE_SUBAGENT_MODEL`). The classifier is the **combination**:
`ds4-high` **and** `max_tokens` below `NOTHINK_BELOW` **and** thinking already
disabled. Subagent requests at `ds4-high` carry a large `max_tokens` and
thinking on, so they fall through to the existing ds4 path untouched.

Rather than hardcode `2112`, the detector is: `ds4-high` + `max_tokens ≤
NOTHINK_BELOW` + thinking disabled. That captures the classifier while
remaining robust to Claude Code changing the exact budget.

### Forwarding to Anthropic

When the classifier is detected and the profile's route is `anthropic`:

1. `payload["model"]` is set to a real Anthropic model id. The classifier is a
   small permission call; **haiku is the right model** (cheap, fast, and the
   classification task is trivial). Configurable per profile via
   `DS4_CLASSIFIER_MODEL` with `claude-haiku-4-5` as the default.
2. The `reasoning_effort` field the ds4 rewrite added is removed — Anthropic
   does not accept `reasoning_effort` at the top level on the subscription.
   (Haiku doesn't support effort; drop it entirely.)
3. The body is forwarded to `https://api.anthropic.com/v1/messages` with
   `Authorization: Bearer <token>`, `anthropic-version: 2023-06-01`, and the
   same `content-type`.
4. The upstream reply is relayed back byte-for-byte, exactly like the existing
   ds4 relay.

The request is **not** streamed differently — the existing `_relay` streaming
loop handles it unchanged. The classifier call is a single non-streamed
response.

### Auth: `claude setup-token` → `DS4_CLASSIFIER_TOKEN`

The proxy needs a subscription credential it can use directly. The keychain's
stored OAuth access tokens are all **expired** (Claude Code refreshes them
in-memory at launch — verified on this machine: every
`Claude Code-credentials-*` item's `expiresAt` is in the past), so a proxy
that reads the keychain cold gets a dead token, 401s, and fails open to ds4 —
the feature silently does nothing. Reimplementing Claude Code's OAuth refresh
is fragile and undocumented.

The working path is the one the vision design already named for launchd-safe
auth: **`claude setup-token`**. It mints a long-lived (one-year) subscription
token from the existing login. The proxy reads it from the environment:

1. Run `claude setup-token` once; it prints `sk-ant-oat01-...`.
2. Export it as `DS4_CLASSIFIER_TOKEN` when running `install.sh`. The install
   sweep bakes **every `DS4_*` var** into the launchd agent's environment
   (verified: `install.sh` line ~231), so the token reaches the proxy with no
   install.sh change.
3. The proxy reads `DS4_CLASSIFIER_TOKEN` at startup. No keychain dependency,
   no refresh logic — a static long-lived token.

If `DS4_CLASSIFIER_TOKEN` is unset/empty, the classifier route **fails open**:
requests fall through to the ds4 path. The token is held in process memory,
never logged.

> **Token rotation:** the token expires in a year. Rotation is re-running
> `claude setup-token` and `install.sh`. This is the documented tradeoff of a
> static token — accepted in exchange for a fast raw relay with working auth.
> (Vision uses the keychain via its *child* process, which has a full interactive
> session to refresh; the classifier's in-proxy relay does not.)

### Per-profile config

A new `classifier` row in `PROFILES`, defaulting to `anthropic`:

| profile | classifier route |
|---|---|
| `direct` | `anthropic` (default) |
| `openrouter` | `anthropic` (default) |
| `nous` | `anthropic` (default) |

Opt-out: `"classifier": "ds4"` on any profile, or the `DS4_CLASSIFIER` env var
(`DS4_CLASSIFIER=ds4` forces ds4 everywhere). The env var is the escape hatch
for "I don't want auto-mode tool intent going to Anthropic either".

## Failure handling — fail open, never brick

Any failure on the Anthropic path — no token, token expired beyond the single
401 retry, network error, Anthropic 5xx — **falls through to the ds4 path**:
the body reverts to its ds4 shape (the `ds4-high` sentinel stays / is restored)
and the existing relay forwards it as today. The classifier still works, just
on the less-trusted backend. A hard error is never returned to the client.

The one deliberate exception: a **400 from Anthropic** (malformed request) is
forwarded as-is. The classifier's request shape is Anthropic's own; a 400 means
Claude Code sent something the classifier path didn't expect, and failing open
to ds4 on that would mask a real mismatch. This matches the vision design's
"never forward an image-shaped block" strictness on the one thing that must
not silently degrade.

## Honest ceiling

- **Subagents at `ds4-high` stay on ds4.** Only the classifier signature
  (small `max_tokens` + thinking off) is rerouted. Subagent calls are
  legitimate work on the cheap tier, not a security gate.
- **The classifier sees the tool call, not the tool result.** Auto mode's
  classifier decides *before* the tool runs. The sensitive context (the
  proposed command) is what gets sent to Anthropic — which is exactly the
  point. The tool *results* stay on ds4 with the main loop.
- **This does not change which model runs the main loop.** Only the small
  permission-gate call moves. Everything else keeps the current DeepSeek
  routing.

## Security / privacy notes

- **Trusted boundary:** the security gate now lives on the Anthropic
  subscription (a trusted provider) instead of a China-hosted model. This is
  the entire point of the change.
- **No new credential surface:** a long-lived token from `claude setup-token`,
  read from `DS4_CLASSIFIER_TOKEN`. It is a static token on disk (in the
  launchd env), rotatable by re-running setup-token + install. This is the
  documented tradeoff of the trusted-boundary win.
- **Token custody:** in-process only, re-read on 401. The `security` call is
  scoped to the generic-password item, not a broad keychain dump.
- **Unchanged trust model:** the loopback listener is still unauthenticated
  (pre-existing); a local process could in principle spend Anthropic quota via
  the classifier path. Same accepted limitation as vision.

## Scope / non-goals

- **Not** changing the main loop's model — the main thread stays on DeepSeek.
- **Not** routing subagents to Anthropic — only the classifier signature.
- **Not** adding a vision-native or Anthropic-native profile.
- **Not** replacing the keychain auth with a static token.

## Testing

- Unit tests over the classifier **detector** (signature match, subagent
  fall-through, thinking-on fall-through, boundary `max_tokens`).
- Unit tests over the **forward** shape: model rewritten to
  `claude-haiku-4-5`, `reasoning_effort` stripped, correct upstream URL and
  auth header.
- **Fail-open** tests: no token → ds4 path; Anthropic network error → ds4 path.
- **Opt-out** tests: `classifier: "ds4"` and `DS4_CLASSIFIER=ds4` leave the
  request on the ds4 path.
- The `urlopen` call is **mocked** in tests so the suite stays offline and
  deterministic.
- Existing suite stays green (`python3 -m unittest discover -s tests`).
