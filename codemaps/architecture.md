> Generated: 2026-08-05 | Token-lean format for LLM context

# cc-ds4 Architecture

cc-ds4 runs DeepSeek V4 inside Claude Code without touching the Anthropic setup.
Per-profile config dirs select a provider; a local proxy rewrites requests per
profile. Pure Python 3.9+, stdlib only, no deps.

## Layout

```
profiles/       setup prompts (deepseek-direct, openrouter, nous, kimi)
src/
  proxy.py          one proxy process, one port per profile
  vision.py         image -> text via local `claude -p --model haiku` child
  classifier.py     classifier -> Anthropic subscription relay
  ds4-proxy-kickstart.sh   SessionStart hook that starts the proxy
  ds4-link-memory.sh       shares project memory across profiles
  commands/ds4-effort.md   /ds4-effort slash command
  statusline/        common.py + direct.py / openrouter.py / nous.py
skills/
  ds4-skill-family/  headless ds4 subagent CLI (plan/verify/review/implement)
  ds4-plan/ ds4-review/ ds4-verify/ ds4-implement/   discrete role wrappers
config/         cship configs (per-provider)
tests/          unittest suite (269 tests)
tools/render_svg.py   statusline SVG rendering
install.sh      symlinks statusline/proxy/hooks into a profile
```

## Proxy (src/proxy.py, ~1050 lines)

One process serves all profiles, each on its own port. Reads `PROFILES` table;
socket-activated by launchd on macOS.

> A Go rewrite (`src/go/`, this repo) is byte-compatible with this proxy and
> the differential harness (`tests/diff/run_diff.py`) is GREEN against it —
> the Go proxy matches this Python proxy on the Phase A corpus. Production
> stays on this Python proxy until the Go binary implements launchd socket
> activation; the swap is gated on the harness staying green.

- **Request rewrite** (`rewrite`): maps `ds4-*` sentinel model → real upstream
  model + `reasoning_effort`; disables thinking when `max_tokens <= NOTHINK_BELOW`;
  injects ZDR block; injects missing thinking blocks (direct only).
- **Relay**: forwards to upstream, retries transient statuses, streams back.
- **Classifier routing** (`classifier.py`): the auto-mode permission classifier
  is forwarded to the Anthropic subscription (not DeepSeek) by default.
  `DS4_CLASSIFIER=zdr` routes it to the or-ds4 (OpenRouter ZDR) route instead;
  `DS4_CLASSIFIER=ds4` keeps it on the profile's own upstream. zdr fails open to
  Anthropic, then ds4.
- **Failover breaker** (circuit-breaker): on sustained transient errors a
  profile routes to its `failover` target. Closes only after
  `FAILOVER_PROBES_TO_CLOSE` consecutive clean probes.
- **Vision** (`vision.py`): image blocks → text via a local `claude -p` child,
  content-hash cached, fail-open.
- **Effort override**: per-profile `effort-override` file read on each request.

### PROFILES table (src/proxy.py:279)

| profile | port | upstream | model | zdr | max_out | failover |
|---|---|---|---|---|---|---|
| direct | 31500 | api.deepseek.com/anthropic | none (literal) | no | — | none |
| openrouter | 31501 | openrouter.ai/api | deepseek/deepseek-v4-flash-0731 | yes | 65536 | none |
| nous | 31502 | inference-api.nousresearch.com | deepseek/deepseek-v4-flash-0731 | no | 65536 | direct |

### Key constants (src/proxy.py)

| constant | default | meaning |
|---|---|---|
| `NOTHINK_BELOW` | 8192 | disable thinking when max_tokens ≤ this |
| `TRANSIENT_STATUS` | {429,500,502,503,524,529} | statuses treated as transient (retried + striker) |
| `FAILOVER_WINDOW` / `FAILOVER_RATE` | 12 / 0.25 | breaker window and trip threshold |
| `FAILOVER_RECHECK` | 60s | min gap between probes |
| `FAILOVER_PROBES_TO_CLOSE` | 3 | consecutive clean probes to close circuit |
| `EFFORT` | max/xhigh/high/low | `ds4-*` sentinel → reasoning_effort |
| `IDLE_EXIT` | 900s | proxy exits after idle |
| `CLASSIFIER_ROUTE` / `CLASSIFIER_MODEL` | anthropic / claude-sonnet-5 | classifier relay target; `zdr` routes to or-ds4 (OpenRouter ZDR) |

## Vision (src/vision.py)

Image blocks → text descriptions. `_child_cmd` spawns `claude -p --model haiku`
on the Anthropic profile (scrubbed env). `rewrite_images` swaps base64 images;
cache by content hash. `DS4_VISION=0` restores pass-through. Budget: 8 images/request.

## Statusline (src/statusline/)

`common.py` has the `Statusline` base (transcript accounting, cost maths,
`harvest_usage`). Subclasses: `direct.py` (DeepSeek rates + balance), `openrouter.py`
(rates + spend from proxy), `nous.py` (rates from proxy; no credits/balance — no
public endpoint). The bar prices sessions at real per-token rates (Claude Code's
own cost figure is wrong on ds4 profiles).

## Skills (skills/)

`ds4-skill-family/bin/ds4-run` — dispatches headless `claude -p` children on a
profile; scrubs env, sets `CLAUDE_CONFIG_DIR`, parses `result:`/`error:`.
`ds4-effort` reads/writes the effort override. Four role wrappers fix `--role`.
`ds4-implement` is write-capable and needs sandbox off.

## Data flow

```
claude -p --model ds4-xhigh
  -> ANTHROPIC_BASE_URL http://127.0.0.1:31502 (nous)
  -> proxy: rewrite (sentinel -> model+effort, thinking off if small)
  -> classifier? -> Anthropic subscription (sonnet-5) : upstream
  -> upstream -> relay streams back
  failover: nous -> direct on transient error burst
```

## Tests (tests/, 269 tests)

Unit + integration (FakeUpstream, mock). Coverage: proxy rewrite/relay, failover
breaker (incl. 500 + consecutive-probe regressions), vision cache, statusline
money maths, classifier relay, install.sh (shell-outs to throwaway profiles).
