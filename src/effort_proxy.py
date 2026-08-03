#!/usr/bin/env python3
"""Map Claude Code model tiers to OpenRouter reasoning_effort values.

Also serves GET /__spend, which the statusline uses to price a session correctly.
Claude Code prices the ds4-* sentinels against Anthropic's table and overstates
DeepSeek cost by orders of magnitude.
"""
import http.server, json, os, threading, time, urllib.request, urllib.error

UPSTREAM = os.environ.get("DS4_UPSTREAM", "https://openrouter.ai/api")
REAL_MODEL = os.environ.get("DS4_MODEL", "deepseek/deepseek-v4-flash-0731")
PORT = int(os.environ.get("DS4_PROXY_PORT", "8799"))
VERBOSE = os.environ.get("DS4_VERBOSE") == "1" or os.environ.get("DS4_DEBUG") == "1"
# Route only to zero-data-retention endpoints. Set DS4_ZDR=0 to turn it off.
ZDR = os.environ.get("DS4_ZDR", "1") == "1"

# ZDR endpoints whose context is smaller than the 1M this profile advertises.
# settings.json tells Claude Code the window is 1048576; if a request lands here
# instead, a long session overflows the endpoint rather than the declared window.
# Recheck with /api/v1/models/{id}/endpoints -> context_length when providers change.
LOW_CONTEXT = ["Io Net"]

# Smallest max_completion_tokens in the ZDR pool (DeepInfra and Io Net both 65536).
# Clamp so a larger inherited CLAUDE_CODE_MAX_OUTPUT_TOKENS cannot exceed it.
MAX_OUT = int(os.environ.get("DS4_MAX_OUT", "65536"))

EFFORT = {
    "ds4-max": "max",
    "ds4-xhigh": "xhigh",
    "ds4-high": "high",
    "ds4-low": "low",
}

PROFILE = os.path.expanduser("~/.claude-or-ds4")
LEDGER = os.path.join(PROFILE, "spend-ledger.jsonl")
LEDGER_MIN_INTERVAL = 300      # don't append more than once every 5 minutes
CREDITS_TTL = 60               # /api/v1/credits is polled on every statusline render
WEEK = 7 * 86400


def _api_key():
    """Server-side key, needed for the account endpoints /__spend calls."""
    k = os.environ.get("OPENROUTER_API_KEY")
    if k:
        return k
    try:
        env = json.load(open(os.path.join(PROFILE, "settings.json")))["env"]
        return env.get("ANTHROPIC_AUTH_TOKEN") or ""
    except Exception:
        return ""


API_KEY = _api_key()
_cache = {"credits": (0.0, None), "pricing": None}
_lock = threading.Lock()


def _get_json(path, timeout=6):
    req = urllib.request.Request(UPSTREAM.rstrip("/") + path)
    req.add_header("authorization", "Bearer " + API_KEY)
    with urllib.request.urlopen(req, timeout=timeout) as r:
        return json.load(r)


def pricing():
    """Per-token rates. /api/v1/models reports the CHEAPEST endpoint, which under
    ZDR routing is where most traffic actually lands, so it is the right estimate
    here — but it is an estimate, not a bill."""
    with _lock:
        if _cache["pricing"]:
            return _cache["pricing"]
    try:
        d = _get_json(f"/v1/models/{REAL_MODEL}/endpoints")["data"]
        p = min(d["endpoints"], key=lambda e: float(e["pricing"]["prompt"]))["pricing"]
        out = {k: float(p[k]) for k in ("prompt", "completion", "input_cache_read") if k in p}
    except Exception:
        return None
    with _lock:
        _cache["pricing"] = out
    return out


def credits():
    """(total_credits, total_usage), cached — the statusline renders constantly."""
    now = time.time()
    with _lock:
        ts, val = _cache["credits"]
        if val is not None and now - ts < CREDITS_TTL:
            return val
    try:
        d = _get_json("/v1/credits")["data"]
        val = (float(d["total_credits"]), float(d["total_usage"]))
    except Exception:
        return None
    with _lock:
        _cache["credits"] = (now, val)
    return val


