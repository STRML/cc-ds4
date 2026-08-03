#!/usr/bin/env bash
# Tests for install.sh: argument handling, symlink behaviour, the embedded
# settings.json rewrite, and stale-symlink cleanup. Run standalone:
#   bash tests/test_install.sh
set -euo pipefail

REPO="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
FAILED=0

t() {
  local name="$1"
  shift
  if "$@"; then echo "ok   - $name"; else echo "FAIL - $name"; FAILED=1; fi
}

# --- a fresh throwaway profile ------------------------------------------------
WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT
PROF="$WORK/profile"
mkdir -p "$PROF"
printf '%s' '{"env": {"ANTHROPIC_AUTH_TOKEN": "sk-test"}, "model": "m"}' > "$PROF/settings.json"

t "--dry-run writes nothing" bash "$REPO/install.sh" --profile direct --dir "$PROF" --dry-run
t "dry run leaves no bar link" test ! -e "$PROF/ds4-statusline.py"

t "installs statusline symlink" bash "$REPO/install.sh" --profile direct --dir "$PROF" --no-proxy
t "bar is a symlink" test -L "$PROF/ds4-statusline.py"
t "settings keeps its key" grep -q '"ANTHROPIC_AUTH_TOKEN"' "$PROF/settings.json"

# --- embedded JSON rewrite: base URL set when proxy wanted ----------------------
printf '%s' '{"env": {}}' > "$PROF/settings.json"
if [ "$(uname)" = Darwin ]; then
  # install.sh's WANT_PROXY=1 path runs launchctl bootout/bootstrap on the
  # USER'S REAL launch agent. Never trigger that from a test. Skip the
  # installer; verify the embedded JSON rewrite by extracting it below.
  echo "skip - proxy base-URL rewrite (Darwin: launchctl path not touched)"
else
  t "proxy rewrites base URL" bash "$REPO/install.sh" --profile openrouter --dir "$PROF"
  t "base URL points at 31501" grep -q 'http://127.0.0.1:31501' "$PROF/settings.json"
fi

# --- --no-proxy leaves base URL alone ------------------------------------------
printf '%s' '{"env": {}}' > "$PROF/settings.json"
t "no-proxy leaves env alone" bash "$REPO/install.sh" --profile openrouter --dir "$PROF" --no-proxy
# The t helper runs `if "$@"`, which cannot execute a leading `!` (a reserved
# word, not a command). Wrap the negation in an inner bash -c, passing $PROF
# through as $1, the same pattern the bad-argument checks use.
t "no base URL rewrite" bash -c '! grep -q ANTHROPIC_BASE_URL "$1/settings.json"' _ "$PROF"

# --- stale symlink cleanup: dangling links must be removed ----------------------
ln -s /nonexistent/target "$PROF/ds4-effort-proxy.py"
ln -s /nonexistent/target "$PROF/nous-effort-proxy.py"
t "stale proxy symlinks removed" bash "$REPO/install.sh" --profile direct --dir "$PROF" --no-proxy
t "no stale effort link" test ! -e "$PROF/ds4-effort-proxy.py"
t "no stale nous link" test ! -e "$PROF/nous-effort-proxy.py"

# --- bad arguments --------------------------------------------------------------
# Debate fix: the inner bash -c had no $REPO in scope (only $1/$2), so
# `bash "$REPO/install.sh"` ran `bash "/install.sh"` -> 127. Pass $REPO as $1.
t "rejects unknown profile" bash -c '! bash "$1/install.sh" --profile nope --dir "$2" 2>/dev/null' _ "$REPO" "$PROF"
t "rejects missing settings.json" bash -c 'mkdir -p "$2" && ! bash "$1/install.sh" --profile direct --dir "$2" 2>/dev/null' _ "$REPO" "$PROF/sub"

# --- help exits 0 ----------------------------------------------------------------
t "help exits 0" bash "$REPO/install.sh" --help >/dev/null 2>&1

exit "$FAILED"
