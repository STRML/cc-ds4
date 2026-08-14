#!/usr/bin/env bash
# Install the status line into a Claude Code profile, and the shared proxy agent.
#
# This does not create the profile. Use the setup prompt in profiles/ for that;
# this installs the pieces that get updates.
#
#   ./install.sh --profile openrouter          # ~/.claude-or-ds4
#   ./install.sh --profile direct              # ~/.claude-ds4
#   ./install.sh --profile nous                # ~/.claude-nous
#   ./install.sh --profile direct --no-proxy   # status line only
#
# One proxy process serves every profile, each on its own port, so a profile's
# settings.json is unchanged and unaware it is shared. The proxy binary holds the
# table and fixes the profile directories, so --dir is not accepted. On macOS
# this also writes and loads a single launch agent that runs it.
#
# The status line is symlinked into the profile directory, so the profile is the
# interface and this checkout is the source of truth: git pull updates it. It
# matters that settings.json points at the PROFILE path, since that string is the
# same on every machine. The cship config is copied, not linked, since it is
# meant to be edited.
set -euo pipefail

REPO="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROFILE="" DIR="" DRY=0 WANT_PROXY=1
LABEL="com.strml.cc-ds4.proxy"
PLIST="$HOME/Library/LaunchAgents/$LABEL.plist"

# The proxy binary, built from this checkout. install.sh builds it and the plist
# ProgramArguments exec it. It replaced a Python implementation whose behaviour
# it reproduces byte for byte, which tests/diff/run_diff.py asserted against the
# Python original before that original was removed.
GO_DIR="$REPO/src/go"
GO_BIN="$GO_DIR/cmd/ds4-proxy/ds4-proxy"

# build_go: toolchain preflight + build. Runs BEFORE any write so a missing go
# or a failed build leaves the profile files and plist untouched.
build_go() {
  if ! command -v go >/dev/null 2>&1; then
    echo "go not found on PATH — cannot build the ds4-proxy binary" >&2
    exit 1
  fi
  go_version="$(go version 2>/dev/null | sed -n 's/^go version go\([0-9.]*\).*/\1/p')"
  if [ -z "$go_version" ]; then
    echo "could not determine go version" >&2
    exit 1
  fi
  # Go 1.26 is the floor (the module's go.mod pins it).
  if ! awk -F. -v v="$go_version" 'BEGIN { exit !(v+0 >= 1.26) }'; then
    echo "go $go_version is too old; need >= 1.26 (go.mod pins 1.26.5)" >&2
    exit 1
  fi
  ( cd "$GO_DIR" && go build -o "$GO_BIN" ./cmd/ds4-proxy ) || {
    echo "go build of the ds4-proxy binary failed" >&2
    exit 1
  }
  # Verify the binary serves --ports before writing anything that depends on it.
  if ! "$GO_BIN" --ports >/dev/null 2>&1; then
    echo "built ds4-proxy binary failed its --ports smoke check" >&2
    exit 1
  fi
}

while [ $# -gt 0 ]; do
  case "$1" in
    --profile)  PROFILE="${2:-}"; shift 2 ;;
    --dir)      echo "--dir is not supported: the proxy only serves the three fixed profile directories" >&2
                echo "  (~/.claude-ds4, ~/.claude-or-ds4, ~/.claude-nous)" >&2
                echo "  use one of those profiles or pick a different machine" >&2
                exit 2 ;;
    --dry-run)  DRY=1; shift ;;
    --no-proxy) WANT_PROXY=0; shift ;;
    -h|--help)  sed -n '2,21p' "$0" | sed 's/^# \{0,1\}//'; exit 0 ;;
    *) echo "unknown argument: $1" >&2; exit 2 ;;
  esac
done

case "$PROFILE" in
  openrouter) SCRIPT="$REPO/src/statusline/openrouter.py"
              CONFIG="$REPO/config/cship-openrouter.toml"
              PORT=31501
              LAUNCHER="claude-or-ds4"
              DOC="openrouter.md"
              DIR="${DIR:-$HOME/.claude-or-ds4}" ;;
  direct)     SCRIPT="$REPO/src/statusline/direct.py"
              CONFIG="$REPO/config/cship-direct.toml"
              PORT=31500
              LAUNCHER="claude-ds4"
              DOC="deepseek-direct.md"
              DIR="${DIR:-$HOME/.claude-ds4}" ;;
  nous)       SCRIPT="$REPO/src/statusline/nous.py"
              CONFIG="$REPO/config/cship-nous.toml"
              PORT=31502
              LAUNCHER="claude-nous"
              DOC="nous.md"
              DIR="${DIR:-$HOME/.claude-nous}" ;;
  *) echo "usage: $0 --profile openrouter|direct|nous [--no-proxy] [--dry-run]" >&2; exit 2 ;;
