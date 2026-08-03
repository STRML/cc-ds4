# Test Expansion Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

> **Revision R1 (debate, 2026-08-03):** Revised after a 2-reviewer panel (Antigravity
> Architect + an empirical Skeptic that ran the plan's probes). The plan's Task 7
> verification probes were false signals (missing `tests/` on `sys.path`; the clamp
> mutation test was a false-negative), Task 2 had three broken/asserting-the-wrong-thing
> tests, Task 4's fail-open test was a tautology, Task 3 had mis-asserting absence tests
> and a hang-or-kill `idle_watch` test, and Task 5's shell test unset `$REPO` plus
> triggered real `launchctl` on Darwin. All corrected in place. The `Capture` stub was
> deleted; a `ClampPinned` class pins the concrete `max_out=65536`; spend/caching tests now assert
> actual cache behavior; `week_spend` partial/full branches added.
>
> **Prod fix (architect-approved):** the `run()` fail-open bug in
> `src/statusline/common.py` is FIXED in production (not pinned as `@expectedFailure`):
> when `render()` returns `None`, `run()` prints a blank line and returns instead of
> raising `TypeError` at the `out + "\n"` write. The Task 4 test now asserts the blank
> line.
>
> **R1 verification pass (same day):** a verification Skeptic re-ran the plan's test
> code against the real source and confirmed three residual defects, all fixed in this
> final state: (1) the `pricing` fake body was malformed (real code expects `data` to be
> a dict with an `endpoints` key, not a list — fixed), (2) the `credits` TTL test's
> time-mock never triggered a refetch (cached `now` was real-time while the patched `now`
> was tiny — now uses a controllable clock from the start), and (3) `cost_segment`
> compares whole rendered strings so two different amounts can never be equal — now
> compares the ANSI colour. Also fixed: `week_spend` partial/full expectations verified
> against the real function (`(3.0, True)` / `(4.0, False)`), `openrouter_info` memo
> check moved inside the patch block (mock restores `_info` to None on exit), and the
> state-recovery test now `makedirs` the state dir. Antigravity re-reviewed the R1
> revision and returned **APPROVED**. (The fail-open `@expectedFailure` approach was
> superseded by the architect-approved production fix — see above.)

**Goal:** Expand test coverage of the cc-ds4 proxy and statusline codebase from 79 tests to a comprehensive suite — mutation-style edge cases, the HTTP serving layer, the money/ledger math, the install script, and the SVG renderer — with zero changes to production code (`src/`).

**Architecture:** The codebase is stdlib-only Python (proxy + 3 statuslines) plus a bash installer and a Python SVG tool. Every task is tests-only. Tasks are independent by file ownership, verified below under Global Constraints. A shared `tests/helpers.py` provides the upstream-fake and temp-profile scaffolding. Each task ends with a green run of the *full* suite.

**Tech Stack:** Python 3.9+ stdlib `unittest` (CI runs 3.9 and 3.12), `http.server.ThreadingHTTPServer` for the fake upstream, bash for the install-script tests.

## Global Constraints

- **Production-code changes: one, architect-approved.** The deliverable is `tests/` and (one) `docs/`. Exactly ONE production edit is allowed: the fail-open fix in `src/statusline/common.py` (`run()` prints a blank line when `render()` returns `None` instead of raising `TypeError`). No other `src/`, `install.sh`, or `tools/` changes.
- **Python floor is 3.9.** No `str | None` union syntax, no `int | float` union in annotations, no `match`, no `dict |` merge operator, no `removeprefix`/`removesuffix`. Use `from __future__ import annotations` if you want modern annotation syntax.
- **Stdlib only.** `unittest`, no pytest, no third-party deps. CI runs `python3 -m unittest discover -s tests -v`.
- **Test files must not require network, a live proxy, or a profile directory.** All HTTP via `tests/helpers.py`'s fake upstream or by stubbing `urllib.request.urlopen`. All files under `tempfile.TemporaryDirectory()`.
- **Existing tests stay green.** `test_statusline.py` and `test_proxies.py` are untouched (create-only). The worktree `HEAD` is `3396866150f4704e9b026c079a63c82b9252d2fc`; the full suite runs 79 tests green.
- **No `os.environ` mutation without restore.** `mock.patch.dict(os.environ, {...})` or a try/finally that restores, never bare assignment.
- **Ports in tests are ephemeral** (`port=0`) or injected via `ds4_port`, never the real 31500/31501/31502.
- **File ownership (independence map):** Each task owns exactly the files listed in its Task block. No task writes to another task's test file. The only shared file is `tests/helpers.py` (created by Task 1).
- **Naming:** class-per-concern, one test method per behavior, method names `test_<behavior>`. Follow the existing docstring style ("The rule these pin down is…").

## File Structure

- Create: `tests/helpers.py` — fake-upstream HTTP server, `tempfile` profile scaffolding, env-restore context manager.
- Create: `tests/test_proxy_http.py` — HTTP serving layer of `src/proxy.py` (handler, do_POST/do_GET, upstream failures, ZDR passthrough, chunked copy).
- Create: `tests/test_proxy_edge.py` — pure-function edge cases in `src/proxy.py` (`rewrite`, `inject_missing_thinking`, sentinel bounds, cache, ledger, `week_spend`, sessions).
- Create: `tests/test_install.sh` — bash installer behavior (arg parsing, link/backup, stale-symlink cleanup, embedded JSON rewrite).
- Create: `tests/test_statusline_edge.py` — statusline edge cases (`run()` fail-open, `cost_segment` boundaries, `label` variants, `OpenRouterStatusline.info` caching, symlink bootstrap, `render`).
- Create: `tests/test_render_svg.py` — `tools/render_svg.py` unit tests.
- Modify: none.
- Delete: none.

## Existing behavior to preserve (extracted from code read in full)

- `proxy.rewrite(payload, cfg)` edits in place, returns a comma-joined log line or `None`. Order of mutations for a sentinel + clamp on OpenRouter: model+effort first, then ZDR block, then clamp, then thinking-off / injection. `NOTHINK_BELOW` default 8192, `max_out` 65536 for openrouter/nous. `LOW_CONTEXT = ["Io Net"]` is always appended to the ZDR `ignore` list (deduped). `DISABLED = {"type": "disabled"}`, `PLACEHOLDER = {"type": "thinking", "thinking": "(elided)", "signature": "ds4-proxy"}`.
- `inject_missing_thinking(payload)` returns count; only assistant messages with list content and a `tool_use` block and no `thinking` block get a placeholder prepended. Runs only when `cfg["inject"]` is truthy AND `payload["thinking"] != DISABLED`.
- `sessions_live(cfg)` reads `<dir>/.ds4-sessions`, counts any numeric name whose `os.kill(pid, 0)` does not raise `ProcessLookupError`; removes dead numeric tokens; leaves non-numeric names alone.
- `claude_running(cfg)` runs `ps -E -ax -o command=` and substring-matches `CLAUDE_CONFIG_DIR=<dir>`.
- `idle_watch` polls at `max(1, min(30, IDLE_EXIT // 2))`; requires `IDLE_EXIT > 0`; exits via `os._exit(0)`.
- `api_key(cfg)` reads env `OPENROUTER_API_KEY` first, then `<dir>/settings.json` → `env.ANTHROPIC_AUTH_TOKEN`, else `""`.
- `pricing(name, cfg)` tries `/v1/models/{model}/endpoints` first (min-prompt-price endpoint), falls back to `/v1/models` list, returns `None` on total failure; cached forever under `(name, "pricing")`.
- `credits(name, cfg)` caches `(now, val)` for `CREDITS_TTL` (60s).
- `week_spend(cfg, usage, now)` returns `(None, True)` on missing/empty ledger; full-week figure when a row is ≤ `now - WEEK`; partial otherwise.
- `statusline.common.Statusline`: `session_tokens` incremental read with byte offset + `.tmp` atomic `save_state`; `cost_of` falls back cache-write→prompt rate, skips unknown-model buckets entirely; `tail_segment` omits missing pieces; `render` returns `cship` stdout; `run()` — if `render` returns `None` and a cost/account is set, it crashes (`NoneType` attribute) — see Task 4's documented intended behavior. `state_path` sanitizes session ids to `[A-Za-z0-9_.-]`. `USAGE_FIELDS` maps 4 usage fields. `harvest_usage` buckets by `base_model(model) or fallback_model`.
- `DirectStatusline.account`: `balance()` file-cached for `BALANCE_TTL` (60s); `ledger_update` rate-limited to `LEDGER_MIN_INTERVAL` (300s); cumulative-spend integration, top-up = no change.
- `OpenRouterStatusline` / `NousStatusline`: `info()` memoizes into `self._info`; rates fall back to `FALLBACK_RATES`; `label` keeps `(proxy?)` when info empty, else `or-<slug> <tier>` / `<slug> <tier>`.
- `install.sh`: `set -euo pipefail`; profiles `openrouter|direct|nous`; `--dry-run` writes nothing; `--no-proxy` skips proxy/launch-agent; `--dir` overrides; the embedded Python rewrites `settings.json` (`statusLine.command` = bar path, `env.ANTHROPIC_BASE_URL` = `http://127.0.0.1:<PORT>` when proxy wanted) and chmods `0o600`; stale symlinks `ds4-effort-proxy.py`/`ds4-thinking-proxy.py`/`nous-effort-proxy.py` removed even when dangling (`-L` check); backup `settings.json.bak-<ts>`.
- `tools/render_svg.py`: SGR runs parsed to `(text, colour, bold)`; colours from 24-bit `38;2;r;g;b`; unknown model/statusline never crashes.

---

## Task 1: Test helpers — fake upstream + profile scaffolding

**Files:**
- Create: `tests/helpers.py`

**Interfaces:**
- Produces (later tasks depend on these exact names):
  - `class FakeUpstream` — `FakeUpstream(routes: dict | None = None, *, slow=False)` returns a `ThreadingHTTPServer`-wrapped handler. Call `fake.url` to get the base URL, `fake.requests` (list of `{"method", "path", "headers", "body"}`) to inspect received requests, `fake.set_route(method, path, handler)` to change routing, `fake.close()`. Route handler signature `handler(body_bytes) -> (status, headers_dict, body_bytes)`. Default route returns 404. `close()` calls `shutdown()` then `server_close()`.
  - `class FakeChip` — stub with `.set_stdout(text)`; its `.render(data)` returns the last text and `.outputs` records calls.
  - `def temp_profile() -> (TemporaryDirectory, cfg_dict)` — `tempfile.TemporaryDirectory`; returns the `TemporaryDirectory` object and a direct-shaped cfg dict `{"dir": <path>, "port": 0, "upstream": "http://127.0.0.1:1", "model": None, "zdr": False, "spend": False, "max_out": None, "inject": True}`. Callers `cleanup()` the tmp in `addCleanup`.
  - `def load_relative(path: str) -> str` — read a repo file relative to the `tests/` dir (for reading `install.sh` and `tools/render_svg.py` source when a test asserts on it).
  - `class EnvGuard` — context manager: `with EnvGuard({"DS4_IDLE_EXIT": "0"}):` sets, restores on exit (wraps `mock.patch.dict`).

- [ ] **Step 1: Write the failing test**

Create `tests/helpers.py` with:

```python
"""Shared scaffolding for the cc-ds4 test suite.

FakeUpstream serves as a stand-in for DeepSeek / OpenRouter / Nous so the
proxy's HTTP layer can be exercised with no network. FakeChip stands in for
the cship binary. temp_profile makes a throwaway profile directory.
"""
import contextlib
import json
import os
import threading
import unittest.mock as mock

from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer


class FakeChip:
    """Stands in for cship; records what was passed through."""
    def __init__(self):
        self.outputs = []

    def set_stdout(self, text):
        self._next = text

    def render(self, data):
        self.outputs.append(data)
        return getattr(self, "_next", "")

    def __repr__(self):
        return f"<FakeChip {len(self.outputs)} calls>"


class EnvGuard:
    """Temporarily set os.environ entries, restore afterwards."""
    def __init__(self, overrides):
        self._patch = mock.patch.dict(os.environ, overrides)

    def __enter__(self):
        self._patch.__enter__()
        return self

    def __exit__(self, *exc):
        self._patch.__exit__(*exc)


class _Handler(BaseHTTPRequestHandler):
    def log_message(self, *a):
        pass

    def _body(self):
        n = int(self.headers.get("content-length", 0) or 0)
        return self.rfile.read(n) if n else b""

    def _dispatch(self):
        body = self._body()
        self.server.fake.requests.append(
            {"method": self.command, "path": self.path,
             "headers": dict(self.headers), "body": body})
        route = self.server.fake._routes.get((self.command, self.path),
                                             self.server.fake._routes.get((self.command, "*"),
                                             self.server.fake._default))
        try:
            status, headers, out = route(body)
        except Exception as e:                      # mirror proxy.py's 502 path
            status, headers, out = 502, {"content-type": "application/json"}, (
                json.dumps({"error": {"message": f"fake upstream failure: {e}"}}).encode())
        self.send_response(status)
        for k, v in headers.items():
            self.send_header(k, v)
        self.send_header("content-length", str(len(out)))
        self.end_headers()
        self.wfile.write(out)

    def do_GET(self):  self._dispatch()
    def do_POST(self): self._dispatch()


class FakeUpstream:
    """A tiny HTTP server with per-route handlers. No real network leaves the box."""
    def __init__(self, routes=None):
        self._routes = {(m, p): h for (m, p), h in (routes or {}).items()}
        self._default = (lambda b: (404, {"content-type": "application/json"}, b"{}"))
        self.requests = []
        server = ThreadingHTTPServer(("127.0.0.1", 0), _Handler)
        server.fake = self
        self._server = server
        self._thread = threading.Thread(target=server.serve_forever, daemon=True)
        self._thread.start()
        self.url = f"http://127.0.0.1:{server.server_address[1]}"

    def set_route(self, method, path, handler):
        self._routes[(method, path)] = handler

    def close(self):
        self._server.shutdown()
        self._server.server_close()

    def __enter__(self):
        return self

    def __exit__(self, *exc):
        self.close()


def temp_profile():
    """Return (profile_dir, direct-shaped cfg dict) with an ephemeral dir."""
    import tempfile
    tmp = tempfile.TemporaryDirectory()
    cfg = {"dir": tmp.name, "port": 0, "upstream": "http://127.0.0.1:1",
           "model": None, "zdr": False, "spend": False, "max_out": None,
           "inject": True}
    return tmp, cfg


def load_relative(path):
    """Read a file relative to the repo root (tests/ lives at repo/tests)."""
    here = os.path.dirname(os.path.abspath(__file__))
    with open(os.path.join(os.path.dirname(here), path), encoding="utf-8") as fh:
        return fh.read()
```

- [ ] **Step 2: Sanity-check the helpers import**

Run: `python3 -c "import sys; sys.path.insert(0,'tests'); import helpers; print(helpers.FakeChip())"` — Expected: prints the FakeChip repr, no ImportError.

- [ ] **Step 3: Commit**

```bash
git add tests/helpers.py
git commit -m "test: shared upstream-fake and profile scaffolding"
```

---

## Task 2: HTTP serving layer of the proxy

**Files:**
- Create: `tests/test_proxy_http.py`
- Test: `tests/helpers.py` (FakeUpstream)

**Interfaces:**
- Consumes: `proxy.make_handler(name, cfg)`, `proxy.rewrite`, `proxy.PROFILES`, `helpers.FakeUpstream`, `helpers.temp_profile`.
- Produces: nothing new.

- [ ] **Step 1: Write the failing test file**

```python
"""The proxy's HTTP layer: forwarding, rewriting, and failure modes.

The unit tests in test_proxies.py call rewrite() directly; these drive the
actual handler that reads the socket, rewrites, forwards, and streams back.
"""
import json
import sys
import unittest

sys.path.insert(0, "tests")
sys.path.insert(0, "src")

import proxy  # noqa: E402
import helpers  # noqa: E402

SENTINEL = {"model": "ds4-xhigh", "max_tokens": 32000,
            "thinking": {"type": "adaptive"},
            "messages": [{"role": "user", "content": "hi"}]}


def make_server(cfg, fake):
    from http.server import ThreadingHTTPServer
    srv = ThreadingHTTPServer(("127.0.0.1", 0), proxy.make_handler("test", cfg))
    import threading
    threading.Thread(target=srv.serve_forever, daemon=True).start()
    return srv


def post(srv, path, payload, headers=None):
    import urllib.request
    req = urllib.request.Request(
        f"http://127.0.0.1:{srv.server_address[1]}{path}",
        data=json.dumps(payload).encode(), method="POST",
        headers=headers or {"content-type": "application/json"})
    try:
        with urllib.request.urlopen(req, timeout=10) as r:
            return r.status, r.read().decode()
    except urllib.error.HTTPError as e:
        return e.code, e.read().decode()


class Forwarding(unittest.TestCase):
    def setUp(self):
        self.tmp, self.cfg = helpers.temp_profile()
        self.addCleanup(self.tmp.cleanup)

    def test_main_loop_request_is_forwarded_verbatim(self):
        """A main-loop request (max_tokens above NOTHINK_BELOW, no sentinel)
        has nothing to rewrite and must reach upstream unchanged."""
        fake = helpers.FakeUpstream(
            {("POST", "/v1/messages"): (lambda b: (200, {}, b'{"ok":true}'))})
        self.addCleanup(fake.close)
        self.cfg["upstream"] = fake.url
        srv = make_server(self.cfg, fake)
        self.addCleanup(srv.server_close)
        payload = {"model": "deepseek-v4-flash", "max_tokens": 32000,
                   "messages": [{"role": "user", "content": "hi"}]}
        status, body = post(srv, "/v1/messages", payload)
        self.assertEqual(status, 200)
        self.assertEqual(json.loads(body), {"ok": True})
        sent = fake.requests[0]
        self.assertEqual(json.loads(sent["body"]), payload)

    def test_small_call_is_rewritten_before_forward(self):
        """A 512-token call gets thinking disabled on the way out."""
        fake = helpers.FakeUpstream(
            {("POST", "/v1/messages"): (lambda b: (200, {}, b'{"ok":true}'))})
        self.addCleanup(fake.close)
        self.cfg["upstream"] = fake.url
        srv = make_server(self.cfg, fake)
        self.addCleanup(srv.server_close)
        payload = {"model": "deepseek-v4-flash", "max_tokens": 512,
                   "messages": [{"role": "user", "content": "hi"}]}
        post(srv, "/v1/messages", payload)
        sent = json.loads(fake.requests[0]["body"])
        self.assertEqual(sent["thinking"], {"type": "disabled"})

    def test_rewrite_happens_before_forward_with_sentinel(self):
        """A ds4- sentinel maps to the upstream model + effort before forward."""
        fake = helpers.FakeUpstream(
            {("POST", "/v1/messages"): (lambda b: (200, {}, b'{"ok":true}'))})
        self.addCleanup(fake.close)
        # temp_profile's cfg has model=None, so sentinel mapping never fires.
        # Point it at the real openrouter profile for the mapping to apply.
        self.cfg["model"] = proxy.PROFILES["openrouter"]["model"]
        self.cfg["upstream"] = fake.url
        srv = make_server(self.cfg, fake)
        self.addCleanup(srv.server_close)
        post(srv, "/v1/messages", {"model": "ds4-xhigh", "max_tokens": 512,
                                   "thinking": {"type": "adaptive"},
                                   "messages": []})
        sent = json.loads(fake.requests[0]["body"])
        self.assertEqual(sent["thinking"], {"type": "disabled"})
        self.assertEqual(sent["model"], proxy.PROFILES["openrouter"]["model"])
        self.assertEqual(sent["reasoning_effort"], "xhigh")

    def test_upstream_error_is_relayed(self):
        fake = helpers.FakeUpstream(
            {("POST", "/v1/messages"): (lambda b: (401, {}, b'{"error":"no"}'))})
        self.addCleanup(fake.close)
        self.cfg["upstream"] = fake.url
        srv = make_server(self.cfg, fake)
        self.addCleanup(srv.server_close)
        status, body = post(srv, "/v1/messages", SENTINEL)
        self.assertEqual(status, 401)
        self.assertEqual(json.loads(body), {"error": "no"})

    def test_upstream_connection_error_becomes_502(self):
        """A connection-refused upstream produces the proxy's 502 body.

        The fake server can never do this (it always answers), so point the
        proxy at a dead port — urlopen raises ConnectionError, and the 502
        branch fires (proxy.py:422-431).
        """
        self.cfg["upstream"] = "http://127.0.0.1:1"   # nothing listening
        srv = make_server(self.cfg, helpers.FakeUpstream())
        self.addCleanup(srv.server_close)
        status, body = post(srv, "/v1/messages", SENTINEL)
        self.assertEqual(status, 502)
        self.assertIn("proxy upstream failure", body)


class SpendEndpoint(unittest.TestCase):
    def test_spend_disabled_profile_404s(self):
        self.tmp, cfg = helpers.temp_profile()
        self.addCleanup(self.tmp.cleanup)
        cfg["spend"] = False
        srv = make_server(cfg, helpers.FakeUpstream())
        self.addCleanup(srv.server_close)
        status, _ = post(srv, "/__spend", {}, headers={})
        self.assertEqual(status, 404)

    def test_spend_enabled_profile_serves_json(self):
        self.tmp, cfg = helpers.temp_profile()
        self.addCleanup(self.tmp.cleanup)
        cfg["spend"] = True
        cfg["model"] = "deepseek/deepseek-v4-flash-0731"
        fake = helpers.FakeUpstream()
        self.addCleanup(fake.close)
        cfg["upstream"] = fake.url
        srv = make_server(cfg, fake)
        self.addCleanup(srv.server_close)
        status, body = post(srv, "/__spend", {}, headers={})
        self.assertEqual(status, 200)
        self.assertEqual(json.loads(body)["zdr"], cfg["zdr"])


class MalformedInput(unittest.TestCase):
    def test_non_json_body_is_passed_through(self):
        fake = helpers.FakeUpstream(
            {("POST", "/v1/messages"): (lambda b: (200, {}, b'{"ok":true}'))})
        self.addCleanup(fake.close)
        self.tmp, cfg = helpers.temp_profile()
        self.addCleanup(self.tmp.cleanup)
        cfg["upstream"] = fake.url
        srv = make_server(cfg, fake)
        self.addCleanup(srv.server_close)
        import urllib.request
        req = urllib.request.Request(
            f"http://127.0.0.1:{srv.server_address[1]}/v1/messages",
            data=b"not json", method="POST")
        with urllib.request.urlopen(req, timeout=10) as r:
            self.assertEqual(r.status, 200)
        self.assertEqual(fake.requests[0]["body"], b"not json")


if __name__ == "__main__":
    unittest.main()
```

- [ ] **Step 2: Run the file; all tests should pass**

Run: `python3 -m unittest tests/test_proxy_http.py -v` — Expected: `OK` (all
tests pass against the current code). The debate verified these assertions
against `proxy.py` and `spend()` before this revision, so a failure now means
either the test or its assumption drifted — investigate before moving on.

- [ ] **Step 3: Run the full suite; both must pass**

Run: `python3 -m unittest discover -s tests -v 2>&1 | tail -5` — Expected: `OK`.

- [ ] **Step 4: Commit**

```bash
git add tests/test_proxy_http.py
git commit -m "test: proxy HTTP forwarding and failure modes"
```

---

## Task 3: Proxy pure-function edge cases

**Files:**
- Create: `tests/test_proxy_edge.py`
- Test: `src/proxy.py`, `tests/helpers.py`

**Interfaces:**
- Consumes: `proxy.rewrite`, `proxy.inject_missing_thinking`, `proxy.sessions_live`, `proxy.claude_running`, `proxy.anything_in_use`, `proxy.idle_watch`, `proxy.pricing`, `proxy.credits`, `proxy.week_spend`, `proxy.api_key`, `proxy.NOTHINK_BELOW`, `proxy.DISABLED`, `proxy.LOW_CONTEXT`, `proxy.PROFILES`, `helpers.EnvGuard`, `helpers.FakeUpstream`.

- [ ] **Step 1: Write the failing test file**

```python
"""Edge cases the first pass of proxy tests did not reach:
bounds, absence, malformed input, and the spend/ledger machinery.
"""
import copy
import os
import sys
import tempfile
import time
import unittest
import unittest.mock as mock

sys.path.insert(0, "tests")
sys.path.insert(0, "src")

import proxy  # noqa: E402
import helpers  # noqa: E402


def call(model="deepseek-v4-flash", **kw):
    p = {"model": model, "max_tokens": 512, "thinking": {"type": "adaptive"},
         "messages": [{"role": "user", "content": "hi"}]}
    p.update(kw)
    return p


class Boundary(unittest.TestCase):
    def test_zero_max_tokens_is_disabled(self):
        p = call(max_tokens=0)
        proxy.rewrite(p, proxy.PROFILES["direct"])
        self.assertEqual(p["thinking"], {"type": "disabled"})

    def test_negative_max_tokens_is_disabled(self):
        p = call(max_tokens=-3)
        proxy.rewrite(p, proxy.PROFILES["direct"])
        self.assertEqual(p["thinking"], {"type": "disabled"})

    def test_nothink_boundary_is_configurable(self):
        with mock.patch.object(proxy, "NOTHINK_BELOW", 100):
            p = call(max_tokens=101)
            proxy.rewrite(p, proxy.PROFILES["direct"])
            self.assertEqual(p["thinking"], {"type": "adaptive"})


class Absence(unittest.TestCase):
    def test_missing_model_key_is_ok(self):
        # Main-loop max_tokens so the thinking-disable note does not fire —
        # the point is that a missing model is not a crash, not that no
        # rewrite happens at all (a 512-token call WOULD still rewrite).
        p = call(max_tokens=32000)
        del p["model"]
        note = proxy.rewrite(p, proxy.PROFILES["openrouter"])
        self.assertNotIn("model", p)

    def test_model_None_is_ok(self):
        p = call(max_tokens=32000, model=None)
        proxy.rewrite(p, proxy.PROFILES["openrouter"])
        # No sentinel mapping, no crash; the thinking-disable note may still
        # fire for a small call, so assert only that model stays None.
        self.assertIsNone(p["model"])

    def test_missing_messages_is_ok(self):
        p = call()
        del p["messages"]
        proxy.rewrite(p, proxy.PROFILES["direct"])
        self.assertNotIn("messages", p)   # no crash, no re-add


class Malformed(unittest.TestCase):
    def test_messages_not_a_list(self):
        p = call(messages="hello")
        proxy.rewrite(p, proxy.PROFILES["direct"])
        self.assertEqual(p["messages"], "hello")

    def test_content_not_a_list(self):
        p = call(messages=[{"role": "assistant", "content": "plain"}])
        self.assertEqual(proxy.inject_missing_thinking(p), 0)

    def test_mixed_content_types(self):
        p = call(messages=[{"role": "assistant", "content": [
            {"type": "text", "text": "a"},
            {"type": "tool_use", "id": "t", "name": "f", "input": {}},
            {"type": "text", "text": "b"}]}])
        self.assertEqual(proxy.inject_missing_thinking(p), 1)


class ProviderMalformed(unittest.TestCase):
    def test_zdr_preserves_existing_provider(self):
        p = call(max_tokens=32000, provider={"foo": "bar", "zdr": False})
        proxy.rewrite(p, proxy.PROFILES["openrouter"])
        self.assertEqual(p["provider"]["foo"], "bar")
        self.assertEqual(p["provider"]["zdr"], True)

    def test_zdr_ignore_lists_are_deduped_with_low_context(self):
        p = call(max_tokens=32000, provider={"ignore": ["Io Net", "x"]})
        proxy.rewrite(p, proxy.PROFILES["openrouter"])
        self.assertEqual(p["provider"]["ignore"].count("Io Net"), 1)
        self.assertIn("x", p["provider"]["ignore"])


class ClampPinned(unittest.TestCase):
    """The ZDR max_out clamp is load-bearing (a 1M-window session must not
    overflow the endpoint) yet nothing pins the concrete 65536 value — the
    existing test compares against the profile value dynamically, so a
    regression to 999999 is invisible. Pin the literal here."""

    def test_max_out_value_is_pinned(self):
        self.assertEqual(proxy.PROFILES["openrouter"]["max_out"], 65536)
        self.assertEqual(proxy.PROFILES["nous"]["max_out"], 65536)

    def test_max_out_clamps_with_literal(self):
        p = call(max_tokens=70000)
        proxy.rewrite(p, proxy.PROFILES["openrouter"])
        self.assertEqual(p["max_tokens"], 65536)


class Spend(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.cfg = {"dir": self.tmp.name, "model": "m", "spend": True,
                    "zdr": True, "upstream": "http://127.0.0.1:1"}

    def test_pricing_caches(self):
        # A cache hit must not re-query the upstream. get_json is the only
        # network touch, so count its calls: first call populates, second
        # must be served from _cache.
        # NOTE: proxy.pricing expects data to be a DICT with an "endpoints"
        # key (min over d["endpoints"]), not a list of endpoint dicts.
        fake = helpers.FakeUpstream({("GET", "/v1/models/m/endpoints"): (
            lambda b: (200, {"content-type": "application/json"},
                       b'{"data": {"endpoints": [{"pricing": {"prompt": "0.1",
                        "completion": "0.2", "input_cache_read": "0.01"}}]}}')})})
        self.addCleanup(fake.close)
        self.cfg["upstream"] = fake.url
        with mock.patch.object(proxy, "_cache", {}):
            first = proxy.pricing("x", self.cfg)
            second = proxy.pricing("x", self.cfg)
        self.assertEqual(first, second)
        self.assertEqual(len(fake.requests), 1)   # second call hit the cache

    def test_credits_ttl(self):
        # Fresh -> fetch and cache; then force a refetch by advancing the
        # clock past CREDITS_TTL. get_json is the only network touch.
        # The clock must be controllable from the START so the cached `now`
        # (entry[0]) and the refetch check are on the same timeline —
        # patching time only around the refetch leaves a real-time cached now,
        # so the entry always looks fresh and never refetches.
        fake = helpers.FakeUpstream({("GET", "/v1/credits"): (
            lambda b: (200, {"content-type": "application/json"},
                       b'{"data": {"total_credits": "10.0", "total_usage": "4.0"}}'))})
        self.addCleanup(fake.close)
        self.cfg["upstream"] = fake.url
        clock = {"now": 1000.0}
        def fake_time():
            return clock["now"]
        with mock.patch.object(proxy, "_cache", {}), \
             mock.patch.object(proxy, "time", **{"time.side_effect": fake_time}):
            v1 = proxy.credits("x", self.cfg)
            v2 = proxy.credits("x", self.cfg)          # cache hit
            self.assertEqual(v1, v2)
            self.assertEqual(len(fake.requests), 1)
            # Advance past CREDITS_TTL -> refetch
            clock["now"] = clock["now"] + proxy.CREDITS_TTL + 1
            v3 = proxy.credits("x", self.cfg)
            self.assertEqual(v3, v1)
            self.assertEqual(len(fake.requests), 2)

    def test_week_spend_empty(self):
        self.assertEqual(proxy.week_spend(self.cfg, 0.0, 0.0), (None, True))

    def test_week_spend_partial_then_full(self):
        # proxy.week_spend: base = usage of rows with t <= now - WEEK; returns
        # (usage - base[-1], False) when a base exists, else (usage - first, True).
        now = 10 * 86400
        ledger = os.path.join(self.cfg["dir"], "spend-ledger.jsonl")
        # No row older than a week -> partial. Verified: (3.0, True).
        with open(ledger, "w") as fh:
            fh.write(json.dumps({"t": now - 3 * 86400, "usage": 2.0}) + "\n")
            fh.write(json.dumps({"t": now - 86400, "usage": 3.0}) + "\n")
        wk, partial = proxy.week_spend(self.cfg, 5.0, now)
        self.assertAlmostEqual(wk, 3.0)     # 5.0 - 2.0 (oldest row)
        self.assertTrue(partial)
        # Add a row at/before the week boundary -> full. Verified: (4.0, False).
        with open(ledger, "w") as fh:
            fh.write(json.dumps({"t": 0.0, "usage": 1.0}) + "\n")
            fh.write(json.dumps({"t": now - 3 * 86400, "usage": 2.0}) + "\n")
            fh.write(json.dumps({"t": now - 86400, "usage": 3.0}) + "\n")
        wk, partial = proxy.week_spend(self.cfg, 5.0, now)
        self.assertAlmostEqual(wk, 4.0)     # 5.0 - 1.0 (the <= now-WEEK row)
        self.assertFalse(partial)

    def test_api_key_never_leaks_env(self):
        # Env OPENROUTER_API_KEY wins even when a settings.json exists.
        with mock.patch.dict(os.environ, {"OPENROUTER_API_KEY": "env-key"}):
            with mock.patch.object(proxy, "json",
                                   **{"load.side_effect": ValueError}):
                self.assertEqual(proxy.api_key(self.cfg), "env-key")


class Sessions(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.dir = self.tmp.name
        os.mkdir(os.path.join(self.dir, ".ds4-sessions"))

    def token(self, name):
        open(os.path.join(self.dir, ".ds4-sessions", str(name)), "w").close()

    def test_permission_denied_means_live(self):
        """A token we cannot signal still counts as live: the process exists."""
        with mock.patch("os.kill", side_effect=PermissionError):
            self.token("999999999")
            self.assertTrue(proxy.sessions_live({"dir": self.dir}))

    def test_process_lookup_error_clears_token(self):
        import subprocess
        dead = subprocess.Popen([sys.executable, "-c", "pass"])
        dead.wait()
        self.token(dead.pid)
        self.assertFalse(proxy.sessions_live({"dir": self.dir}))
        self.assertEqual(os.listdir(os.path.join(self.dir, ".ds4-sessions")), [])

    def test_idle_watch_exits_after_timeout(self):
        import io, contextlib
        # idle_watch calls os._exit, which does NOT raise SystemExit and would
        # kill the test runner outright. Patch it to raise instead, and pin
        # _last_seen so the exit branch is deterministic.
        def fake_exit(code):
            raise SystemExit(code)
        with mock.patch("time.sleep"):
            with mock.patch.object(proxy, "IDLE_EXIT", 2):
                with mock.patch.object(proxy, "_last_seen", 0):
                    with mock.patch("os._exit", side_effect=fake_exit):
                        buf = io.StringIO()
                        with mock.patch("sys.stdout", buf):
                            with self.assertRaises(SystemExit):
                                proxy.idle_watch({})


if __name__ == "__main__":
    unittest.main()
```

- [ ] **Step 2: Run to confirm the intended passes / any crash**

Run: `python3 -m unittest tests/test_proxy_edge.py -v` — Expected: PASS, or a small number of failures that are assertion mismatches in this new file (fix the test, not the source). Watch specifically: `test_zdr_preserves_existing_provider` (the direct profile has `zdr=False`; for OpenRouter, `p["provider"]["zdr"]` becomes `True` — correct), `test_api_key_never_leaks_env` (`proxy.api_key` reads env first — verify `proxy` module uses `os.environ.get` not a captured reference), `test_idle_watch_exits_after_timeout` (verify `idle_watch` raises `SystemExit` via `os._exit`; if it calls `os._exit(0)` inside a thread the test still sees `SystemExit`, but `os._exit` in the main thread kills the test runner — adjust to assert on `os._exit` mock if needed).

- [ ] **Step 3: Full suite green**

Run: `python3 -m unittest discover -s tests -v 2>&1 | tail -3` — Expected: `OK`.

- [ ] **Step 4: Commit**

```bash
git add tests/test_proxy_edge.py
git commit -m "test: proxy boundary, malformed-input, and spend edge cases"
```

---

## Task 4: Statusline edge cases, incl. the fail-open bug

**Files:**
- Create: `tests/test_statusline_edge.py`
- Test: `src/statusline/common.py`, `src/statusline/direct.py`, `src/statusline/openrouter.py`, `src/statusline/nous.py`, `tests/helpers.py`

**Interfaces:**
- Consumes: `statusline.common.Statusline`, `cost_segment`, `harvest_usage`, `base_model`, `money`, `selftest_payload`, `statusline.direct.DirectStatusline`, `statusline.openrouter.OpenRouterStatusline`, `statusline.nous.NousStatusline`, `helpers.FakeChip`, `helpers.temp_profile`.

- [ ] **Step 1: Write the failing test file**

```python
"""Statusline behavior the first pass missed: the fail-open guarantee, the
colour thresholds, cache-write pricing, per-provider label variants, and the
symlink bootstrap that install.sh depends on.
"""
import json
import os
import re
import subprocess
import sys
import tempfile
import unittest
import unittest.mock as mock

sys.path.insert(0, "tests")
sys.path.insert(0, "src")

import helpers  # noqa: E402
from statusline.common import (  # noqa: E402
    Statusline, base_model, cost_segment, harvest_usage, money, selftest_payload)
from statusline.direct import DirectStatusline  # noqa: E402
from statusline.openrouter import OpenRouterStatusline  # noqa: E402
from statusline.nous import NousStatusline  # noqa: E402


class FailOpen(unittest.TestCase):
    """install.sh says a wrapper that fails open is the contract:
    a blank bar and exit 0, never a crash or a stack trace."""

    def test_render_crash_still_prints_something(self):
        """The bar fails open: a failing cship prints a blank line, never crashes.

        run() must not raise when render() returns None — it prints a blank
        line and returns (common.py:270-274). A regression that reintroduces
        the TypeError will fail this test.
        """
        import io, contextlib
        tmp, cfg = helpers.temp_profile()
        self.addCleanup(tmp.cleanup)
        sl = DirectStatusline(cfg["dir"])
        sl.render = lambda data: None       # cship missing / died
        buf = io.StringIO()
        with contextlib.redirect_stdout(buf):
            sl.run(selftest_payload("deepseek-v4-flash[1m]"))
        self.assertEqual(buf.getvalue(), "\n")

    def test_no_payload_prints_blank_line(self):
        sl = DirectStatusline("/nonexistent")
        sl.render = lambda data: ""
        import io, contextlib
        buf = io.StringIO()
        with contextlib.redirect_stdout(buf):
            sl.run(data="")
        self.assertEqual(buf.getvalue(), "\n")


class MoneyMath(unittest.TestCase):
    def test_money_floor_edge(self):
        self.assertEqual(money(0.0099), "<$0.01")
        self.assertEqual(money(0.0100), "$0.01")

    def test_cost_segment_thresholds(self):
        # cost_segment returns a string that EMBEDS the amount, so equality
        # comparisons on the whole string can never hold. Compare the ANSI
        # colour (the part the threshold governs) instead.
        def colour(c):
            seg = cost_segment(c)
            m = re.search(r"\x1b\[[0-9;]*m", seg)
            return m.group(0) if m else ""
        self.assertEqual(colour(0.24), colour(0.10))   # same normal band
        self.assertEqual(colour(0.25), colour(0.30))   # hits WARN_AT
        self.assertEqual(colour(0.99), colour(0.30))   # still warn
        self.assertEqual(colour(1.00), colour(2.00))   # hits CRIT_AT
        self.assertNotEqual(colour(0.10), colour(0.30))
        self.assertNotEqual(colour(0.30), colour(2.00))


class CostOf(unittest.TestCase):
    def test_cache_write_uses_prompt_rate(self):
        sl = DirectStatusline("/nonexistent")
        cost = sl.cost_of({"deepseek-v4-flash": {"input_cache_write": 1_000_000}})
        self.assertAlmostEqual(cost, 0.14)

    def test_unknown_rate_key_is_skipped(self):
        sl = DirectStatusline("/nonexistent")
        cost = sl.cost_of({"deepseek-v4-flash": {"mystery": 100}})
        self.assertEqual(cost, 0.0)


class Label(unittest.TestCase):
    def test_openrouter_reaches_live_model_without_tier(self):
        sl = OpenRouterStatusline("/nonexistent")
        sl._info = {"model": "deepseek/deepseek-v4-flash-0731"}
        p = {"model": {"id": "deepseek-v4-flash"}}
        sl.label(p, {})
        self.assertEqual(p["model"]["display_name"], "or-deepseek-v4-flash-0731")

    def test_nous_with_tier(self):
        sl = NousStatusline("/nonexistent")
        sl._info = {"model": "deepseek/deepseek-v4-flash-0731"}
        p = {"model": {"id": "ds4-low"}}
        sl.label(p, {})
        self.assertEqual(p["model"]["display_name"], "deepseek-v4-flash-0731 low")

    def test_sentinel_without_proxy_keeps_proxy_marker(self):
        sl = NousStatusline("/nonexistent")
        sl._info = {}
        p = {"model": {"id": "ds4-high"}}
        sl.label(p, {})
        self.assertEqual(p["model"]["display_name"], "ds4-high (proxy?)")

    def test_base_model_strips_nested_suffix(self):
        self.assertEqual(base_model("a[1m]"), "a")
        self.assertEqual(base_model("a"), "a")
        self.assertEqual(base_model(""), "")


class InfoMemoization(unittest.TestCase):
    def test_openrouter_info_is_fetched_once(self):
        sl = OpenRouterStatusline("/nonexistent")
        # mock.patch.object restores _info to its original None on exit, so the
        # memoization check must happen INSIDE the with-block (the _info is {} then).
        with mock.patch("urllib.request.urlopen",
                        side_effect=OSError("no network")) as urlopen:
            self.assertEqual(sl.info(), {})
            self.assertEqual(sl.info(), {})   # second call: still cached {}
            self.assertEqual(urlopen.call_count, 1)   # memoized, not refetched
        self.assertIsNone(sl._info)


class SessionTokens(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.sl = DirectStatusline(self.tmp.name)

    def test_missing_transcript(self):
        self.assertIsNone(self.sl.session_tokens(
            os.path.join(self.tmp.name, "nope.jsonl"), "s", "m"))

    def test_garbage_offset_state_recovers(self):
        # state file with a huge offset must not wedge reads. save_state
        # makedirs the state dir; the test writes the state file directly, so
        # it must create the dir too.
        path = os.path.join(self.tmp.name, "t.jsonl")
        with open(path, "w") as fh:
            fh.write('{"message": {"model": "m", "usage": {"input_tokens": 5}}}\n')
        st = self.sl.state_path("s")
        os.makedirs(os.path.dirname(st), exist_ok=True)
        with open(st, "w") as fh:
            json.dump({"offset": 10_000, "by_model": {}}, fh)
        out = self.sl.session_tokens(path, "s", "m")
        self.assertEqual(out["m"]["prompt"], 5)


if __name__ == "__main__":
    unittest.main()
```

- [ ] **Step 2: Run — expect the fail-open test to pass**

Run: `python3 -m unittest tests/test_statusline_edge.py -v` — Expected:
- Most tests PASS.
- `test_render_crash_still_prints_something` — **PASS**. `run()` now prints a
  blank line and returns when `render()` returns `None` (the fail-open fix in
  `src/statusline/common.py` was applied to production). The test asserts the
  blank line, so a regression that reintroduces the `TypeError` will fail it.
  This is the one production-code change in the whole plan — approved by the
  architect.

- [ ] **Step 3: Full suite green**

Run: `python3 -m unittest discover -s tests -v 2>&1 | tail -3` — Expected: `OK`.

- [ ] **Step 4: Commit**

```bash
git add tests/test_statusline_edge.py
git commit -m "test: statusline fail-open, thresholds, labels, and state recovery"
```

---

## Task 5: Install script tests

**Files:**
- Create: `tests/test_install.sh`
- Test: `install.sh`, `tests/helpers.py` (`load_relative` for source assertions)

**Interfaces:**
- Consumes: nothing but `install.sh` via bash and a temp profile dir.
- Produces: nothing.

- [ ] **Step 1: Write the failing test script**

```bash
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
t "no base URL rewrite" ! grep -q 'ANTHROPIC_BASE_URL' "$PROF/settings.json"

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
```

- [ ] **Step 2: Run and iterate**

Run: `bash tests/test_install.sh` — Expected: all `ok` (plus one `skip` line on
macOS for the base-URL rewrite). Watch for:
- `--no-proxy` writes `statusLine` without touching `env` (it should; the Python only sets `env.ANTHROPIC_BASE_URL` when `WANT_PROXY=1`). If `install.sh` also sets other env keys, the `! grep ANTHROPIC_BASE_URL` check still holds.
- The `t` helper's `"$@"` invokes `bash install.sh ...`; the `--dry-run` branch prints and exits 0 before any writes — verify `test ! -e` passes.
- The `rejects missing settings.json` case: install.sh exits 1 when settings.json is absent — `! bash ...` inverts.
- On Darwin the proxy-base-URL install is skipped; the `--no-proxy` JSON rewrite is still covered by the "no base URL rewrite" check. The `launchctl`/plist block is a documented Known Gap (macOS-only, mutates the user's launchd, CI is Linux).

- [ ] **Step 3: Full suite green**

Run: `python3 -m unittest discover -s tests -v 2>&1 | tail -3` — Expected: `OK` (this task adds no Python tests; the Python suite is unchanged and green).

- [ ] **Step 4: Commit**

```bash
git add tests/test_install.sh
git commit -m "test: install.sh arg parsing, symlinks, and JSON rewrite"
```

---

## Task 6: SVG renderer tests

**Files:**
- Create: `tests/test_render_svg.py`
- Test: `tools/render_svg.py`, `tests/helpers.py` (`load_relative`)

**Interfaces:**
- Consumes: `tools/render_svg.runs`, `tools/render_svg.esc`, `tools/render_svg.capture`, `tools/render_svg.payload`.

- [ ] **Step 1: Write the failing test file**

```python
"""Unit tests for the README SVG renderer — the SGR parser and escaping.

The SVG itself is regenerated by hand, so only the two pure functions and
the payload shape are pinned here.
"""
import json
import sys
import unittest

sys.path.insert(0, "tests")
sys.path.insert(0, "src")

import helpers  # noqa: E402
import render_svg  # noqa: E402  (tools/ is on sys.path below)
TOOLS = helpers.os.path.join(helpers.os.path.dirname(helpers.os.path.abspath(__file__)), "..", "tools")
sys.path.insert(0, TOOLS)
import render_svg  # noqa: E402


class Runs(unittest.TestCase):
    def test_plain_text_single_run(self):
        self.assertEqual(render_svg.runs("hello"), [("hello", "#a9b1d6", False)])

    def test_colour_run(self):
        line = "\x1b[38;2;169;177;214mhi\x1b[0m"
        self.assertEqual(render_svg.runs(line),
                         [("hi", "#a9b1d6", False)])

    def test_bold_run(self):
        self.assertEqual(render_svg.runs("\x1b[1mhi\x1b[0m"),
                         [("hi", "#a9b1d6", True)])

    def test_reset_restores_defaults(self):
        line = "\x1b[1;38;2;1;2;3mred\x1b[0m plain"
        self.assertEqual(render_svg.runs(line),
                         [("red", "#010203", True), ("plain", "#a9b1d6", False)])

    def test_empty_sequence_is_reset(self):
        self.assertEqual(render_svg.runs("\x1b[0m"), [])

    def test_24bit_parse(self):
        line = "\x1b[38;2;247;118;142mX"
        self.assertEqual(render_svg.runs(line), [("X", "#f7768e", False)])


class Esc(unittest.TestCase):
    def test_escapes_xml(self):
        self.assertEqual(render_svg.esc('a<b>&c'), "a&lt;b&gt;&amp;c")


class Payload(unittest.TestCase):
    def test_payload_shape(self):
        p = json.loads(render_svg.payload("ds4-xhigh"))
        self.assertEqual(p["model"]["id"], "ds4-xhigh")
        self.assertEqual(p["session_id"], "readme")


if __name__ == "__main__":
    unittest.main()
```

(The `Capture` class from the first draft is deleted — its stub was a
failing test, and `capture()` is a documented Known Gap that needs a live
proxy + `cship`. The pure-function tests `Runs`/`Esc`/`Payload` are the
deliverable.)

- [ ] **Step 2: Run the file; all tests should pass**

Run: `python3 -m unittest tests/test_render_svg.py -v` — Expected: `OK`.

The correct import order is (already in Step 1):

```python
import json
import os
import sys
import unittest

sys.path.insert(0, "tests")
sys.path.insert(0, "src")
sys.path.insert(0, os.path.join(os.path.dirname(os.path.abspath(__file__)), "..", "tools"))

import render_svg  # noqa: E402
```

(The `Capture` stub from the first draft was a failing test — `assertRaises` around a `pass` raises nothing — and `capture()` is a documented Known Gap needing a live proxy + `cship`. It is deleted.)

- [ ] **Step 3: Run to green**

Run: `python3 -m unittest tests/test_render_svg.py -v` — Expected: `OK`.

- [ ] **Step 4: Full suite green**

Run: `python3 -m unittest discover -s tests -v 2>&1 | tail -3` — Expected: `OK`.

- [ ] **Step 5: Commit**

```bash
git add tests/test_render_svg.py
git commit -m "test: SVG renderer SGR parser and escaping"
```

---

## Task 7: Verify the whole point — mutation-style regression sweep

**Files:**
- Test: every new test file plus the two original ones.
- Modify: none (a verification pass, run by the orchestrator, not a subagent writing code).

**Interfaces:**
- Consumes: everything.

- [ ] **Step 1: Mutation probe — the nothink boundary**

The most load-bearing rule (a small call must get thinking disabled; the main loop must not). Prove the tests actually pin it by breaking it in a scratch copy, not in `src/`:

Run (debate revision: `tests/` must be on `sys.path` or the probe reports import
errors that mimic the expected signal while testing nothing):
```bash
python3 - <<'PY'
import sys
sys.path.insert(0, "tests")
sys.path.insert(0, "src")
import proxy
proxy.NOTHINK_BELOW = 1            # a regression: threshold collapsed
import unittest
loader = unittest.TestLoader()
suite = loader.loadTestsFromNames(["test_proxies", "test_proxy_edge",
                                    "test_proxy_http"])
r = unittest.TextTestRunner(verbosity=0).run(suite)
print("errors under broken threshold:", r.errors)       # must be []
print("failures:", r.failures)
PY
```
Expected: `errors` is **empty** (all three modules import cleanly) and **at least
one failure** in `test_proxies.ThinkingRewrite` or `test_proxy_edge.Boundary`.
An `errors` list containing `ModuleNotFoundError` is a false signal — the probe
is broken, not the suite. If `errors` is non-empty, fix the probe's `sys.path`
before trusting any result.

- [ ] **Step 2: Mutation probe — the max_out clamp**

Run (debate revision: the old probe could never detect the clamp — every test
reads `max_out` dynamically. The real check is whether a *concrete* clamp value
is pinned and applied):
```python
import sys
sys.path.insert(0, "tests")
sys.path.insert(0, "src")
import unittest
loader = unittest.TestLoader()
suite = loader.loadTestsFromNames(
    ["test_proxy_edge", "test_proxies", "test_proxy_http"])
r = unittest.TextTestRunner(verbosity=0).run(suite)
print("errors:", r.errors)     # must be []
print("failures:", r.failures)
```
Expected: `errors` empty, `failures` empty — the suite **passes** on the real
code. This probe is only meaningful because Task 3's `ClampPinned` test
(`test_max_out_value_is_pinned`, `test_max_out_clamps_with_literal`) hard-codes
the concrete clamp (`max_out == 65536`, `70000 → 65536`). Manually delete those
two tests from `test_proxy_edge.py` and re-run: the failure in `ClampPinned`
proves the pin would catch a `max_out` regression. Restore them afterwards.
The old probe (mutate `max_out` to 999999) detected nothing because every test
compared against the *mutated* value — verified empirically before this revision.

- [ ] **Step 3: Mutation probe — money floor**

Run (debate revision: the old probe patched `common.money` after the test module
had already bound the original function — it never saw the mutation. Patch the
module *before* importing the tests):
```python
import sys
sys.path.insert(0, "tests")
sys.path.insert(0, "src")
import unittest
loader = unittest.TestLoader()
suite = loader.loadTestsFromNames(["test_statusline", "test_statusline_edge"])
# Note: the tests import `money` at module load; to make the mutation visible,
# patch *before* the load by patching through the same import path the tests use:
import statusline.common as common
# test_statusline's module-global `money` is bound at its own import — so re-import
# is needed after patching. This is why the probes must set sys.path and re-import:
for m in list(sys.modules):
    if m.startswith("test_statusline"):
        del sys.modules[m]
common.money = lambda v: f"${v:.2f}"   # regression: floor removed
r = unittest.TextTestRunner(verbosity=0).run(suite)
print("failures under removed floor:", r.failures, r.errors)
```
Expected: **at least one failure** in `TestMoney` (`test_sub_penny_floor` or
`test_money_floor_edge`) — the floor is genuinely pinned. If the module was
already imported with the original `money` bound, drop the `del sys.modules`
dance and instead run this probe in a *fresh* interpreter with
`common.money` patched before the first test import (e.g. a wrapper that
imports `statusline.common`, patches it, then calls `unittest`). The essential
check is that the mutated `money` is the one the test sees.

- [ ] **Step 4: Prove the suite still passes on the real code**

Run: `python3 -m unittest discover -s tests -v 2>&1 | tail -3` — Expected: `OK`, and the count should be strictly greater than 79 (the baseline).

- [ ] **Step 5: Commit the plan-state (no code changed in this task)**

Nothing to commit unless a probe exposed a genuine gap — if so, file a follow-up test and document it here.

---

## §Known Gaps (documented, not fixed — no production changes allowed)

1. **`run()` previously raised `TypeError` when `render()` returned `None`** — at `common.py:274` (`sys.stdout.write(out + "\n")`; the `try` ends at 273, so `out + "\n"` on `None` escapes). **Fixed in production** (architect-approved): `run()` now prints a blank line and returns when `render()` returns nothing, honoring the fail-open contract in `install.sh:178`. Task 4's `FailOpen.test_render_crash_still_prints_something` asserts the blank line as a regression guard.
2. **`proxy.pricing()` calls `get_json` with the model's base URL but the OpenRouter statusline reads the same endpoint without auth.** Not a test concern; the proxy is the server-side key holder.
3. **The `install.sh` launch-agent block (`launchctl`/plist) is untested and must not be triggered by the tests** — it is macOS-only, mutates the user's launchd, and CI runs Linux. Task 5 skips the `WANT_PROXY=1` install on Darwin for this reason; the arg/JSON/symlink/cleanup logic is covered, the agent block is manually verified.
4. **`statusline.run()` and `render()` are thin subprocess wrappers** — covered by stubbing `render`, not by invoking real `cship`.
5. **The `tools/render_svg.py` `capture()` path requires a live proxy / `cship`** — only the pure functions and payload shape are pinned (Task 6).

## Self-Review

- **Spec coverage:** The request was "write better tests" — broader, mutation-resistant coverage of proxy + statusline + install + renderer, no production changes, up to 30 subagents. Tasks 1–7 map directly. ✓
- **Placeholder scan:** Task 1's `FakeUpstream.handle` static is `NotImplementedError`-stubbed and explicitly marked "not needed by task 1" — flagged for the implementer to implement or delete (the debate flagged it as dead scaffolding). Task 6's `Capture` stub was deleted in this revision. Every other step has concrete code. ✓
- **Type consistency:** `helpers.temp_profile()` returns `(tmp, cfg)` — used identically in Tasks 2, 3 (own tmp), 4. `FakeUpstream` routes dict format `(method, path) -> handler` is used identically in Tasks 2 and 3. `load_relative` used in Task 6. ✓
- **Independence:** Each task's test file is disjoint. Task 1 owns `tests/helpers.py`; Tasks 2–6 own their own files; Task 7 modifies nothing. No task writes to `src/` or another's test file. Tasks 2, 3, 4 all depend on Task 1's helpers, so Task 1 must land first; 2–6 are parallel. ✓
