#!/usr/bin/env python3
"""The differential harness: Python proxy vs the Go proxy, byte-for-byte.

The Python proxy is the frozen oracle. The harness boots it in-process (import
proxy, patch PROFILES upstreams to point at canned FakeUpstreams, call
serve()), fires the corpus at it, and records a reference response for every
case: (status, managed headers, body bytes) plus the upstream's record of the
outbound request. Then it boots the Go binary at the same upstreams, fires the
same corpus, and asserts the Go side reproduces the Python side exactly.

Raw body bytes are compared, not decoded JSON, so a divergence in whitespace,
key order, or escaping is caught. /__spend is status + JSON-shape only (its
body carries timing-dependent numbers and is not byte-comparable).

--python-only runs just the Python side: a self-test of the harness wiring.
It must PASS. Without it, the harness also tries to boot the Go binary, which
is not yet wired to accept fake upstreams (Task 9), so it fails gracefully
with a clear "Go not wired" message — the red light of the TDD loop.

Exit code 0 means the corpus passed; non-zero means a mismatch (or a boot
failure), with a report on stdout.
"""
import argparse
import json
import os
import shutil
import subprocess
import sys
import tempfile
import urllib.error
import urllib.request
from unittest import mock

# tests/diff -> tests -> src, so both `import proxy` and `import helpers` work
# no matter which directory the harness is launched from.
HERE = os.path.dirname(os.path.abspath(__file__))
sys.path.insert(0, os.path.dirname(HERE))                             # tests/
sys.path.insert(0, os.path.join(os.path.dirname(HERE), "..", "src"))  # src/

import proxy          # noqa: E402
import helpers        # noqa: E402
import corpus as C    # noqa: E402
import fake_upstream  # noqa: E402

REPO = os.path.dirname(os.path.dirname(HERE))    # repo root (parent of tests/)
GO_MAIN = os.path.join(REPO, "src", "go", "cmd", "ds4-proxy")
GO_BIN = os.path.join(REPO, "src", "go", "cmd", "ds4-proxy", "ds4-proxy")

REFERENCE_PROFILE = "nous"
FAILOVER_PROFILE = "direct"
CLIENT_TOKEN = "client-token"

# Proxy module constants that are bound at import time from DS4_* env (or
# hardcoded) — a boot that wants to change them must patch the module
# attribute, not the env: env is read at import, so an EnvGuard at serve()
# time is too late. HARNESS_DEFAULTS pins every such constant to a
# deterministic value so a host running a real ds4 config in the ambient env
# cannot leak it into the self-test; a scenario's __env__ overrides on top.
MODULE_KNOBS = {
    "FAILOVER_ENABLED": lambda s: s == "1",
    "FAILOVER_WINDOW": int,
    "FAILOVER_RATE": float,
    "FAILOVER_RECHECK": int,
    "FAILOVER_PROBE_TIMEOUT": int,
    "FAILOVER_PROBES_TO_CLOSE": int,
    "RETRY_ATTEMPTS": int,
    "RETRY_BACKOFF": float,
    "RELAY_TIMEOUT": int,
    "NOTHINK_BELOW": int,
    "IDLE_EXIT": int,
    "REQUIRE_OWNED_SOCKET": lambda s: s == "1",
}
HARNESS_DEFAULTS = {
    "FAILOVER_ENABLED": "1",
    "FAILOVER_WINDOW": "12",
    "FAILOVER_RATE": "0.25",
    "FAILOVER_RECHECK": "60",
    "FAILOVER_PROBE_TIMEOUT": "6",
    "FAILOVER_PROBES_TO_CLOSE": "1",
    "RETRY_ATTEMPTS": "3",
    "RETRY_BACKOFF": "1.5",
    "RELAY_TIMEOUT": "60",
    "NOTHINK_BELOW": "8192",
    "IDLE_EXIT": "900",
    "REQUIRE_OWNED_SOCKET": "0",
}

FAILURES = []


def _log(msg):
    sys.stdout.write(msg + "\n")
    sys.stdout.flush()


def _fmt_body(b):
    """Decode for display; body bytes may be non-UTF8, so degrade gracefully."""
    if isinstance(b, bytes):
        try:
            return b.decode("utf-8")
        except UnicodeDecodeError:
            return repr(b)
    return b


# ── upstream scenarios ───────────────────────────────────────────────────────

def _spend_ok(body):
    """A spend endpoint that answers without touching any ledger."""
    return 200, {"content-type": "application/json"}, json.dumps(
        {"model": "deepseek/deepseek-v4-flash-0731", "zdr": False,
         "pricing": {"prompt": 0.25, "completion": 1.0}}).encode()


