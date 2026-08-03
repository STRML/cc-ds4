# Vision routing for DeepSeek profiles — design

Date: 2026-08-03 · Status: approved draft, under review

## Problem

An image in a Claude Code transcript is sent as an inline `{type: "image", source: {type: "base64", ...}}` block in the user message. Every DeepSeek V4 profile fails on it:

- **deepseek direct** drops image blocks silently and answers from surrounding text — a confident wrong answer.
- **openrouter / nous** return `404 No endpoints found that support image input`.

Worse, the failure is sticky: the image stays in the history, every later turn re-sends it, and the session loops on the same error. This is the live symptom that started the work.

DeepSeek V4 is text-only, and no `deepseek*` model on any provider here accepts image input. So the only way to make images usable on these profiles is to **translate the image into text the main model can reason over** before it sees the request.

## Architecture

A single mechanism in the existing shared proxy (`src/proxy.py`): when a request body contains image blocks and the profile has vision enabled, each image is decoded, described once by a vision-capable model, cached by content hash, and swapped for a text block carrying the description. DeepSeek then reasons over text and never fails on pixels.

```
claude → proxy (per profile) → sees image block(s) in request body
              │
              ├─ cache hit (content-hash) → swap for cached text description
              │
              └─ miss → describe via the profile's vision route → cache → swap → forward to DeepSeek
```

### Per-profile vision config

A new `vision` row in `PROFILES`:

| profile | describer | how the description is obtained |
|---|---|---|
| `openrouter` | luna (upstream, profile's key) | base64 image → luna `/v1/messages` → description |
| `nous` | luna (upstream, profile's key) | same, over the Nous base URL |
| `direct` | local `claude -p --model haiku` (Anthropic OAuth) | decode to temp file → child `Read` tool → description |
| all | cache | content-hash key, LRU + TTL, per-profile dir |

- `openrouter` and `nous` send the image to luna over the **profile's own key** (`api_key(name, cfg)` — the same path that already serves spend, never a process-wide var). The session still runs on DeepSeek; only the *description* touches luna.
- `direct` has no upstream vision model, so it describes locally with a spawned `claude -p` on an **Anthropic profile** (subscription credits, no new credential).
- A profile with `vision` off (or absent) keeps today's exact code path — the rewrite is a no-op.

### The `claude -p` direct path (verified constraints)

- `claude -p` cannot carry image bytes: it serializes images as `[Image #1]` text markers, and there is no image/attachment flag. So the proxy writes the decoded image to a temp file and lets the child's **`Read` tool** pull the bytes.
- The child must not inherit the ds4 config-dir, so it is spawned with an **explicit Anthropic `--settings`** (`~/.claude/settings.json` by default; `DS4_HAIKU_SETTINGS` override).
- The Anthropic profile is **OAuth** (no base URL, no auth token), so `claude -p --settings ~/.claude/settings.json --model haiku` uses the machine's Anthropic subscription. **Haiku is the cheap describer.**
- Spawn shape: `claude -p --bare --settings <anthropic-settings> --model haiku --add-dir <tmpdir> --allowedTools 'Read(<tmpdir>/*)' --append-system-prompt 'Describe this image for a text-only model.' <image path>`.
  - `--bare` skips hooks, plugins, LSP, and background prefetch, keeping the child fast and hermetic.
  - `--add-dir` grants `Read` access to the temp file; the prompt points at the path.
- **Risk: launchd → keychain.** The proxy runs under a launchd agent, which may not unlock the login keychain, so the OAuth login may not resolve there. Mitigation: probe during implementation; if the keychain fails under launchd, expose `DS4_HAIKU_SETTINGS` pointing at an API-key settings file as the escape hatch. On the interactive path the proxy is spawned from a shell where the keychain is unlocked, so it works there.

### Luna on openrouter / nous

The exact luna model id is **read from the profile's `/v1/models` at first use** (pick the entry whose id contains `luna` and whose input modalities include image), rather than hardcoded. If no such model exists, the profile degrades to fail-open placeholder behavior. This avoids guessing the id in config.

## Cache

- **Dir:** `<profile>/.ds4-vision/` (same `.ds4-*` hygiene as `.ds4-sessions` — never touches Claude Code's own state dirs).
- **Key:** SHA-256 of the image block's `data` + `media_type`.
- **Value:** the description text.
- **Policy:** LRU cap (e.g. 256 entries) + TTL (e.g. 30 days). Repeated images across a session, or the same screenshot pasted twice, hit cache instead of re-describing.
- Cache is best-effort: corruption or unreadable entries are treated as misses.

## Failure handling — fail open, never brick

The vision call has a timeout (upstream HTTP and the `claude -p` child both bounded). Any failure — timeout, missing describer, no luna model, child exit nonzero — swaps in a **neutral placeholder** and forwards the request to DeepSeek anyway:

> `[Image N — text-only model; the attached image could not be described. Describe or OCR it yourself.]`

A hard error is never returned. This is what unbricks the original loop: once the image becomes text (or a placeholder), the poisoned history stops 404ing and the session proceeds.

## Honest ceiling

The description is a **lossy proxy, not pixels**. DeepSeek never sees the image; it reasons over a text description. Charts, UI layouts, and transcripts degrade. This is the accepted limitation of the approach — images remain usable but not faithfully visible. For pixel-faithful vision, a vision-native profile (Kimi K3, Anthropic) remains the option; this work makes the DeepSeek profiles not-broken.

## Security / privacy notes

- Images sent to luna on `openrouter`/`nous` leave the machine and are billed to the profile's key. That is the privacy cost of vision on those profiles and should be surfaced in the README.
- The `direct` path is **local**: the image stays on the machine, described by a local `claude -p` child under the Anthropic subscription. No image bytes leave except through the child.
- No unscoped key reach is introduced: upstream vision uses the profile's own `api_key(name, cfg)`, never a process-wide variable.

## Scope / non-goals

- **Not** switching profiles or adding a vision-native profile.
- **Not** passing pixels to DeepSeek (impossible — text-only).
- **Not** OCR as the direct fallback (out of scope; the `claude -p` child handles direct).

## Testing

- Unit tests over the image-block rewrite: cache hit, cache miss (describer called once), fail-open (describer error → placeholder, request still forwarded), malformed image block.
- The `claude -p` spawn is **mocked** in tests so the suite stays offline and deterministic.
- Existing 154-test suite stays green.
