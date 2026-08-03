#!/usr/bin/env python3
"""One proxy for every DeepSeek V4 profile.

Each profile gets its own listener on its own port, so `settings.json` on each
side is unchanged and unaware this is shared. What varies between them is a row
in PROFILES, not a separate script.

Why any proxy exists at all:

  * V4 is in thinking mode by default and Claude Code cannot turn it off. It
    sends thinking={"type":"adaptive"}, which no provider serving this model
    implements. A main-loop request at max_tokens=32000 is unaffected; a small
    utility call is not, because the thinking block consumes the whole budget
    before the tool call is emitted. Measured on the direct endpoint at
    max_tokens=512 with a forced tool decision: 3 of 5 truncated, two of those
    with no tool_use block at all. The permission classifier behind
    `defaultMode: auto` is one of these calls, so the symptom is intermittent
    classifier errors. thinking={"type":"disabled"} clears it, and every
    endpoint here honours that spelling even though none honours its own.
  * Claude Code exposes model tiers but no per-tier effort knob, so the
    OpenRouter and Nous profiles map a `ds4-*` sentinel model name onto
    reasoning_effort.
  * OpenRouter needs a per-request zero-data-retention block.
  * The status line needs real pricing, which /__spend serves.

Run it with no arguments. It serves every profile in PROFILES whose directory
exists, and exits once none of them is in use.
"""
import http.server, json, os, re, subprocess, sys, threading, time, urllib.request, urllib.error

HOME = os.path.expanduser("~")
VERBOSE = os.environ.get("DS4_VERBOSE") == "1" or os.environ.get("DS4_DEBUG") == "1"

# Below this many max_tokens, turn thinking off. Claude Code's utility calls
# arrive with a few hundred tokens of budget; the main loop arrives at 32000.
# Nothing observed lands in between, so the exact value is not delicate.
NOTHINK_BELOW = int(os.environ.get("DS4_NOTHINK_BELOW", "8192"))

# Exit once no profile is in use for this long. 0 disables and runs forever.
IDLE_EXIT = int(os.environ.get("DS4_IDLE_EXIT", "900"))

# Cloudflare 403s the stdlib's default urllib User-Agent ("error code: 1010"),
# which hits Nous Portal and intermittently OpenRouter. A curl UA is proven to
# pass, and is harmless on the endpoints that never cared.
UA = os.environ.get("DS4_UA", "curl/8.4.0")

EFFORT = {"ds4-max": "max", "ds4-xhigh": "xhigh", "ds4-high": "high", "ds4-low": "low"}

# Transient upstream statuses to retry in the relay. A raw forward of any of
# these kills the whole claude -p process ("Execution error") and loses the
# worker's in-flight work; absorbing them here turns a blip into a success.
# 524 is Cloudflare's origin-timeout; 429/529 are rate limit / overload.
TRANSIENT_STATUS = {429, 502, 503, 524, 529}
RETRY_ATTEMPTS = 3
RETRY_BACKOFF = 1.5          # seconds, scaled by attempt number


def _is_anthropic_model(name):
    """True for a literal Anthropic model id that the sentinel system missed."""
    n = name.lower()
    return any(k in n for k in ("sonnet", "opus", "haiku", "claude-"))


def should_retry(payload):
    """True when a transient error on this request should be retried in-proxy.

    The main thread (ds4-xhigh) has its own 10x-backoff retry, so retrying here
    would double up. Subagent tiers (ds4-high/max/low via CLAUDE_CODE_SUBAGENT_MODEL)
    die with "Execution error" on a raw forward, so they need the proxy guard.
    """
    tier = payload.get("model") if isinstance(payload, dict) else None
    return isinstance(tier, str) and tier != "ds4-xhigh"


# The full set /ds4-effort accepts. The tier map above only names the four
# Claude Code tiers; the override file may name any of these.
EFFORT_LEVELS = ("max", "xhigh", "high", "medium", "low", "minimal", "none")

# ZDR endpoints whose context is smaller than the 1M the profile advertises. A
# long session that routes here overflows the endpoint rather than the declared
# window. Recheck /api/v1/models/{id}/endpoints -> context_length when the
# provider list changes.
LOW_CONTEXT = ["Io Net"]

WEEK = 7 * 86400
LEDGER_MIN_INTERVAL = 300      # don't sample spend more than once every 5 min
CREDITS_TTL = 60               # /__spend is hit on every status line render

