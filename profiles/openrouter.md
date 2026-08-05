# PROMPT: Set up a `claude-or-ds4` launcher (DeepSeek V4 Flash 0731 via OpenRouter, in Claude Code)

> One of three Claude Code provider profiles. [Index](../README.md) · [`claude-ds4` — DeepSeek direct](deepseek-direct.md) · [`claude-kimi` — Moonshot Kimi K3](kimi.md)


Paste everything below this line into a Claude Code session (or any capable coding
agent with shell access) on the machine you want set up. It requires macOS or Linux.

---

You are setting up an isolated Claude Code profile that runs **DeepSeek V4 Flash
(0731 build)** instead of Anthropic's models, launched with a dedicated `claude-or-ds4`
command. The user's normal `claude` command must keep working against Anthropic,
completely untouched. Follow these instructions exactly. Where a step says ASK, stop
and ask the user before continuing.

## Non-negotiable safety rules

1. **Never** write `ANTHROPIC_BASE_URL`, `ANTHROPIC_AUTH_TOKEN`, or any model
   override into `~/.claude/settings.json`, `~/.zshrc`, `~/.bashrc`,
   `~/.config/fish/config.fish`, or any other global location. A global base-URL
   override silently reroutes EVERY Claude Code session on the machine, including
   ones currently running. All overrides go only into the copied
   `~/.claude-or-ds4/settings.json` created below.
2. **Never** run `claude-switch` (a ccam command) and disable it as described in
   step 9. It works by replacing `~/.claude` with a symlink; if `~/.claude` is a
   real directory (the normal case), switching would displace the user's primary
   installation. This setup uses a dedicated launcher command instead.
3. Do not modify, move, or delete anything already inside `~/.claude/`. You will
   only read from it and symlink to it.
4. If any command fails, stop and show the user the error rather than improvising
   a different mechanism.

## Step 0 — Preflight

Run and report results before changing anything:

```bash
command -v claude && claude --version   # Claude Code installed?
command -v git
command -v python3
echo $SHELL                              # fish, zsh, or bash — remember this
ls -la ~/.claude 2>/dev/null | head -5   # does a primary install exist? symlink or real dir?
test -L ~/.claude && echo "~/.claude IS A SYMLINK" || echo "~/.claude is a real dir (expected)"
ls ~/.claude-or-ds4 2>/dev/null && echo "PROFILE ALREADY EXISTS — ask user before overwriting"
```

- If `claude` is missing: `npm install -g @anthropic-ai/claude-code` (ASK first if
  npm is also missing).
- If `~/.claude` doesn't exist, the user has never run Claude Code. Have them run
  `claude` once and exit, so the directory exists.
- If `~/.claude` is already a symlink, this user actively uses ccam's switch
  mechanism — ASK before applying step 9's guard, and adapt: symlink targets in
  step 4 should point at the real directory the symlink resolves to.

## Step 1 — ASK which backend

The same model is reachable two ways. ASK the user which they want. **Default to
OpenRouter** unless they say otherwise.

| Backend | Base URL | Model ID | Auth | Notes |
|---|---|---|---|---|
| **OpenRouter** (default) | `https://openrouter.ai/api` | `deepseek/deepseek-v4-flash-0731` | real key required | 1M context. Hosted, nothing to run locally. |
| **Local ds4-server** | `http://127.0.0.1:8765` | `deepseek-v4-flash` | none (any placeholder) | Requires the GGUF on disk and the server running. Context limited by what you launched with. |

Both speak the Anthropic `/v1/messages` API, so Claude Code talks to either without
a translation layer.

Record their choice as BASE_URL and MODEL. If they pick local, remind them
`ds4-server` must be running before `claude-or-ds4` will respond, and that the pinned
`-0731` build is whatever `ds4flash.gguf` points at.

### If OpenRouter — get the key

Key comes from https://openrouter.ai/settings/keys. Have the user paste it; refer to
it as `$OPENROUTER_KEY`. If they don't have one yet, proceed with the literal
placeholder `REPLACE_WITH_OPENROUTER_API_KEY` and tell them at the end where to paste
the real key.

OpenRouter authenticates via `ANTHROPIC_AUTH_TOKEN`. `ANTHROPIC_API_KEY` must be set
to an **explicitly empty string**, not merely absent — OpenRouter's own Claude Code
guide calls this out, and leaving a stale key in place surfaces as a confusing
model-not-found error rather than a 401.