esac

[ -d "$DIR" ] || { echo "no profile at $DIR — create it first with profiles/*.md" >&2; exit 1; }
SETTINGS="$DIR/settings.json"

# A port reaches the plist and the base URL, and both end up in XML or JSON, so
# anything not a plain decimal is rejected rather than escaped. The range starts
# at 1024 because the agent runs unprivileged and cannot bind below it.
valid_port() {
  case "$1" in
    ''|*[!0-9]*) return 1 ;;
  esac
  [ "$1" -ge 1024 ] && [ "$1" -le 65535 ]
}

# DS4_PORT_<PROFILE> overrides the port and the proxy binary is what honours it, so
# effective value comes from there instead of a second copy of the mapping. The
# hardcoded PORT above is the fallback for a profile the binary cannot see.
# Build the Go proxy BEFORE the --ports lookup and before any write: a
# missing toolchain or a failed build leaves the profile files and plist
# untouched, and the binary must exist for its --ports to be consulted.
[ "$WANT_PROXY" = 1 ] && build_go

# Without this the plist binds the override while settings.json still points at
# the default, which leaves Claude talking to a port nothing listens on.
if [ "$WANT_PROXY" = 1 ]; then
  eff="$("$GO_BIN" --ports 2>/dev/null \
         | awk -v p="$PROFILE" '$1 == p {print $2}')"
  [ -n "$eff" ] && PORT="$eff"
fi
valid_port "$PORT" || {
  echo "port '$PORT' is not a decimal 1024-65535 (check DS4_PORT_$(echo "$PROFILE" | tr '[:lower:]' '[:upper:]'))" >&2
  exit 1
}
[ -f "$SETTINGS" ] || { echo "no settings.json in $DIR" >&2; exit 1; }

command -v cship >/dev/null 2>&1 || echo "warning: cship not on PATH; edit CSHIP in $SCRIPT" >&2

BAR_DST="$DIR/ds4-statusline.py"
HOOK_SRC="$REPO/src/ds4-proxy-kickstart.sh"
HOOK_DST="$DIR/ds4-proxy-kickstart.sh"
MEMLINK_SRC="$REPO/src/ds4-link-memory.sh"
MEMLINK_DST="$DIR/ds4-link-memory.sh"
CMD_SRC="$REPO/src/commands/ds4-effort.md"
CMD_DST="$DIR/commands/ds4-effort.md"
echo "profile:  $DIR"
echo "bar:      $BAR_DST -> $SCRIPT"
echo "config:   $DIR/cship.toml  (from $(basename "$CONFIG"))"
echo "memory:   $MEMLINK_DST -> $MEMLINK_SRC  (shares memory with ~/.claude)"
echo "command:  $CMD_DST -> $CMD_SRC  (/ds4-effort sets effort mid-session)"
if [ "$WANT_PROXY" = 1 ]; then
  echo "proxy:    $GO_BIN  (this profile on :$PORT)"
  echo "hook:     $HOOK_DST -> $HOOK_SRC  (SessionStart kickstart)"
  echo "base URL: http://127.0.0.1:$PORT"
  [ "$(uname)" = Darwin ] && echo "agent:    $PLIST"
fi
[ "$DRY" = 1 ] && { echo "(dry run, nothing written)"; exit 0; }

# Say so when we replace a real file: that is someone's hand-copied setup from the
# profile prompt, and the symlink silently changes where updates come from.
link() {
  local src="$1" dst="$2"
  if [ -f "$dst" ] && [ ! -L "$dst" ]; then
    echo "replaced: $dst was a real file, now a symlink into the checkout"
  fi
  chmod +x "$src"
  ln -sfn "$src" "$dst"
}

cp "$CONFIG" "$DIR/cship.toml"
link "$SCRIPT" "$BAR_DST"
link "$MEMLINK_SRC" "$MEMLINK_DST"
[ "$WANT_PROXY" = 1 ] && link "$HOOK_SRC" "$HOOK_DST"