def _upstreams(label):
    """Fresh FakeUpstreams and profile flags for one corpus case.

    The retry and failover cases are stateful: a shared fake would have its
    503 spent by whichever proxy ran first, so the harness builds fresh fakes
    per proxy per case. Returns (fakes, flags):
      fakes  — {profile_name: FakeUpstream} for this case
      flags  — per-profile cfg overrides the oracle and (later) the Go side
               must share (inject / spend / failover declaration)
    """
    fakes = []
    flags = {}

    def fake(routes):
        f = helpers.FakeUpstream(routes)
        fakes.append(f)
        return f

    if label == "retry-503":
        # One fake, 503-then-200: the proxy's own retry loop re-drives the
        # same URL, so attempts land here in arrival order. The outbound
        # retry_count is asserted, so a proxy that does not retry (1) or
        # retries too many (>2) is caught.
        flags["__env__"] = {"RETRY_BACKOFF": "0"}
        return {REFERENCE_PROFILE: fake(
            {("POST", "/v1/messages"): fake_upstream.retry_503_then_200()})}, flags

    if label == "failover":
        # The reference profile (nous) sits behind a flaky upstream and
        # declares a failover target. Its breaker opens after three 503s, so
        # the corpus fires it three times before the happy-path assertion.
        # In-proxy retries are off (RETRY_ATTEMPTS=1): a ds4-high 503 would
        # otherwise be retried twice per request, which both muddies the
        # strike accounting and costs 1.5s+3s of backoff per warm-up.
        flags[REFERENCE_PROFILE] = {"failover": FAILOVER_PROFILE}
        # window=3, rate=1.0 -> every transient error is a strike, three open
        # it. These are import-time-bound, so they ride MODULE_KNOBS, not env.
        flags["__env__"] = {"RETRY_ATTEMPTS": "1", "RETRY_BACKOFF": "0",
                            "FAILOVER_WINDOW": "3", "FAILOVER_RATE": "1.0",
                            "FAILOVER_RECHECK": "60",
                            "FAILOVER_PROBES_TO_CLOSE": "1"}
        flaky = fake({("POST", "/v1/messages"): fake_upstream.messages_503})
        direct = fake({("POST", "/v1/messages"): fake_upstream.sse_ok})
        return {REFERENCE_PROFILE: flaky, FAILOVER_PROFILE: direct}, flags

    if label == "thinking-inject":
        # The direct profile's inject=True repairs an assistant tool_use
        # history missing its thinking block. Exercise it so the reference
        # body actually contains the placeholder.
        flags[REFERENCE_PROFILE] = {"inject": True}
        return {REFERENCE_PROFILE: fake(
            {("POST", "/v1/messages"): fake_upstream.sse_ok})}, flags

    if label == "spend":
        flags[REFERENCE_PROFILE] = {"spend": True}
        return {REFERENCE_PROFILE: fake({("GET", "/__spend"): _spend_ok})}, flags

    # Default: the reference profile answers /v1/messages with the canned SSE
    # stream.
    return {REFERENCE_PROFILE: fake(
        {("POST", "/v1/messages"): fake_upstream.sse_ok})}, flags


# ── the Python oracle ────────────────────────────────────────────────────────

def _profile_cfg(name, tmp_dir, upstream, flags):
    """One profile cfg: a temp dir holding a key, pointed at an upstream.

    The real proxy.PROFILES table is the source of truth — the oracle must
    exercise the exact knobs (inject, max_out, spend, failover) the Go side's
    genprofiles generates from the same table, or a divergence would be a
    config artifact rather than a proxy bug. Only dir/upstream/port are
    swapped for the sandbox.
    """
    key = "ds4-direct-key" if name == FAILOVER_PROFILE else CLIENT_TOKEN
    with open(os.path.join(tmp_dir, "settings.json"), "w") as fh:
        json.dump({"env": {"ANTHROPIC_AUTH_TOKEN": key}}, fh)
    cfg = dict(proxy.PROFILES[name])
    cfg.update({"dir": tmp_dir, "upstream": upstream, "port": 0})
    cfg.update(flags.get(name, {}))
    return cfg


def _py_env():
    """A clean env for the oracle.

    DS4_PORT_* overrides would point a profile at a port that is not ours, and
    the classifier route must be ds4 (the reference upstream). Everything else
    that reads DS4_* at import time is pinned by HARNESS_DEFAULTS patched onto
    the module in _boot_python — env here is only what the running proxy reads
    at request time.
    """
    env = dict(os.environ)
    env["DS4_CLASSIFIER"] = "ds4"
    for k in list(env):
        if k.startswith("DS4_PORT_"):
            env.pop(k)
    return env


