# PROMPT: Set up a `claude-ds4` launcher (DeepSeek V4 inside Claude Code, via ccam)

> One of three Claude Code provider profiles. [Index](../README.md) · [`claude-or-ds4` — DeepSeek via OpenRouter](openrouter.md) · [`claude-kimi` — Moonshot Kimi K3](kimi.md)


Paste everything below this line into a Claude Code session (or any capable coding
agent with shell access) on the machine you want set up. It requires macOS or Linux.

---

You are setting up an isolated Claude Code profile that runs **DeepSeek V4** against
DeepSeek's own Anthropic-compatible endpoint, launched with a dedicated `claude-ds4`
command. One small local proxy sits in the path, for one reason given in step 5; there
is no gateway and no format translation. The user's normal `claude` command must keep
working against Anthropic, completely untouched. Follow these instructions exactly.
Where a step says ASK, stop and ask the user.

## Non-negotiable safety rules

1. **Never** write `ANTHROPIC_BASE_URL`, `ANTHROPIC_AUTH_TOKEN`, or any model
   override into `~/.claude/settings.json`, `~/.zshrc`, `~/.bashrc`,
   `~/.config/fish/config.fish`, or any other global location. A global base-URL
   override silently reroutes EVERY Claude Code session on the machine, including
   ones currently running. All overrides go only into the copied
   `~/.claude-ds4/settings.json` created below.
2. **Never** run `claude-switch` (a ccam command) and disable it as described in
   step 9. It replaces `~/.claude` with a symlink; if `~/.claude` is a real directory
   (the normal case), switching would displace the user's primary installation.
3. Do not modify, move, or delete anything already inside `~/.claude/`. You will only
   read from it and symlink to it.
4. If any command fails, stop and show the user the error rather than improvising.

## What this endpoint actually does

Verified live 2026-08-02, not taken from documentation. Read this before configuring,
because several of these facts contradict what you would reasonably assume.

- Base URL is `https://api.deepseek.com/anthropic`. Claude Code appends
  `/v1/messages` itself.
- **Exactly two model names are accepted**: `deepseek-v4-flash` and
  `deepseek-v4-pro`. Anything else is an error naming those two. There is no dated
  build: `deepseek-v4-flash-0731` and `deepseek-v4-flash-20260731` are both rejected.
  If the user needs a pinned dated build, this endpoint cannot provide it and they
  want an OpenRouter setup instead.
- **Claude model names auto-map, by prefix**, which is the useful part. Anything
  starting with `claude-opus` resolves to `deepseek-v4-pro`; `claude-sonnet` and
  `claude-haiku` prefixes resolve to `deepseek-v4-flash`. This makes `/model` a real
  model switch rather than a relabelling.

  The prefix match is the whole string, not a fixed list: `claude-opus-anything`,
  `claude-opus-4-pro`, and bare `claude-opus` all return `deepseek-v4-pro`. This
  matters when a tool hardcodes Claude model IDs and gives you no override — the
  request still routes sensibly. **When you can set the name yourself, use the
  literal `deepseek-v4-pro` / `deepseek-v4-flash` instead.** Claude Code's `/model`
  picker displays the raw configured string, and the literal names say exactly what
  they call.

- **DeepSeek's docs are wrong about the fallback.** They state unsupported model
  names default to `deepseek-v4-flash`. They do not: `totally-made-up-name` returns
  an error naming the two valid models. Only `claude-*` prefixed names map. Every
  other string must be an exact valid name or the request fails.
- **Context ceiling is at least 1,030,000 tokens.** A single request of that size was
  accepted and billed at 996,485 input tokens.

- **Append `[1m]` to the model name or Claude Code will think you have 200K.**
  `CLAUDE_CODE_AUTO_COMPACT_WINDOW` sets only the compaction threshold; it does not
  declare the window, and no environment variable does. Claude Code infers the window
  from the model ID, where a `[1m]` suffix means 1M. DeepSeek accepts and strips that
  suffix — `deepseek-v4-flash[1m]` resolves to `deepseek-v4-flash` — so it satisfies
  both sides at once. Note the brackets are load-bearing: `deepseek-v4-flash-1m`
  is rejected outright.