# /ds4-effort needs a commands dir. The profile prompt symlinks one to
# ~/.claude/commands when that exists; otherwise a real dir here is fine —
# Claude Code reads $CLAUDE_CONFIG_DIR/commands either way. The command itself
# refuses on the direct profile, so installing it everywhere is safe.
mkdir -p "$DIR/commands"
link "$CMD_SRC" "$CMD_DST"

# The ds4 subagent skill family + discrete role skills. The profile's skills/
# dir is usually a symlink to ~/.claude/skills; install into it directly so a
# normal `claude` (and every profile) can invoke them. Best-effort — a skills
# dir that is a real dir still works. `ln -sfn` repairs a dangling symlink
# (left by a deleted checkout) that plain `ln -s` would leave dead.
for skill in ds4-skill-family ds4-plan ds4-review ds4-verify ds4-implement; do
  SKILL_SRC="$REPO/skills/$skill"
  SKILL_DST="$DIR/skills/$skill"
  [ -e "$SKILL_SRC" ] || continue
  ln -sfn "$SKILL_SRC" "$SKILL_DST" 2>/dev/null \
    && echo "skill:    $SKILL_DST -> $SKILL_SRC" \
    || echo "skill:    (skills dir unavailable; symlink manually: ln -sfn $SKILL_SRC $(dirname "$SKILL_DST"))" >&2
done

# Memory is shared with the real ~/.claude: project memory under this profile dir
# is symlinked to the canonical copy so notes are visible on every profile. Run
# it now for existing projects; the SessionStart hook runs it again for new ones.
"$MEMLINK_DST" "$DIR" 2>/dev/null || true

# A previous release gave each profile its own proxy copy. One process serves them
# all now, so leaving those behind means a stale second binder fighting for the port.
# -e is false for a dangling symlink, and after the merge these point at deleted
# files, so -L has to be tested too or the stale links survive the upgrade.
# Guarded by WANT_PROXY: --no-proxy leaves the files (and the base URL above)
# intact so the status line still points at a live proxy.
if [ "$WANT_PROXY" = 1 ]; then
  for old in "$DIR/ds4-effort-proxy.py" "$DIR/ds4-thinking-proxy.py" "$DIR/nous-effort-proxy.py"; do
    if [ -e "$old" ] || [ -L "$old" ]; then
      rm -f "$old"
      echo "removed:  $old (superseded by the shared proxy)"
    fi
  done
fi

BACKUP="$SETTINGS.bak-$(date +%Y%m%d%H%M%S)"
cp -p "$SETTINGS" "$BACKUP"

BAR_DST="$BAR_DST" WANT_PROXY="$WANT_PROXY" PORT="$PORT" DIR="$DIR" python3 - "$SETTINGS" <<'PY'
import json, os, sys
p = sys.argv[1]
with open(p) as fh:
    s = json.load(fh)
s["statusLine"] = {"type": "command", "command": os.environ["BAR_DST"], "padding": 0}
if os.environ["WANT_PROXY"] == "1":
    url = "http://127.0.0.1:" + os.environ["PORT"]
    was = s.setdefault("env", {}).get("ANTHROPIC_BASE_URL")
    s["env"]["ANTHROPIC_BASE_URL"] = url
    if was != url:
        print(f"base URL: {was} -> {url}")
    # SessionStart fires on resume too, so a restored cmux session starts the
    # proxy before its first request. See src/ds4-proxy-kickstart.sh.
    cmd = os.environ["DIR"] + "/ds4-proxy-kickstart.sh"
    s.setdefault("hooks", {}).setdefault("SessionStart", [{"matcher": "*", "hooks": []}])
    hooks = s["hooks"]["SessionStart"][0]["hooks"]
    if not any(h.get("command") == cmd for h in hooks):
        hooks.append({"type": "command", "command": cmd, "timeout": 15})
with open(p, "w") as fh:
    json.dump(s, fh, indent=2)
os.chmod(p, 0o600)
PY

echo "backup:   $BACKUP"

