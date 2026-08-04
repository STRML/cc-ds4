# ds4-subagent-skills Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Ship a skill family in this repo (`skills/ds4-skill-family/`) that lets a coordinator on any Claude profile dispatch headless `claude -p` subagents onto the `claude-ds4`, `claude-or-ds4`, and `claude-nous` profiles — with the model tier, reasoning effort, and read-only/editing behavior chosen per role (plan / implement / verify / review / loops).

**Architecture:** A single SKILL.md exposes the `Skill(ds4:...)` workflow. It delegates all the shelling-out to one Python CLI (`bin/ds4-run`), so the coordinator instructs a Bash call (not a child), and the output contract is `result:<text>` / `error:<text>` parsed from the `--output-format json` transcript. The CLI scrubs inherited Claude/ds4/cmux env (the vision.py recipe), sets `CLAUDE_CONFIG_DIR` to the target profile, and shells out to a child `claude -p` with a fixed flag set + role-appropriate permission mode. An optional bash launcher (`bin/ds4-effort`) writes the profile's `effort-override` file that the proxy already reads.

**Tech Stack:** Python 3 stdlib (no deps), Claude Code `claude -p` CLI, the existing per-profile `effort-override` mechanism (`src/proxy.py:302`). The skill, CLI, and launcher are wired into profiles via `install.sh` (one new symlink) and a one-line addition to the README layout section.

## Global Constraints

