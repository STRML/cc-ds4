# PROMPT: Set up a `claude-kimi` launcher (Kimi K3 inside Claude Code, via ccam)

> One of three Claude Code provider profiles. [Index](../README.md) · [`claude-ds4` — DeepSeek direct](deepseek-direct.md) · [`claude-or-ds4` — DeepSeek via OpenRouter](openrouter.md)


Paste everything below this line into a Claude Code session (or any capable coding
agent with shell access) on the machine you want set up. It requires macOS or Linux.

---

You are setting up an isolated Claude Code profile that runs Moonshot's **Kimi K3**
model instead of Anthropic's models, launched with a dedicated `claude-kimi` command.
The user's normal `claude` command must keep working against Anthropic, completely
untouched. Follow these instructions exactly. Where a step says ASK, stop and ask the
user before continuing.

## Non-negotiable safety rules

1. **Never** write `ANTHROPIC_BASE_URL`, `ANTHROPIC_AUTH_TOKEN`, or any model
   override into `~/.claude/settings.json`, `~/.zshrc`, `~/.bashrc`,
   `~/.config/fish/config.fish`, or any other global location. A global base-URL
   override silently reroutes EVERY Claude Code session on the machine, including
   ones currently running. All overrides go only into the copied
   `~/.claude-kimi/settings.json` created below.
2. **Never** run `claude-switch` (a ccam command) and disable it as described in
   step 7. It works by replacing `~/.claude` with a symlink; if `~/.claude` is a
   real directory (the normal case), switching would displace the user's primary
   installation. This setup uses a dedicated launcher command instead, which is
   just as convenient and cannot break anything.
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
ls ~/.claude-kimi 2>/dev/null && echo "PROFILE ALREADY EXISTS — ask user before overwriting"
```

- If `claude` is missing: `npm install -g @anthropic-ai/claude-code` (ASK first if
  npm is also missing).
- If `~/.claude` doesn't exist, the user has never run Claude Code. Have them run
  `claude` once and exit, so the directory exists.
- If `~/.claude` is already a symlink, this user actively uses ccam's switch
  mechanism — ASK before applying step 7's guard, and adapt: symlink targets in
  step 3 should point at the real directory the symlink resolves to.

## Step 1 — Get the API key

ASK the user which kind of Kimi access they have. The two are NOT interchangeable —
the key must match the endpoint:

| Access type | Where the key comes from | Base URL | Model name |
|---|---|---|---|
| **Pay-as-you-go** (default) | https://platform.kimi.ai/console/api-keys | `https://api.moonshot.ai/anthropic` | `kimi-k3` |
| **Kimi Code subscription** | kimi.com/code membership | `https://api.kimi.com/coding/` | `k3` (or `k3[1m]` on Allegretto+ tiers for 1M context) |

Record their choice as BASE_URL and MODEL for the steps below. Have the user paste
the key; refer to it as `$KIMI_KEY`. If they don't have one yet, proceed with the
literal placeholder `REPLACE_WITH_KIMI_API_KEY` and tell them at the end where to
paste the real key.

One wrinkle: the pay-as-you-go endpoint authenticates via the env var
`ANTHROPIC_AUTH_TOKEN`; the subscription endpoint documents `ANTHROPIC_API_KEY`.
Use the right variable name for their choice (call it AUTH_VAR below), and never
set both.

## Step 2 — Install ccam (account manager, used for its conventions)

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
grep -qx "kimi" ~/.config/ccam/accounts || echo "kimi" >> ~/.config/ccam/accounts
```

## Step 3 — Build the profile directory

Everything shareable is a **symlink** into `~/.claude` (so the user's skills, hooks,
agents, plugins, and instructions stay live in both profiles — edit once, applies
everywhere). Only `settings.json` is a **copy**, because that's where the Kimi
overrides live and they must not leak back.

```bash
mkdir ~/.claude-kimi
cd ~/.claude-kimi
for f in CLAUDE.md RTK.md agents commands hooks plugins rules skills statusline-command.sh workflows; do
  [ -e "$HOME/.claude/$f" ] && ln -s "$HOME/.claude/$f" "$f"