if [ "$WANT_PROXY" = 1 ] && [ "$(uname)" = Darwin ]; then
  mkdir -p "$(dirname "$PLIST")"

  # The plist carries the DS4_* knobs into the agent. The proxy reads them at
  # startup, and launchd starts the agent from its own environment, so anything
  # exported when install.sh runs is baked in. Sweep the whole DS4_* namespace so
  # a knob the proxy adds later works without a second edit here. Values are XML
  # entities only; the rest of the heredoc body is not re-expanded.
  #
  # Vision spawns `claude` directly. Under launchd the bare name is not on PATH,
  # so bake the absolute binary into the agent. `|| true` keeps the
  # `set -euo pipefail` install alive on a machine with no claude on PATH —
  # vision simply fails open there. Validate it is a real executable (a cmux
  # shim is a temp file that vanishes after reboot); an invalid path is left
  # empty so the proxy falls back to shutil.which at startup.
  _ds4_claude="$(command -v claude || true)"
  if [ -n "$_ds4_claude" ] && [ ! -x "$_ds4_claude" ]; then
    _ds4_claude=""
  fi
  DS4_CLAUDE_BIN="$_ds4_claude"
  export DS4_CLAUDE_BIN

  # The launchd agent env is sparse by default: no HOME, and a PATH of
  # /usr/bin:/bin:/usr/sbin:/sbin. The vision child needs HOME to find
  # ~/.claude (the Anthropic profile) + the login keychain, and needs the real
  # claude bin dir on PATH or it exits 127 "claude not found in PATH" and every
  # image placeholders. Bake the install-time values in so the agent matches the
  # user's session. The claude dir is derived from the resolved binary's dir.
  _ds4_claude_dir="$(dirname "$_ds4_claude" 2>/dev/null || true)"
  DS4_AGENT_HOME="${HOME:-$HOME}"
  DS4_AGENT_USER="${USER:-$(id -un)}"
  DS4_AGENT_LOGNAME="${LOGNAME:-$(id -un)}"
  if [ -n "$_ds4_claude_dir" ]; then
    DS4_AGENT_PATH="/usr/bin:/bin:/usr/sbin:/sbin:$_ds4_claude_dir"
  else
    DS4_AGENT_PATH="/usr/bin:/bin:/usr/sbin:/sbin"
  fi
  export DS4_AGENT_HOME DS4_AGENT_USER DS4_AGENT_LOGNAME DS4_AGENT_PATH

  # The launchd-owned listener is the local ownership boundary. A manual
  # self-bound loopback listener is intentionally rejected by the proxy in the
  # installed agent, preventing a different local account from winning a
  # restart race and presenting a fake proxy.
  export DS4_REQUIRE_OWNED_SOCKET=1

  # The permission classifier gates every tool call in auto mode. By default it
  # routes to the operator's Anthropic subscription (a trusted boundary) rather
  # than DeepSeek — which needs a subscription token. Ask once at install time,
  # since whoever runs install.sh is almost certainly using their own claude:
  #   * Anthropic (default) — run `claude setup-token` and bake the token.
  #   * or-ds4 / ZDR — bake DS4_CLASSIFIER=zdr; the classifier uses the or-ds4
  #     route (OpenRouter, ZDR on). No subscription token, no training — the
  #     gate runs on DeepSeek V4 Flash via OpenRouter instead.
  #   * DeepSeek — bake DS4_CLASSIFIER=ds4; no token, classifier stays local.
  # A pre-set DS4_CLASSIFIER or DS4_CLASSIFIER_TOKEN skips the prompt. Only
  # prompt on an interactive TTY (tests/CI/--dry-run are non-interactive).
  if [ -z "${DS4_CLASSIFIER:-}" ] && [ -z "${DS4_CLASSIFIER_TOKEN:-}" ]; then
    if [ "$DRY" = 0 ] && [ -t 0 ]; then
      printf "  Route the permission classifier to your Anthropic subscription? [Y/n] (z = or-ds4 ZDR) " >&2
      read -r _route
      case "${_route:-y}" in
        z|Z)
          export DS4_CLASSIFIER=zdr
          echo "  Classifier -> or-ds4 (OpenRouter ZDR)." >&2
          ;;
        y|Y|"")
          if command -v claude >/dev/null 2>&1; then
            echo "  Running 'claude setup-token' to mint a subscription token..." >&2
            _tok="$(claude setup-token 2>/dev/null | tail -1)"
            if [ -n "$_tok" ]; then
              export DS4_CLASSIFIER_TOKEN="$_tok"
              echo "  Baked DS4_CLASSIFIER_TOKEN into the agent." >&2
            else
              echo "  setup-token produced no token." >&2
              printf "  Route the classifier to DeepSeek instead? [Y/n] " >&2
              read -r _fallback
              case "${_fallback:-y}" in
                y|Y|"") export DS4_CLASSIFIER=ds4; echo "  Classifier -> DeepSeek." >&2 ;;
                *) echo "  Leaving unset; the classifier will fail open to ds4." >&2 ;;
              esac
            fi
          else
            echo "  'claude' not on PATH." >&2
            printf "  Route the classifier to DeepSeek instead? [Y/n] " >&2
            read -r _fallback
            case "${_fallback:-y}" in
              y|Y|"") export DS4_CLASSIFIER=ds4; echo "  Classifier -> DeepSeek." >&2 ;;
              *) echo "  Leaving unset; the classifier will fail open to ds4." >&2 ;;
            esac
          fi
          ;;
        n|N)
          export DS4_CLASSIFIER=ds4
          echo "  Classifier -> DeepSeek." >&2
          ;;
      esac
    else
      echo "  (non-interactive: classifier defaults to Anthropic and fails open to ds4"
      echo "   until DS4_CLASSIFIER_TOKEN is set, or set DS4_CLASSIFIER=zdr to use"
      echo "   or-ds4 / OpenRouter ZDR, or DS4_CLASSIFIER=ds4 to opt out)" >&2
    fi
  fi

  PLIST_ENV=""
  while IFS= read -r kv; do
    case "$kv" in
      DS4_*=*)
        key="${kv%%=*}"
        val="${kv#*=}"
        val="${val//&/&amp;}"
        val="${val//</&lt;}"
        val="${val//>/&gt;}"
        PLIST_ENV+="    <key>${key}</key>
    <string>${val}</string>