PROFILES = {
    "direct": {
        "port": 31500,
        "dir": f"{HOME}/.claude-ds4",
        "upstream": "https://api.deepseek.com/anthropic",
        # DeepSeek's own endpoint takes real model names and ignores
        # reasoning_effort, so no sentinel rewriting here.
        "model": None,
        "zdr": False,
        "spend": False,
        "max_out": None,
        # Only this endpoint requires an assistant tool_use message to replay its
        # thinking block. Claude Code 2.x does replay it, so this is a guard
        # against a path that drops it, not a fix for an observed failure.
        "inject": True,
    },
    "openrouter": {
        "port": 31501,
        "dir": f"{HOME}/.claude-or-ds4",
        "upstream": "https://openrouter.ai/api",
        "model": "deepseek/deepseek-v4-flash-0731",
        "zdr": True,
        "spend": True,
        # Smallest max_completion_tokens in the ZDR pool (DeepInfra, Io Net).
        "max_out": 65536,
        "inject": False,
    },
    "nous": {
        "port": 31502,
        "dir": f"{HOME}/.claude-nous",
        "upstream": "https://inference-api.nousresearch.com",
        "model": "deepseek/deepseek-v4-flash-0731",
        # Nous 403s any provider block, zdr or otherwise.
        "zdr": False,
        "spend": True,
        "max_out": 65536,
        "inject": False,
    },
}

DISABLED = {"type": "disabled"}
PLACEHOLDER = {"type": "thinking", "thinking": "(elided)", "signature": "ds4-proxy"}

_last_seen = time.time()
_lock = threading.Lock()
_cache = {}                    # (name, kind) -> cached value
_effort_cache = {}             # path -> (mtime_ns, ctime_ns, size, ino, level|None)
_inflight = 0                  # relayed requests open; idle_watch must not exit while nonzero


# ── request rewriting ────────────────────────────────────────────────────────

def effort_override(cfg):
    """Per-profile effort pin from <profile>/effort-override, or None.

    One line, one of EFFORT_LEVELS; /ds4-effort is the writer. A file survives
    a proxy restart, and the stat-keyed cache keeps the read off the
    per-request path: a request is one stat plus a dict lookup unless the file
    changed. The key also carries the inode: the command writes via an atomic
    replace, which allocates a fresh inode even on filesystems whose clock
    tick is coarser than the gap between writes (ext2/ext3, FAT), where
    mtime/ctime alone would go stale. An absent file, or one holding anything
    outside EFFORT_LEVELS, reads as None (tier default) — OpenRouter accepts
    the parameter and DeepSeek drops unknown values without error, so an
    invalid level must fail here rather than vanish upstream.
    """
    path = os.path.join(cfg["dir"], "effort-override")
    try:
        st = os.stat(path)
    except OSError:
        return None
    with _lock:
        hit = _effort_cache.get(path)
        if hit and hit[:4] == (st.st_mtime_ns, st.st_ctime_ns, st.st_size, st.st_ino):
            return hit[4]
    level = None
    try:
        with open(path, encoding="utf-8") as fh:
            raw = fh.read().strip()
        if raw in EFFORT_LEVELS:
            level = raw
    except OSError:
        pass
    with _lock:
        _effort_cache[path] = (st.st_mtime_ns, st.st_ctime_ns, st.st_size, st.st_ino, level)
    return level

def inject_missing_thinking(payload):
    """Count of assistant tool_use messages given a placeholder thinking block.

    DeepSeek 400s on a history where one is missing, and does not validate the
    signature, so a placeholder is enough.
    """
    n = 0
    for m in payload.get("messages") or []:
        if not isinstance(m, dict) or m.get("role") != "assistant":
            continue
        blocks = m.get("content")
        if not isinstance(blocks, list):
            continue
        kinds = {b.get("type") for b in blocks if isinstance(b, dict)}
        if "tool_use" in kinds and "thinking" not in kinds:
            blocks.insert(0, dict(PLACEHOLDER))
            n += 1
    return n


