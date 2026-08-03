# PROMPT: Set up a `claude-nous` launcher (DeepSeek V4 Flash 0731 via Nous Portal, in Claude Code)

> One of several Claude Code provider profiles. [Index](../README.md) · [`claude-ds4` — DeepSeek direct](deepseek-direct.md) · [`claude-or-ds4` — OpenRouter](openrouter.md) · [`claude-kimi` — Moonshot Kimi K3](kimi.md)

Paste everything below this line into a Claude Code session (or any capable coding
agent with shell access) on the machine you want set up. It requires macOS or Linux.

---

You are setting up an isolated Claude Code profile that runs **DeepSeek V4 Flash
(0731 build)** instead of Anthropic's models, billed through **Nous Portal**, launched
with a dedicated `claude-nous` command. The user's normal `claude` command must keep
working against Anthropic, completely untouched. Follow these instructions exactly.
Where a step says ASK, stop and ask the user before continuing.

## Non-negotiable safety rules

1. **Never** write `ANTHROPIC_BASE_URL`, `ANTHROPIC_AUTH_TOKEN`, or any model
   override into `~/.claude/settings.json`, `~/.zshrc`, `~/.bashrc`,
   `~/.config/fish/config.fish`, or any other global location. A global base-URL
   override silently reroutes EVERY Claude Code session on the machine, including
   ones currently running. All overrides go only into the copied
   `~/.claude-nous/settings.json` created below.
2. **Never** run `claude-switch` (a ccam command) and disable it as described in
   step 9. It works by replacing `~/.claude` with a symlink; if `~/.claude` is a
   real directory (the normal case), switching would displace the user's primary
   installation. This setup uses a dedicated launcher command instead.
3. Do not modify, move, or delete anything already inside `~/.claude/`. You will
   only read from it and symlink to it.
4. If any command fails, stop and show the user the error rather than improvising
   a different mechanism.

## What is different about Nous Portal (read before starting)

Nous Portal is a billing middleman for `deepseek/deepseek-v4-flash-0731`, aimed at
this repo's other two DeepSeek profiles. Two things are worth knowing before setup:

- **It is cheaper than OpenRouter right now.** Nous serves the discounted rate in
  its pricing API. The status line prices sessions at that discounted rate. There is
  no guarantee the discount lasts — the pricing is read live, not baked in.
- **It has no zero-data-retention control.** OpenRouter's `provider: {zdr: true}`
  block gets a **403** from Nous (an empty `error code: 1010` is Cloudflare; a 403
  with no body field is the portal rejecting the `provider` field). So this profile
  runs with ZDR **off** (`DS4_ZDR=0`). Treat it like the direct profile for privacy,
  not like the OpenRouter one.
- **No credits/balance endpoint.** It is a flat subscription (optionally topped up),
  but there is no public `/credits`-style endpoint, so the status line shows only
  session cost — no `📆 7d` or `💳 left` segments. The dashboard at
  portal.nousresearch.com shows the balance.
- **Cloudflare.** Nous sits behind Cloudflare, which 403s the Python stdlib's
  default `urllib` User-Agent (`error code: 1010`). The proxy in this repo sends a
  `curl`-style UA (`DS4_UA`), which is what makes this work at all.
- **Text-only model.** DeepSeek V4 has no vision; screenshots and diagrams will not
  work.

## Step 0 — Preflight

Run and report results before changing anything:

```bash
command -v claude && claude --version   # Claude Code installed?
command -v git
command -v python3
echo $SHELL                              # fish, zsh, or bash — remember this
ls -la ~/.claude 2>/dev/null | head -5   # does a primary install exist? symlink or real dir?
test -L ~/.claude && echo "~/.claude IS A SYMLINK" || echo "~/.claude is a real dir (expected)"
ls ~/.claude-nous 2>/dev/null && echo "PROFILE ALREADY EXISTS — ask user before overwriting"
```

- If `claude` is missing: `npm install -g @anthropic-ai/claude-code` (ASK first if npm
  is also missing).
- If `~/.claude` doesn't exist, the user has never run Claude Code. Have them run
  `claude` once and exit, so the directory exists.
- If `~/.claude` is already a symlink, this user actively uses ccam's switch
  mechanism — ASK before applying step 9's guard, and adapt: symlink targets in step
  4 should point at the real directory the symlink resolves to.

## Step 1 — Get the key