def ledger_append(usage, now):
    """Sample lifetime usage so a rolling 7-day figure survives restarts."""
    try:
        last = 0.0
        if os.path.exists(LEDGER):
            with open(LEDGER, "rb") as fh:
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
        with open(LEDGER, "a") as fh:
            fh.write(json.dumps({"t": now, "usage": usage}) + "\n")
    except OSError:
        pass


def week_spend(usage, now):
    """(spend_over_7d, is_partial). Partial = ledger is younger than a week."""
    try:
        rows = []
        with open(LEDGER) as fh:
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
    cutoff = now - WEEK
    base = [u for t, u in rows if t <= cutoff]
    if base:
        return max(0.0, usage - base[-1]), False
    return max(0.0, usage - rows[0][1]), True


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

    def do_GET(self):
        if self.path != "/__spend":
            self._json(404, {"error": {"message": "not found"}})
            return
        now = time.time()
        out = {"model": REAL_MODEL, "zdr": ZDR}
        p = pricing()
        if p:
            out["pricing"] = p
        c = credits()
        if c:
            total, usage = c
            out["remaining"] = total - usage
            out["usage"] = usage
            ledger_append(usage, now)
            wk, partial = week_spend(usage, now)
            if wk is not None:
                out["week"] = wk
                out["week_partial"] = partial
        self._json(200, out)

    def do_POST(self):
        body = self.rfile.read(int(self.headers.get("content-length", 0)))
        try:
            payload = json.loads(body)
        except ValueError:
            payload = None

        if isinstance(payload, dict):
            tier = payload.get("model")
            effort = EFFORT.get(tier)
            if effort:
                payload["model"] = REAL_MODEL
                payload["reasoning_effort"] = effort
                if VERBOSE:
                    print(f"  {tier} -> {REAL_MODEL} reasoning_effort={effort}", flush=True)
            elif VERBOSE:
                print(f"  passthrough model={tier!r}", flush=True)
            if ZDR:
                prov = payload.get("provider")
                if not isinstance(prov, dict):
                    prov = {}
                prov["zdr"] = True
                prov["data_collection"] = "deny"
                ignore = [p for p in prov.get("ignore", []) if p not in LOW_CONTEXT]
                prov["ignore"] = ignore + LOW_CONTEXT
                payload["provider"] = prov

            want = payload.get("max_tokens")
            if isinstance(want, int) and want > MAX_OUT:
                payload["max_tokens"] = MAX_OUT
                if VERBOSE:
                    print(f"  clamped max_tokens {want} -> {MAX_OUT}", flush=True)

            if VERBOSE:
                print(f"  -> model={payload.get('model')} effort={payload.get('reasoning_effort')} "
                      f"max_tokens={payload.get('max_tokens')}", flush=True)
            body = json.dumps(payload).encode()

        req = urllib.request.Request(
            UPSTREAM.rstrip("/") + self.path,
            data=body,
            method="POST",
        )
        for k, v in self.headers.items():
            if k.lower() not in ("host", "content-length", "accept-encoding"):
                req.add_header(k, v)
        req.add_header("content-length", str(len(body)))

        try:
            up = urllib.request.urlopen(req)
        except urllib.error.HTTPError as e:
            up = e
        except Exception as e:
            self.send_response(502)
            msg = json.dumps({"error": {"message": f"proxy upstream failure: {e}"}}).encode()
            self.send_header("content-type", "application/json")
            self.send_header("content-length", str(len(msg)))
            self.send_header("connection", "close")
            self.end_headers()
            self.wfile.write(msg)
            return

        if VERBOSE:
            print(f"  <- {up.status}", flush=True)

        self.send_response(up.status)
        for k, v in up.headers.items():
            if k.lower() not in ("transfer-encoding", "content-encoding", "connection"):
                self.send_header(k, v)
        self.send_header("connection", "close")
        self.end_headers()
        while True:
            chunk = up.read(8192)
            if not chunk:
                break
            self.wfile.write(chunk)
            self.wfile.flush()
        up.close()


if __name__ == "__main__":
    print(f"ds4 effort proxy on 127.0.0.1:{PORT} -> {UPSTREAM} ({REAL_MODEL}) zdr={ZDR}", flush=True)
    http.server.ThreadingHTTPServer(("127.0.0.1", PORT), Handler).serve_forever()
