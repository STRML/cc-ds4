---
name: ds4-skill-family
description: "Run headless DeepSeek-V4 subagents (ds4, or-ds4, nous-ds4) from any Claude profile for plan, implement, verify, and review work, with the opus-5 main loop as coordinator. Use when you want a task run on a cc-ds4 profile, spawn a ds4 worker, or dispatch a subagent on DeepSeek."
---

# ds4 subagent family

Dispatch headless `claude -p` children onto a cc-ds4 profile. The coordinator
(any Claude session) picks the profile, model tier, reasoning effort, and
permission mode per role. The child returns `result:<text>` / `error:<text>`.

## When to use

- Plan, review, verify, or implement on DeepSeek V4 from a normal Anthropic
  session (or any profile), keeping the expensive main loop as coordinator.
- A quick pass that does not need the Anthropic main loop's context.

## How to dispatch

Run the CLI via a Bash call. After `install.sh` the skill lands at
`~/.claude/skills/ds4-skill-family`; from any cwd use that absolute path.
If running from inside the cc-ds4 checkout the repo-relative path works:

```bash
~/.claude/skills/ds4-skill-family/bin/ds4-run \
  --profile {direct|openrouter|nous} \
  --tier {max|xhigh|high|low} \
  --role {plan|implement|verify|review} \
  [--model <id>] [--timeout <secs>] \
  --prompt-text '<prompt>'
```

The skill's own Bash calls must use `dangerouslyDisableSandbox: true` for
write-capable (`implement`) runs — the sandbox blocks the child's
`session-env/` writes. Read-only runs keep the sandbox.

Parse `result:<text>` / `error:<text>` from stdout. Exit 0 = ok.

Choose tier/role from `references/roles.md` and profile from `references/profiles.md`.

## Workflow

1. **Choose profile** — `nous` (cheapest, opportunistic) or `openrouter` (ZDR,
   safest) for real work; `direct` only for scratch. `references/profiles.md`.
2. **Choose tier** — `max`/`xhigh` for planning and load-bearing review; `high`
   for implementation; `low` for mechanical sweeps and quick verify. Never route
   money/security/irreversible work lower than `high` with a Fable/Opus verify.
   On `direct` the tier is ignored (the endpoint takes no `reasoning_effort` and
   exposes only `deepseek-v4-flash`/`-pro`) — use or-ds4/nous for effort control.
3. **Run** via Bash. Wait for completion; the CLI blocks until the child exits.
4. **Ground the result** — spot-check load-bearing claims against the code
   (`references/roles.md` verify floor: never verify with the same tier that
   produced the artifact).
5. **Report** — synthesize the child's output; do not dump raw JSON.