- **Prompt caching is implicit and automatic.** `cache_control` is ignored entirely.
  In measurement, an identical 32,653-token prompt billed 32,653 input tokens on the
  first call and **13** on every call after, with the rest served as cache reads.
- `output_config.effort` is accepted and validated (an invalid value returns a
  deserialisation error naming the field). However, across three runs each at `low`,
  `high`, and `max`, median output tokens came out 225 / 191 / 272, which is not
  monotonic and is swamped by run-to-run variance. Treat effort as unproven here and
  use the global `CLAUDE_CODE_EFFORT_LEVEL` instead.
- The shared `/ds4-effort` command is installed on this profile too, but refuses
  to write: with effort unproven here there is nothing to override, and the
  command says so rather than pretend.
- `reasoning_effort` (the OpenRouter spelling) is **silently ignored**. So are
  unknown parameter names generally. Do not rely on an absent error meaning success.
- Also ignored: `anthropic-beta`, `anthropic-version`, `container`, `mcp_servers`,
  `cache_control`, `top_k`, `disable_parallel_tool_use`, and image, document, and
  redacted-thinking content blocks. `thinking` is supported but `budget_tokens` is
  ignored.
- **Thinking mode is on by default and it breaks Claude Code's small calls.** This is
  why this profile needs a proxy. Claude Code sends
  `thinking: {"type":"adaptive","display":"omitted"}`; DeepSeek does not implement
  `adaptive`, so V4 keeps thinking. The main loop at `max_tokens=32000` is fine. A
  utility call is not: at `max_tokens=512` with a forced tool decision, 3 of 5 runs
  returned `stop_reason=max_tokens`, two of them with no `tool_use` block at all. The
  permission classifier behind `defaultMode: auto` is one of these calls, so the
  symptom is intermittent classifier errors.
- **`thinking: {"type":"disabled"}` is honoured, and turns all of it off.** Output on
  that same call drops to 141-175 tokens from 386-512, and latency to 2.0s from 5.2s.
  Note the contrast with the bullet above: the Anthropic spelling works where
  `reasoning_effort` does nothing. Published reports that V4 has no reachable
  non-thinking mode are describing the OpenAI-compatible endpoint.
- **While thinking is on, `tool_choice` naming a specific tool is rejected**:
  `400 Thinking mode does not support this tool_choice`, every time, in 0.4s. `auto`,
  `none`, and omitting it are accepted. Disabling thinking makes the named form work.
- **An assistant message carrying a `tool_use` must carry its `thinking` block back**,
  or the request 400s with "The `content[].thinking` in the thinking mode must be
  passed back to the API". Claude Code 2.x does replay it, verified on the wire, so
  this is a latent trap rather than a live failure. It is what breaks the
  OpenAI-format routers, which drop the block in translation.
- Measured performance at a 33k-token prompt: 1.32s median time to first token, ~50
  tok/s end to end, 10.1% coefficient of variation over 8 runs.

## Step 0 — Preflight

```bash
command -v claude && claude --version
command -v git
command -v python3
echo $SHELL
ls -la ~/.claude 2>/dev/null | head -5
test -L ~/.claude && echo "~/.claude IS A SYMLINK" || echo "~/.claude is a real dir (expected)"
ls ~/.claude-ds4 2>/dev/null && echo "PROFILE ALREADY EXISTS — ask before overwriting"
```

- If `claude` is missing: `npm install -g @anthropic-ai/claude-code` (ASK first if npm
  is also missing).
- If `~/.claude` doesn't exist, have the user run `claude` once and exit.
- If `~/.claude` is already a symlink, the user actively uses ccam's switch mechanism.
  ASK before applying step 9, and point step 3's symlinks at the resolved directory.

## Step 1 — Get the API key

Key comes from https://platform.deepseek.com/api_keys. Have the user paste it. If they
don't have one, proceed with the literal placeholder `REPLACE_WITH_DEEPSEEK_API_KEY`
and tell them at the end where it goes.

Check the balance before going further, since a dry account fails in confusing ways:

```bash
curl -s https://api.deepseek.com/user/balance -H "authorization: Bearer $KEY"
```

