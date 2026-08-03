# Handoff: fix 12 code-review findings in cc-ds4

Paste this whole file into a fresh session in `/Users/samuelreed/git/oss/cc-ds4`.

## Context you need

This repo runs DeepSeek V4 inside Claude Code through per-profile config
directories. Commit `3396866` merged three separate proxy scripts into one
`src/proxy.py` that serves three profiles on three localhost ports:

    31500  direct      ~/.claude-ds4       api.deepseek.com/anthropic
    31501  openrouter  ~/.claude-or-ds4    openrouter.ai/api
    31502  nous        ~/.claude-nous      inference-api.nousresearch.com

A code review of that commit produced the 12 findings below. Your job is to fix
them.

**Line numbers are from commit `3396866` and are already stale.** The working
tree has since gained a SessionStart kickstart hook
(`src/ds4-proxy-kickstart.sh`, wired in `install.sh`) plus a status line prefix
change, and your own fixes will shift things further. Treat every line number as
a hint. Locate the real site by the quoted code in each finding, and re-read the
file before each edit.

Uncommitted work is in the tree when you start. Run `git status` first and do not
discard anything you did not write.

**The machine you are on is actively using these proxies.** One launchd agent,
`com.strml.cc-ds4.proxy`, runs the single process. Killing it kills every live
Claude Code session on all three profiles. Do not run `launchctl bootout`,
`pkill`, or `install.sh` against the live agent until the very end, and say so
before you do.

## Ground rules

- Fix the cause, not the symptom. No band-aids.
- Every fix gets a regression test in `tests/`. A fix with no test is not done.
- Do not reformat or "tidy" code you are not fixing. Keep the diff reviewable.
- Comments explain *why*, never *what*. Match the existing density and tone.
- No em dashes anywhere. Use a period, a comma, a colon, or parentheses.
- Work through the list in order. Commit in logical groups, not all at once.

## Verification, run after every fix

    python3 -m unittest discover -s tests     # currently 80 tests, all pass
    /usr/bin/python3 -m unittest discover -s tests   # 3.9, what launchd runs
    python3 -m compileall -q src tools
    shellcheck --severity=warning install.sh
    bash -n install.sh

All five must be clean before you commit. CI runs the same set plus a job that
boots the proxy and sends it a request.

---

## The findings

Eight are CONFIRMED (verified against the code by an independent pass). Four are
PLAUSIBLE, meaning the mechanism is real but the trigger may not occur in
practice. For a PLAUSIBLE one, decide and say which way you went. Not fixing it
is a valid answer if you explain why.

### 1. CONFIRMED, security. `src/proxy.py:174` sends the OpenRouter key to Nous.

`api_key()` checks the process-wide `OPENROUTER_API_KEY` before the profile's own
`settings.json`. Before the merge each profile was its own process with its own
launchd environment, so that variable could be scoped. Now one process serves all
three, and with `OPENROUTER_API_KEY` exported, `api_key(nous_cfg)` returns it and
`get_json()` sends it as a bearer token to `inference-api.nousresearch.com`.

Fix: read the key from the profile's `settings.json` first. If you keep an env
override at all, namespace it per profile (`DS4_KEY_NOUS` and friends). Do not
leave a single unscoped variable that can reach any upstream.

### 2. CONFIRMED. `install.sh:163` kills live sessions on other profiles.

Every run unconditionally boots out `com.strml.cc-ds4.proxy`, terminating the one
process serving all three profiles. The regenerated plist is `RunAtLoad=false`,
so nothing restarts it and the other profiles get connection-refused.

Fix: only reload when the generated plist differs from the one on disk. Write to
a temp file, compare, and skip `bootout`/`bootstrap` when identical. If a reload
is genuinely needed and the job was running, kickstart it again afterwards.

### 3. CONFIRMED. `install.sh:129` generates a plist with no environment.

The plist has no `EnvironmentVariables` block, so every `DS4_*` knob that
`src/proxy.py` reads and the profile docs advertise (`DS4_DEBUG`,
`DS4_NOTHINK_BELOW`, `DS4_IDLE_EXIT`, `DS4_UA`, `DS4_PORT_<NAME>`) is dead under
the installed agent. Setting one produces no effect and no error.

Fix: emit an `EnvironmentVariables` dict. Carry through any `DS4_*` present in
the environment at install time, or add an explicit flag for it. Whichever you
pick, the profile docs must describe how to set these under launchd.

### 4. CONFIRMED. `install.sh:98` makes `--no-proxy` delete the proxy.

The parent commit had `[ "$WANT_PROXY" = 1 ] && link ...` guarding proxy file
manipulation. The new cleanup loop at lines 98-103 runs unconditionally, so
`--no-proxy` removes the profile's proxy files while an earlier run's base URL
still points at the proxy port.

Fix: put the cleanup loop behind the `WANT_PROXY` guard.

### 5. CONFIRMED. `src/proxy.py:455` lets one busy port kill all three profiles.