def rewrite(payload, cfg):
    """Edit payload in place for one profile. Returns a log line, or None."""
    notes = []

    if cfg["model"]:
        tier = payload.get("model")
        effort = EFFORT.get(tier)
        if effort:
            payload["model"] = cfg["model"]
            payload["reasoning_effort"] = effort_override(cfg) or effort
            notes.append(f"{tier} -> {cfg['model']} effort={payload['reasoning_effort']}")
        elif isinstance(tier, str) and _is_anthropic_model(tier):
            # A literal Anthropic model (sonnet, claude-sonnet-4-5, opus, ...)
            # bypassed the sentinel system and would bill real Anthropic rates on
            # this profile's upstream. The /model picker can expose these via
            # gateway discovery; rewrite defensively so nothing leaks.
            payload["model"] = cfg["model"]
            notes.append(f"{tier} -> {cfg['model']} (literal Anthropic model)")

    # DS4_ZDR only ever disables ZDR, never enables it on a profile whose table
    # row does not support it (Nous 403s any provider block at all).
    if cfg["zdr"] and os.environ.get("DS4_ZDR", "1") != "0":
        prov = payload.get("provider")
        if not isinstance(prov, dict):
            prov = {}
        prov["zdr"] = True
        prov["data_collection"] = "deny"
        ignore = [p for p in prov.get("ignore", []) if p not in LOW_CONTEXT]
        prov["ignore"] = ignore + LOW_CONTEXT
        payload["provider"] = prov

    want = payload.get("max_tokens")
    if isinstance(want, int):
        if cfg["max_out"] and want > cfg["max_out"]:
            payload["max_tokens"] = cfg["max_out"]
            notes.append(f"clamped max_tokens {want} -> {cfg['max_out']}")
        # Decide thinking from the post-clamp value so a NOTHINK_BELOW raised
        # above max_out still disables thinking on a clamped request.
        if payload["max_tokens"] <= NOTHINK_BELOW:
            payload["thinking"] = dict(DISABLED)
            notes.append(f"max_tokens={payload['max_tokens']} -> thinking disabled")

    # With thinking off the endpoint stops asking for the block, so only repair
    # a history on requests that still have thinking on.
    if cfg["inject"] and payload.get("thinking") != DISABLED:
        n = inject_missing_thinking(payload)
        if n:
            notes.append(f"injected {n} missing thinking block(s)")

    return ", ".join(notes) or None


# ── spend reporting, for the status line ─────────────────────────────────────

def api_key(name, cfg):
    """Server-side key, read from the profile's settings.json.

    One process serves every profile, so the key must come from the profile's
    own file, never from a process-wide variable that any profile can reach.
    DS4_KEY_<NAME> is a per-profile override on top of that file.
    """
    try:
        with open(os.path.join(cfg["dir"], "settings.json")) as fh:
            k = json.load(fh)["env"].get("ANTHROPIC_AUTH_TOKEN")
            if k:
                return k
    except Exception:
        pass
    return os.environ.get(f"DS4_KEY_{name.upper()}", "")


def get_json(name, cfg, path, timeout=6):
    req = urllib.request.Request(cfg["upstream"].rstrip("/") + path)
    req.add_header("authorization", "Bearer " + api_key(name, cfg))
    req.add_header("user-agent", UA)
    with urllib.request.urlopen(req, timeout=timeout) as r:
        return json.load(r)


def pricing(name, cfg):
    """Per-token rates, cached forever.

    OpenRouter serves per-deployment data under /v1/models/{id}/endpoints, and
    the cheapest endpoint is where ZDR routing mostly lands, so it is the right
    estimate there. Nous has no such sub-resource and publishes the price on the
    model itself in /v1/models, already discounted. Same key names either way.
    """
    with _lock:
        if _cache.get((name, "pricing")):
            return _cache[(name, "pricing")]
    try:
        d = get_json(name, cfg, f"/v1/models/{cfg['model']}/endpoints")["data"]
        p = min(d["endpoints"], key=lambda e: float(e["pricing"]["prompt"]))["pricing"]
    except Exception:
        try:
            d = get_json(name, cfg, "/v1/models")["data"]
            p = next(m["pricing"] for m in d if m.get("id") == cfg["model"])
        except Exception:
            return None
    out = {k: float(p[k]) for k in ("prompt", "completion", "input_cache_read") if k in p}
    with _lock:
        _cache[(name, "pricing")] = out
    return out


def credits(name, cfg):
    """(total_credits, total_usage), cached. Absent on providers without it."""
    now = time.time()
    with _lock:
        entry = _cache.get((name, "credits"))
    if entry and now - entry[0] < CREDITS_TTL:
        return entry[1]
    try:
        d = get_json(name, cfg, "/v1/credits")["data"]
        val = (float(d["total_credits"]), float(d["total_usage"]))
    except Exception:
        return None
    with _lock:
        _cache[(name, "credits")] = (now, val)
    return val


def ledger_append(cfg, usage, now):
    """Sample lifetime usage so a rolling 7-day figure survives restarts."""
    ledger = os.path.join(cfg["dir"], "spend-ledger.jsonl")
    try:
        last = 0.0
        if os.path.exists(ledger):
            with open(ledger, "rb") as fh:
                fh.seek(0, 2)
                fh.seek(max(0, fh.tell() - 4096))
                tail = fh.read().splitlines()
            for raw in reversed(tail):
                try:
                    last = json.loads(raw)["t"]
                    break
                except Exception:
                    continue
        if now - last < LEDGER_MIN_INTERVAL:
            return
        with open(ledger, "a") as fh:
            fh.write(json.dumps({"t": now, "usage": usage}) + "\n")
    except OSError:
        pass