"
        ;;
    esac
  done < <(env)

  # One Sockets entry per served profile, keyed by profile name because that is
  # the name the binary passes to launch_activate_socket. Ports come from the
  # binary's own --ports so its profile table stays the only declaration.
  PLIST_SOCKETS=""
  while read -r sock_name sock_port; do
    [ -n "$sock_name" ] || continue
    valid_port "$sock_port" || {
      echo "agent:    $sock_name port '$sock_port' is not a decimal 1024-65535; not writing plist" >&2
      exit 1
    }
    PLIST_SOCKETS+="    <key>${sock_name}</key>
    <dict>
      <key>SockNodeName</key>
      <string>127.0.0.1</string>
      <key>SockServiceName</key>
      <string>${sock_port}</string>
    </dict>
"
  done < <("$GO_BIN" --ports)

  if [ -z "$PLIST_SOCKETS" ]; then
    echo "agent:    ds4-proxy --ports listed no profiles; not writing plist" >&2
    exit 1
  fi

  PLIST_TMP="$(mktemp "${TMPDIR:-/tmp}/$LABEL.plist.XXXXXX")"
  cat > "$PLIST_TMP" <<PLISTEOF
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key>
  <string>$LABEL</string>

  <!-- The Go binary is built and smoke-tested above. It collects the sockets
       launchd binds below via launch_activate_socket, so it satisfies the
       Sockets contract: launchd owns the ports and hands over already-listening
       fds. Absent a launchd parent (a manual run, the differential harness) the
       same code falls back to binding its own ports. -->
  <key>ProgramArguments</key>
  <array>
    <string>$GO_BIN</string>
  </array>

  <!-- These are the DS4_* knobs present when install.sh ran, e.g.
       DS4_IDLE_EXIT=0 to run forever. Set one by exporting it and re-running
       install.sh, which rewrites the plist and reloads the agent. The
       classifier routes to Anthropic via DS4_CLASSIFIER_TOKEN (claude
       setup-token); export it before install.sh so the agent can serve it. -->
  <key>EnvironmentVariables</key>
  <dict>
    <!-- The agent env is sparse by default (no HOME, minimal PATH). The vision
         child needs HOME to find ~/.claude + the login keychain, and the real
         claude bin dir on PATH (else exit 127, every image placeholders). -->
    <key>HOME</key>
    <string>$DS4_AGENT_HOME</string>
    <key>USER</key>
    <string>$DS4_AGENT_USER</string>
    <key>LOGNAME</key>
    <string>$DS4_AGENT_LOGNAME</string>
    <key>PATH</key>
    <string>$DS4_AGENT_PATH</string>