Note the base URL is `https://openrouter.ai/api` with **no** `/v1`. Claude Code
appends `/v1/messages` itself. Adding `/v1` yourself produces `/v1/v1/messages` and
every request 404s.

OpenRouter also supports a tilde-prefixed `~vendor/model-latest` form that floats to
the current build (e.g. `~deepseek/deepseek-v4-flash-latest`). This setup
deliberately avoids it and pins `-0731`.

Current OpenRouter pricing for this model is $0.09 per million input tokens and $0.18
per million output. The undated `deepseek/deepseek-v4-flash` costs more ($0.14/$0.28)
and floats to whatever build is current, which is why this setup pins `-0731`.

## Step 2 — ASK how effort should map to model tiers

Claude Code exposes four model tiers (fable, opus, sonnet, haiku). This setup points
all of them at one model and varies **reasoning effort** instead. ASK which approach:

Both options run the proxy. It is not optional any more: V4 keeps thinking whatever
Claude Code asks for, and that truncates every small internal call it makes, so the
proxy has to turn thinking off on those regardless of how effort is configured. Step 6
has the measurements. What the two options change is only whether the proxy also
rewrites effort per tier.

**Option A — uniform effort (simpler).**
All tiers use the same model and one global effort level. Set
`CLAUDE_CODE_EFFORT_LEVEL` once. Picking a tier in `/model` changes nothing about how
hard the model thinks.

**Option B — per-tier effort (what you probably want).**
Each tier maps to a different effort:

| Tier | Effort |
|---|---|
| fable | `max` |
| **opus (default)** | `xhigh` |
| sonnet | `high` |
| haiku | `low` |

This cannot be done with model IDs alone. OpenRouter accepts `reasoning_effort` as a
**request parameter**, and for this model there are no effort-suffixed slugs (no
`deepseek/deepseek-v4-flash-0731-high` or similar — verify with
`curl -s https://openrouter.ai/api/v1/models | grep v4-flash` if you want to confirm
before building the proxy). `CLAUDE_CODE_EFFORT_LEVEL` is a single global with no
per-tier variant. So a small proxy has to inject the parameter. Step 6 builds it.

Valid OpenRouter effort values: `max`, `xhigh`, `high`, `medium`, `low`, `minimal`,
`none`.

## Step 3 — Install ccam (account manager, used for its conventions)

```bash
git clone https://github.com/dody87/ccam ~/.local/share/ccam
```

- **zsh/bash:** run `bash ~/.local/share/ccam/install.sh`, then reload the rc file.
- **fish:** upstream install.sh only targets bash/zsh. Instead create
  `~/.config/fish/conf.d/ccam.fish` containing exactly:

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

Then create the accounts registry:

```bash
mkdir -p ~/.config/ccam
touch ~/.config/ccam/accounts
grep -qx "or-ds4" ~/.config/ccam/accounts || echo "or-ds4" >> ~/.config/ccam/accounts
```

## Step 4 — Build the profile directory

Everything shareable is a **symlink** into `~/.claude` (so skills, hooks, agents,
plugins, and instructions stay live in both profiles — edit once, applies
everywhere). Only `settings.json` is a **copy**, because that's where the overrides
live and they must not leak back.

```bash
mkdir ~/.claude-or-ds4
cd ~/.claude-or-ds4
for f in CLAUDE.md RTK.md agents commands hooks plugins rules skills statusline-command.sh workflows; do
  [ -e "$HOME/.claude/$f" ] && ln -s "$HOME/.claude/$f" "$f"
done
ls -la ~/.claude-or-ds4
```

(Missing entries are fine — the loop only links what exists.)

## Step 5 — Copy and modify settings.json

Substitute BASE_URL, MODEL, and the key from step 1. Every model variable must be
set; omitting one causes a silent failure in whatever scenario uses it.
`fallbackModel` must be removed if present, since Anthropic model IDs don't exist on
this endpoint and a fallback attempt would error.

**For Option A (uniform effort)** — replace the ALL-CAPS placeholders and run:

```bash
python3 - <<'EOF'
import json, os
src = os.path.expanduser("~/.claude/settings.json")
dst = os.path.expanduser("~/.claude-or-ds4/settings.json")
s = json.load(open(src)) if os.path.exists(src) else {}
MODEL = "MODEL_ID"
overrides = {
    "ANTHROPIC_BASE_URL": "BASE_URL",
    "ANTHROPIC_AUTH_TOKEN": "KEY_OR_PLACEHOLDER",
    "ANTHROPIC_MODEL": MODEL,
    "ANTHROPIC_DEFAULT_OPUS_MODEL": MODEL,
    "ANTHROPIC_DEFAULT_SONNET_MODEL": MODEL,
    "ANTHROPIC_DEFAULT_HAIKU_MODEL": MODEL,
    "ANTHROPIC_DEFAULT_FABLE_MODEL": MODEL,
    "CLAUDE_CODE_SUBAGENT_MODEL": MODEL,
    "ENABLE_TOOL_SEARCH": "false",
    "CLAUDE_CODE_AUTO_COMPACT_WINDOW": "1048576",
    "CLAUDE_CODE_MAX_CONTEXT_TOKENS": "1048576",
    "CLAUDE_CODE_MAX_OUTPUT_TOKENS": "65536",
    "CLAUDE_CODE_EFFORT_LEVEL": "xhigh",
}
s.setdefault("env", {}).update(overrides)
s["model"] = MODEL
s.pop("fallbackModel", None)
s["env"]["ANTHROPIC_API_KEY"] = ""   # must be blank, not absent
json.dump(s, open(dst, "w"), indent=2)
print("wrote", dst)
EOF
chmod 600 ~/.claude-or-ds4/settings.json
```

**For Option B (per-tier effort)** — same, but each tier gets a distinct sentinel
name that the step 6 proxy understands, and the base URL points at the proxy:

```bash
python3 - <<'EOF'
import json, os
src = os.path.expanduser("~/.claude/settings.json")
dst = os.path.expanduser("~/.claude-or-ds4/settings.json")
s = json.load(open(src)) if os.path.exists(src) else {}
overrides = {
    "ANTHROPIC_BASE_URL": "http://127.0.0.1:31501",
    "ANTHROPIC_AUTH_TOKEN": "KEY_OR_PLACEHOLDER",
    "ANTHROPIC_MODEL": "ds4-xhigh",
    "ANTHROPIC_DEFAULT_OPUS_MODEL": "ds4-xhigh",
    "ANTHROPIC_DEFAULT_SONNET_MODEL": "ds4-high",
    "ANTHROPIC_DEFAULT_HAIKU_MODEL": "ds4-low",
    "ANTHROPIC_DEFAULT_FABLE_MODEL": "ds4-max",
    "CLAUDE_CODE_SUBAGENT_MODEL": "ds4-high",
    "ENABLE_TOOL_SEARCH": "false",
    "CLAUDE_CODE_AUTO_COMPACT_WINDOW": "1048576",
    "CLAUDE_CODE_MAX_CONTEXT_TOKENS": "1048576",
    "CLAUDE_CODE_MAX_OUTPUT_TOKENS": "65536",
}
s.setdefault("env", {}).update(overrides)
s["model"] = "ds4-xhigh"
s.pop("fallbackModel", None)
s["env"]["ANTHROPIC_API_KEY"] = ""   # must be blank, not absent
json.dump(s, open(dst, "w"), indent=2)
print("wrote", dst)
EOF
chmod 600 ~/.claude-or-ds4/settings.json
```

Notes:

**`CLAUDE_CODE_AUTO_COMPACT_WINDOW` alone does not give you the 1M window.** It sets
the compaction threshold and nothing else. Claude Code still resolves an unrecognised
`ds4-*` sentinel to a 200,000 default, so you compact at 200k and waste 80% of the
window you came here for. `CLAUDE_CODE_MAX_CONTEXT_TOKENS` is what actually sets it.

Measured on Claude Code 2.1.220 by pointing `statusLine` at a script that dumps its
stdin payload, then reading `context_window.context_window_size`:

| profile env | reported window |
|---|---|
| `AUTO_COMPACT_WINDOW` only | 200000 |
| plus `MAX_CONTEXT_TOKENS=1048576` | 1048576 |
| DeepSeek direct, model name `deepseek-v4-flash[1m]` | 1000000 |

That last row is why only the OpenRouter profile is affected. Claude Code infers the
window from a `[1m]` suffix in the model ID, and the direct profile can carry one
because DeepSeek accepts and strips it. A sentinel like `ds4-xhigh` never can, so it
falls to the default. Note the suffix yields a round 1,000,000 while the env var gives
the exact 1,048,576.

The dump-the-statusline trick is the general tool here, and it is worth knowing
independently: it is the only place Claude Code exposes its resolved context window,
cost, and model name as machine-readable JSON.

- If the user picked local ds4-server, set both to whatever `-c/--ctx` the server was
  launched with instead, or compaction fires far too late and requests overflow.