DeepSeek's own docs specify `ANTHROPIC_API_KEY`. `ANTHROPIC_AUTH_TOKEN` also works and
is what this setup uses, matching the other ccam profiles. **Set only one.** Having
both populated causes auth conflicts.

## Step 2 — Install ccam (skip if already present)

```bash
test -d ~/.local/share/ccam || git clone https://github.com/dody87/ccam ~/.local/share/ccam
```

- **zsh/bash:** run `bash ~/.local/share/ccam/install.sh`, then reload the rc file.
- **fish:** upstream install.sh only targets bash/zsh. Create
  `~/.config/fish/conf.d/ccam.fish`:

```fish
# ccam-style account launcher (fish port; claude-switch intentionally omitted)
set -l _ccam_file $HOME/.config/ccam/accounts
if test -f "$_ccam_file"
    while read -l _ccam_line
        set -l _ccam_label (string replace -r '#.*' '' -- $_ccam_line | string trim)
        test -z "$_ccam_label"; and continue
        set -l _ccam_dir "$HOME/.claude-$_ccam_label"
        test -d "$_ccam_dir"; or continue
        alias claude-$_ccam_label "env CLAUDE_CONFIG_DIR=$_ccam_dir command claude"
    end < "$_ccam_file"
end
```

Register the account:

```bash
mkdir -p ~/.config/ccam
touch ~/.config/ccam/accounts
grep -qx "ds4" ~/.config/ccam/accounts || echo "ds4" >> ~/.config/ccam/accounts
```

## Step 3 — Build the profile directory

Everything shareable is a symlink into `~/.claude`, so skills, hooks, agents, and
plugins stay live in both profiles. Only `settings.json` is a copy, because that is
where the overrides live and they must not leak back.

```bash
mkdir -p ~/.claude-ds4
cd ~/.claude-ds4
for f in CLAUDE.md RTK.md agents commands hooks plugins rules skills statusline-command.sh workflows; do
  [ -e "$HOME/.claude/$f" ] && ln -sfn "$HOME/.claude/$f" "$f"
done
ls -la ~/.claude-ds4
```

Verify every symlink resolves before continuing; a dangling one silently drops that
capability from the profile.

## Step 4 — Write settings.json

Replace `DEEPSEEK_KEY_HERE`, then run. Note `ANTHROPIC_API_KEY` is set to an empty
string rather than removed, and that the base URL points at the local proxy from step
5 rather than at DeepSeek. Everything else about the profile is unchanged by that:
the proxy forwards to `https://api.deepseek.com/anthropic`.

```bash
python3 - <<'EOF'
import json, os, stat
src = os.path.expanduser("~/.claude/settings.json")
dst = os.path.expanduser("~/.claude-ds4/settings.json")
s = json.load(open(src)) if os.path.exists(src) else {}
OPUS, FLASH = "deepseek-v4-pro[1m]", "deepseek-v4-flash[1m]"  # [1m] declares the context window
s.setdefault("env", {}).update({
    "ANTHROPIC_BASE_URL": "http://127.0.0.1:31500",   # the step 5 proxy, not DeepSeek
    "ANTHROPIC_AUTH_TOKEN": "DEEPSEEK_KEY_HERE",
    "ANTHROPIC_API_KEY": "",
    "ANTHROPIC_MODEL": FLASH,
    "ANTHROPIC_DEFAULT_OPUS_MODEL": OPUS,      # -> deepseek-v4-pro
    "ANTHROPIC_DEFAULT_FABLE_MODEL": OPUS,     # -> deepseek-v4-pro
    "ANTHROPIC_DEFAULT_SONNET_MODEL": FLASH,
    "ANTHROPIC_DEFAULT_HAIKU_MODEL": FLASH,
    "CLAUDE_CODE_SUBAGENT_MODEL": FLASH,
    "ENABLE_TOOL_SEARCH": "false",
    "CLAUDE_CODE_AUTO_COMPACT_WINDOW": "1048576",
    "CLAUDE_CODE_EFFORT_LEVEL": "xhigh",
})
s["model"] = FLASH
s.pop("fallbackModel", None)
tmp = dst + ".tmp"
json.dump(s, open(tmp, "w"), indent=2)
os.chmod(tmp, stat.S_IRUSR | stat.S_IWUSR)
os.replace(tmp, dst)
print("wrote", dst)
EOF
```

