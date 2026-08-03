<h1 align="center">cc-ds4</h1>

<p align="center">
  <strong>Run DeepSeek V4 in Claude Code without breaking your Anthropic setup.</strong><br>
  Isolated profiles, zero-data-retention routing, and a status line that reports what you actually spent.
</p>

<p align="center">
  <a href="https://github.com/STRML/cc-ds4/actions/workflows/tests.yml"><img alt="tests" src="https://github.com/STRML/cc-ds4/actions/workflows/tests.yml/badge.svg"></a>
  <a href="LICENSE"><img alt="license" src="https://img.shields.io/badge/license-MIT-blue.svg"></a>
  <img alt="python" src="https://img.shields.io/badge/python-3.9%2B-blue.svg">
  <img alt="dependencies" src="https://img.shields.io/badge/dependencies-none-brightgreen.svg">
</p>

<p align="center">
  <img src="assets/statusline.svg" alt="Two status lines: ds-deepseek-v4-flash and or-deepseek-v4-flash-0731, each showing context usage, session cost, 7-day spend, and credit remaining." width="100%">
</p>

---

Claude Code talks to Anthropic over a documented HTTP API, and it will talk to
anything that speaks the same shape. Several providers now serve an
Anthropic-compatible `/v1/messages` endpoint, so you can point Claude Code at a
different model without patching the client.

The catch is that Claude Code reads its configuration from one directory, and the
obvious way to redirect it (exporting `ANTHROPIC_BASE_URL`, or editing
`~/.claude/settings.json`) redirects **every** session on the machine, including ones
already running. That is the failure mode this whole setup exists to avoid.

The fix is per-profile config directories. Each profile is its own
`~/.claude-<label>` with its own `settings.json` carrying the provider's base URL,
key, and model names. `CLAUDE_CONFIG_DIR` selects one at launch. Your normal `claude`
keeps hitting Anthropic. Nothing global is touched.

## Quick start

Paste a setup prompt into a Claude Code session and it does the work, asking before
anything irreversible:

| Profile | Provider | Model | Pins a dated build? | Extra process | Setup |
|---|---|---|---|---|---|
| `claude-ds4` | DeepSeek direct | `deepseek-v4-flash` / `-pro` | no | none | [prompt](profiles/deepseek-direct.md) |
| `claude-or-ds4` | OpenRouter | `deepseek-v4-flash-0731` | **yes** | proxy on :8799 | [prompt](profiles/openrouter.md) |
| `claude-nous` | Nous Portal | `deepseek-v4-flash-0731` | **yes** | proxy on :8800 | [prompt](profiles/nous.md) |
| `claude-kimi` | Moonshot | `kimi-k3` / `k3` | n/a | none | [prompt](profiles/kimi.md) |

Already have a profile and just want the corrected status line:

```sh
git clone https://github.com/STRML/cc-ds4 && cd cc-ds4
./install.sh --profile openrouter        # or --profile direct / --profile nous
```

> [!WARNING]
> **Claude Code's cost figure is wrong on these profiles, and plausibly wrong.**
> It prices whatever model name you give it against Anthropic's table. One measured
> session reported **$0.152731** against **$0.002637** actual. The multiplier scales
> with the output-token share, so you cannot divide it out. That is what the status
> line in this repo exists to fix.

## Which one: this is a privacy decision, not a performance one

The two DeepSeek profiles reach similar models at very different privacy costs, and
that should drive the choice.

**`claude-ds4` (DeepSeek direct) is faster and cheaper.** It measured 1.32s median time
to first token on a 33k prompt at roughly 50 tok/s, beating every OpenRouter provider
tested. Its prompt caching is implicit and automatic, which matters more than raw
speed for agent work: an identical 32,653-token prompt billed 32,653 tokens on the
first call and **13** on every call after. It needs no helper process.

You pay for that by sending your prompts to DeepSeek, under terms that permit
retention and training, on infrastructure in the PRC. For a scratch repo that may be
fine. For work code, customer data, or anything under an NDA, it is not.