$PLIST_ENV  </dict>

  <!-- launchd binds these itself and hands the listening fds to the process it
       starts on the first connection; the proxy collects them via
       launch_activate_socket. Owning a socket is what makes the job on-demand,
       which is the point: a job with no demand criteria is reaped a couple of
       minutes after kickstart ("service inactive" then "removing service" in
       the launchd log), and an earlier KeepAlive/SuccessfulExit=false here did
       not prevent that, because refusing to restart is not a demand criterion.
       It also means the ports answer while the proxy is stopped, so the idle
       exit costs a cold start rather than a connection refused. -->
  <key>Sockets</key>
  <dict>
$PLIST_SOCKETS  </dict>

  <!-- No KeepAlive: it would fight the Sockets contract by restarting a process
       launchd is meant to start on demand. RunAtLoad stays off for the same
       reason - the first request starts it. -->
  <key>RunAtLoad</key>
  <false/>

  <key>StandardOutPath</key>
  <string>$HOME/.claude-ds4-proxy.log</string>
  <key>StandardErrorPath</key>
  <string>$HOME/.claude-ds4-proxy.log</string>
</dict>
</plist>
PLISTEOF

  if cmp -s "$PLIST_TMP" "$PLIST"; then
    rm -f "$PLIST_TMP"
    echo "agent:    unchanged, not reloaded"
  else
    mv "$PLIST_TMP" "$PLIST"
    # One process serves every profile, so only take it down when the plist
    # actually changed. Record whether it was running first, so a reload can
    # bring it back and not drop live sessions; RunAtLoad=false means a freshly
    # bootstrapped job is parked, so "running" has to be captured pre-bootout.
    # The ports answer either way once bootstrap returns, because launchd holds
    # the sockets. The kickstart below only saves a live session the cold start.
    was_running=0
    if launchctl print "gui/$(id -u)/$LABEL" 2>/dev/null | grep -q 'state = running'; then
      was_running=1
    fi
    # bootout is async and a job that does not exist is not an error, so both
    # exit states are fine.
    launchctl bootout "gui/$(id -u)/$LABEL" 2>/dev/null || true
    # Wait for the old job to really leave, else bootstrap on the same label
    # races it and fails under set -e mid-install. bootout is async, so the exit
    # status is unreliable mid-drain; key on the state line instead.
    n=0
    while launchctl print "gui/$(id -u)/$LABEL" 2>/dev/null | grep -q 'state ='; do
      n=$((n + 1))
      [ "$n" -ge 20 ] && break
      sleep 0.25
    done
    if launchctl bootstrap "gui/$(id -u)" "$PLIST"; then
      if [ "$was_running" = 1 ]; then
        launchctl kickstart "gui/$(id -u)/$LABEL" 2>/dev/null || true
      fi
      echo "agent:    loaded $LABEL"
    else
      echo "agent:    plist written but launchctl bootstrap failed; re-run install.sh or kickstart manually" >&2
      exit 1
    fi
  fi

  # Old per-profile agents from before the merge would fight for the same ports.
  for old in thinking-proxy effort-proxy nous-proxy; do
    if launchctl print "gui/$(id -u)/com.strml.cc-ds4.$old" >/dev/null 2>&1; then
      launchctl bootout "gui/$(id -u)/com.strml.cc-ds4.$old" 2>/dev/null || true
      rm -f "$HOME/Library/LaunchAgents/com.strml.cc-ds4.$old.plist"
      echo "removed:  agent com.strml.cc-ds4.$old (superseded)"
    fi
  done
fi

echo
echo "Verify the bar renders before walking away — a wrapper that fails open turns"
echo "a syntax error into a blank bar and exit 0:"
echo
echo "  $BAR_DST"

if [ "$WANT_PROXY" = 1 ]; then
  cat <<EOF

The profile routes through the proxy, and NOTHING WORKS until it is running.
Every request gets connection-refused, which looks exactly like a bad key:

  nc -z 127.0.0.1 $PORT

A SessionStart hook now kickstarts the proxy, so a fresh or resumed session
starts it without the launcher. The '$LAUNCHER' function in profiles/$DOC still
matters: it starts the proxy and registers a session so it is not reaped
mid-use. A bare ccam alias is not enough. To start it by hand right now:

  launchctl kickstart gui/$(id -u)/$LABEL     # or run $GO_BIN directly
EOF
fi
