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

Skip this entirely if the user chose Option A.

The proxy is mostly a pass-through: OpenRouter already speaks the Anthropic
`/v1/messages` shape, so it rewrites one field and adds routing preferences.

The proxy is `src/effort_proxy.py` in this repo. Copy it verbatim to
`~/.claude-or-ds4/ds4-effort-proxy.py` rather than retyping it. If this machine has a
checkout, `./install.sh --profile openrouter` puts it there as a symlink instead, so
`git pull` updates it. It is ~260 lines and
has grown past the point where inlining it here helps. What it does:

| Concern | Behaviour |
|---|---|
| tier → effort | rewrites the `ds4-*` sentinel to the real slug, injects `reasoning_effort` |
| zero data retention | injects `provider: {"zdr": true, "data_collection": "deny"}`; `DS4_ZDR=0` disables |
| context floor | `ignore: ["Io Net"]` — ZDR-eligible but only 262,100 context vs 1,048,576 elsewhere |
| output ceiling | clamps `max_tokens` to 65536, the floor of the ZDR pool |
| thinking | `max_tokens` at or below 8192 gets `thinking: {"type":"disabled"}`; `DS4_NOTHINK_BELOW` moves the line |
| cost reporting | serves `GET /__spend` with live rates, 7-day spend, and credits remaining |
| debugging | `DS4_DEBUG=1` logs `-> model=… effort=… max_tokens=…` and `<- status` per request |

The `/__spend` endpoint needs a key of its own, since a statusline render carries no
client credentials. It reads `OPENROUTER_API_KEY` from the environment, falling back
to `ANTHROPIC_AUTH_TOKEN` in the profile settings.

The thinking row deserves a note, because the mechanism is not obvious. V4 thinks by
default and Claude Code cannot turn it off: it sends
`thinking: {"type":"adaptive","display":"omitted"}`, and no provider serving this model
implements `adaptive`. On a small call that is fatal, because the thinking block
consumes the whole budget before the tool call is emitted. Measured on `-0731` at
`max_tokens=512` with a forced tool decision: 301-412 output tokens with thinking on,
101-163 with it off. The permission classifier behind `defaultMode: auto` is one of
these calls, which is why the symptom is intermittent classifier failures rather than
anything that looks like a routing problem.

