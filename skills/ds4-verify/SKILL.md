---
name: ds4-verify
description: "Run a headless DeepSeek-V4 verifier subagent (ds4, or-ds4, nous-ds4) to independently re-check a claim or artifact against the code, with an opus-5 main loop as coordinator. Use for adversarial verification of a finding, spec claim, or implementation change."
---

# ds4-verify

Dispatch a read-only, `--role verify` subagent on a cc-ds4 profile to
adversarially re-check a claim or artifact. The child reads the repo and returns
verified/discrepant/unfounded verdicts.

## Verify floor

**Never verify with the same tier that produced the artifact.** ds4 `low`
artifact → `high` verify; `high` artifact → `max` verify; a `max` artifact needs
a Claude or Fable pass (not another ds4 run). Money/security/irreversible work
never routes lower than `high`.

## Dispatch

```bash
~/.claude/skills/ds4-skill-family/bin/ds4-run \
  --profile {nous|openrouter} \
  --tier {high|xhigh|max} \
  --role verify \
  --prompt-text '<claim to verify; be specific>'
```

Read-only; sandboxed. Set `--tier` one step above the tier that produced the
artifact.

## Output

The child returns a `result:<verdict>` — verified, discrepant, or unfounded,
with file:line evidence. Ground the three highest-severity claims yourself
before acting.