done
ls -la ~/.claude-kimi
```

(Missing entries are fine — the loop only links what exists. If the user keeps other
shared instruction files in `~/.claude/` that their setup loads, link those too.)

## Step 4 — Copy and modify settings.json

Substitute BASE_URL, MODEL, AUTH_VAR, and the key from step 1. Every one of the six
model variables must be set — the official Moonshot doc warns that omitting any of
them causes silent failures in the scenario that uses it. `fallbackModel` must be
removed if present: Anthropic model IDs don't exist on Kimi's endpoint and a
fallback attempt would error.

If `python3` is available (preferred; handles a missing or pre-populated source
file), run this — after replacing the four ALL-CAPS placeholders in the `overrides`
dict:

```bash
python3 - <<'EOF'
import json, os
src = os.path.expanduser("~/.claude/settings.json")
dst = os.path.expanduser("~/.claude-kimi/settings.json")
s = json.load(open(src)) if os.path.exists(src) else {}
overrides = {
    "ANTHROPIC_BASE_URL": "BASE_URL",
    "AUTH_VAR": "KIMI_KEY_OR_PLACEHOLDER",
    "ANTHROPIC_MODEL": "MODEL",
    "ANTHROPIC_DEFAULT_OPUS_MODEL": "MODEL",
    "ANTHROPIC_DEFAULT_SONNET_MODEL": "MODEL",
    "ANTHROPIC_DEFAULT_HAIKU_MODEL": "MODEL",
    "ANTHROPIC_DEFAULT_FABLE_MODEL": "MODEL",
    "CLAUDE_CODE_SUBAGENT_MODEL": "MODEL",
    "ENABLE_TOOL_SEARCH": "false",
    "CLAUDE_CODE_AUTO_COMPACT_WINDOW": "1048576",
    "CLAUDE_CODE_EFFORT_LEVEL": "max",
}
s.setdefault("env", {}).update(overrides)
s["model"] = "MODEL"
s.pop("fallbackModel", None)
json.dump(s, open(dst, "w"), indent=2)
print("wrote", dst)
EOF
chmod 600 ~/.claude-kimi/settings.json
```

Notes:
- `CLAUDE_CODE_AUTO_COMPACT_WINDOW=1048576` assumes the 1M-context model. For the
  subscription's plain `k3` on a lower tier, drop that line.
- If the user's sandbox blocks writes to `~/.claude-kimi`, rerun the command with
  sandbox disabled for that one call — creating files in a new home-dir folder is
  exactly what the bypass is for.
- If the user's copied settings contain an `ANTHROPIC_API_KEY` from before and you
  are using `ANTHROPIC_AUTH_TOKEN` (or vice versa), delete the other one — having
  both set causes auth conflicts.

## Step 5 — Skip onboarding

```bash
echo '{"hasCompletedOnboarding": true}' > ~/.claude-kimi/.claude.json
chmod 600 ~/.claude-kimi/.claude.json
```

## Step 6 — Create the launcher

Fish users already have it: the conf.d script from step 2 generates `claude-kimi`
from the accounts registry on next shell start.

zsh/bash users — if ccam's install.sh didn't already generate a per-account alias,
add this to `~/.zshrc` (or `~/.bashrc`):

```bash
alias claude-kimi='CLAUDE_CONFIG_DIR="$HOME/.claude-kimi" claude'
```

## Step 7 — Disable claude-switch

Only when `~/.claude` is a real directory (the expected case from step 0). Append to
the same rc file (fish: add to the conf.d file from step 2):

zsh/bash:

```bash
claude-switch() { echo "claude-switch is disabled on this machine: ~/.claude is a real directory, not a ccam symlink. Use the claude-<name> launchers instead (e.g. claude-kimi)."; return 1; }
```

fish:

```fish
function claude-switch
    echo "claude-switch is disabled on this machine: ~/.claude is a real directory, not a ccam symlink. Use the claude-<name> launchers instead (e.g. claude-kimi)."
    return 1
end
```

## Step 8 — Verify

1. Open a NEW terminal (so aliases/functions load).
2. `claude-switch` → must print the disabled message and change nothing.
3. `claude-kimi` in any project directory.
4. Inside the session run `/status`. Success = Base URL shows the BASE_URL from
   step 1 and the model shows MODEL. (On the subscription endpoint the display
   name can still read like a Claude model even though requests route to Kimi —
   the Base URL line is the authoritative check.)
5. Ask it something trivial to confirm a live response.
6. Then run plain `claude` and confirm `/status` still shows Anthropic's API —
   proving isolation held.

If step 4 shows the Anthropic URL instead: the settings copy didn't load — confirm
`claude-kimi` actually sets `CLAUDE_CONFIG_DIR` (run
`env CLAUDE_CONFIG_DIR="$HOME/.claude-kimi" claude` directly to bisect). If you get
a 401: the key doesn't match the endpoint — recheck the step 1 table.

## Final report to the user

Tell them, concretely:
- `claude-kimi` = Kimi K3, `claude` = normal Anthropic. Nothing global changed.
- Where the key lives (`~/.claude-kimi/settings.json`, `env` block) and that the
  file is chmod 600 — and if the placeholder is still in there, that nothing will
  work until they replace it.
- Their skills, hooks, agents, commands, and plugins are symlinked, so changes made
  in either profile's shared assets apply to both automatically.
- `claude-switch` is deliberately disabled and why.
- Session history and per-project state for the Kimi profile accumulate separately
  inside `~/.claude-kimi/`.
