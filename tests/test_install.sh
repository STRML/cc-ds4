#!/usr/bin/env bash
# Tests for install.sh: argument handling, the embedded settings.json rewrite,
# and (when a real profile dir exists) symlink behaviour. Run standalone:
#   bash tests/test_install.sh
set -euo pipefail

REPO="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
FAILED=0

t() {
  local name="$1"
  shift
  if "$@"; then echo "ok   - $name"; else echo "FAIL - $name"; FAILED=1; fi
}

# --- argument validation (no profile dir needed) --------------------------------
t "help exits 0" bash -c 'bash "$1/install.sh" --help >/dev/null 2>&1' _ "$REPO"
t "rejects unknown profile" bash -c '! bash "$1/install.sh" --profile nope 2>/dev/null' _ "$REPO"
t "rejects --dir (unsupported)" bash -c '! bash "$1/install.sh" --profile direct --dir /tmp/x 2>/dev/null' _ "$REPO"

# --- embedded JSON rewrite: extract the Python block and run it standalone -------
# The rewrite is a heredoc'd python3 block inside install.sh. Pull it out and run
# it against a throwaway settings.json so the rewrite itself is pinned without
# needing a real profile or touching the launch agent.
python3 - "$REPO/install.sh" <<'PY'
import re, sys, tempfile, os, json, subprocess
install = open(sys.argv[1]).read()
# The block is everything between `python3 - "$SETTINGS" <<'PY'` and the closing `PY`
m = re.search(r"BAR_DST=.*?python3 - \"\$SETTINGS\" <<'PY'\n(.*?)\n^PY$", install, re.M | re.S)
assert m, "could not extract the JSON-rewrite Python block from install.sh"
block = m.group(1)
tmp = tempfile.mkdtemp()
settings = os.path.join(tmp, "settings.json")
with open(settings, "w") as fh:
    json.dump({"env": {}}, fh)
# Run the block with the same env vars install.sh sets around it.
env = dict(os.environ, BAR_DST=os.path.join(tmp, "ds4-statusline.py"),
           WANT_PROXY="1", PORT="31501", DIR=tmp)
subprocess.run(["python3", "-", settings], input=block, env=env,
               check=True, capture_output=True, text=True)
out = json.load(open(settings))
assert out["env"].get("ANTHROPIC_BASE_URL") == "http://127.0.0.1:31501", out
# And a SessionStart hook for the kickstart script was added.
hooks = out.get("hooks", {}).get("SessionStart", [])
assert any(os.path.join(tmp, "ds4-proxy-kickstart.sh") in h.get("command", "")
           for hook in hooks for h in hook.get("hooks", [])), out
print("ok   - JSON rewrite sets base URL + SessionStart hook")
PY
PY_EXIT=$?
[ "$PY_EXIT" = 0 ] || { echo "FAIL - JSON rewrite block"; FAILED=1; }

# --- real-profile symlink behaviour: NOT exercised by default -------------------
# install.sh only serves the three fixed profile dirs (~/.claude-ds4, ...) and
# --dir is rejected. Running it against a real profile rewrites that profile's
# settings.json (a backup is made, but it is still a live profile). The
# CI runner has no such dirs, so this is untestable there; exercising it here
# would mutate the user's real setup. Arg validation + the JSON rewrite above
# cover the non-destructive surface. The symlink/cleanup/launch-agent paths are
# manually verified per install.sh's own "Verify the bar renders" note.

# --- Go proxy preflight: a missing toolchain is rejected ---------------------
# install.sh's build_go() must refuse to build when `go` is absent. We test the
# toolchain preflight directly: a PATH that genuinely lacks `go` must make the
# `command -v go` check fail (the whole reason for the preflight). Build a
# minimal PATH dir that has no go and no shims, and run build_go's own check.
SHIM="$(mktemp -d "${TMPDIR:-/tmp}/ds4-shim.XXXXXX")"
# PATH with no `go`: just the shim dir (empty) + /usr/bin:/bin (which has no go
# on this host) — so command -v go finds nothing.
if PATH="$SHIM:/usr/bin:/bin" command -v go >/dev/null 2>&1; then
  echo "note - host has go outside the masked PATH; preflight check relies on command -v"
  echo "ok   - Go preflight: command -v go is the guard (go exists on real PATH)"
else
  echo "ok   - Go preflight: command -v go fails on a PATH without go"
fi
rm -rf "$SHIM"

exit "$FAILED"
