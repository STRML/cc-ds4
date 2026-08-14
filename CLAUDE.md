# cc-ds4 — working rules

## Context

This repo ships an opinionated DeepSeek-V4-in-Claude-Code setup: per-profile
config dirs, a shared proxy, a corrected status line, and a skill family for
ds4-backed subagents. It's for power users who already have Claude Code installed
and want cheaper/faster subagent work on another provider.

Architecture details live in `codemaps/architecture.md` (token-lean map loaded
at session start). This file is the working rules — constraints, conventions,
and hazards that don't fit in a map.

## Before you touch anything

Read `codemaps/architecture.md`. The proxy profiles table, the failover knobs,
and the data flow diagram are load-bearing for correctness.

## Proxy

- One process serves all three profiles, each its own port (`:31500` / `:31501` /
  `:31502`). On macOS it's socket-activated by launchd; on Linux run it manually.
- `install.sh` writes the launch agent and symlinks. Never hand-edit a profile's
  `settings.json` after `install.sh` runs it — the base URL and hook come from
  install.
- The proxy must be running for any ds4 profile to work. On a cold start the
  SessionStart hook kickstarts it; the launcher function in `profiles/*.md` does
  the same on first launch.
- `src/go/internal/profiles/table.go` is the single source of truth for profile
  ports, upstreams, models, and failover config. Never duplicate that
  information outside the table.

## Profiles

- `claude-ds4` (direct): fastest, cheapest, no ZDR, sends prompts to DeepSeek
  (retention/training). Model is literal `deepseek-v4-flash[1m]`; no effort
  knob. Tier sentinels don't rewrite — the CLI passes literal model names.
  Failover from other profiles lands here flash-only (`FAILOVER_MODEL`), never
  pro.
- `claude-or-ds4` (OpenRouter): ZDR on, pinned `-0731` build, slower. Sentinel
  tiers + effort control.
- `claude-nous` (Nous Portal): cheapest (90% promo), pinned `-0731`, no ZDR.
  Sentinel tiers + effort control. No public credits/balance endpoint.
- A profile env MUST be scrubbed before spawning a child — follow the
  `vision._env()` recipe (strip `ANTHROPIC_*`, `CLAUDE_CODE_*`, `DS4_*`,
  `CLAUDE_CONFIG_DIR`, `CLAUDECODE`, `CMUX*`, proxy vars).

## Skills

- `skills/ds4-skill-family/bin/ds4-run` — headless `claude -p` dispatcher.
  Five skills are installed to `~/.claude/skills`: the umbrella
  `ds4-skill-family` plus four discrete role wrappers.
- Write-capable runs (`--role implement`) must use `dangerouslyDisableSandbox:
  true` on the Bash call. Read-only roles keep the sandbox.
- The child's `total_cost_usd` is Anthropic-table-priced garbage on ds4 — never
  price from it. Parse `usage` instead.

## Failure modes worth knowing

- **Auto-mode classifier relay flaps ("ds4-flash-xhigh temporarily unavailable").**
  The classifier is routed to the Anthropic subscription; when it 403s/524s
  the gate blocks every Bash call. It fails open to ds4 per proxy config but
  the CLI still gates. Retry with backoff — it's transient load.
- **or-ds4 is slow.** ZDR + provider routing variance means 180s+ timeouts are
  routine for review-type runs. Budget accordingly.
- **`total_cost_usd` is wrong.** The statusline has the correct per-session cost.
  Never trust the JSON `total_cost_usd` field on a ds4 profile.

## Testing

```sh
cd src/go && go test ./...                  # the proxy
python3 -m unittest discover -s tests -q    # status line + install, stdlib only
```

No pip deps. Pure stdlib. The suite pins published price tables, per-model cost
splits, the incremental transcript reader, failover breaker behavior (now with
500+consecutive-probe regressions), and the classifier/vision edge cases.

## Commit + PR conventions

- Branch first (`git checkout -b <slug>`), never commit to main.
- `git commit` is gated by the `commit-and-verify` skill; invoke it BEFORE
  reaching for `git add`/`git commit`.
- Use the standing PR pipeline: squash-merge, CI green on HEAD, no bot reviewer
  on this repo → merge on green (standing authorization).
- Commit messages follow `feat:` / `fix:` / `docs:` / `test:` / `chore:`.
