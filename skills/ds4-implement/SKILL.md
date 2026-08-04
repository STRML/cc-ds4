---
name: ds4-implement
description: "Dispatch a headless DeepSeek-V4 implementation subagent (ds4, or-ds4, nous-ds4) that writes code in the working tree and runs tests, with an opus-5 main loop as coordinator. Use for quick implementation changes executed by DeepSeek V4."
---

# ds4-implement

Dispatch a write-capable, `--role implement` subagent on a cc-ds4 profile. The
child edits files in the working tree and can run tests (Bash). **This is the
only ds4 role that can write.**

## CRITICAL: sandbox-disabled

The Bash call MUST use `dangerouslyDisableSandbox: true`. The child's
`session-env/` writes hit an EPERM inside the coordinator's sandbox, so the
dispatch Bash call must escape it. The child is scoped to the cwd (project
working tree) and uses `--permission-mode acceptEdits` — it auto-accepts file
writes and auto-denies anything outside `--add-dir`.

## Dispatch

```bash
~/.claude/skills/ds4-skill-family/bin/ds4-run \
  --profile {nous|openrouter} \
  --tier high \
  --role implement \
  --prompt-text '<task description>'
```

- Profile: prefer `nous` for cheap edits; `openrouter` (ZDR) when the context
  carries private code.
- Tier: default `high`. Use `xhigh` for the hardest bugs; `low` for mechanical
  sweeps and boilerplate.
- The child sees the repo from the coordinator's cwd. Give it a specific task
  and a clear acceptance check (test to run, invariant to verify).

## Output

The child reports `result:<what it did>`. After the run, `git diff` to review
the changes in the tree. Don't merge or commit without your own review pass.
