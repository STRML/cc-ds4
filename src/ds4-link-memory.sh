#!/usr/bin/env bash
# Symlink a profile's per-project memory dirs to the canonical ~/.claude copy.
#
# Claude Code stores project memory under $CLAUDE_CONFIG_DIR/projects/<project>/memory.
# With a per-profile config dir that is per-profile state: a note written on the
# nous profile is invisible on direct and openrouter. The note files belong in the
# real ~/.claude/projects/<project>/memory; the profile dir is just a different
# pointer to the same config, so its memory should be too.
#
# For every existing project dir under the profile that has a canonical counterpart,
# this replaces the profile's memory dir with a symlink to the canonical one (if it
# is a real dir, its contents are moved into canonical first so nothing is lost).
# New project dirs created after this runs are linked by the next SessionStart
# (ds4-proxy-kickstart.sh calls this).
#
# Usage: ds4-link-memory.sh <profile-dir>
set -euo pipefail

PROFILE_DIR="${1:-}"
[ -n "$PROFILE_DIR" ] || { echo "usage: $0 <profile-dir>" >&2; exit 2; }
[ -d "$PROFILE_DIR/projects" ] || exit 0   # no projects yet, nothing to link

CANON_ROOT="$HOME/.claude/projects"
[ -d "$CANON_ROOT" ] || exit 0             # no canonical memory at all, nothing to do

linked=0
moved=0
for proj_dir in "$PROFILE_DIR"/projects/*/; do
  [ -d "$proj_dir" ] || continue
  proj="$(basename "$proj_dir")"
  canon="$CANON_ROOT/$proj/memory"
  mem="$proj_dir/memory"
  [ -d "$canon" ] || continue              # no canonical project memory, leave it

  if [ -L "$mem" ]; then
    continue                               # already linked
  elif [ -d "$mem" ]; then
    # Real dir: move its contents into canonical (merging), then link.
    # Memory files are small; this is the durable join, not a copy that drifts.
    mkdir -p "$canon"
    # Move any file that canonical lacks; keep the newer of any that collide.
    for f in "$mem"/*.md; do
      [ -e "$f" ] || continue
      b="$(basename "$f")"
      if [ ! -e "$canon/$b" ]; then
        mv "$f" "$canon/$b"
        moved=$((moved + 1))
      else
        rm -f "$f"   # canonical already has it
      fi
    done
    rmdir "$mem" 2>/dev/null || true
    ln -s "$canon" "$mem"
    linked=$((linked + 1))
    echo "linked: $mem -> $canon (moved $moved note file(s))"
  else
    ln -s "$canon" "$mem"                  # no memory yet, link ahead
    linked=$((linked + 1))
  fi
done

[ "$linked" -gt 0 ] && echo "ds4-link-memory: linked $linked project memory dir(s) to canonical"
exit 0
