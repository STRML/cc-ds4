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

**The escalation only exists on `direct`.** `direct` is the one profile whose
endpoint serves the pro family. On `openrouter` and `nous` both families resolve
to the same flash model and both `-xhigh` sentinels carry the same default
effort, so `--tier pro-xhigh` and `--tier flash-xhigh` produce a byte-identical
upstream request — the verify step reruns the artifact's own tier under a
different name. The one thing that does change is adverse: the proxy gives the
pro family a single upstream attempt and flash three, on the assumption that pro
is the main loop and has its own retry. Asking for pro on nous therefore buys
less retry against the flakiest upstream and nothing else.

So: verify on `direct` when you need a real tier step, or verify with a Claude
or Fable pass. Do not read a `pro-*` tier on `openrouter`/`nous` as a stronger
model.
