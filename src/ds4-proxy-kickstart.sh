#!/usr/bin/env bash
# Start the shared ds4 proxy from a Claude Code SessionStart hook.
#
# A profile's settings.json routes every request through the proxy on its port,
# so nothing works until that port answers. The launcher functions start it, but
# they are only on the interactive path. When cmux restores a session it replays
# `claude --resume <id>` through its own wrapper and never touches the launcher,
# so a cold start leaves every restored profile pointing at a dead port. This
# hook is registered as SessionStart in each profile's settings.json, which fires
# on resume too, and makes the proxy answer before the first request goes out.
#
# It reads the profile's port straight from its own settings.json (ANTHROPIC_BASE_URL),
# so it does not need an argument and cannot drift from what the profile uses.
# Fails open: if launchctl is missing (Linux) or the agent is unknown, it just
# exits 0. The launcher's longer, port-waiting kickstart still covers the
# interactive path.
set -euo pipefail

# Fast path: the port already answers, so the proxy is up and nothing to do.
port() {
    python3 - "$1" <<'PY'
import json, re, sys
try:
    with open(sys.argv[1]) as fh:
        env = json.load(fh).get("env", {})
    m = re.match(r"http://127\.0\.0\.1:(\d+)", env.get("ANTHROPIC_BASE_URL", ""))
    if m:
        sys.stdout.write(m.group(1))
except Exception:
    pass
PY
}

profile_port="$(port "${CLAUDE_CONFIG_DIR:-$HOME}/settings.json" 2>/dev/null || true)"
[ -n "$profile_port" ] || exit 0
if nc -z 127.0.0.1 "$profile_port" 2>/dev/null; then
    exit 0
fi

launchctl kickstart "gui/$(id -u)/com.strml.cc-ds4.proxy" 2>/dev/null || exit 0

for _ in $(seq 40); do
    nc -z 127.0.0.1 "$profile_port" 2>/dev/null && exit 0
    sleep 0.25
done