`fallbackModel` must go: Anthropic model IDs don't exist here and a fallback attempt
errors. `CLAUDE_CODE_EFFORT_LEVEL` accepts `low`, `medium`, `high`, `xhigh`; `max` is
normally session-only but persists when set through this variable, and it overrides
the in-session `/effort` command.

## Step 5 — The proxy

Every profile here routes through one shared proxy, `src/proxy.py`. It listens on
a separate port per profile (31500 for this one), so this profile's
`settings.json` is unaware it is shared. What differs between profiles is a row in
that file's `PROFILES` table, not a separate script.

What it does for this profile:

| Concern | Behaviour |
|---|---|
| small calls | `max_tokens` at or below 8192 gets `thinking: {"type":"disabled"}`; `DS4_NOTHINK_BELOW` moves the line |
| main loop | arrives at 32000, passed through untouched, thinking intact |
| history repair | gives any assistant `tool_use` message a placeholder `thinking` block if it has none, which only this endpoint requires |
| model names | passed through: DeepSeek takes real names and ignores `reasoning_effort` |
| debugging | `DS4_DEBUG=1` logs each rewrite and any non-200 status |

`./install.sh --profile direct` is what installs it: it points `settings.json` at
`http://127.0.0.1:31500`, and on macOS writes and loads a single launch agent,
`com.strml.cc-ds4.proxy`, that runs it. Confirm it answers:

```bash
launchctl kickstart gui/$(id -u)/com.strml.cc-ds4.proxy
sleep 1
curl -s -o /dev/null -w "proxy responded: %{http_code}\n" -X POST \
  http://127.0.0.1:31500/v1/messages -H 'content-type: application/json' \
  -d '{"model":"deepseek-v4-flash","max_tokens":8,"messages":[{"role":"user","content":"hi"}]}'
```

A 401 is expected and correct without a key in the header: it proves the proxy
forwarded upstream.

**Nothing works when the proxy is down.** Every request gets connection-refused,
which looks exactly like a bad endpoint or a bad key. Check `nc -z 127.0.0.1 31500`
before investigating anything else, and read `~/.claude-ds4-proxy.log`.

The proxy exits on its own once no profile is in use and nothing has come through
for `DS4_IDLE_EXIT` seconds (default 900). It counts a profile as in use when a
session token under `<profile>/.ds4-sessions` has a live PID, or when `ps` shows a
`claude` process with that `CLAUDE_CONFIG_DIR`. Set `DS4_IDLE_EXIT=0` to disable
that and run forever. Without a launcher there is nothing to start it again, so
step 8 matters.

On Linux, or without launchd, run it yourself: `python3 src/proxy.py &`.

Setting a knob under launchd: the plist bakes in whatever `DS4_*` variables are
exported when install.sh runs. For example, `DS4_IDLE_EXIT=0 ./install.sh
--profile direct` makes the agent run forever (also replaces the plist and
reloads the agent, which drops any live session on all three profiles).

## Step 6 — SessionStart notice

Model tiers resolving to different models is genuinely surprising six months later.
Write `~/.claude-ds4/session-start-info.sh`:

```sh
#!/bin/sh
cat <<'EOF'
=== claude-ds4 profile: DeepSeek direct ===

Endpoint: https://api.deepseek.com/anthropic via the local proxy on :31500
Usage and spend: https://platform.deepseek.com/usage

  opus, fable        -> deepseek-v4-pro     (larger, slower, costs more)
  sonnet, haiku      -> deepseek-v4-flash   (the default workhorse)
  default at startup -> deepseek-v4-flash

Pick a named tier in /model. The first entry, "Default (recommended)", shows
Anthropic's model name and $15/$75 pricing; both are wrong here and the string
it sends is not one of the configured ones.

Only two model names exist here; there is no dated build and 0731 cannot be
pinned. Effort is uniform via CLAUDE_CODE_EFFORT_LEVEL. Prompt caching is
implicit and automatic; cache_control is ignored. Context ceiling verified
above 1,030,000 tokens.

Nothing works if the proxy on :31500 is down — every request gets
connection-refused. Check it first: nc -z 127.0.0.1 31500
EOF
```