- `CLAUDE_CODE_MAX_OUTPUT_TOKENS=65536` is the smallest `max_completion_tokens` in the
  ZDR-eligible pool. If your global `~/.claude/settings.json` sets a larger value it is
  inherited by the copy, so set it explicitly. Step 6's proxy also clamps `max_tokens`
  so a stale env cannot reintroduce it.
- On `CLAUDE_CODE_EFFORT_LEVEL`: `low`, `medium`, `high`, and `xhigh` persist across
  sessions. `max` is normally session-only but does persist when set via this env
  var. It overrides the in-session `/effort` command.
- If the sandbox blocks writes to `~/.claude-or-ds4`, rerun with sandbox disabled for
  that one call.
- Add `"CLAUDE_CODE_SKIP_FAST_MODE_ORG_CHECK": "1"` to the overrides if the user
  wants the `/fast` toggle available. OpenRouter's guide notes fast mode only applies
  to specific Opus builds, so on DeepSeek it just unlocks the UI without changing
  routing.
- OpenRouter's own guide warns that Claude Code is tuned for Anthropic models and may
  misbehave on others. Expect rough edges in tool-use-heavy flows regardless of how
  clean this config is.

## Step 6 — The proxy

Every profile here routes through one shared proxy, `src/proxy.py`. It listens on
a separate port per profile (31501 for this one), so this profile's
`settings.json` is unaware it is shared. What differs between profiles is a row in
that file's `PROFILES` table, not a separate script.

What it does for this profile:

| Concern | Behaviour |
|---|---|
| tier → effort | rewrites the `ds4-*` sentinel to the real slug and injects `reasoning_effort` |
| mid-session effort | `/ds4-effort <level>` writes `<profile>/effort-override`; the proxy applies it to the next request, overriding the tier map |
| small calls | `max_tokens` at or below 8192 gets `thinking: {"type":"disabled"}`; `DS4_NOTHINK_BELOW` moves the line |
| zero data retention | injects `provider: {"zdr": true, "data_collection": "deny"}` |
| context floor | `ignore: ["Io Net"]` — ZDR-eligible but only 262,100 context vs 1,048,576 elsewhere |
| output ceiling | clamps `max_tokens` to 65536, the floor of the ZDR pool |
| cost reporting | serves `GET /__spend` with live rates, 7-day spend, and credits remaining |
| classifier (optional) | `DS4_CLASSIFIER=zdr` routes the auto-mode permission classifier here (ZDR forced on) instead of the Anthropic subscription — no subscription tokens spent, gate runs on DeepSeek V4 Flash via OpenRouter |
| Cloudflare | sends a `curl`-style `User-Agent` (`DS4_UA`) |
| debugging | `DS4_DEBUG=1` logs each rewrite and any non-200 status |

### Changing effort mid-session: `/ds4-effort`

`CLAUDE_CODE_EFFORT_LEVEL` is read once at startup, and `/effort` changes an
internal value that never reaches the request body — on this profile it changes
nothing. The proxy is the only thing that can move the level live, and
`install.sh` installs the write side as a `/ds4-effort` slash command. It writes
`<profile>/effort-override`, one line, one of `max` / `xhigh` / `high` /
`medium` / `low` / `minimal` / `none`:

- `/ds4-effort high` sets the level for this profile; the proxy applies it to
  the next request, no restart.
- `/ds4-effort` with no argument reports the current override, or says there is
  none (meaning the tier defaults from step 2 apply).
- Anything else is rejected against the valid set — OpenRouter accepts the
  parameter and DeepSeek drops unknown values without error, so an invalid
  level must fail here, not vanish upstream.

The override survives a proxy restart (it is a file, not process state) and is
per profile: one proxy process serves all three, so a global would leak across
them the same way a shared key would. The direct profile maps no effort at all,
so the command refuses there.

`./install.sh --profile openrouter` is what installs it: it points `settings.json` at
`http://127.0.0.1:31501`, and on macOS writes and loads a single launch agent,
`com.strml.cc-ds4.proxy`, that runs it. Confirm it answers:

```bash
launchctl kickstart gui/$(id -u)/com.strml.cc-ds4.proxy
sleep 1
curl -s -o /dev/null -w "proxy responded: %{http_code}\n" -X POST \
  http://127.0.0.1:31501/v1/messages -H 'content-type: application/json' \
  -d '{"model":"ds4-low","max_tokens":8,"messages":[{"role":"user","content":"hi"}]}'
```