**`claude-or-ds4` (OpenRouter) is slower and costs more, and is the safer default.**
OpenRouter sits between you and the inference provider, and it supports
**zero-data-retention routing**: set `zdr: true` and requests will only go to
endpoints contractually bound not to retain them. The setup here turns that on by
default.

The cost is real and worth stating plainly. You lose implicit caching, which is the
larger expense for agent workloads, because the only endpoint for this model that
supports it is DeepSeek's own and ZDR excludes it. You add a local proxy that has to
be running. Provider routing varies in speed and price from request to request.

**Use `claude-or-ds4` when a pinned build matters,** too. DeepSeek's own API accepts
exactly two model names and has no dated variants, so `deepseek-v4-flash` floats to
whatever they ship. OpenRouter serves `deepseek/deepseek-v4-flash-0731` explicitly at
1,048,576 context.

**`claude-kimi` is independent** of the other two and predates them. Same profile
structure, different provider.

## Zero data retention on OpenRouter

Two ways to turn it on, and you want the second.

Account-wide at [openrouter.ai/settings/privacy](https://openrouter.ai/settings/privacy)
applies to everything on the key, including other tools. Per-request is scoped to this
profile and cannot be silently changed from a web dashboard, so the `claude-or-ds4`
proxy injects it into every request body:

```json
"provider": {"zdr": true, "data_collection": "deny"}
```

Facts worth knowing before you rely on it:

- **`/api/v1/models/{id}/endpoints` does not expose any ZDR field.** The API tells you
  price, quantization, throughput, latency, and uptime per endpoint, but nothing about
  retention. The only way to learn the ZDR pool is to send `zdr: true` and read back
  which provider answered.
- **ZDR is not as restrictive as it sounds.** 7 of 11 endpoints survive the filter
  for `deepseek-v4-flash-0731`:

  | | endpoints |
  |---|---|
  | ✅ ZDR-eligible | DeepInfra, Fireworks, Novita, Parasail, SiliconFlow, Io Net, Mancer 2 |
  | ❌ filtered out | GMICloud, Cloudflare, AtlasCloud |
- **It does not cost you quantization quality.** DeepInfra answers most requests
  because it is cheapest, and it is the one fp4 endpoint in the pool, but the other six
  ZDR providers are fp8 and routing reaches them regularly.
- **It can cost you context, and this one bites.** Endpoints for the same model do not
  all serve the same window. Io Net is ZDR-eligible but caps at 262,100 tokens against
  1,048,576 everywhere else, so a long session that happens to route there overflows
  the endpoint rather than the window you configured. The proxy adds
  `ignore: ["Io Net"]` for exactly this reason. Recheck `context_length` per endpoint
  when the provider list changes.
- **Verify by pinning, not by observing.** Provider selection fluctuates enough that
  a handful of samples will mislead you badly. To test whether a specific provider is
  ZDR-eligible, send `provider: {"only": ["Novita"], "zdr": true}` and see whether it
  answers or returns "No endpoints found matching your data policy".

## The `claude-nous` variant: Nous Portal

[`claude-nous`](profiles/nous.md) is a third way to reach the same pinned
`deepseek-v4-flash-0731` model, billed through [Nous Portal](https://portal.nousresearch.com)
rather than OpenRouter or DeepSeek directly. Same per-profile isolation, same
per-tier effort proxy — the differences are the point:

- **It can be far cheaper.** Nous exposes the discounted per-token rate in its
  `/v1/models` pricing (at the time of writing, 90% off the `-0731` list price).
  The status line prices sessions at that rate, live. The discount is a promotion,
  not a guarantee — for that reason treat `claude-nous` as opportunistic, and
  re-check the fallback rates in `src/statusline/nous.py` if the pricing changes.
- **No zero-data-retention control.** Nous 403s OpenRouter's
  `provider: {zdr: true}` block (empty body — the portal rejects the unknown
  `provider` field). The proxy runs with `DS4_ZDR=0`. Privacy-wise this is a
  **direct-style** profile, not an OpenRouter-style one: requests are governed by
  Nous's own, undisclosed retention policy. Do not reach for it with NDA data.
- **A subscription, optionally topped up.** It exposes **no public credits or
  balance endpoint**, so the status line shows only the session cost — no `📆 7d`
  or `💳 left` segments. The balance lives in the portal dashboard.
- **Cloudflare.** Nous sits behind Cloudflare, which 403s the stdlib's default
  `urllib` User-Agent (`error code: 1010`). The proxy now sends a `curl`-style UA
  (`DS4_UA`) on every outbound request — required for Nous, harmless for the other
  profiles it forwards.
- **Pinned build.** Like OpenRouter, Nous serves the dated `deepseek-v4-flash-0731`
  at 1,048,576 context, so the build does not float. It also lists a
  `~deepseek/...-latest` alias, which the setup deliberately avoids.

Like the OpenRouter profile it needs the effort proxy running (on `:8800`); the
launcher starts it on demand.

## Neither DeepSeek profile can see images

Verified by sending a real PNG, not by reading capability metadata. An image with the
words "PURPLE 7391 / ZEBRA MARMALADE" plus a prompt asking for a transcription:

| endpoint | model | result |
|---|---|---|
| DeepSeek direct | `deepseek-v4-flash[1m]` | replied `NO IMAGE` |
| DeepSeek direct | `deepseek-v4-pro[1m]` | replied `NO IMAGE` |
| OpenRouter | `deepseek-v4-flash-0731` | HTTP 404, `No endpoints found that support image input` |

DeepSeek V4 is text-only, and no `deepseek*` model on OpenRouter accepts image input.

> [!CAUTION]
> The direct endpoint drops image blocks **without any error**. The model then answers
> from your surrounding text as though the image were absent, so a screenshot it never
> received produces a confident wrong answer rather than a refusal.

Keep a vision-capable profile for those turns. On OpenRouter this at least fails
loudly with a 404; on the direct endpoint it does not fail at all.

## Things that cost real time to discover

<details>
<summary>Nine findings, each of which wasted an hour somewhere. Worth reading before you debug anything.</summary>

- **Base URL trailing path differs by provider.** Claude Code appends `/v1/messages`
  itself. OpenRouter wants `https://openrouter.ai/api` with no `/v1`; adding it
  yields `/v1/v1/messages` and 404s everything.
- **`ANTHROPIC_API_KEY` usually needs to be `""`, not absent.** A stale value there
  surfaces as a confusing model-not-found rather than a clean 401.
- **Effort control is not portable.** `CLAUDE_CODE_EFFORT_LEVEL` is a single global
  with no per-tier variant. OpenRouter takes `reasoning_effort` as a request
  parameter; DeepSeek ignores that spelling entirely and takes `output_config.effort`
  instead. Neither accepts effort inside a model ID for these models. This is the
  entire reason `claude-or-ds4` needs a proxy: tiers are the only per-request knob
  Claude Code exposes, so the proxy reads a sentinel model name and rewrites it.
- **Silence is not success.** DeepSeek drops unknown parameters without error, so a
  200 response proves nothing about whether your parameter did anything. Probe with a
  deliberately invalid value: if it errors, the field is real. OpenRouter's
  Anthropic-compatible endpoint does honour `provider` routing, which is not obvious
  and is worth confirming the same way.
- **On OpenRouter, one model id is many deployments.** Price, quantization, speed, and
  caching all vary per provider. `/api/v1/models` shows only the cheapest endpoint,
  which is why pricing looks flat when it is not. Output price varied 5.6x across
  providers for one model here.
- **Do not benchmark providers by hand.** OpenRouter publishes per-endpoint
  throughput, latency, and uptime from real traffic at
  `/api/v1/models/{id}/endpoints`, with far more samples than you can generate.
- **The `/model` picker's first entry lies.** "Default (recommended)" shows
  Anthropic's model name and pricing regardless of what the profile points at. Pick a
  named tier instead.
- **`CLAUDE_CODE_AUTO_COMPACT_WINDOW` does not set the context window**, only the
  compaction threshold. Without `CLAUDE_CODE_MAX_CONTEXT_TOKENS`, Claude Code resolves
  an unrecognised model name to a 200,000 default and you lose 80% of a 1M window
  without any warning. Details and the measurement in the OpenRouter file.
- **The cost figure in the status line is wrong, by a lot.** Claude Code prices
  whatever model name you gave it against Anthropic's table, so a session that cost
  cents on OpenRouter can display as dollars. Measured here: **$0.152731 shown against
  $0.002637 actual**, and the multiplier grows with the output-token share, so it is
  not a constant you can divide out. It reads as plausible, which is worse than
  reading as zero. Both setup prompts include a replacement bar that prices sessions
  correctly.
- **Implicit caching is the whole cost story on the direct profile.** One real session
  billed 38.5M cache-read tokens against 472k fresh input. At DeepSeek's $0.0028 per
  million for a cache hit that is 11 cents instead of $5.39. Anything that breaks
  cache reuse costs far more than any per-token price difference between providers.
- **One session's numbers are not a routing rule.** OpenRouter provider selection
  drifts enough between requests that consecutive batches will show you a pattern that
  is not there. Interleave your comparisons, or pin with `provider.only` and read the
  error.

</details>

## Layout

```
profiles/           setup prompts — paste one into Claude Code
  deepseek-direct.md    DeepSeek direct, no proxy. Fastest, least private.
  openrouter.md         OpenRouter, pinned -0731, ZDR, needs the proxy.
  nous.md               Nous Portal, pinned -0731, no ZDR, needs the proxy.
  kimi.md               Moonshot's Kimi K3.
src/
  effort_proxy.py       tier to effort, optional ZDR routing, context and output guards, /__spend
  statusline/
    common.py           transcript accounting and cost maths, shared
    direct.py           DeepSeek rates, balance-integrated spend
    openrouter.py       rates and spend from the proxy
    nous.py             rates from the proxy; no credits/balance segments
config/             cship configs with the Anthropic-only segments removed
tests/              41 tests over the money maths and transcript parsing
install.sh          point an existing profile at the corrected status line
```

All three profiles share a layout: a directory under `~/.claude-<label>`, everything
symlinked to `~/.claude` except `settings.json`, which is a real copy so the overrides
cannot leak back into your primary install.

## Installing the status line

The setup prompts handle this, but if the profile already exists:

```sh
./install.sh --profile openrouter     # or: --profile direct / --profile nous
./install.sh --profile direct --dry-run
```

`settings.json` is pointed at this checkout rather than a copy, so `git pull` updates
the bar. Verify it renders before walking away — a wrapper that fails open turns a
syntax error into a blank bar and exit 0:

```sh
src/statusline/direct.py
```

## Tests

```sh
python3 -m unittest discover -s tests -v
```

No dependencies beyond the standard library. The suite pins the published price
tables, the per-model cost split, the incremental transcript reader (including
partial trailing lines and compaction), and the ledger's handling of top-ups.

## Credits

The 200k context-window trap, the inherited-`MAX_OUTPUT_TOKENS` gap, the ~150x cost
overstatement, and the launcher that starts the proxy on demand all came from
[@seanperkins](https://github.com/seanperkins), who ran this guide end to end on zsh
and wrote up what broke. Those findings were re-verified here on Claude Code 2.1.220
before being folded in.

Two of their notes are corrected rather than copied. `max_completion_tokens` is not a
single model-wide 65536: it varies per endpoint from 65536 to 1048576, and 65536 is
the floor across the ZDR pool rather than the model's ceiling. And `context_length`
varies per endpoint too, which is a trap their notes do not cover.
