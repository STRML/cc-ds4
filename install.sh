#!/usr/bin/env bash
# Install the proxy and a corrected status line into an existing Claude Code profile.
#
# This does not create the profile. Use the setup prompt in profiles/ for that;
# this installs the two pieces that get updates.
#
#   ./install.sh --profile openrouter          # ~/.claude-or-ds4
#   ./install.sh --profile direct              # ~/.claude-ds4
#   ./install.sh --profile direct --dir ~/.claude-something-else
#   ./install.sh --profile direct --no-proxy   # status line only
#
# The proxy and the status line are symlinked into the profile directory, so the
# profile is the interface and this checkout is the source of truth: `git pull`
# updates both without re-running anything. That is the same shape as the profile's
# other entries, which are symlinks into ~/.claude.
#
# It matters that settings.json and the launcher both point at the PROFILE path
# rather than at the checkout. The launcher is hand-copied out of a doc into a
# shell config, and $HOME/.claude-ds4/... is the same string on every machine.
#
# The cship config is copied, not linked, since it is meant to be edited.
#
# A setup done straight from profiles/*.md copies the proxy in rather than linking
# it, because there may be no checkout on that machine. Running this afterwards
# replaces that copy with a symlink and says so.
set -euo pipefail

REPO="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROFILE="" DIR="" DRY=0 WANT_PROXY=1

while [ $# -gt 0 ]; do
  case "$1" in
    --profile)  PROFILE="${2:-}"; shift 2 ;;
    --dir)      DIR="${2:-}"; shift 2 ;;
    --dry-run)  DRY=1; shift ;;
    --no-proxy) WANT_PROXY=0; shift ;;
    -h|--help)  sed -n '2,15p' "$0" | sed 's/^# \{0,1\}//'; exit 0 ;;
    *) echo "unknown argument: $1" >&2; exit 2 ;;
  esac
done

case "$PROFILE" in
  openrouter) SCRIPT="$REPO/src/statusline/openrouter.py"
              CONFIG="$REPO/config/cship-openrouter.toml"
              PROXY_SRC="$REPO/src/effort_proxy.py"
              PROXY_DST="ds4-effort-proxy.py"
              PORT=31501
              LAUNCHER="claude-or-ds4"
              DOC="openrouter.md"
              DIR="${DIR:-$HOME/.claude-or-ds4}" ;;
  direct)     SCRIPT="$REPO/src/statusline/direct.py"
              CONFIG="$REPO/config/cship-direct.toml"
              PROXY_SRC="$REPO/src/thinking_proxy.py"
              PROXY_DST="ds4-thinking-proxy.py"
              PORT=31500
              LAUNCHER="claude-ds4"
              DOC="deepseek-direct.md"
              DIR="${DIR:-$HOME/.claude-ds4}" ;;
  nous)       SCRIPT="$REPO/src/statusline/nous.py"
              CONFIG="$REPO/config/cship-nous.toml"
              PROXY_SRC="$REPO/src/effort_proxy.py"
              PROXY_DST="ds4-effort-proxy.py"
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

echo "profile:  $DIR"
echo "bar:      $BAR_DST -> $SCRIPT"
echo "config:   $DIR/cship.toml  (from $(basename "$CONFIG"))"
if [ "$WANT_PROXY" = 1 ]; then
  echo "proxy:    $DIR/$PROXY_DST -> $PROXY_SRC  (:$PORT)"
  echo "base URL: http://127.0.0.1:$PORT"
fi
[ "$DRY" = 1 ] && { echo "(dry run, nothing written)"; exit 0; }

# Say so when we replace a real file, since that is someone's hand-copied setup
# from the profile prompt and the symlink silently changes where updates come from.
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
[ "$WANT_PROXY" = 1 ] && link "$PROXY_SRC" "$DIR/$PROXY_DST"

BACKUP="$SETTINGS.bak-$(date +%Y%m%d%H%M%S)"
cp -p "$SETTINGS" "$BACKUP"

BAR_DST="$BAR_DST" WANT_PROXY="$WANT_PROXY" PORT="$PORT" python3 - "$SETTINGS" <<'PY'
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
with open(p, "w") as fh:
    json.dump(s, fh, indent=2)
os.chmod(p, 0o600)
PY

echo "backup:   $BACKUP"
echo
echo "Verify it renders before walking away — a wrapper that fails open turns a"
echo "syntax error into a blank bar and exit 0:"
echo
echo "  $BAR_DST"

if [ "$WANT_PROXY" = 1 ]; then
  cat <<EOF

The profile now routes through the proxy, and NOTHING WORKS until something
starts it. Every request gets connection-refused, which looks exactly like a bad
key. Check it first:

  nc -z 127.0.0.1 $PORT

If you do not already have a '$LAUNCHER' function that starts the proxy, add the
one from profiles/$DOC (the Launcher step). A bare ccam alias is not enough. To
start it by hand right now:

  python3 $DIR/$PROXY_DST &
EOF
fi