Use the Anthropic spelling. `reasoning: {"enabled": false}`, OpenRouter's own, is
dropped without error — one more instance of silence not being success. The direct
profile carries the same rule for the same reason; see
[its step 5](deepseek-direct.md#step-5--the-thinking-proxy).

Make it executable and confirm it starts:

```bash
chmod +x ~/.claude-or-ds4/ds4-effort-proxy.py
python3 ~/.claude-or-ds4/ds4-effort-proxy.py &
sleep 1
curl -s -o /dev/null -w "proxy responded: %{http_code}\n" -X POST \
  http://127.0.0.1:31501/v1/messages -H 'content-type: application/json' \
  -d '{"model":"ds4-low","max_tokens":8,"messages":[{"role":"user","content":"hi"}]}'
```

A 401 here is expected and correct without a key in the header — it proves the proxy
forwarded upstream.

**Nothing works when the proxy is down.** Every request gets connection-refused, which
looks exactly like a broken provider or a bad key. Check `nc -z 127.0.0.1 31501` before
investigating anything else. Step 8 wires the launcher to start it automatically, so
you should not have to think about this after setup.

To confirm the ZDR injection is actually reaching the wire rather than trusting the
banner, point the proxy at a local echo server and read the body it emits. `provider`
should be present alongside `reasoning_effort`:

```bash
DS4_UPSTREAM=http://127.0.0.1:31598 DS4_PROXY_PORT=31502 python3 ~/.claude-or-ds4/ds4-effort-proxy.py &
```

To point the proxy at a local inference server instead of OpenRouter:
`DS4_UPSTREAM=http://127.0.0.1:8765 DS4_MODEL=deepseek-v4-flash DS4_ZDR=0 python3 ~/.claude-or-ds4/ds4-effort-proxy.py`

## Step 7 — Skip onboarding

```bash
echo '{"hasCompletedOnboarding": true}' > ~/.claude-or-ds4/.claude.json
chmod 600 ~/.claude-or-ds4/.claude.json
```

## Step 8 — Create the launcher

The generated ccam alias is not enough here, because this profile is dead without the
proxy. Override it with a launcher that starts the proxy first and refuses to run if
it never comes up.

Fish — write `~/.config/fish/conf.d/zz-ds4-proxy.fish`. **The filename must sort after
`ccam.fish`**, because ccam defines `claude-or-ds4` as a plain alias in a loop over the
accounts registry and the last definition wins. A file in `fish/functions/` will not
work: fish skips autoload entirely when a function is already defined.

```fish
function __ds4_proxy_up --description 'Start the ds4 effort proxy unless it is already listening'
    if nc -z 127.0.0.1 31501 2>/dev/null
        return 0
    end

    # Supervisor loop, not a bare nohup. The proxy has exited mid-session
    # before and taken the profile down with it; this restarts it in place.
    fish -c 'while true
                 /usr/bin/python3 $HOME/.claude-or-ds4/ds4-effort-proxy.py
                 sleep 1
             end' >>$HOME/.claude-or-ds4/proxy.log 2>&1 &
    disown

    for i in (seq 40)
        nc -z 127.0.0.1 31501 2>/dev/null; and return 0
        sleep 0.25
    end
    echo "claude-or-ds4: proxy never came up on :31501 — see ~/.claude-or-ds4/proxy.log" >&2
    return 1
end

function claude-or-ds4 --description 'Claude Code on the OpenRouter DeepSeek profile'
    __ds4_proxy_up; or return 1
    env CLAUDE_CONFIG_DIR=$HOME/.claude-or-ds4 command claude $argv
end

function claude-or-ds4-stop --description 'Stop the ds4 effort proxy'
    # Matches both the supervisor loop and the python child, since the
    # supervisor's command line contains the script path too.
    pkill -f 'ds4-effort-proxy.py' >/dev/null 2>&1
end
```

zsh/bash — same idea in `~/.zshrc` or `~/.bashrc`:

```bash
claude-or-ds4() {
  if ! nc -z 127.0.0.1 31501 2>/dev/null; then
    ( while true; do python3 "$HOME/.claude-or-ds4/ds4-effort-proxy.py"; sleep 1; done ) \
      >>"$HOME/.claude-or-ds4/proxy.log" 2>&1 &
    disown
    for _ in $(seq 40); do nc -z 127.0.0.1 31501 2>/dev/null && break; sleep 0.25; done
  fi
  nc -z 127.0.0.1 31501 2>/dev/null || { echo "proxy never came up on :31501" >&2; return 1; }
  CLAUDE_CONFIG_DIR="$HOME/.claude-or-ds4" command claude "$@"
}
```

Two decisions worth flagging to the user:

- **The proxy outlives the session.** Tearing it down when Claude exits would kill it
  out from under a second concurrent session, so it keeps running.
  `claude-or-ds4-stop` when you want the port back.

Either way, the launcher only takes effect in a **new** shell. An already-open terminal
still holds the old alias, launches without the proxy, and gets connection-refused.
That is the single most likely thing to go wrong right after setup.

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
- That the model is text-only, so screenshots and diagrams will not work. On the
  OpenRouter path this fails loudly with a 404; on DeepSeek direct the image is
  dropped silently and the model answers as if it were never sent.
- Where the key lives (`~/.claude-or-ds4/settings.json`, `env` block), that the file is
  chmod 600, and that nothing works until the placeholder is replaced if it's still
  there.
- The tier-to-effort mapping they chose, so they know what `/model` is actually doing.
- Their skills, hooks, agents, commands, and plugins are symlinked, so changes in
  either profile apply to both.
- `claude-switch` is deliberately disabled and why.
- Session history and per-project state accumulate separately in `~/.claude-or-ds4/`.
