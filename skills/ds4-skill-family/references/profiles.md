# Profiles

| profile | CLAUDE_CONFIG_DIR | port | sentinel tiers | effort override | privacy |
|---|---|---|---|---|---|
| direct | ~/.claude-ds4 | 31500 | no (literal ids; the tier's family half still picks pro vs flash) | no | sends to DeepSeek (retention/training) |
| openrouter | ~/.claude-or-ds4 | 31501 | yes (ds4-{pro,flash}-{xhigh,medium}) | yes | ZDR on by default |
| nous | ~/.claude-nous | 31502 | yes | yes | no ZDR, 90% promo pricing |

A sentinel is `ds4-<family>-<effort>`: the family half picks the model, the
effort half the default reasoning effort. Only `direct` actually serves the pro
family; on or-ds4 and nous both families resolve to the same flash model.
The proxy must be up on the profile's port. The launcher
`bin/ds4-effort` writes the profile's `effort-override` file.

The `direct` profile takes no sentinel tier and no effort override: the endpoint
takes the literal ids `deepseek-v4-pro[1m]`/`deepseek-v4-flash[1m]` and ignores
`reasoning_effort`. The tier's FAMILY half still applies — `--tier pro-xhigh` on
direct runs pro, the one endpoint that serves it — but the effort half has
nowhere to go, so `pro-xhigh` and `pro-medium` are the same request.
Use or-ds4/nous when the task needs effort control or a verify floor.

When `nous` fails over to `openrouter`, every tier runs `deepseek/deepseek-v4-flash-0731:nitro` —
the fallback is deliberately flash-only, never pro.

Latency: `openrouter` (ZDR) is markedly slower than `nous`/`direct` per request —
a review run on or-ds4 exceeded 180s and needed a 300s timeout, while the same
run on nous finished well under that. Budget the `--timeout` accordingly for
or-ds4 review/plan runs.

Cost warning: the child's `total_cost_usd` is Anthropic-table-priced garbage on
a ds4 profile. Price from the JSON `usage` fields instead (or trust the status
line). Every spawn pays a fixed system-prompt overhead (~1.4k input tokens with
`CLAUDE_CODE_DISABLE_CLAUDE_MDS=1`, or ~7.7k without).
