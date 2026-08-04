# Roles and tiers

| Role | permission | tier | when |
|---|---|---|---|
| plan | plan | xhigh/max | design, decomposition, architecture |
| review | plan | xhigh/max | critique a diff/spec; read-only |
| verify | plan | >= the tier that produced the artifact | adversarial re-check of a claim/artifact |
| implement | acceptEdits | high | make edits in the worktree |

Write-capable runs (`implement`) must use `dangerouslyDisableSandbox: true` on
the Bash call. Read-only roles (`plan`, `review`, `verify`) run inside the
sandbox.

Verify floor (mirrors the advisor-mode cabinet): never verify an artifact with
the same tier that produced it. ds4 `low` artifact → `high` verify; `high`
artifact → `max` verify; a max artifact is verified with a Claude/Opus main-loop
or Fable pass. Money/security/irreversible work never routes lower than `high`.