Then `chmod +x` it and append the hook without clobbering inherited ones:

```bash
chmod +x ~/.claude-ds4/session-start-info.sh
python3 - <<'EOF'
import json, os, stat
p = os.path.expanduser("~/.claude-ds4/settings.json")
hook = os.path.expanduser("~/.claude-ds4/session-start-info.sh")
s = json.load(open(p))
groups = s.setdefault("hooks", {}).setdefault("SessionStart", [])
groups = [g for g in groups if not any("session-start-info.sh" in (h.get("command") or "")
                                       for h in (g.get("hooks") or []))]
groups.append({"hooks": [{"type": "command", "command": hook}]})
s["hooks"]["SessionStart"] = groups
tmp = p + ".tmp"
json.dump(s, open(tmp, "w"), indent=2)
os.chmod(tmp, stat.S_IRUSR | stat.S_IWUSR)
os.replace(tmp, p)
print("SessionStart groups:", len(s["hooks"]["SessionStart"]))
EOF
```

Filtering before appending keeps this idempotent across re-runs.

## Step 7 — Skip onboarding

```bash
echo '{"hasCompletedOnboarding": true}' > ~/.claude-ds4/.claude.json
chmod 600 ~/.claude-ds4/.claude.json
```

## Step 8 — Launcher

The ccam alias is not enough: this profile is dead without the proxy, and the
proxy has to be told a session started or it will time out under one. Override
the alias with a launcher that does both.

The launcher only covers the interactive path. When cmux restores this profile
after a relaunch it replays `claude --resume <id>` through its own wrapper, never
touching the launcher. `install.sh` registers a `SessionStart` hook
(`ds4-proxy-kickstart.sh`) that fires on resume too and starts the proxy, so a
restored session works on a cold boot.

Memory is shared across profiles: `install.sh` symlinks this profile's
`projects/*/memory` to the real `~/.claude/projects/*/memory` (see
`ds4-link-memory.sh`), so notes written on any profile are visible on all of
them.

Fish — write `~/.config/fish/conf.d/zz-ds4-proxy.fish`. **The filename must sort
after `ccam.fish`**, because ccam defines these as plain aliases in a loop and the
last definition wins. A file in `fish/functions/` will not work: fish skips
autoload entirely when a function is already defined. One file covers every
profile, so if another is already installed, add a function to it rather than
starting a second file.

```fish
set -g __ds4_label com.strml.cc-ds4.proxy

function __ds4_up --description 'Kickstart the shared proxy and wait for a port: <port> <name>'
    set -l port $argv[1]
    set -l name $argv[2]
    if not nc -z 127.0.0.1 $port 2>/dev/null
        launchctl kickstart gui/(id -u)/$__ds4_label 2>/dev/null
        for i in (seq 40)
            nc -z 127.0.0.1 $port 2>/dev/null; and break
            sleep 0.25
        end
    end
    if not nc -z 127.0.0.1 $port 2>/dev/null
        echo "$name: proxy never came up on :$port — see ~/.claude-ds4-proxy.log" >&2
        return 1
    end
end

function __ds4_run --description 'Register a session, run claude, deregister: <dir> <port> <name>'
    set -l dir $argv[1]
    set -l port $argv[2]
    set -l name $argv[3]
    __ds4_up $port $name; or return 1

    # The token is what stops the idle timer reaping the proxy under an open but
    # quiet session. .ds4-sessions, never sessions: the latter is Claude Code's own.
    set -l token $dir/.ds4-sessions/$fish_pid
    mkdir -p $dir/.ds4-sessions
    touch $token

    env CLAUDE_CONFIG_DIR=$dir command claude $argv[4..]
    set -l rc $status
    # Ctrl-C kills claude and fish resumes here, so this runs on that path too. A
    # hard-killed shell leaves the token; the proxy clears it once the PID dies.
    rm -f $token
    return $rc
end

function claude-ds4 --description 'Claude Code on the DeepSeek direct profile'
    __ds4_run $HOME/.claude-ds4 31500 claude-ds4 $argv
end

# One process, so this takes every profile down with it.
function claude-ds4-stop --description 'Stop the shared ds4 proxy'
    launchctl kill TERM gui/(id -u)/$__ds4_label 2>/dev/null
    echo "ds4 proxy stopped (all profiles)"
end
```