def week_spend(cfg, usage, now):
    """(spend_over_7d, is_partial). Partial = ledger is younger than a week."""
    try:
        rows = []
        with open(os.path.join(cfg["dir"], "spend-ledger.jsonl")) as fh:
            for raw in fh:
                try:
                    r = json.loads(raw)
                    rows.append((float(r["t"]), float(r["usage"])))
                except Exception:
                    continue
    except OSError:
        return None, True
    if not rows:
        return None, True
    rows.sort()
    base = [u for t, u in rows if t <= now - WEEK]
    if base:
        return max(0.0, usage - base[-1]), False
    return max(0.0, usage - rows[0][1]), True


def spend(name, cfg):
    now = time.time()
    out = {"model": cfg["model"], "zdr": cfg["zdr"]}
    p = pricing(name, cfg)
    if p:
        out["pricing"] = p
    c = credits(name, cfg)
    if c:
        total, usage = c
        out["remaining"] = total - usage
        out["usage"] = usage
        ledger_append(cfg, usage, now)
        wk, partial = week_spend(cfg, usage, now)
        if wk is not None:
            out["week"] = wk
            out["week_partial"] = partial
    return out


# ── lifecycle ────────────────────────────────────────────────────────────────

def sessions_live(cfg):
    """True if any registered session PID is alive. Clears tokens that are not.

    The directory is .ds4-sessions, never sessions: the latter is Claude Code's
    own state directory and nothing here may reap a file it did not create.
    """
    d = os.path.join(cfg["dir"], ".ds4-sessions")
    try:
        names = os.listdir(d)
    except OSError:
        return False
    live = False
    for n in names:
        try:
            pid = int(n)
        except ValueError:
            continue
        try:
            os.kill(pid, 0)
        except ProcessLookupError:
            try:
                os.unlink(os.path.join(d, n))
            except OSError:
                pass
            continue
        except OSError:
            pass          # PermissionError and friends still mean it exists
        live = True
    return live


def claude_running(cfg):
    """True if a live claude process is using this profile.

    The check that does not depend on the launcher: `ps -E` prints each
    process's environment and Claude Code is started with CLAUDE_CONFIG_DIR set,
    so a session is visible however it was launched. Session tokens stay as a
    second signal because `ps -E` is macOS spelling. A boundary match keeps a
    backup directory with this one as a prefix from pinning the proxy up.
    """
    try:
        out = subprocess.run(["ps", "-E", "-ax", "-o", "command="],
                             stdout=subprocess.PIPE, stderr=subprocess.DEVNULL,
                             timeout=10).stdout.decode("utf8", "replace")
    except Exception:
        return False
    return bool(re.search(re.escape("CLAUDE_CONFIG_DIR=" + cfg["dir"]) + r"(?=\s|$)", out))


def anything_in_use(served):
    return any(sessions_live(cfg) or claude_running(cfg) for cfg in served.values())


