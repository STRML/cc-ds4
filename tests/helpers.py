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
        endpoint = (self.command, self.path)
        self.server.fake.requests.append(
            {"method": self.command, "path": self.path,
             "headers": dict(self.headers), "body": body})
        bucket = self.server.fake.requests_by_endpoint.setdefault(endpoint, [])
        bucket.append(self.server.fake.requests[-1])
        # A consecutive run against the same endpoint is a retry sequence.
        self.server.fake.retry_count[endpoint] = \
            self.server.fake.retry_count.get(endpoint, 0) + 1
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
        # Keyed by (method, path): the outbound requests per endpoint, in
        # arrival order, plus a running count of every hit. The differential
        # harness asserts on these so each proxy's outbound calls can be
        # compared, not just the bytes the client saw.
        self.requests_by_endpoint = {}
        self.retry_count = {}
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
        # Join the serve_forever thread so it does not linger past the fixture's
        # life (a leaked non-joined thread keeps the interpreter from exiting
        # cleanly in the harness, which boots many fakes per run).
        self._thread.join()

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