A 401 is expected and correct without a key in the header: it proves the proxy
forwarded upstream.

**Nothing works when the proxy is down.** Every request gets connection-refused,
which looks exactly like a bad endpoint or a bad key. Check `nc -z 127.0.0.1 31501`
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
--profile openrouter` makes the agent run forever (also replaces the plist and
reloads the agent, which drops any live session on all three profiles).

## Step 7 — Skip onboarding

```bash
echo '{"hasCompletedOnboarding": true}' > ~/.claude-or-ds4/.claude.json
chmod 600 ~/.claude-or-ds4/.claude.json
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

function claude-or-ds4 --description 'Claude Code on the OpenRouter DeepSeek profile'
    __ds4_run $HOME/.claude-or-ds4 31501 claude-or-ds4 $argv
end

# One process, so this takes every profile down with it.
function claude-ds4-stop --description 'Stop the shared ds4 proxy'
    launchctl kill TERM gui/(id -u)/$__ds4_label 2>/dev/null
    echo "ds4 proxy stopped (all profiles)"
end
```

zsh/bash equivalent:

```bash
claude-or-ds4() {
  if ! nc -z 127.0.0.1 31501 2>/dev/null; then
    launchctl kickstart "gui/$(id -u)/com.strml.cc-ds4.proxy" 2>/dev/null
    for _ in $(seq 40); do nc -z 127.0.0.1 31501 2>/dev/null && break; sleep 0.25; done
  fi
  nc -z 127.0.0.1 31501 2>/dev/null || { echo "proxy never came up on :31501" >&2; return 1; }
  mkdir -p "$HOME/.claude-or-ds4/.ds4-sessions"
  local token="$HOME/.claude-or-ds4/.ds4-sessions/$$"
  touch "$token"
  CLAUDE_CONFIG_DIR="$HOME/.claude-or-ds4" claude "$@"
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

Only when `~/.claude` is a real directory (the expected case from step 0). Append to
the same rc file (fish: add to the conf.d file from step 3):

zsh/bash:

```bash
claude-switch() { echo "claude-switch is disabled on this machine: ~/.claude is a real directory, not a ccam symlink. Use the claude-<name> launchers instead (e.g. claude-or-ds4)."; return 1; }
```

fish:

```fish
function claude-switch
    echo "claude-switch is disabled on this machine: ~/.claude is a real directory, not a ccam symlink. Use the claude-<name> launchers instead (e.g. claude-or-ds4)."
    return 1
end
```

## Step 10 — Verify

1. Open a NEW terminal (so aliases/functions load).
2. `claude-switch` → must print the disabled message and change nothing.
3. Confirm the proxy is running: `nc -z 127.0.0.1 31501`. Both options need it. If
   local backend, confirm `ds4-server` is up too:
   `curl -s -o /dev/null -w "%{http_code}\n" http://127.0.0.1:8765/v1/models`
4. `claude-or-ds4` in any project directory.
5. Inside the session run `/status`. Success = Base URL shows the proxy at
   `http://127.0.0.1:31501`, which is expected and not a misconfiguration.
6. Ask it something trivial to confirm a live response.
7. Option B only: switch tiers with `/model` and confirm each still answers. To prove
   effort is actually varying, run the proxy in the foreground and add a
   `print(payload["model"], effort)` line temporarily.
8. Confirm ZDR is on the wire. The startup banner saying `zdr=True`
   proves only that the flag parsed, so check the body the proxy actually emits by
   pointing a second instance at a local echo server (see step 6). `provider` must be
   present next to `reasoning_effort`.
9. Then run plain `claude` and confirm `/status` still shows Anthropic's API —
   proving isolation held.

Do **not** try to verify ZDR by watching which provider answers. Routing fluctuates
enough between requests that a small sample will convince you of something false in
either direction. Pin instead: `provider: {"only": ["Novita"], "zdr": true}` either
answers or returns "No endpoints found matching your data policy", and that is a real
answer about one provider.

If step 5 shows the Anthropic URL instead: the settings copy didn't load — confirm
`claude-or-ds4` actually sets `CLAUDE_CONFIG_DIR` (run
`env CLAUDE_CONFIG_DIR="$HOME/.claude-or-ds4" claude` directly to bisect). A 401 means
the key is missing or wrong. A 404 on a model name means a sentinel name leaked
through without the proxy running.

## Step 11 — Optional: a status line that is not lying

Skip if the user does not use a status line. If they use `cship`, offer this.

Out of the box the bar shows Anthropic-priced cost, and — because `usage_limits` is
fetched from Anthropic over OAuth rather than from stdin — their real Claude
subscription's 5h/7d windows and credit balance, none of which apply when every
request goes to OpenRouter.

The cost error is the one that matters. Claude Code prices the `ds4-*` sentinel
against Anthropic's table. Measured on one real session here:

| source | figure |
|---|---|
| Claude Code `total_cost_usd` | $0.152731 |
| recomputed at OpenRouter rates | $0.002637 |

That is 58x overstated on a mostly-input session, and it grows with the share of
output tokens because the two providers' output-price ratios differ most. It reads as
plausible, which is worse than reading as zero.

Run the installer from a checkout of this repo:

```bash
./install.sh --profile openrouter        # --dry-run first if you want to see it
```

It copies `config/cship-openrouter.toml` into the profile, backs up `settings.json`,
symlinks `~/.claude-or-ds4/ds4-statusline.py` at `src/statusline/openrouter.py` in the
checkout, and points `statusLine` at that symlink. `git pull` updates the bar. Adjust
`CSHIP` in `src/statusline/common.py` if `cship` is not at `~/.cargo/bin/cship`.

The wrapper rewrites the JSON payload and lets `cship` render it, rather than
regexing the coloured output afterwards. `cost.total_cost_usd` and
`model.display_name` are both read from stdin and so are correctable at the source;
`usage_limits` is not, which is why the config drops it instead.

Per-session cost is recomputed from the session transcript at real rates, with the
same scope and reset semantics as Claude Code's own figure. The transcript is read
incrementally from a stored byte offset, so it stays cheap as the session grows.
7-day spend and credits come from the proxy's `/__spend`.

Verify it renders before walking away:

```bash
src/statusline/openrouter.py          # with a tty, prints a sample bar
```

That self-test exists for a reason. A wrapper that swallows exceptions is right for
robustness, but it turns a plain `NameError` into a blank bar and exit 0. Silence is
not success.

Renders as `or-deepseek-v4-flash-0731 xhigh  ░░░░░░░░░░3%  💰 <$0.01 · 📆 7d $0.31 · 💳 $18.42 left`.

The `or-` prefix names the **backend**, not the endpoint that served the request. That
is deliberate: OpenRouter re-routes between providers request to request, so a bar
showing the live provider would flicker between DeepInfra, Novita, and SiliconFlow
while nothing meaningful changed. The sibling DeepSeek-direct profile uses `ds-` the
same way.

If you do want the serving endpoint, it is available — it appears as `provider` in
non-streaming response bodies and in the SSE `message_start` event, so the proxy can
sniff it on the way through and hand it to the statusline via `/__spend`.

Two things worth telling the user:
- Sub-penny sessions. `cship` formats cost to 2 decimals with no conditional, so a
  whole DeepSeek session renders as `$0.00`. The wrapper renders the cost segment
  itself with a `<$0.01` floor.
- If the proxy is down the model name shows as `or-ds4-xhigh (proxy?)` rather than the
  real slug, which makes a dead proxy visible at a glance.

## Final report to the user

Tell them, concretely:
- `claude-or-ds4` = DeepSeek V4 Flash 0731, `claude` = normal Anthropic. Nothing global
  changed.
- Which backend they ended up on, and what has to be running for it to work (the
  proxy, the local server, or nothing). If the launcher starts the proxy, say so, and
  say that an already-open terminal will not have picked it up yet.
- That zero data retention is on by default, what it means (requests only reach
  endpoints contractually bound not to retain them), and that `DS4_ZDR=0` disables it.
- That the model is text-only, so images are transcribed to text by the proxy
  (a local `claude -p --model haiku` on the Anthropic profile, cached by content
  hash) before DeepSeek sees them. The description is a lossy proxy, not the
  pixels. `DS4_VISION=0` restores the old behavior (image blocks forwarded
  unchanged — 404 or silently dropped).
- Where the key lives (`~/.claude-or-ds4/settings.json`, `env` block), that the file is
  chmod 600, and that nothing works until the placeholder is replaced if it's still
  there.
- The tier-to-effort mapping they chose, so they know what `/model` is actually doing.
- Their skills, hooks, agents, commands, and plugins are symlinked, so changes in
  either profile apply to both.
- `claude-switch` is deliberately disabled and why.
- Session history and per-project state accumulate separately in `~/.claude-or-ds4/`.
