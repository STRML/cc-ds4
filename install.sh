#!/usr/bin/env bash
# Install the status line into a Claude Code profile, and the shared proxy agent.
#
# This does not create the profile. Use the setup prompt in profiles/ for that;
# this installs the pieces that get updates.
#
#   ./install.sh --profile openrouter          # ~/.claude-or-ds4
#   ./install.sh --profile direct              # ~/.claude-ds4
#   ./install.sh --profile nous                # ~/.claude-nous
#   ./install.sh --profile direct --dir ~/.claude-something-else
#   ./install.sh --profile direct --no-proxy   # status line only
#
# One proxy process serves every profile, each on its own port, so a profile's
# settings.json is unchanged and unaware it is shared. src/proxy.py holds the
# table. On macOS this also writes and loads a single launch agent that runs it.
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

while [ $# -gt 0 ]; do
  case "$1" in
    --profile)  PROFILE="${2:-}"; shift 2 ;;
    --dir)      DIR="${2:-}"; shift 2 ;;
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
  *) echo "usage: $0 --profile openrouter|direct|nous [--dir PATH] [--no-proxy] [--dry-run]" >&2; exit 2 ;;
esac

[ -d "$DIR" ] || { echo "no profile at $DIR — create it first with profiles/*.md" >&2; exit 1; }
SETTINGS="$DIR/settings.json"
[ -f "$SETTINGS" ] || { echo "no settings.json in $DIR" >&2; exit 1; }

command -v cship >/dev/null 2>&1 || echo "warning: cship not on PATH; edit CSHIP in $SCRIPT" >&2

BAR_DST="$DIR/ds4-statusline.py"
HOOK_SRC="$REPO/src/ds4-proxy-kickstart.sh"
HOOK_DST="$DIR/ds4-proxy-kickstart.sh"

echo "profile:  $DIR"
echo "bar:      $BAR_DST -> $SCRIPT"
echo "config:   $DIR/cship.toml  (from $(basename "$CONFIG"))"
if [ "$WANT_PROXY" = 1 ]; then
  echo "proxy:    $REPO/src/proxy.py  (this profile on :$PORT)"
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
[ "$WANT_PROXY" = 1 ] && link "$HOOK_SRC" "$HOOK_DST"

# A previous release gave each profile its own proxy copy. One process serves them
# all now, so leaving those behind means a stale second binder fighting for the port.
# -e is false for a dangling symlink, and after the merge these point at deleted
# files, so -L has to be tested too or the stale links survive the upgrade.
for old in "$DIR/ds4-effort-proxy.py" "$DIR/ds4-thinking-proxy.py" "$DIR/nous-effort-proxy.py"; do
  if [ -e "$old" ] || [ -L "$old" ]; then
    rm -f "$old"
    echo "removed:  $old (superseded by the shared proxy)"
  fi
done

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
  cat > "$PLIST" <<PLISTEOF
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key>
  <string>$LABEL</string>

  <key>ProgramArguments</key>
  <array>
    <string>/usr/bin/python3</string>
    <string>$REPO/src/proxy.py</string>
  </array>

  <!-- KeepAlive must be this dict, not <false/>. With RunAtLoad and KeepAlive
       both off, launchd sees a job with no demand criteria and SIGTERMs it a
       couple of minutes after kickstart. SuccessfulExit=false gives it a reason
       to leave a running job alone, while still not restarting the clean exit(0)
       the idle timer performs. A crash exits nonzero and does get restarted. -->
  <key>RunAtLoad</key>
  <false/>
  <key>KeepAlive</key>
  <dict>
    <key>SuccessfulExit</key>
    <false/>
  </dict>

  <key>StandardOutPath</key>
  <string>$HOME/.claude-ds4-proxy.log</string>
  <key>StandardErrorPath</key>
  <string>$HOME/.claude-ds4-proxy.log</string>
</dict>
</plist>
PLISTEOF
  launchctl bootout "gui/$(id -u)/$LABEL" 2>/dev/null || true
  launchctl bootstrap "gui/$(id -u)" "$PLIST"
  echo "agent:    loaded $LABEL"

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

  launchctl kickstart gui/$(id -u)/$LABEL     # or: python3 $REPO/src/proxy.py &
EOF
fi