def _boot_python(fakes, flags):
    """Boot the Python proxy in-process against the fakes; return (server, stop).

    serve() sets require_client_auth=True, so the profile dir must hold a key
    and the client must present it. The classifier route is patched to ds4
    because CLASSIFIER_ROUTE is bound at import time — env at boot is too late.
    """
    tmp = tempfile.TemporaryDirectory()
    served = {}
    for name, fake in fakes.items():
        # One directory per profile: each writes its own settings.json, and the
        # failover target's key must not clobber the reference profile's.
        d = os.path.join(tmp.name, name)
        os.makedirs(d, exist_ok=True)
        served[name] = _profile_cfg(name, d, fake.url, flags)

    patcher = mock.patch.object(proxy, "PROFILES", served)
    patcher.start()
    route = mock.patch.object(proxy, "CLASSIFIER_ROUTE", "ds4")
    route.start()

    # Module constants are bound at import, so a scenario's env knob must be
    # applied to the module attribute itself — patching env at boot is too
    # late for them. Every knob is restored on stop. HARNESS_DEFAULTS pins the
    # constants first so ambient DS4_* cannot leak into the self-test, then
    # the case's __env__ overrides on top.
    knob_patches = []
    knob_values = dict(HARNESS_DEFAULTS)
    knob_values.update(flags.get("__env__", {}))
    for name, convert in MODULE_KNOBS.items():
        raw = knob_values.get(name)
        if raw is not None:
            old = getattr(proxy, name)
            setattr(proxy, name, convert(raw))
            knob_patches.append((name, old))

    # The breaker is process-global; clear it so one case's outcome cannot
    # leak into the next boot.
    proxy._failover.clear()

    # serve() binds cfg["port"]. Pin a concrete free port so the listener is
    # discoverable, and set it on the cfg (not DS4_PORT_* env) so the serve()
    # banner names the real port.
    port = _free_port()
    served[REFERENCE_PROFILE]["port"] = port
    env = _py_env()
    with helpers.EnvGuard(env):
        ok = proxy.serve(REFERENCE_PROFILE, served[REFERENCE_PROFILE])
    if not ok:
        for name, old in knob_patches:
            setattr(proxy, name, old)
        route.stop()
        patcher.stop()
        tmp.cleanup()
        raise RuntimeError("python proxy failed to bind")

    srv = _server_on_port(port)
    if srv is None:                                # pragma: no cover
        for name, old in knob_patches:
            setattr(proxy, name, old)
        route.stop()
        patcher.stop()
        tmp.cleanup()
        raise RuntimeError("could not locate python proxy listener")

    def stop():
        srv.server_close()
        proxy._failover.clear()
        for name, old in knob_patches:
            setattr(proxy, name, old)
        route.stop()
        patcher.stop()
        tmp.cleanup()

    return srv, stop


def _free_port():
    """A port that was free a moment ago (bind 0, close, hand back)."""
    import socket
    s = socket.socket()
    s.bind(("127.0.0.1", 0))
    port = s.getsockname()[1]
    s.close()
    return port


def _server_on_port(port):
    """The live ThreadingHTTPServer bound to port, or None."""
    import http.server
    import threading
    for t in threading.enumerate():
        target = getattr(t, "_target", None)
        if target is None:
            continue
        fn = getattr(target, "__self__", None)
        if not isinstance(fn, http.server.ThreadingHTTPServer):
            continue
        try:
            if fn.server_address[1] == port:
                return fn
        except (IndexError, OSError):
            continue
    return None


def _http(url, method, headers, body):
    """Fire one request, collapsing errors like test_proxy_http.py does."""
    req = urllib.request.Request(url, data=body, method=method, headers=headers)
    try:
        with urllib.request.urlopen(req, timeout=30) as r:
            return r.status, r.headers, r.read()
    except urllib.error.HTTPError as e:
        return e.code, e.headers, e.read()


def _managed_headers(raw, fakes):
    """The headers the harness compares, with x-ds4-upstream normalized.

    The upstream URL is ephemeral (a per-boot port), so it is mapped back to
    the profile name: "nous" for the reference upstream, "direct" for the
    failover target. That is the deterministic value both proxies must emit.
    """
    out = {}
    for k in C.MANAGED_HEADERS:
        v = raw.get(k)
        if v is not None:
            out[k] = v
    via = raw.get("x-ds4-upstream")
    if via:
        for name, fake in fakes.items():
            if via == fake.url:
                out["x-ds4-upstream"] = name
                break
        else:
            out["x-ds4-upstream"] = via
    return out


