# Profiles

| profile | CLAUDE_CONFIG_DIR | port | sentinel tiers | effort override | privacy |
|---|---|---|---|---|---|
| direct | ~/.claude-ds4 | 31500 | no (literal `deepseek-v4-flash[1m]`) | no | sends to DeepSeek (retention/training) |
| openrouter | ~/.claude-or-ds4 | 31501 | yes (ds4-max/xhigh/high/low) | yes | ZDR on by default |
| nous | ~/.claude-nous | 31502 | yes | yes | no ZDR, 90% promo pricing |

Effort: `ds4-max`→max, `ds4-xhigh`→xhigh, `ds4-high`→high, `ds4-low`→low
(proxy.py `EFFORT`). The proxy must be up on the profile's port. The launcher
`bin/ds4-effort` writes the profile's `effort-override` file.

Cost warning: the child's `total_cost_usd` is Anthropic-table-priced garbage on
a ds4 profile. Price from the JSON `usage` fields instead (or trust the status
line). Every spawn pays a fixed system-prompt overhead (~1.4k input tokens with
`CLAUDE_CODE_DISABLE_CLAUDE_MDS=1`, or ~7.7k without).
