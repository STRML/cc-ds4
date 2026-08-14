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
  --timeout 900 \
  --prompt-text '<diff or review prompt>'
```

- Profile: prefer `openrouter` when the diff/context carries private code (ZDR).
  `nous` for scratch; `direct` for quick turnaround where privacy is unconstrained.
  Never silently fall back to `nous` for private code when `openrouter` fails.
- Tier: `xhigh` (default) or `max` for load-bearing merges. or-ds4 runs are slow;
  300 is too tight for a real review, use 900+.
- The child Reads the repo **from the spawn cwd** — so `cd` into the worktree that
  holds the commit under review, in the same command. Launching from the session's
  default directory silently reviews whatever branch happens to be checked out
  there. Echo `pwd && git rev-parse --abbrev-ref HEAD` ahead of the run to prove it.
- Auth fails fast and cheap (`401 User not found` when a profile has no credentials
  file). Before a long run, burn ten seconds on a `--tier pro-medium` probe that
  just asks it to reply `AUTH OK`.

## What it is good at

Ask for adversarial work explicitly and it delivers: "try to construct an input
that defeats this loop" produced a real defeating input rather than a description
of one. It will also check a claim against a real artifact if you name the artifact
("verify this against `<capture file>`, not against the fixture") — which is how you
catch a test suite whose fixture and parser share the same wrong assumption.

## Output

report:<text> — the findings. Spot-check load-bearing claims against the code,
especially any claim citing a file:line that looks precise (verify it with Read).
Never let the review gate a merge without also reading the three highest-severity
claims yourself.