The Nous Portal key comes from https://portal.nousresearch.com → account / API
settings (an `sk-...` value; the portal's `api-docs` page documents it). Have the
user paste it and call it `$NOUS_KEY`. If they don't have one yet, proceed with the
literal placeholder `REPLACE_WITH_NOUS_API_KEY` and tell them at the end where to
paste the real key.

Billing is a flat subscription, optionally topped up with credits. Either way the
key is the same; no separate billing credential is needed for inference. (The
credits balance is only visible in the portal dashboard — there is no public API for
it, so it is not shown in the status line.)

The base URL the proxy forwards to is `https://inference-api.nousresearch.com` with
**no** `/v1`. Claude Code appends `/v1/messages` itself; the proxy is configured with
the bare host so it does not produce `/v1/v1/messages`.

The model is pinned to **`deepseek/deepseek-v4-flash-0731`** at 1,048,576 context
(confirmed in the portal's `/v1/models`). Nous also lists a `~deepseek/...-latest`
alias, deliberately avoided so the build cannot float.

## Step 2 — ASK how effort should map to model tiers

Claude Code exposes four model tiers (fable, opus, sonnet, haiku). This setup points
all of them at one model and varies **reasoning effort** instead. Nous accepts
`reasoning_effort` as a request parameter, so per-tier effort needs the tiny local
proxy (Option B). ASK:

**Option A — uniform effort (simpler, no extra process).**
All tiers use one global effort level (`CLAUDE_CODE_EFFORT_LEVEL=xhigh`), pointed
directly at `https://inference-api.nousresearch.com` with the real model name. No
proxy. Skips steps 6 and 8's proxy handling. Fine if the user never changes `/model`.

**Option B — per-tier effort (what you usually want, needs the proxy).**
Each tier maps to a different effort, so `/model` actually changes how hard it thinks:

| Tier | Effort |
|---|---|
| fable | `max` |
| **opus (default)** | `xhigh` |
| sonnet | `high` |
| haiku | `low` |

`CLAUDE_CODE_EFFORT_LEVEL` is a single global with no per-tier variant, so this
needs the proxy to inject `reasoning_effort` per request. Steps 6–8 build and launch
it, exactly like the OpenRouter profile.

The rest of this prompt assumes **Option B**. If the user picks A, set the model
name directly and skip the proxy steps.

## Step 3 — Install ccam (account manager, used for its conventions)

```bash
git clone https://github.com/dody87/ccam ~/.local/share/ccam
```

- **zsh/bash:** run `bash ~/.local/share/ccam/install.sh`, then reload the rc file.
- **fish:** upstream install.sh only targets bash/zsh. Instead create
  `~/.config/fish/conf.d/ccam.fish` containing exactly:

```fish
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
grep -qx "nous" ~/.config/ccam/accounts || echo "nous" >> ~/.config/ccam/accounts
```

## Step 4 — Build the profile directory

Everything shareable is a **symlink** into `~/.claude` (so skills, hooks, agents,
plugins, and instructions stay live in both profiles — edit once, applies
everywhere). Only `settings.json` is a **copy**, because that's where the overrides
live and they must not leak back.

```bash
mkdir ~/.claude-nous
cd ~/.claude-nous
for f in CLAUDE.md RTK.md agents commands hooks plugins rules skills statusline-command.sh workflows; do
  [ -e "$HOME/.claude/$f" ] && ln -s "$HOME/.claude/$f" "$f"
done
ls -la ~/.claude-nous
```

(Missing entries are fine — the loop only links what exists.)

## Step 5 — Copy and modify settings.json

For Option B, run this with the placeholders replaced. Confirm the base URL points
at the proxy (port 31502), every model variable is set, and `fallbackModel` is
removed:

```bash
python3 - <<'EOF'
import json, os
src = os.path.expanduser("~/.claude/settings.json")
dst = os.path.expanduser("~/.claude-nous/settings.json")
s = json.load(open(src)) if os.path.exists(src) else {}
KEY = "REPLACE_WITH_NOUS_API_KEY"   # from step 1
overrides = {
    "ANTHROPIC_BASE_URL": "http://127.0.0.1:31502",
    "ANTHROPIC_AUTH_TOKEN": KEY,
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
chmod 600 ~/.claude-nous/settings.json
```

For Option A, the same but pointing straight at Nous: `ANTHROPIC_BASE_URL` =
`https://inference-api.nousresearch.com`, and every `MODEL`/tier set to
`deepseek/deepseek-v4-flash-0731`, dropping the sentinels.

Notes:

- **`CLAUDE_CODE_MAX_CONTEXT_TOKENS=1048576` is what makes the 1M window real.**
  `CLAUDE_CODE_AUTO_COMPACT_WINDOW` alone still resolves the unrecognised `ds4-*`
  sentinel to a 200,000 default and compacts at 200k, wasting 80% of the window.
  This is the same trap documented for the OpenRouter profile.
- The key is the file's `ANTHROPIC_AUTH_TOKEN`, chmod 600. The proxy forwards the
  auth header Claude Code sends, so nothing else needs the key. The proxy's own
  server-side calls (pricing) use the same file or a `OPENROUTER_API_KEY`/`DS4_API_KEY`
  env var, but pricing here needs no auth anyway.
- If the sandbox blocks writes to `~/.claude-nous`, rerun with sandbox disabled for
  that one call.

## Step 6 — The proxy

Every profile here routes through one shared proxy, `src/proxy.py`. It listens on
a separate port per profile (31502 for this one), so this profile's
`settings.json` is unaware it is shared. What differs between profiles is a row in
that file's `PROFILES` table, not a separate script.

What it does for this profile:

| Concern | Behaviour |
|---|---|
| tier → effort | rewrites the `ds4-*` sentinel to the real slug and injects `reasoning_effort` (Nous accepts it) |
| small calls | `max_tokens` at or below 8192 gets `thinking: {"type":"disabled"}`; `DS4_NOTHINK_BELOW` moves the line |
| provider routing | **off** — Nous 403s any `provider` block (`zdr`, `data_collection`, …) |
| output ceiling | clamps `max_tokens` to 65536 |
| Cloudflare | sends a `curl`-style `User-Agent` (`DS4_UA`, default `curl/8.4.0`); without it every call 403s `error code: 1010` |
| cost reporting | serves `GET /__spend` with live discounted rates; **no** credits/7-day fields (no public endpoint) |
| debugging | `DS4_DEBUG=1` logs each rewrite and any non-200 status |

`./install.sh --profile nous` is what installs it: it points `settings.json` at
`http://127.0.0.1:31502`, and on macOS writes and loads a single launch agent,
`com.strml.cc-ds4.proxy`, that runs it. Confirm it answers:

```bash
launchctl kickstart gui/$(id -u)/com.strml.cc-ds4.proxy
sleep 1
curl -s -o /dev/null -w "proxy responded: %{http_code}\n" -X POST \
  http://127.0.0.1:31502/v1/messages -H 'content-type: application/json' \
  -d '{"model":"ds4-low","max_tokens":8,"messages":[{"role":"user","content":"hi"}]}'
```

A 401 is expected and correct without a key in the header: it proves the proxy
forwarded upstream.

**Nothing works when the proxy is down.** Every request gets connection-refused,
which looks exactly like a bad endpoint or a bad key. Check `nc -z 127.0.0.1 31502`
before investigating anything else, and read `~/.claude-ds4-proxy.log`.

The proxy exits on its own once no profile is in use and nothing has come through
for `DS4_IDLE_EXIT` seconds (default 900). It counts a profile as in use when a
session token under `<profile>/.ds4-sessions` has a live PID, or when `ps` shows a
`claude` process with that `CLAUDE_CONFIG_DIR`. Set `DS4_IDLE_EXIT=0` to disable
that and run forever. Without a launcher there is nothing to start it again, so
step 8 matters.

On Linux, or without launchd, run it yourself: `python3 src/proxy.py &`.

## Step 7 — Skip onboarding

```bash
echo '{"hasCompletedOnboarding": true}' > ~/.claude-nous/.claude.json
chmod 600 ~/.claude-nous/.claude.json
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

function claude-nous --description 'Claude Code on the Nous Portal profile'
    __ds4_run $HOME/.claude-nous 31502 claude-nous $argv
end

# One process, so this takes every profile down with it.
function claude-ds4-stop --description 'Stop the shared ds4 proxy'
    launchctl kill TERM gui/(id -u)/$__ds4_label 2>/dev/null
    echo "ds4 proxy stopped (all profiles)"
end
```

zsh/bash equivalent:

```bash
claude-nous() {
  if ! nc -z 127.0.0.1 31502 2>/dev/null; then
    launchctl kickstart "gui/$(id -u)/com.strml.cc-ds4.proxy" 2>/dev/null
    for _ in $(seq 40); do nc -z 127.0.0.1 31502 2>/dev/null && break; sleep 0.25; done
  fi
  nc -z 127.0.0.1 31502 2>/dev/null || { echo "proxy never came up on :31502" >&2; return 1; }
  mkdir -p "$HOME/.claude-nous/.ds4-sessions"
  local token="$HOME/.claude-nous/.ds4-sessions/$$"
  touch "$token"
  CLAUDE_CONFIG_DIR="$HOME/.claude-nous" claude "$@"
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
claude-switch() { echo "claude-switch is disabled on this machine: ~/.claude is a real directory, not a ccam symlink. Use the claude-<name> launchers instead (e.g. claude-nous)."; return 1; }
```

fish:

```fish
function claude-switch
    echo "claude-switch is disabled on this machine: ~/.claude is a real directory, not a ccam symlink. Use the claude-<name> launchers instead (e.g. claude-nous)."
    return 1
end
```

## Step 10 — Verify

1. Open a NEW terminal (so aliases/functions load).
2. `claude-switch` → must print the disabled message and change nothing.
3. Confirm the proxy is listening: `nc -z 127.0.0.1 31502 && echo up`.
4. `claude-nous` in any project directory.
5. Inside the session run `/status`. Success = Base URL shows `http://127.0.0.1:31502`
   (Option A: the Nous base URL).
6. Ask it something trivial to confirm a live response.
7. Switch tiers with `/model` and confirm each still answers. (To prove effort is
   actually varying, run the proxy in the foreground with `DS4_VERBOSE=1` — it logs
   `model=deepseek/deepseek-v4-flash-0731 effort=…`.)
8. Then run plain `claude` and confirm `/status` still shows Anthropic's API —
   proving isolation held.

**Proxy quirk reminder:** a 403 with no message from Nous usually means a stale
`provider`-blocked build — ensure `DS4_ZDR=0`. A 403 body reading `error code: 1010`
is Cloudflare — the UA is not being sent. A 401 means the key is missing or wrong.

## Step 11 — Optional: a status line that is not lying

Skip if the user does not use a status line. If they use `cship`, offer this.

Out of the box the bar shows Anthropic-priced cost, which overstates DeepSeek by
about two orders of magnitude (see the README's measurements). The provider-agnostic
installer installs the corrected bar:

```bash
./install.sh --profile nous          # from your checkout; --dry-run first if you want
```

It copies `config/cship-nous.toml` into the profile, backs up `settings.json`, and
points `statusLine` at `src/statusline/nous.py` **in the checkout** so `git pull`
updates the bar. **Adjust `CSHIP` in `src/statusline/common.py` if `cship` is not at
`~/.cargo/bin/cship`** (on this workspace it is at `~/.local/bin/cship`).

Verify it renders before walking away — a wrapper that fails open turns a syntax
error into a blank bar and exit 0:

```bash
src/statusline/nous.py          # with a tty, prints a sample bar
```

Renders as `deepseek-v4-flash-0731 xhigh  ░░░░░░░░░░ 5%  💰 $0.03`. There is **no
backend tag** on the model name: Nous Portal is the only router behind this profile,
so the slug is already unambiguous (OpenRouter uses `or-`, direct uses `ds-`). The
tail shows the session cost only — **no `📆 7d` or `💳 left` segments**, because Nous
exposes no public credits endpoint. The `💰` figure is priced at the discounted
90%-off rate; if the discount ends, the figure (and the fallback in
`src/statusline/nous.py`) must be updated to match.

If the proxy is down the model name shows as `ds4-xhigh (proxy?)` rather than the
real slug, which makes a dead proxy visible at a glance.

## Final report to the user

Tell them, concretely:

- `claude-nous` = DeepSeek V4 Flash 0731 billed via Nous Portal; `claude` = normal
  Anthropic. Nothing global changed.
- What has to be running for it to work: the effort proxy on `:31502`, which the
  launcher starts for you. An already-open terminal will not have picked it up yet.
- **Privacy:** unlike the OpenRouter profile, there is **no zero-data-retention
  control** — Nous rejects the `provider` block, so requests are governed by Nous's
  own (undisclosed) retention policy. Treat it like the direct profile, not the
  OpenRouter one.
- The model is text-only — screenshots and diagrams will not work.
- Where the key lives (`~/.claude-nous/settings.json`, `env.ANTHROPIC_AUTH_TOKEN`,
  chmod 600), and that nothing works until the placeholder is replaced if it is
  still there. The credits balance shows only in the portal dashboard — there is no
  API for it, so it is not in the status line.
- The tier-to-effort mapping they chose, so they know what `/model` is actually
  doing.
- Their skills, hooks, agents, commands, and plugins are symlinked, so changes in
  either profile apply to both.
- `claude-switch` is deliberately disabled and why.
- Session history and per-project state accumulate separately in `~/.claude-nous/`.
