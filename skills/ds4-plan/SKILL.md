---
name: ds4-plan
description: "Run a headless DeepSeek-V4 planning subagent (ds4, or-ds4, nous-ds4) for design, decomposition, and architecture work, with an opus-5 main loop as coordinator. Use when you want a plan drafted on a cc-ds4 profile or a design pass run on DeepSeek."
---

# ds4-plan

Dispatch a read-only, `--role plan` subagent on a cc-ds4 profile to produce a
design, decomposition, or architecture pass. The coordinator stays on the main
loop; DeepSeek V4 does the drafting.

## Dispatch

Run the CLI via a Bash call (read-only; the sandbox is fine):

```bash
~/.claude/skills/ds4-skill-family/bin/ds4-run \
  --profile {nous|openrouter|direct} \
  --tier {pro-xhigh|pro-medium} \
  --role plan \
  [--model <id>] [--timeout <secs>] \
  --prompt-text '<what to plan>'
```

- `nous` (cheapest, no ZDR) or `openrouter` (ZDR, slower) for real work;
  `direct` for scratch. Details in `../ds4-skill-family/references/profiles.md`.
- `xhigh` is the default tier for planning; use `max` for the hardest design
  forks. On `direct` the tier is ignored (no `reasoning_effort` on the endpoint).
- The child cannot write files — it returns a proposal. Say so in the prompt
  ("propose an approach; do not write files") so it doesn't try to draft into a
  plan file.

## Report

Synthesize the child's `result:<text>` into the session — the design, the
trade-offs, the open questions. Do not dump raw JSON. Ground any load-bearing
claim against the code before acting on it.
