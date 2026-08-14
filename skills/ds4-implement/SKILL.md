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
  --tier flash-xhigh \
  --role implement \
  --timeout 1800 \
  --prompt-text '<task description>'
```

**`--timeout` is not optional.** Omit it and the 300s default silently yields a
**no-op with exit 0** — no error, no edits, an empty `git diff`, and a result of
just `error:timed out after 300s`. Any real implementation task needs 1800.

- Profile: prefer `nous` for cheap edits; `openrouter` (ZDR) when the context
  carries private code. Never silently fall back to `nous` for private code when
  `openrouter` fails — that is a data decision, so surface it.
- Tier: default `flash-xhigh`. Use `pro-xhigh` for the hardest bugs;
  `flash-medium` for mechanical sweeps and boilerplate.
- The child sees the repo from the coordinator's cwd. Give it a specific task
  and a clear acceptance check (test to run, invariant to verify).

## Output

The child reports `result:<what it did>`. After the run, `git diff` to review
the changes in the tree. Don't merge or commit without your own review pass.

**The result channel is unreliable — never infer "no report" means "no work."**
A run has completed every edit correctly and still returned a malformed empty
`result:` with a leaked tool-call token. `git diff` is the source of truth, not
the report. Two consequences for the brief:

- Tell the child to leave changes **uncommitted**, so you can always see them.
- Ask for the report anyway (files changed, tests added, mutation-check result,
  final gate line), but treat its absence as "verify yourself", not as failure.

**You keep the verification obligation.** Run the project's gate yourself, and
mutation-check at least one load-bearing new test — revert the fix in place,
confirm the test fails, re-apply. Restore by re-applying, never `git checkout`,
since the child's work is uncommitted. A green suite proves the tests pass, not
that they would catch the bug.