def _header(headers, key):
    """A header value, matched case-insensitively.

    http.client canonicalizes header names on the wire, so a client's
    lowercase 'authorization' may be recorded as 'Authorization'. The
    comparison must not care about the case the proxy happened to use.
    """
    if key in headers:
        return headers[key]
    for k, v in headers.items():
        if k.lower() == key.lower():
            return v
    return None


def _outbound(fakes):
    """Fold the upstreams' recorded requests into the compare shape.

    For each (method, path) the FakeUpstream records requests in arrival order
    plus a consecutive retry count. The harness compares method, path, auth
    header, body bytes, and retry_count — enough to catch a proxy that
    rewrote the body, sent the wrong key, hit the wrong path, or failed to
    retry. The flaky/direct pair (failover) is folded in arrival order across
    both fakes.
    """
    out = []
    for name, fake in fakes.items():
        for (method, path), reqs in fake.requests_by_endpoint.items():
            for r in reqs:
                out.append({"upstream": name, "method": method, "path": path,
                            "auth": _header(r["headers"], "authorization"),
                            "body": r["body"],
                            "retry_count": fake.retry_count.get((method, path))})
    return out


def _shape(body):
    """/__spend comparison: status + JSON shape, not byte parity."""
    try:
        obj = json.loads(body)
        return {"keys": sorted(obj.keys())}
    except Exception:
        return {"raw": _fmt_body(body)}


def _fire(case, fakes, flags):
    """Run one corpus case against a booted proxy. Returns a result dict.

    Stateful cases fire the warm-up requests the scenario needs (three 503s
    for failover), so the recorded result is the stable endpoint state both
    proxies must reproduce. The caller owns booting and closing the fakes.
    """
    label, method, path, headers, body = case
    srv, stop = _boot_python(fakes, flags)
    try:
        url = f"http://127.0.0.1:{srv.server_address[1]}"
        if label == "failover":
            for _ in range(3):
                _http(url + path, method, headers, body)
        status, raw, rbody = _http(url + path, method, headers, body)
    finally:
        stop()

    mh = _managed_headers(raw, fakes)
    if path == "/__spend":
        compare = ("spend", _shape(rbody))
    else:
        compare = ("full", status, mh, rbody)

    return {"label": label, "status": status, "headers": mh,
            "body": rbody, "compare": compare, "outbound": _outbound(fakes)}


# ── the Go side ──────────────────────────────────────────────────────────────
# Task 9 wires the Go binary to accept fake upstreams and actually serve the
# corpus. Until then this stays False and the harness reports the red light:
# a prebuilt binary that ignores the fakes would otherwise exit 0 doing
# nothing and read as a false GREEN.

GO_ACCEPTS_FAKE_UPSTREAMS = False


def _go_boot_error():
    """A clear reason the Go side cannot be compared, or None (it can)."""
    if not GO_ACCEPTS_FAKE_UPSTREAMS:
        return "the Go proxy is not yet wired to accept fake upstreams (Task 9)"
    if not os.path.exists(GO_MAIN):
        return f"no Go cmd dir at {GO_MAIN}"
    if not os.path.exists(GO_BIN):
        return f"Go binary not built at {GO_BIN} — run go build first"
    if not shutil.which("go"):
        return "no 'go' on PATH to run the binary"
    return None


def _run_go(fakes, flags):
    """Boot the Go binary at the fake upstreams; return a subprocess.

    The Go side mirrors the Python oracle's serving shape: the fake upstreams
    ride in via per-profile DS4_UPSTREAM_* overrides, and the client presents
    the profile key. Task 9 wires the actual flags/args; today this is
    unreachable because GO_ACCEPTS_FAKE_UPSTREAMS is False.
    """
    if not GO_ACCEPTS_FAKE_UPSTREAMS:              # pragma: no cover
        raise RuntimeError("Go not wired: GO_ACCEPTS_FAKE_UPSTREAMS is False")
    args = ["go", "run", "."]
    go_env = _py_env()
    for name, fake in fakes.items():
        go_env[f"DS4_UPSTREAM_{name.upper()}"] = fake.url
    return subprocess.Popen(args, cwd=GO_MAIN, env=go_env)


# ── comparison ───────────────────────────────────────────────────────────────

