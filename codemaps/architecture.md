> Generated: 2026-08-05 | Token-lean format for LLM context

# cc-ds4 Architecture

cc-ds4 runs DeepSeek V4 inside Claude Code without touching the Anthropic setup.
Per-profile config dirs select a provider; a local proxy rewrites requests per
profile. The proxy is Go; the status line and install script are Python 3.9+,
stdlib only, no deps.

## Layout

```
profiles/       setup prompts (deepseek-direct, openrouter, nous, kimi)
src/
  go/               the proxy: one process, one port per profile
  ds4-proxy-kickstart.sh   SessionStart hook that starts the proxy
  ds4-link-memory.sh       shares project memory across profiles
  commands/ds4-effort.md   /ds4-effort slash command
  statusline/        common.py + direct.py / openrouter.py / nous.py
skills/
  ds4-skill-family/  headless ds4 subagent CLI (plan/verify/review/implement)
  ds4-plan/ ds4-review/ ds4-verify/ ds4-implement/   discrete role wrappers
config/         cship configs (per-provider)
tests/          status line + install tests (85, stdlib only)
tools/render_svg.py   statusline SVG rendering
install.sh      symlinks statusline/proxy/hooks into a profile
```

## Proxy (src/go/, Go)

One process serves all profiles, each on its own port, socket-activated by
launchd on macOS. Built by `install.sh` to `src/go/cmd/ds4-proxy/ds4-proxy`,
which the launch agent execs.

A Python implementation preceded this one. It was deleted after a differential
harness proved the two byte-identical; what that harness asserted survives as
`TestRewriteMatchesPythonGolden`, which replays the Python proxy's frozen
output from `internal/proxy/testdata/rewrite_golden.json`.

### Packages

| package | holds |
|---|---|
| `internal/profiles` | the profile table (`table.go`), home expansion, port resolution |
| `internal/proxy` | handler, rewrite, relay, classifier, breaker, spend, vision, idle, effort |
| `internal/sockets` | launchd socket activation (cgo) and the plain-bind fallback |
| `internal/relay` | the idle-deadline dial wrapper |
| `internal/jsonpy` | order-preserving JSON codec reproducing Python's `json.dumps` bytes |
| `cmd/ds4-proxy` | `--ports` for install.sh, listener setup, idle watch |

- **Request rewrite** (`rewrite.go`): sentinel → real model + `reasoning_effort`;
  disables thinking when `max_tokens <= nothinkBelow`; injects the ZDR block
  (skipping models listed in `ZDRSkipModels`); injects missing thinking blocks
  (direct only).
- **Relay** (`relay.go`): forwards upstream, retries transient statuses, streams
  back. Only the main-loop tier is exempt from in-proxy retry.
- **Classifier routing** (`classifier.go`): `DS4_CLASSIFIER` picks where the
  auto-mode permission gate is judged. The classifier `DS4_CLASSIFIER=zdr`
  routes it to the or-ds4 OpenRouter route; `DS4_CLASSIFIER=ds4` keeps it on the
  profile's own upstream. zdr fails open to Anthropic, then ds4. Detection keys
  on the flash-family sentinels plus a small `max_tokens`.
- **Failover breaker** (`breaker.go`): on sustained transient errors a profile
  routes to its failover target. Clean probes arm a real-request trial; the
  circuit closes only when that request is served clean by the profile's own
  upstream, since a probe passing on a lull is not proof of recovery.
- **Vision** (`vision.go`): image blocks → text via a local `claude -p` child,
  content-hash cached, fail-open. The child's environment is scrubbed so it
  cannot recurse back into the proxy.
- **Spend** (`spend.go`): `GET /__spend` serves pricing, credits, and rolling
  week spend for the status line.
- **Idle exit** (`idle.go`): exits once no profile is in use and nothing has
  come through, so launchd can restart on demand.
- **Effort override** (`effort.go`): per-profile `effort-override` file, stat-cached.

