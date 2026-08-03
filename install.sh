#!/usr/bin/env bash
# Install a corrected status line into an existing Claude Code profile.
#
# This does not create the profile. Use the setup prompt in profiles/ for that;
# this only replaces the status line once the profile exists.
#
#   ./install.sh --profile openrouter          # ~/.claude-or-ds4
#   ./install.sh --profile direct              # ~/.claude-ds4
#   ./install.sh --profile direct --dir ~/.claude-something-else
#
# settings.json is pointed at this checkout rather than a copy, so `git pull`
# updates the bar. The cship config IS copied, since it is meant to be tweaked.
set -euo pipefail

REPO="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROFILE="" DIR="" DRY=0

while [ $# -gt 0 ]; do
  case "$1" in
    --profile) PROFILE="${2:-}"; shift 2 ;;
    --dir)     DIR="${2:-}"; shift 2 ;;
    --dry-run) DRY=1; shift ;;
    -h|--help) sed -n '2,13p' "$0" | sed 's/^# \{0,1\}//'; exit 0 ;;
    *) echo "unknown argument: $1" >&2; exit 2 ;;
  esac
done

case "$PROFILE" in
  openrouter) SCRIPT="$REPO/src/statusline/openrouter.py"
              CONFIG="$REPO/config/cship-openrouter.toml"
              DIR="${DIR:-$HOME/.claude-or-ds4}" ;;
  direct)     SCRIPT="$REPO/src/statusline/direct.py"
              CONFIG="$REPO/config/cship-direct.toml"
              DIR="${DIR:-$HOME/.claude-ds4}" ;;
  *) echo "usage: $0 --profile openrouter|direct [--dir PATH] [--dry-run]" >&2; exit 2 ;;
esac

[ -d "$DIR" ] || { echo "no profile at $DIR — create it first with profiles/*.md" >&2; exit 1; }
SETTINGS="$DIR/settings.json"
[ -f "$SETTINGS" ] || { echo "no settings.json in $DIR" >&2; exit 1; }

command -v cship >/dev/null 2>&1 || echo "warning: cship not on PATH; edit CSHIP in $SCRIPT" >&2

echo "profile:  $DIR"
echo "bar:      $SCRIPT"
echo "config:   $DIR/cship.toml  (from $(basename "$CONFIG"))"
[ "$DRY" = 1 ] && { echo "(dry run, nothing written)"; exit 0; }

chmod +x "$SCRIPT"
cp "$CONFIG" "$DIR/cship.toml"

BACKUP="$SETTINGS.bak-$(date +%Y%m%d%H%M%S)"
cp -p "$SETTINGS" "$BACKUP"

SCRIPT="$SCRIPT" python3 - "$SETTINGS" <<'PY'
import json, os, sys
p = sys.argv[1]
s = json.load(open(p))
s["statusLine"] = {"type": "command", "command": os.environ["SCRIPT"], "padding": 0}
json.dump(s, open(p, "w"), indent=2)
os.chmod(p, 0o600)
PY

echo "backup:   $BACKUP"
echo
echo "Verify it renders before walking away — a wrapper that fails open turns a"
echo "syntax error into a blank bar and exit 0:"
echo
echo "  $SCRIPT"