- The child never runs under `--bare` (strips keychain auth). Use the composed lean-query flags from `~/.claude/docs/guardrails/claude-code-lore.md`.
- `CLAUDE_CODE_DISABLE_CLAUDE_MDS=1` (kills the MDS banner that bloats headless input; also strips a `dcl` token). Verified: drops 7.7k→1.4k input tokens on a trivial call.
- Always `--no-session-persistence --strict-mcp-config --disable-slash-commands --settings '{"disableAllHooks":true}' --output-format json`. No session litter.
- The child inherits the coordinator's keys only for scrubbed profiles; the skill is useless outside this repo's machine, so no graceful degradation needed.
- Cost/monitoring is the coordinator's job (parse the JSON `usage` fields); the skill never does token math.
- Write-capable runs must use `dangerouslyDisableSandbox: true` on the Bash call (the sandbox blocks the child's `session-env/` writes). Read-only runs keep the sandbox.
- Profiles: `claude-ds4` (direct, no sentinel tiers — model is literal `deepseek-v4-flash[1m]`; no effort override), `claude-or-ds4` (tiers + effort), `claude-nous` (tiers + effort). Settings mirror `src/proxy.py:243` `PROFILES`.
- Skill family is installed as a symlink from the repo checkout; profiles' `skills/` dirs already symlink to `~/.claude/skills`.

---

### Task 1: Write the Python CLI `bin/ds4-run`

**Files:**
- Create: `skills/ds4-skill-family/bin/ds4-run`

**Interfaces:**
- Consumes: nothing
- Produces: `ds4-run --profile {direct|openrouter|nous} --tier {max|xhigh|high|low} [--role {plan|implement|verify|review}] [--model MODEL] [--timeout SECS] --prompt-text '<text>'` — prints `result:<text>` or `error:<text>` to stdout, exit 0/1.

- [ ] **Step 1: Write the script**

```bash
#!/usr/bin/env python3
"""Run a headless `claude -p` child on a cc-ds4 profile.

The coordinator instructs a Bash call (not a subagent). The child's
--output-format json transcript is parsed for `type=="result"` and its
`result` text. The transcript's `total_cost_usd` is Anthropic-table-priced
garbage on a ds4 profile, so it is never surfaced.
"""
import argparse, json, os, subprocess, sys

PROFILES = {
    "direct":   {"dir": "~/.claude-ds4",   "port": 31500, "sentinel": False},
    "openrouter": {"dir": "~/.claude-or-ds4", "port": 31501, "sentinel": True},
    "nous":     {"dir": "~/.claude-nous",  "port": 31502, "sentinel": True},
}
# ds4-* sentinel -> reasoning_effort (proxy.py EFFORT). --tier maps to these.
TIERS = {"max": "ds4-max", "xhigh": "ds4-xhigh", "high": "ds4-high", "low": "ds4-low"}
ROLE_PERMISSION = {"plan": "plan", "review": "plan", "verify": "plan", "implement": "acceptEdits"}

SCRUB_PREFIXES = ("ANTHROPIC_", "CLAUDE_CODE_", "DS4_")
SCRUB_EXACT = ("CLAUDE_CONFIG_DIR", "CLAUDECODE")

def _env(profile_dir):
    e = {k: v for k, v in os.environ.items()
         if not (any(k.startswith(p) for p in SCRUB_PREFIXES) or k in SCRUB_EXACT
                 or k.startswith("CMUX") or k in ("NODE_OPTIONS", "AI_AGENT"))}
    e["CLAUDE_CONFIG_DIR"] = profile_dir
    e["CLAUDE_CODE_DISABLE_CLAUDE_MDS"] = "1"
    return e

def _parse_result(stdout):
    dec = json.JSONDecoder(); start = 0
    while True:
        i = stdout.find("{", start)
        if i < 0: return None, "no JSON result block"
        try: obj, end = dec.raw_decode(stdout, i)
        except json.JSONDecodeError: start = i + 1; continue
        if isinstance(obj, dict) and obj.get("type") == "result":
            if obj.get("is_error"):
                return None, obj.get("result") or "subagent returned is_error"
            text = obj.get("result")
            if isinstance(text, str) and text.strip(): return text, None
        start = end

def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--profile", required=True, choices=list(PROFILES))
    ap.add_argument("--tier", required=True, choices=list(TIERS))
    ap.add_argument("--role", default="plan", choices=list(ROLE_PERMISSION))
    ap.add_argument("--model", default=None, help="literal model id (bypasses sentinel)")
    ap.add_argument("--timeout", type=int, default=300)
    ap.add_argument("--prompt-text", required=True)
    a = ap.parse_args()
    cfg = PROFILES[a.profile]
    if cfg["sentinel"]:
        model = a.model or TIERS[a.tier]
    else:
        model = a.model or "deepseek-v4-flash[1m]"
    dir_path = os.path.expanduser(cfg["dir"])
    cmd = ["claude", "-p",
           "--settings", '{"disableAllHooks":true}',
           "--permission-mode", ROLE_PERMISSION[a.role],
           "--model", model,
           "--tools", "Read,Write,Edit,Bash",
           "--disable-slash-commands", "--strict-mcp-config",
           "--no-session-persistence", "--output-format", "json",
           a.prompt_text]
    try:
        r = subprocess.run(cmd, env=_env(dir_path), capture_output=True,
                           text=True, timeout=a.timeout, cwd=os.getcwd(),
                           stdin=subprocess.DEVNULL)
    except subprocess.TimeoutExpired:
        print("error:timed out after %ds" % a.timeout); sys.exit(1)
    text, err = _parse_result(r.stdout)
    if err:
        print("error:" + err); sys.exit(1)
    print("result:" + text)

if __name__ == "__main__":
    main()
```

- [ ] **Step 2: chmod +x and sanity-run a trivial prompt**

Run:
```bash
chmod +x skills/ds4-skill-family/bin/ds4-run
skills/ds4-skill-family/bin/ds4-run --profile nous --tier low --prompt-text 'Reply with exactly: OK'
```
Expected: prints `result:OK` (or `result:OK ...`), exit 0.

- [ ] **Step 3: Commit**

```bash
git add skills/ds4-skill-family/bin/ds4-run
git commit -m "feat: add ds4-run headless subagent CLI"
```

---

### Task 2: Write the launcher `bin/ds4-effort`

**Files:**
- Create: `skills/ds4-skill-family/bin/ds4-effort`

**Interfaces:**
- Consumes: `$DS4_PROFILE` (default `nous`), existing `effort-override` in `<profile>/effort-override`
- Produces: writes the profile's `effort-override` file (atomic); stdout `effort: <level>` or `error:...`. Refuses on `direct` (unproven, mirrors `src/commands/ds4-effort.md`).

- [ ] **Step 1: Write the script**

```bash
#!/bin/bash
# Read or write a profile's effort override (proxy reads it per request).
# Usage: ds4-effort [--profile {direct|openrouter|nous}] [max|xhigh|high|medium|low|minimal|none]
set -euo pipefail
PROFILE="${DS4_PROFILE:-nous}"
case "$PROFILE" in
  direct) echo "error:effort is unproven on the direct profile"; exit 1 ;;
  openrouter|nous) ;;
  *) echo "error:unknown profile $PROFILE"; exit 1 ;;
esac
DIR="$HOME/.claude-$PROFILE"
FILE="$DIR/effort-override"
LEVEL="${1:-}"
if [ -z "$LEVEL" ]; then
  if [ -f "$FILE" ]; then printf 'effort: %s\n' "$(cat "$FILE")"
  else printf 'effort: (default)\n'; fi
  exit 0
fi
case "$LEVEL" in
  max|xhigh|high|medium|low|minimal|none) ;;
  *) echo "error:invalid level '$LEVEL'"; exit 1 ;;
esac
tmp="$(mktemp "$FILE.XXXXXX")"
printf '%s\n' "$LEVEL" > "$tmp"
mv "$tmp" "$FILE"
printf 'effort: %s\n' "$LEVEL"
```

- [ ] **Step 2: chmod +x and verify write/read on nous**

Run:
```bash
chmod +x skills/ds4-skill-family/bin/ds4-effort
DS4_PROFILE=nous skills/ds4-skill-family/bin/ds4-effort medium
DS4_PROFILE=nous skills/ds4-skill-family/bin/ds4-effort
DS4_PROFILE=nous skills/ds4-skill-family/bin/ds4-effort none
```
Expected: `effort: medium`, then `effort: medium`, then `effort: none`. File contains exactly `none\n`.

- [ ] **Step 3: Verify direct refuses**

Run: `DS4_PROFILE=direct skills/ds4-skill-family/bin/ds4-effort high`
Expected: `error:effort is unproven on the direct profile`, exit 1.

- [ ] **Step 4: Commit**

```bash
git add skills/ds4-skill-family/bin/ds4-effort
git commit -m "feat: add ds4-effort override launcher"
```

---

### Task 3: Write `SKILL.md` and `references/`

**Files:**
- Create: `skills/ds4-skill-family/SKILL.md`
- Create: `skills/ds4-skill-family/references/roles.md`
- Create: `skills/ds4-skill-family/references/profiles.md`

**Interfaces:**
- Consumes: Task 1 CLI, Task 2 launcher
- Produces: the invocable skill; docs that give the coordinator the flags for each role and the per-profile port/model/effort table.

- [ ] **Step 1: Write SKILL.md**

```markdown
---
name: ds4-skill-family
description: "Run headless DeepSeek-V4 subagents (ds4, or-ds4, nous-ds4) from any Claude profile for plan, implement, verify, and review work, with the opus-5 main loop as coordinator. Use when you want a task run on a cc-ds4 profile, spawn a ds4 worker, or dispatch a subagent on DeepSeek."
---

# ds4 subagent family

Dispatch headless `claude -p` children onto a cc-ds4 profile. The coordinator
(any Claude session) picks the profile, model tier, reasoning effort, and
permission mode per role. The child returns `result:<text>` / `error:<text>`.

## When to use

- Plan, review, verify, or implement on DeepSeek V4 from a normal Anthropic
  session (or any profile), keeping the expensive main loop as coordinator.
- A quick pass that does not need the Anthropic main loop's context.

## How to dispatch

Run the CLI via a Bash call:

```bash
skills/ds4-skill-family/bin/ds4-run \
  --profile {direct|openrouter|nous} \
  --tier {max|xhigh|high|low} \
  --role {plan|implement|verify|review} \
  [--model <id>] [--timeout <secs>] \
  --prompt-text '<prompt>'
```

The skill's own Bash calls must use `dangerouslyDisableSandbox: true` for
write-capable (`implement`) runs — the sandbox blocks the child's
`session-env/` writes. Read-only runs keep the sandbox.

Parse `result:<text>` / `error:<text>` from stdout. Exit 0 = ok.

Choose tier/role from `references/roles.md` and profile from `references/profiles.md`.

## Workflow

1. **Choose profile** — `nous` (cheapest, opportunistic) or `openrouter` (ZDR,
   safest) for real work; `direct` only for scratch. `references/profiles.md`.
2. **Choose tier** — `max`/`xhigh` for planning and load-bearing review; `high`
   for implementation; `low` for mechanical sweeps and quick verify. Never route
   money/security/irreversible work lower than `high` with a Fable/Opus verify.
3. **Run** via Bash. Wait for completion; the CLI blocks until the child exits.
4. **Ground the result** — spot-check load-bearing claims against the code
   (`references/roles.md` verify floor: never verify with the same tier that
   produced the artifact).
5. **Report** — synthesize the child's output; do not dump raw JSON.
```

- [ ] **Step 2: Write references/roles.md**

```markdown
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
```

- [ ] **Step 3: Write references/profiles.md**

```markdown
# Profiles

| profile | CLAUDE_CONFIG_DIR | port | sentinel tiers | effort override | privacy |
|---|---|---|---|---|---|
| direct | ~/.claude-ds4 | 31500 | no (literal `deepseek-v4-flash[1m]`) | no | sends to DeepSeek (retention/training) |
| openrouter | ~/.claude-or-ds4 | 31501 | yes (ds4-max/xhigh/high/low) | yes | ZDR on by default |
| nous | ~/.claude-nous | 31502 | yes | yes | no ZDR, 90% promo pricing |

Effort: `ds4-max`→max, `ds4-xhigh`→xhigh, `ds4-high`→high, `ds4-low`→low
(proxy.py `EFFORT`). The proxy must be up on the profile's port. The launcher
`bin/ds4-effort` writes the profile's `effort-override` file.

Cost warning: the child's `total_cost_usd` is Anthropic-table-priced garbage on
a ds4 profile. Price from the JSON `usage` fields instead (or trust the status
line). Every spawn pays a fixed system-prompt overhead (~1.4k input tokens with
`CLAUDE_CODE_DISABLE_CLAUDE_MDS=1`, or ~7.7k without).
```

- [ ] **Step 4: Link the family into `~/.claude/skills`**

Run:
```bash
ln -sfn /Users/samuelreed/git/oss/cc-ds4/skills/ds4-skill-family /Users/samuelreed/.claude/skills/ds4-skill-family
```
Note: the profiles' `skills/` dirs already symlink to `~/.claude/skills`, so the
link makes the skill invocable from the coordinator AND every ds4 profile.
(Symlink target `cc-ds4/skills`; the worktree path is transient.)

- [ ] **Step 5: Verify the skill lists and dispatch works from the linked path**

Run:
```bash
ls -la /Users/samuelreed/.claude/skills/ds4-skill-family
/Users/samuelreed/.claude/skills/ds4-skill-family/bin/ds4-run --profile nous --tier low --prompt-text 'Reply with exactly: OK2'
```
Expected: dir listing shows SKILL.md + bin; `result:OK2`.

- [ ] **Step 6: Commit**

```bash
git add skills/ds4-skill-family/SKILL.md skills/ds4-skill-family/references
git commit -m "feat: add ds4 skill family (plan/verify/review/implement)"
```

---

### Task 4: Add install.sh wiring + README layout

**Files:**
- Modify: `install.sh` (add the skill-family symlink install)
- Modify: `README.md` (layout section)

**Interfaces:**
- Consumes: the skill family at `$REPO/skills/ds4-skill-family`
- Produces: one `skills/` symlink per install into the profile's skills dir (a real dir or symlink).

- [ ] **Step 1: Add the symlink to install.sh**

After the existing `commands` install block (~line 138), add:

```bash
# The ds4 subagent skill family. The profile's skills/ dir is usually a symlink
# to ~/.claude/skills; install into it directly so a normal `claude` (and every
# profile) can invoke the skill. Best-effort — a skills dir that is a real dir
# still works.
SKILL_SRC="$REPO/skills/ds4-skill-family"
SKILL_DST="$DIR/skills/ds4-skill-family"
if [ -e "$SKILL_SRC" ] && [ ! -e "$SKILL_DST" ]; then
  ln -s "$SKILL_SRC" "$SKILL_DST" 2>/dev/null && echo "skill:    $SKILL_DST -> $SKILL_SRC" || echo "skill:    (skills dir unavailable; symlink manually: ln -s $SKILL_SRC \$(dirname $SKILL_DST))" >&2
fi
```

- [ ] **Step 2: Add the README layout line**

Add to the `src/` tree in the Layout section (after the `src/commands/` line):

```
  skills/
    ds4-skill-family/      headless ds4 subagents for plan/verify/review/implement
```

- [ ] **Step 3: Verify install.sh --dry-run**

Run: `./install.sh --profile nous --dry-run`
Expected: prints the skill line (or the manual-symlink warning) without writing.

- [ ] **Step 4: Commit**

```bash
git add install.sh README.md
git commit -m "feat: wire ds4 skill family into install and README"
```

---

### Task 5: Smoke-test end to end

**Files:** none (verification only)

**Interfaces:** none

- [ ] **Step 1: Run the suite**

Run: `python3 -m unittest discover -s tests -v`
Expected: existing suite passes (skill adds no Python; nothing breaks).

- [ ] **Step 2: Real plan run on nous**

Run:
```bash
skills/ds4-skill-family/bin/ds4-run --profile nous --tier xhigh --role plan \
  --prompt-text 'In 3 sentences, propose an approach to add a /foo command to cc-ds4. Do not write files.'
```
Expected: `result:` + a 3-sentence plan.

- [ ] **Step 3: Real verify run on nous**

Run:
```bash
skills/ds4-skill-family/bin/ds4-run --profile nous --tier high --role verify \
  --prompt-text 'Check src/proxy.py: does the classifier route to Anthropic when DS4_CLASSIFIER is unset? Answer yes/no with one line of evidence.'
```
Expected: `result:yes ...` (or a grounded no) — verify the grounded claim yourself.

- [ ] **Step 4: Real review run (read-only, sandboxed)**

Run:
```bash
skills/ds4-skill-family/bin/ds4-run --profile nous --tier xhigh --role review \
  --prompt-text 'Review src/proxy.py's failover section for correctness. List up to 3 issues.'
```
Expected: `result:` + 0-3 grounded findings. Spot-check any load-bearing claim against `src/proxy.py`.

- [ ] **Step 5: Optionally implement a scratch edit (write-capable, sandbox-disabled)**

Run (sandbox-disabled) a tiny self-reverting change:
```bash
skills/ds4-skill-family/bin/ds4-run --profile nous --tier high --role implement \
  --prompt-text 'Append the line "# ds4-smoke" to /tmp/ds4-smoke.txt then say DONE.'
```
Expected: file exists with the line; `result:DONE`. Then `rm /tmp/ds4-smoke.txt`.

- [ ] **Step 6: Report results**

Summarize: which of plan/verify/review/implement produced grounded results, any
profile/tier gotchas, and the effort-override side effect (if any).

---

## Self-Review

- **Spec coverage:** plan/implement/verify/review → Task 3 roles + Task 5 smoke; opus-5 coordinator → the skill is invocable from any `claude`; per-profile mapping → references/profiles.md; loops → SKILL.md "Workflow" (coordinator drives iteration).
- **Placeholder scan:** no TODOs/TBDs; every step has concrete code or commands.
- **Type consistency:** `--role`/`--tier`/`--profile` names are consistent across Task 1, Task 3, Task 5. `ds4-run`/`ds4-effort` paths match between tasks.
