---
name: ds4-review
description: "Run a headless DeepSeek-V4 code-review subagent (ds4, or-ds4, nous-ds4) to critique a diff, spec, or plan read-only, with an opus-5 main loop as coordinator. Use when you want a review pass on DeepSeek V4 or a second-opinion critique of a change."
---

# ds4-review

Dispatch a read-only, `--role review` subagent on a cc-ds4 profile to critique a
diff, spec, or plan. The child cannot write files; it returns findings with
file:line evidence (when it can Read the repo).

## Dispatch

Shells out via Bash. Read-only; no `dangerouslyDisableSandbox` needed.

```bash
~/.claude/skills/ds4-skill-family/bin/ds4-run \
  --profile {nous|openrouter} \
  --tier {pro-xhigh|pro-medium} \
  --role review \
  --timeout 300 \
  --prompt-text '<diff or review prompt>'
```

- Profile: prefer `openrouter` when the diff/context carries private code (ZDR).
  `nous` for scratch; `direct` for quick turnaround where privacy is unconstrained.
- Tier: `xhigh` (default) or `max` for load-bearing merges. or-ds4 runs are
  slow enough that a `--timeout 300`+ is routinely needed.
- The child can Read the repo from the spawn cwd. Pass the diff via the prompt
  if it's small, or point the child at a branch/commit range.

## Output

report:<text> — the findings. Spot-check load-bearing claims against the code,
especially any claim citing a file:line that looks precise (verify it with Read).
Never let the review gate a merge without also reading the three highest-severity
claims yourself.
