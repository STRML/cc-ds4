# Roles and tiers

| Role | permission | tier | when |
|---|---|---|---|
| plan | plan | pro-xhigh | design, decomposition, architecture |
| review | plan | pro-xhigh | critique a diff/spec; read-only |
| verify | plan | above the tier that produced the artifact | adversarial re-check of a claim/artifact |
| implement | acceptEdits | flash-xhigh | make edits in the worktree |

Write-capable runs (`implement`) must use `dangerouslyDisableSandbox: true` on
the Bash call. Read-only roles (`plan`, `review`, `verify`) run inside the
sandbox.

Verify floor (mirrors the advisor-mode cabinet): never verify an artifact with
the same tier that produced it. A `flash-medium` artifact → `flash-xhigh`
verify; `flash-xhigh` → `pro-xhigh`; a `pro-xhigh` artifact is verified with a
Claude/Opus main-loop or Fable pass, since there is no ds4 tier above it.
Money/security/irreversible work never routes lower than `pro-xhigh`.

The four tiers are the whole set `ds4-run --tier` accepts: `pro-xhigh`,
`pro-medium`, `flash-xhigh`, `flash-medium`. Anything else exits 2.