def idle_watch(served):
    # Poll no slower than half the timeout, so a short DS4_IDLE_EXIT is honoured
    # promptly instead of being rounded up to the poll interval.
    every = max(1, min(30, IDLE_EXIT // 2))
    while IDLE_EXIT > 0:
        time.sleep(every)
        with _lock:
            open_req = _inflight
        if time.time() - _last_seen < IDLE_EXIT or anything_in_use(served) or open_req:
            continue
        print(f"no profile in use and idle for {IDLE_EXIT}s, exiting", flush=True)
        os._exit(0)


# ── serving ──────────────────────────────────────────────────────────────────

def make_handler(name, cfg):
    class Handler(http.server.BaseHTTPRequestHandler):
        protocol_version = "HTTP/1.1"

        def log_message(self, *a):
            pass

        def _json(self, status, obj):
            msg = json.dumps(obj).encode()
            self.send_response(status)
            self.send_header("content-type", "application/json")
            self.send_header("content-length", str(len(msg)))
            self.send_header("connection", "close")
            self.end_headers()
            self.wfile.write(msg)

        def _touch(self):
            global _last_seen
            _last_seen = time.time()

        def _count(self, n):
            global _inflight
            with _lock:
                _inflight += n

        def do_GET(self):
            self._touch()
            self._count(1)
            try:
                if self.path != "/__spend" or not cfg["spend"]:
                    self._json(404, {"error": {"message": "not found"}})
                    return
                self._json(200, spend(name, cfg))
            finally:
                self._count(-1)

        def do_POST(self):
            self._touch()
            self._count(1)
            try:
                self._relay()
            finally:
                self._count(-1)

        def _relay(self):
            body = self.rfile.read(int(self.headers.get("content-length", 0)))
            try:
                payload = json.loads(body)
            except ValueError:
                payload = None

            if isinstance(payload, dict):
                note = rewrite(payload, cfg)
                body = json.dumps(payload).encode()
                if VERBOSE and note:
                    print(f"  [{name}] {note}", flush=True)

            req = urllib.request.Request(
                cfg["upstream"].rstrip("/") + self.path, data=body, method="POST")
            for k, v in self.headers.items():
                if k.lower() not in ("host", "content-length", "accept-encoding"):
                    req.add_header(k, v)
            req.add_header("user-agent", UA)   # Cloudflare-safe; overrides the client's
            req.add_header("content-length", str(len(body)))

            # Transient upstream errors (rate limit / overload / cloudflare
            # timeout) kill a claude -p subagent when forwarded raw: the worker
            # dies with "Execution error" and loses whatever it was about to
            # send. The main thread has its own 10x-backoff retry, so only
            # retry subagent-tier requests here (the model sentinel tells which
            # tier: subagents default to ds4-high via CLAUDE_CODE_SUBAGENT_MODEL,
            # the main loop to ds4-xhigh via ANTHROPIC_MODEL). Non-transient
            # statuses pass through unchanged.
            do_retry = should_retry(payload)
            up = None
            last_err = None
            for attempt in range(RETRY_ATTEMPTS if do_retry else 1):
                try:
                    up = urllib.request.urlopen(req)
                    break
                except urllib.error.HTTPError as e:
                    last_err = e
                    if e.code not in TRANSIENT_STATUS or attempt + 1 >= RETRY_ATTEMPTS:
                        break
                    if VERBOSE:
                        print(f"  [{name}] <- {e.code}, retrying {attempt + 1}/{RETRY_ATTEMPTS}",
                              flush=True)
                    time.sleep(RETRY_BACKOFF * (attempt + 1))
                except Exception as e:
                    last_err = e
                    break
            if up is None:
                if isinstance(last_err, urllib.error.HTTPError):
                    up = last_err      # exhausted retries; forward the transient error
                else:
                    msg = json.dumps(
                        {"error": {"message": f"proxy upstream failure: {last_err}"}}).encode()
                    self.send_response(502)
                    self.send_header("content-type", "application/json")
                    self.send_header("content-length", str(len(msg)))
                    self.send_header("connection", "close")
                    self.end_headers()
                    self.wfile.write(msg)
                    return

            if VERBOSE and up.status != 200:
                print(f"  [{name}] <- {up.status}", flush=True)

            self.send_response(up.status)
            for k, v in up.headers.items():
                if k.lower() not in ("transfer-encoding", "content-encoding", "connection"):
                    self.send_header(k, v)
            self.send_header("connection", "close")
            self.end_headers()
            try:
                while True:
                    chunk = up.read(8192)
                    if not chunk:
                        break
                    self.wfile.write(chunk)
                    self.wfile.flush()
            finally:
                up.close()

    return Handler


def serve(name, cfg):
    """Bind one listener. False on bind failure so the rest still get served."""
    port = int(os.environ.get(f"DS4_PORT_{name.upper()}", cfg["port"]))
    try:
        srv = http.server.ThreadingHTTPServer(("127.0.0.1", port), make_handler(name, cfg))
    except OSError as e:
        print(f"  {name:<11} :{port} FAILED to bind: {e}", file=sys.stderr, flush=True)
        return False
    print(f"  {name:<11} :{port} -> {cfg['upstream']}", flush=True)
    threading.Thread(target=srv.serve_forever, daemon=True).start()
    return True


def main():
    # An absent profile directory means that profile is not installed here.
    # Binding its port anyway would be a lie to anyone checking with nc.
    served = {n: c for n, c in PROFILES.items() if os.path.isdir(c["dir"])}
    if not served:
        raise SystemExit("no profile directories found; nothing to serve")

    print(f"ds4 proxy: no thinking at or below max_tokens={NOTHINK_BELOW}, "
          f"idle exit {IDLE_EXIT}s", flush=True)
    bound = [serve(name, cfg) for name, cfg in served.items()]
    if not any(bound):
        raise SystemExit("no profile bound; nothing to serve")

    threading.Thread(target=idle_watch, args=(served,), daemon=True).start()
    while True:
        time.sleep(3600)


if __name__ == "__main__":
    main()