def _fire_go(label, fakes, flags):
    """Run one corpus case against the Go proxy. Same result shape as _fire.

    The Go binary is a subprocess: Task 9 defines how it advertises its port
    and how the harness learns it. Until GO_ACCEPTS_FAKE_UPSTREAMS is flipped,
    this raises so the diff loop reports a clear, per-case failure.
    """
    if not GO_ACCEPTS_FAKE_UPSTREAMS:              # pragma: no cover
        raise RuntimeError("Go not wired to accept fake upstreams (Task 9)")
    # task-9: boot _run_go, discover the listener port, fire the corpus at it,
    # and fold the fakes' recordings via _outbound(fakes). Mirrors _fire.
    raise NotImplementedError("Go comparison wired in Task 9")


def _compare_pair(py, go):
    """Assert one case's responses match. Returns list of mismatch strings."""
    label = py["label"]
    diffs = []

    if py["compare"][0] == "spend":
        if py["status"] != go["status"]:
            diffs.append(f"[{label}] /__spend status: python={py['status']} "
                         f"go={go['status']}")
        if py["compare"][1] != go["compare"][1]:
            diffs.append(f"[{label}] /__spend shape: python={py['compare'][1]} "
                         f"go={go['compare'][1]}")
        return diffs

    if py["status"] != go["status"]:
        diffs.append(f"[{label}] status: python={py['status']} go={go['status']}")
    if py["headers"] != go["headers"]:
        diffs.append(f"[{label}] managed headers: python={py['headers']} "
                     f"go={go['headers']}")
    if py["body"] != go["body"]:
        diffs.append(f"[{label}] body bytes: python={_fmt_body(py['body'])[:200]!r} "
                     f"go={_fmt_body(go['body'])[:200]!r}")
    if py["outbound"] != go["outbound"]:
        diffs.append(f"[{label}] outbound: python={py['outbound']} "
                     f"go={go['outbound']}")
    return diffs


# ── the runners ──────────────────────────────────────────────────────────────

def run_python_only():
    """Self-test: run the corpus against the Python oracle alone. Must PASS."""
    _log("== python-only self-test ==")
    bad = 0
    total = 0
    for case in C.cases():
        label = case[0]
        fakes, flags = _upstreams(label)
        try:
            res = _fire(case, fakes, flags)
        finally:
            for f in fakes.values():
                f.close()
        total += 1
        expect = 200 if label not in ("auth-missing",) else 401
        if res["status"] != expect:
            bad += 1
            FAILURES.append(f"python self-test: {label} expected {expect}, "
                            f"got {res['status']}")
            _log(f"  FAIL {label}: status {res['status']} (expected {expect})")
        else:
            _log(f"  ok   {label} ({res['status']})")
    _log(f"python-only: {total - bad}/{total} cases passed")
    return bad == 0


def run_diff():
    """Run the corpus against both proxies and compare. Exit 0 or report."""
    _log("== differential run ==")

    go_err = _go_boot_error()
    if go_err:
        _log(f"GO NOT WIRED: {go_err}")
        _log("(The Go binary is not yet wired to accept fake upstreams; Task 9 "
             "will make this green.)")
        FAILURES.append(f"Go not wired: {go_err}")

    for case in C.cases():
        label = case[0]
        _log(f"-- case {label} --")

        # Python oracle.
        fakes, flags = _upstreams(label)
        try:
            py = _fire(case, fakes, flags)
        finally:
            for f in fakes.values():
                f.close()
        _log(f"  py   {label}: {py['status']}")

        if go_err:
            continue

        # Go binary at the same upstreams. Unreachable until Task 9 flips
        # GO_ACCEPTS_FAKE_UPSTREAMS; _fire_go mirrors _fire's result shape so
        # the comparison below is the real one, not a placeholder.
        fakes, flags = _upstreams(label)
        try:
            go = _fire_go(label, fakes, flags)
        except Exception as e:
            FAILURES.append(f"[{label}] Go boot/fire failed: {e}")
            _log(f"  go   {label}: FAILED ({e})")
            continue
        finally:
            for f in fakes.values():
                f.close()
        diffs = _compare_pair(py, go)
        for d in diffs:
            FAILURES.append(d)
            _log(f"  MISMATCH {d}")
        if not diffs:
            _log(f"  go   {label}: {go['status']} (match)")

    _log("")
    if FAILURES:
        _log(f"DIFFERENTIAL RESULT: RED ({len(FAILURES)} failure(s))")
    else:
        _log("DIFFERENTIAL RESULT: GREEN")


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--python-only", action="store_true",
                        help="self-test the harness against the Python oracle only")
    args = parser.parse_args()

    if args.python_only:
        ok = run_python_only()
    else:
        run_diff()
        ok = not FAILURES

    if FAILURES:
        _log("\nfailures:")
        for f in FAILURES:
            _log(f"  - {f}")
    return 0 if ok else 1


if __name__ == "__main__":
    raise SystemExit(main())
