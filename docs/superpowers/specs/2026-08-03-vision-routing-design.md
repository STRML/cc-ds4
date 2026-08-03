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

### Describer — one local `claude -p` for every profile

Every profile uses the same local `claude -p --model haiku` child on the
Anthropic profile (subscription credits, no new credential, no upstream vision
model). `DS4_VISION` gates the rewrite globally (`DS4_VISION=0` restores the
old pass-through). There is **no per-profile `vision` flag** — all profiles are
treated identically.

### The `claude -p` spawn (proven recipe, from cc-debate + Sean's gist)

- `claude -p` cannot carry image bytes: it serializes images as `[Image #1]` text
  markers, and there is no image/attachment flag. So the proxy decodes the base64
  to a temp file in an isolated `TemporaryDirectory` and the child's **`Read`
  tool** pulls the real pixels. The child's `cwd` is that temp dir, so its only
  readable context is the image (`--allowedTools` is a permission pre-approval,
  not a filesystem sandbox).
- The child is forced onto the Anthropic profile via `CLAUDE_CONFIG_DIR=$HOME/.claude`
  and its env is scrubbed of inherited `ANTHROPIC_*`/`CLAUDE_CODE_*`/`DS4_*`
  (and `CLAUDECODE`, the nested-session guard) so it never routes back to a ds4
  profile or touches a ds4 key.
- Spawn shape: `claude -p --settings '{"disableAllHooks":true}' --model haiku
  --tools Read --allowedTools "Read(<tmp>/*)" --add-dir <tmp>
  --disable-slash-commands --strict-mcp-config --append-system-prompt '...'
  --no-session-persistence --output-format json "Read <img> and describe the
  image."` with `stdin=DEVNULL` (an expired-OAuth prompt crashes the child
  instead of hanging the proxy) and `CLAUDE_CODE_SIMPLE=1`.
- **`CLAUDE_BIN`** resolves the absolute `claude` binary (`DS4_CLAUDE_BIN` baked
  by install.sh, validated against a vanished cmux shim, else `shutil.which`).
  Under launchd the bare name is not on PATH; without it vision fails open.
- **Auth:** the Anthropic profile is OAuth. The launchd agent may not unlock the
  login keychain, so a launchd-safe credential is an implementation-time probe.

### The image-rewrite machinery (ported from Sean's gist)

- **`rewrite_images(payload, cache_dir)`** walks every message's `content` and
  **recurses into `tool_result.content`** — Sean's hard requirement (images
  nested there are where `Read`/screenshot/MCP images land; leaving one is
  silently DROPPED upstream). A non-list `messages` value is skipped. A
  `MAX_IMAGES_PER_REQUEST` budget bounds the serial describe work.
- **`_swap_image`** requires `source.type == "base64"`, a string `data`, and a
  nonempty `media_type` (else placeholder); strict `b64decode(validate=True)`.
  URL sources are unsupported.
- **`transcribe`** returns `(text, fresh)` — `fresh` is always an int `0`/`1`,
  never `None`. Single-flighted per `(cache_dir, key)`.
- **Fail-open everywhere:** any failure swaps in a neutral placeholder
  (`[Image omitted: no usable description was available.]`) and the request is
  forwarded. `placeholder_remaining` scrubs any remaining image block on the
  exception path so no image-shaped block reaches the text-only upstream.

## Cache

- **Dir:** `<profile>/vision-cache/`.
- **Key:** SHA-256 over **model + prompt salt + `media_type` + raw image bytes**
  (a describer or prompt change invalidates old entries).
- **Value:** the description text.
- **Policy:** TTL-on-read (30 days) in `cache_get` — a stale entry is deleted
  and treated as a miss. Atomic writes via tmpfile+`os.replace`. No separate
  eviction pass (a bounded-entry cap was dropped as unneeded complexity).
- Cache is best-effort: corruption or unreadable entries are treated as misses.

## Failure handling — fail open, never brick

Any failure — timeout, missing `claude`, malformed image, child exit nonzero —
swaps in a **neutral placeholder** and forwards the request to DeepSeek anyway:

> `[Image omitted: no usable description was available.]`

A hard error is never returned, and no image-shaped block reaches the upstream.
The rewrite is **per-request**: it does not clear an already-poisoned transcript
(that needs `/compact`/`/clear` client-side).

## Honest ceiling

The description is a **lossy proxy, not pixels**. DeepSeek never sees the image; it reasons over a text description. Charts, UI layouts, and transcripts degrade. This is the accepted limitation of the approach — images remain usable but not faithfully visible. For pixel-faithful vision, a vision-native profile (Kimi K3, Anthropic) remains the option; this work makes the DeepSeek profiles not-broken.

## Security / privacy notes

- **Every profile's image is described locally** by a `claude -p` child under the Anthropic subscription. No image bytes leave the machine except through that child — which is exactly what it exists to do.
- The child is forced onto the Anthropic profile (`CLAUDE_CONFIG_DIR=$HOME/.claude`) and its env is scrubbed of `ANTHROPIC_*`/`CLAUDE_CODE_*`/`DS4_*`/`CLAUDECODE`, so it never touches a ds4 key.
- The transcription is **untrusted data** — an image can contain instructions. The description must be treated as evidence, not a directive.
- **Known limitations (accepted, documented in the README):** the proxy's loopback listener is not authenticated (any local process could spend Anthropic quota via vision); `--allowedTools` is additive, not a filesystem sandbox, so a prompt-injected image could direct the child to read a user-readable file. These are inherent to the design (the child loads the real Anthropic profile) and are not expanded into a full auth/sandbox system here.

## Scope / non-goals

- **Not** switching profiles or adding a vision-native profile.
- **Not** passing pixels to DeepSeek (impossible — text-only).
- **Not** OCR as the direct fallback (out of scope; the `claude -p` child handles direct).
- **Not** clearing an already-poisoned transcript — that requires `/compact`/`/clear` client-side.

## Testing

- Unit tests over the image-block rewrite: cache hit, cache miss (describer called once), fail-open (describer error → placeholder), malformed image block, `tool_result` recursion, non-list messages guard, `CLAUDECODE` scrub, `--no-session-persistence` + `stdin=DEVNULL`, `CLAUDE_BIN` missing.
- The `claude -p` spawn is **mocked** in tests so the suite stays offline and deterministic.
- Existing suite stays green.