zsh/bash equivalent:

```bash
claude-ds4() {
  if ! nc -z 127.0.0.1 31500 2>/dev/null; then
    launchctl kickstart "gui/$(id -u)/com.strml.cc-ds4.proxy" 2>/dev/null
    for _ in $(seq 40); do nc -z 127.0.0.1 31500 2>/dev/null && break; sleep 0.25; done
  fi
  nc -z 127.0.0.1 31500 2>/dev/null || { echo "proxy never came up on :31500" >&2; return 1; }
  mkdir -p "$HOME/.claude-ds4/.ds4-sessions"
  local token="$HOME/.claude-ds4/.ds4-sessions/$$"
  touch "$token"
  CLAUDE_CONFIG_DIR="$HOME/.claude-ds4" claude "$@"
  local rc=$?
  rm -f "$token"
  return $rc
}
```

Ports are fixed rather than dynamic because `settings.json` has to carry a literal
base URL before Claude Code starts. 31500-31502 sit below the ephemeral range on
both Linux (32768-60999) and macOS (49152-65535), so an outbound connection cannot
take one first.

## Step 9 — Disable claude-switch

Only when `~/.claude` is a real directory. Append to the same rc file (fish: the
conf.d file from step 2):

```bash
claude-switch() { echo "claude-switch is disabled on this machine: ~/.claude is a real directory, not a ccam symlink. Use the claude-<name> launchers instead (e.g. claude-ds4)."; return 1; }
```

```fish
function claude-switch
    echo "claude-switch is disabled on this machine: ~/.claude is a real directory, not a ccam symlink. Use the claude-<name> launchers instead (e.g. claude-ds4)."
    return 1
end
```

## Step 10 — Verify

Confirm the tier mapping resolves server-side rather than trusting the config:

```bash
python3 - <<'EOF'
import json, os, urllib.request
s = json.load(open(os.path.expanduser("~/.claude-ds4/settings.json")))["env"]
for tier in ("ANTHROPIC_DEFAULT_OPUS_MODEL", "ANTHROPIC_DEFAULT_SONNET_MODEL"):
    m = s[tier]
    body = json.dumps({"model": m, "max_tokens": 12,
                       "messages": [{"role": "user", "content": "hi"}]}).encode()
    r = urllib.request.Request(s["ANTHROPIC_BASE_URL"] + "/v1/messages", data=body, method="POST")
    r.add_header("content-type", "application/json")
    r.add_header("authorization", "Bearer " + s["ANTHROPIC_AUTH_TOKEN"])
    d = json.load(urllib.request.urlopen(r, timeout=90))
    print(f"  {m:<26} -> {d.get('model')}")
EOF
```

Expect `deepseek-v4-pro[1m] -> deepseek-v4-pro` and
`deepseek-v4-flash[1m] -> deepseek-v4-flash`. Then:

This runs through the proxy, since the base URL in `settings.json` now points at it.
A connection-refused here means the proxy is not up, not that anything is misconfigured.
Then:

1. Open a NEW terminal so aliases load.
2. `claude-switch` prints the disabled message and changes nothing.
3. `claude-ds4` in any project directory; the SessionStart notice should appear, and
   the launcher should have started the proxy without being asked.
4. `/status` shows `http://127.0.0.1:31500`, which is the proxy, not a misconfiguration.
5. Ask something trivial to confirm a live response.
6. `/model` to opus, ask again, confirm it still answers (that path hits v4-pro).
7. `claude-ds4-stop`, then `nc -z 127.0.0.1 31500` should fail and a new `claude-ds4`
   should bring it back.
8. Run plain `claude` and confirm `/status` still shows Anthropic's API.