`serve()` binds with no `try`/`except` and `main()` calls it in a bare loop. If
anything already holds 31501, the `ThreadingHTTPServer` constructor raises
`OSError` and the process exits before binding 31500 or 31502. `KeepAlive` then
restarts it into a loop.

Fix: guard each `serve()`. Log the failure, keep going, and exit nonzero only if
no profile bound at all.

### 6. CONFIRMED. `install.sh:31` accepts `--dir` for a port nothing binds.

`--dir` is accepted and documented at `install.sh:10`, but `PROFILES` in
`src/proxy.py` hardcodes the three directories and only binds a port when that
hardcoded directory exists. A custom-directory install writes a base URL for a
port that never opens.

Fix: either let `proxy.py` learn the directory (an override env var, or a small
config file `install.sh` writes), or make `install.sh` reject `--dir` with a
clear message. Rejecting is fine and much simpler. Update the usage text and the
header comment either way.

### 7. CONFIRMED. `src/proxy.py:141` drops the documented `DS4_ZDR` switch.

ZDR became a hardcoded `PROFILES` column. The old `effort_proxy.py:15` read
`DS4_ZDR` and `profiles/openrouter.md` still tells users `DS4_ZDR=0` disables it.
It now silently does nothing.

Fix: honour `DS4_ZDR` again, or delete the claim from the docs. Prefer honouring
it, since zero-data-retention routing is a privacy control and a stale doc that
overstates a privacy guarantee is the worse failure.

### 8. CONFIRMED. `src/statusline/openrouter.py:38` and `nous.py:38` read the old port name.

`proxy.py:454` renamed the override to `DS4_PORT_<NAME>`, but the status lines
still resolve `/__spend` through `DS4_PROXY_PORT` and `NOUS_PROXY_PORT`.
Overriding a port moves the listener without moving the reader, and the bar
silently drops its pricing and spend segments.

Fix: settle on one name and make both ends use it. Keep the old names working as
a fallback so an existing shell export does not break.

### 9. PLAUSIBLE. `src/proxy.py:364` can exit mid-response.

`_touch()` stamps `_last_seen` once at the top of `do_POST`, before the body is
read, and never again while the response is relayed. A streamed completion can
run for minutes. If nothing else holds the proxy up, `idle_watch` can call
`os._exit(0)` mid-stream and truncate the response.

Fix if you agree it is reachable: track in-flight requests and refuse to exit
while any are open, or stamp `_last_seen` inside the relay loop.

### 10. PLAUSIBLE. `src/proxy.py:156` chains the clamp and the thinking-disable.

Both branches read the pre-clamp `max_tokens` in one `if`/`elif`, so they stay
mutually exclusive only while `NOTHINK_BELOW` sits below `max_out`.
`NOTHINK_BELOW` is env-tunable, so raising it above 65536 makes the
thinking-disable silently stop firing on exactly the requests it was raised for.

Note there is an existing test asserting the current behaviour
(`test_a_clamped_call_still_keeps_thinking`). If you change the logic, change
that test and its docstring to match the new intent.

### 11. PLAUSIBLE. `install.sh:164` runs `bootstrap` unguarded under `set -e`.

`bootout` on line 163 is asynchronous and has `|| true`; `bootstrap` on 164 does
not. If the old job has not finished exiting, bootstrap fails, `set -e` aborts,
and the script stops after `settings.json` was already repointed, so the legacy
agent cleanup at 167-172 never runs.

Fix: wait for the unload, or retry bootstrap, or guard it and verify the job
loaded before continuing.

### 12. PLAUSIBLE. `src/proxy.py:348` prefix-matches other directories.

`claude_running()` tests `f"CLAUDE_CONFIG_DIR={cfg['dir']}" in out` as a bare
substring of `ps` output. A session with `CLAUDE_CONFIG_DIR=~/.claude-ds4-backup`
counts as the direct profile being in use. Because the idle check is global now,
one such process pins the shared proxy up for every profile forever.

Fix: match on a boundary. In `ps -E` output, environment entries are
space-separated, so require a space or end-of-line after the directory.

---

## When you are done

1. All five verification commands clean.
2. Re-read `src/proxy.py` and `install.sh` end to end and check the fixes did not
   contradict each other.
3. Commit in groups with a message that opens with the root cause, then bullets.
   End the message with:
   `Claude-Session: <this session's URL>`
4. Do not push or open a PR without asking.
5. Only then, offer to reload the live agent, and warn that it drops any live
   session on all three profiles.

## Things that are already true, do not "fix" them

- `<profile>/.ds4-sessions` is deliberately not `<profile>/sessions`. The latter
  is Claude Code's own state directory and must never be written to or reaped.
- `KeepAlive={SuccessfulExit:false}` in the plist is deliberate. With `KeepAlive`
  false and `RunAtLoad` false, launchd SIGTERMs the job a couple of minutes after
  kickstart. That was diagnosed from three agents all showing last exit `-15`.
- Ports 31500-31502 are deliberate: below the ephemeral range on both Linux
  (32768-60999) and macOS (49152-65535).
- The status line prefixes `ds-`, `or-`, `nous-` are deliberate. All three
  profiles reach the same model family, so the bar names the backend.