### Profile table (src/go/internal/profiles/table.go)

The single source of truth for ports, upstreams, and models. Nothing else in
the tree may declare them.

| profile | port | upstream | model | zdr | max_out | failover |
|---|---|---|---|---|---|---|
| direct | 31500 | api.deepseek.com/anthropic | none (family map only) | no | 65536 | none |
| openrouter | 31501 | openrouter.ai/api | deepseek/deepseek-v4-flash-0731:nitro | yes | 65536 | none |
| nous | 31502 | inference-api.nousresearch.com | deepseek/deepseek-v4-flash-0731 | no | 65536 | openrouter |

Each profile maps a sentinel's model family to an id. Only direct serves pro:
OpenRouter has no working host for pro-0813 and Nous lists no deepseek pro, so
both point the pro family at their flash id.

### Sentinels (src/go/internal/proxy/rewrite.go)

`ds4-<family>-<effort>`. Family picks the model, effort picks the default
reasoning effort. Precedence, weakest first: the sentinel's default, then a
client-sent `reasoning_effort`, then the `/ds4-effort` pin — the pin is on top
because the status line renders it as active.

| sentinel | family | default effort | slot |
|---|---|---|---|
| `ds4-pro-xhigh` | pro | xhigh | fable, main loop |
| `ds4-pro-medium` | pro | medium | opus |
| `ds4-flash-xhigh` | flash | xhigh | sonnet, subagents |
| `ds4-flash-medium` | flash | medium | haiku |

### Key constants

| constant | default | meaning |
|---|---|---|
| `nothinkBelow` | 8192 | disable thinking when max_tokens ≤ this |
| `transientStatus` | {429,500,502,503,524,529} | statuses treated as transient |
| `DS4_FAILOVER_WINDOW` / `_RATE` | 12 / 0.25 | breaker window and trip threshold |
| `DS4_FAILOVER_RECHECK` | 60s | min gap between probes |
| `DS4_FAILOVER_PROBES_TO_CLOSE` | 3 | consecutive clean probes to ARM a trial; a real request served cleanly is what closes the circuit |
| `DS4_IDLE_EXIT` | 900s | idle before exit; 0 disables |
| `DS4_CLASSIFIER` / `DS4_CLASSIFIER_MODEL` | anthropic / claude-sonnet-5 | classifier relay target; `zdr` routes to or-ds4 (OpenRouter ZDR) |

## Vision (src/go/internal/proxy/vision.go)

Image blocks → text descriptions. Spawns `claude -p --model haiku` on the
Anthropic profile with a scrubbed environment, so the child cannot recurse back
into this proxy. Only `source.type == base64` is transcribed; `file` and `url`
become placeholders, so a request body can never make the proxy open a path.
Cached by content hash. `DS4_VISION=0` restores pass-through.

The per-request image budget is enforced: past the cap an image becomes a
placeholder without spawning a child. Cache hits do not spend budget, or a
cached prefix would permanently placeholder everything after it. The Python
original declared the cap and never consulted it (STRML/cc-ds4#42).

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
claude -p --model ds4-pro-xhigh
  -> ANTHROPIC_BASE_URL http://127.0.0.1:31502 (nous)
  -> proxy: rewrite (sentinel -> model+effort, thinking off if small)
  -> classifier? -> Anthropic subscription (sonnet-5) : upstream
  -> upstream -> relay streams back
  failover: nous -> openrouter on transient error burst
```

## Tests

`cd src/go && go test ./...` covers the proxy: rewrite and relay, the failover
breaker (including the 500 and consecutive-probe regressions), vision cache and
env scrubbing, classifier detection, spend, idle exit, socket binding, and
`TestRewriteMatchesPythonGolden`, which replays the deleted Python proxy's
frozen output byte for byte.

`python3 -m unittest discover -s tests -q` covers what is still Python: status
line money maths and install.sh (shell-outs to throwaway profiles).