**Warn the user about the first entry in the `/model` picker.** It is labelled
"Default (recommended)" and advertises Anthropic's model and pricing, something like
"Opus 4 (1M context) · $15/$75 per Mtok". Both are wrong for this profile: nothing
here costs that, and the string that entry actually sends is Claude Code's internal
default rather than anything configured above. Since unknown names error rather than
falling back, that entry is not guaranteed to work. Tell the user to pick a named
tier explicitly. The custom entries below it are the configured, verified paths.

A 401 means the key is wrong or both auth variables are set. An error naming
`deepseek-v4-pro or deepseek-v4-flash` means a model string reached the API that is
neither an accepted name nor a mappable `claude-*` name. Connection-refused means the
proxy is down; check `~/.claude-ds4/proxy.log`.

## Step 11 — Optional: a status line that is not lying

Skip if the user does not use a status line. If they use `cship`, offer this. It is
the same fix as the OpenRouter profile's step 11, with different plumbing.

Claude Code prices whatever model name you hand it against **Anthropic's** table, so
a DeepSeek session's cost is overstated by roughly two orders of magnitude. It also
shows `usage_limits` — your real Anthropic subscription's 5h/7d windows and credit
balance — which is fetched over OAuth rather than from stdin and so cannot be
corrected, only dropped.

Run the installer from a checkout of this repo:

```bash
./install.sh --profile direct            # --dry-run first if you want to see it
src/statusline/direct.py                 # with a tty, prints a sample bar
```

It copies `config/cship-direct.toml` into the profile, backs up `settings.json`,
symlinks `~/.claude-ds4/ds4-statusline.py` at `src/statusline/direct.py` in the
checkout, and points `statusLine` at that symlink. `git pull` updates the bar.

Renders as `ds-deepseek-v4-flash  ░░░░░░░░░░3%  💰 <$0.01 · 📆 7d $0.31 · 💳 $9.54 left`.
The `ds-` prefix names the backend, matching the `or-` the OpenRouter profile uses.

Three things that differ from the OpenRouter version, and matter:

- **Rates are hardcoded** from https://api-docs.deepseek.com/quick_start/pricing,
  because DeepSeek's API exposes no pricing endpoint (`/models` returns bare ids).
  Check them when you install this; their docs flag a coming peak/off-peak policy at
  2x. Currently, per million: v4-flash $0.14 in / $0.28 out / $0.0028 cache hit,
  v4-pro $0.435 / $0.87 / $0.003625.
- **Tokens are bucketed per model.** This profile maps opus and fable to v4-pro and
  sonnet and haiku to v4-flash, priced 3x apart. Costing a whole session at whichever
  tier was active at render time would be wrong, so each transcript record is
  attributed to the model named on it.
- **DeepSeek reports a balance, not lifetime usage.** `/user/balance` gives a number
  that goes down as you spend and up when you top up, so 7-day spend integrates the
  drops between samples and treats a rise as a top-up worth zero. OpenRouter's
  `/api/v1/credits` returns monotonic `total_usage` and needs none of that.

## Final report to the user

- `claude-ds4` = DeepSeek V4, `claude` = normal Anthropic. Nothing global changed.
- The tier mapping, explicitly: opus and fable reach `deepseek-v4-pro`, everything
  else reaches `deepseek-v4-flash`.
- To pick a tier, use the named entries in `/model`, not "Default (recommended)" —
  that one's label and pricing are Anthropic's and do not describe this profile.
- Key location (`~/.claude-ds4/settings.json`, `env` block), that it is chmod 600, and
  that nothing works until a placeholder is replaced.
- Spend is at https://platform.deepseek.com/usage. There is no spend cap in this
  config; a runaway session bills against account balance.
- 0731 cannot be pinned here, and the flash alias floats to whatever DeepSeek ships.
- Skills, hooks, agents, commands, and plugins are symlinked, so edits apply to both
  profiles.
- `claude-switch` is deliberately disabled and why.
- Session history and per-project state accumulate separately in `~/.claude-ds4/`.
- The proxy on :31500 must be running, the launcher starts it, and `claude-ds4-stop`
  stops it. Connection-refused means it is down and not that the key or config is
  wrong. It exists because DeepSeek V4 cannot be talked out of thinking mode any other
  way, and thinking mode truncates Claude Code's small internal calls.
