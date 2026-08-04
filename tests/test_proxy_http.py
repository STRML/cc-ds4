"""The proxy's HTTP layer: forwarding, rewriting, and failure modes.

The unit tests in test_proxies.py call rewrite() directly; these drive the
actual handler that reads the socket, rewrites, forwards, and streams back.
"""
import json
import os
import shutil
import sys
import tempfile
import unittest
from unittest import mock

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


def raw_post(srv, payload):
    """POST and return (status, headers, body) without collapsing errors."""
    import urllib.request
    req = urllib.request.Request(
        f"http://127.0.0.1:{srv.server_address[1]}/v1/messages",
        data=json.dumps(payload).encode(), method="POST",
        headers={"content-type": "application/json"})
    try:
        with urllib.request.urlopen(req, timeout=10) as r:
            return r.status, r.headers, r.read().decode()
    except urllib.error.HTTPError as e:
        return e.code, e.headers, e.read().decode()


def get(srv, path, headers=None):
    """GET a path on the proxy. The /__spend endpoint is served by do_GET."""
    import urllib.request
    req = urllib.request.Request(
        f"http://127.0.0.1:{srv.server_address[1]}{path}",
        headers=headers or {})
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
    """The /__spend status-line endpoint is a GET, served by do_GET."""

    def test_spend_disabled_profile_404s(self):
        self.tmp, cfg = helpers.temp_profile()
        self.addCleanup(self.tmp.cleanup)
        cfg["spend"] = False
        srv = make_server(cfg, helpers.FakeUpstream())
        self.addCleanup(srv.server_close)
        status, _ = get(srv, "/__spend", headers={})
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
        status, body = get(srv, "/__spend", headers={})
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


class VisionRoutingTest(unittest.TestCase):
    def setUp(self):
        self.tmp, self.cfg = helpers.temp_profile()   # no vision key needed (global knob)
        self.addCleanup(self.tmp.cleanup)
        # FakeUpstream defaults to 404; give it a /v1/messages route -> 200.
        self.fake = helpers.FakeUpstream(
            {("POST", "/v1/messages"): (lambda b: (200, {}, b'{"ok":true}'))})
        self.addCleanup(self.fake.close)

    def _img(self):
        return {"model": "deepseek-v4-flash", "max_tokens": 32000, "messages": [{"role": "user", "content": [
            {"type": "image", "source": {"type": "base64", "media_type": "image/png", "data": "aGk="}}]}]}

    def test_relay_calls_rewrite_and_forwards_rewritten_body(self):
        from unittest import mock
        # The rewrite mutates the payload in place: replace the image block with text.
        def _mutate(payload, cache_dir):
            payload["messages"][0]["content"] = [{"type": "text", "text": "[image transcribed]"}]
            return 1, 1
        self.cfg["upstream"] = self.fake.url
        srv = make_server(self.cfg, self.fake)
        self.addCleanup(srv.server_close)
        with mock.patch.object(proxy, "VISION", True), \
             mock.patch.object(proxy._vision, "rewrite_images", side_effect=_mutate):
            status, body = post(srv, "/v1/messages", self._img())
        self.assertEqual(status, 200)
        sent = json.loads(self.fake.requests[0]["body"])
        # The forwarded body has the image REPLACED by text — no image block.
        self.assertEqual(sent["messages"][0]["content"][0]["type"], "text")

    def test_vision_off_skips_rewrite(self):
        from unittest import mock
        self.cfg["upstream"] = self.fake.url
        srv = make_server(self.cfg, self.fake)
        self.addCleanup(srv.server_close)
        with mock.patch.object(proxy, "VISION", False), \
             mock.patch.object(proxy._vision, "rewrite_images") as mrewrite:
            status, _ = post(srv, "/v1/messages", self._img())
        self.assertEqual(status, 200)
        mrewrite.assert_not_called()
        # image block forwarded unchanged (DS4_VISION=0 restores pass-through)
        sent = json.loads(self.fake.requests[0]["body"])
        self.assertEqual(sent["messages"][0]["content"][0]["type"], "image")


class ClassifierRelayTest(unittest.TestCase):
    """The auto-mode permission classifier forwards to Anthropic, not ds4.

    The classifier is ds4-high + small max_tokens + thinking off. It is
    already an Anthropic-shaped request; the relay points it at the
    subscription with the DS4_CLASSIFIER_TOKEN bearer token. Any failure
    fails open to the ds4 relay.
    """

    def _cfg(self, fake, classifier="anthropic"):
        # The ds4 relay forwards to cfg["upstream"]; point it at the fake so
        # the ds4 path stays offline too.
        return dict(proxy.PROFILES["nous"], classifier=classifier,
                    upstream=fake.url)

    def _classifier_payload(self, **kw):
        # The classifier arrives with adaptive thinking (the proxy's rewrite
        # disables it at small max_tokens, after the classifier relay runs).
        p = {"model": "ds4-high", "max_tokens": 2112,
             "thinking": {"type": "adaptive", "display": "omitted"},
             "messages": [{"role": "user", "content": "hi"}]}
        p.update(kw)
        return p

    def test_classifier_goes_to_anthropic_with_token(self):
        from unittest import mock
        fake = helpers.FakeUpstream(
            {("POST", "/v1/messages"): (lambda b: (200, {}, b'{"ok":true}'))})
        self.addCleanup(fake.close)
        srv = make_server(self._cfg(fake), fake)
        self.addCleanup(srv.server_close)
        with mock.patch.object(proxy, "CLASSIFIER_ROUTE", "anthropic"), \
             mock.patch.object(proxy, "CLASSIFIER_MODEL", "claude-haiku-4-5"), \
             mock.patch.object(proxy._classifier, "CLASSIFIER_UPSTREAM",
                               fake.url + "/v1/messages"), \
             mock.patch.object(proxy._classifier, "classifier_token",
                               return_value="sk-ant-oat01-test"):
            status, body = post(srv, "/v1/messages", self._classifier_payload())
        self.assertEqual(status, 200)
        self.assertEqual(json.loads(body), {"ok": True})
        req = fake.requests[0]
        # the classifier hit the anthropic upstream (the FakeUpstream)
        sent = json.loads(req["body"])
        self.assertEqual(sent["model"], "claude-haiku-4-5")
        self.assertNotIn("reasoning_effort", sent)
        self.assertEqual(req["headers"].get("Authorization"), "Bearer sk-ant-oat01-test")
        self.assertEqual(req["headers"].get("Anthropic-Version"), "2023-06-01")

    def test_classifier_fails_open_to_ds4_without_token(self):
        from unittest import mock
        fake = helpers.FakeUpstream(
            {("POST", "/v1/messages"): (lambda b: (200, {}, b'{"ok":true}'))})
        self.addCleanup(fake.close)
        # ds4 upstream is the nous URL; the classifier upstream is separate.
        srv = make_server(self._cfg(fake), fake)
        self.addCleanup(srv.server_close)
        with mock.patch.object(proxy, "CLASSIFIER_ROUTE", "anthropic"), \
             mock.patch.object(proxy._classifier, "classifier_token", return_value=None):
            status, _ = post(srv, "/v1/messages", self._classifier_payload())
        self.assertEqual(status, 200)
        # the request went to the ds4 upstream (the fake) — the sentinel was
        # mapped to the profile's upstream model, so it is NOT the anthropic model
        sent = json.loads(fake.requests[0]["body"])
        self.assertEqual(sent["model"], proxy.PROFILES["nous"]["model"])
        self.assertNotEqual(sent["model"], "claude-haiku-4-5")
        self.assertEqual(fake.requests[0]["headers"].get("Authorization"), None)

    def test_classifier_opt_out_stays_on_ds4(self):
        from unittest import mock
        fake = helpers.FakeUpstream(
            {("POST", "/v1/messages"): (lambda b: (200, {}, b'{"ok":true}'))})
        self.addCleanup(fake.close)
        srv = make_server(self._cfg(fake, classifier="ds4"), fake)
        self.addCleanup(srv.server_close)
        with mock.patch.object(proxy, "CLASSIFIER_ROUTE", "ds4"):
            post(srv, "/v1/messages", self._classifier_payload())
        sent = json.loads(fake.requests[0]["body"])
        self.assertEqual(sent["model"], proxy.PROFILES["nous"]["model"])

    def test_subagent_request_never_goes_to_anthropic(self):
        from unittest import mock
        fake = helpers.FakeUpstream(
            {("POST", "/v1/messages"): (lambda b: (200, {}, b'{"ok":true}'))})
        self.addCleanup(fake.close)
        srv = make_server(self._cfg(fake), fake)
        self.addCleanup(srv.server_close)
        with mock.patch.object(proxy, "CLASSIFIER_ROUTE", "anthropic"), \
             mock.patch.object(proxy._classifier, "classifier_token",
                               return_value="tok"):
            post(srv, "/v1/messages", self._classifier_payload(max_tokens=32000))
        sent = json.loads(fake.requests[0]["body"])
        # subagent stays on ds4 — never reached the anthropic upstream
        self.assertEqual(sent["model"], proxy.PROFILES["nous"]["model"])
        self.assertEqual(fake.requests[0]["headers"].get("Authorization"), None)

    def test_classifier_400_is_forwarded_not_failed_open(self):
        from unittest import mock
        fake = helpers.FakeUpstream(
            {("POST", "/v1/messages"): (lambda b: (400, {}, b'{"error":"bad"}'))})
        self.addCleanup(fake.close)
        srv = make_server(self._cfg(fake), fake)
        self.addCleanup(srv.server_close)
        with mock.patch.object(proxy, "CLASSIFIER_ROUTE", "anthropic"), \
             mock.patch.object(proxy._classifier, "CLASSIFIER_UPSTREAM",
                               fake.url + "/v1/messages"), \
             mock.patch.object(proxy._classifier, "classifier_token",
                               return_value="tok"):
            status, body = post(srv, "/v1/messages", self._classifier_payload())
        self.assertEqual(status, 400)
        self.assertIn("bad", body)


class RelayTimeoutTest(unittest.TestCase):
    """A stalled upstream must fail fast, not tie up a relay thread.

    nous's Cloudflare 524 hangs up to its 100s relay window; with no socket
    timeout the proxy waits it out, and the failover breaker can't count a
    strike until the request resolves. The socket timeout bounds both.
    """

    def test_stalled_upstream_fails_fast_with_a_timeout(self):
        import time
        fake = helpers.FakeUpstream(
            {("POST", "/v1/messages"): (lambda b: (time.sleep(5) or (200, {}, b'{}')))})
        self.addCleanup(fake.close)
        self.tmp, cfg = helpers.temp_profile()
        self.addCleanup(self.tmp.cleanup)
        cfg["upstream"] = fake.url
        orig = proxy.RELAY_TIMEOUT
        proxy.RELAY_TIMEOUT = 1
        self.addCleanup(setattr, proxy, "RELAY_TIMEOUT", orig)
        srv = make_server(cfg, fake)
        self.addCleanup(srv.server_close)
        t0 = time.time()
        status, body = post(srv, "/v1/messages", SENTINEL)
        elapsed = time.time() - t0
        self.assertEqual(status, 502)
        self.assertIn("proxy upstream failure", body)
        self.assertLess(elapsed, 4, f"relay hung {elapsed:.1f}s instead of timing out")


class FailoverRelayTest(unittest.TestCase):
    """End to end: 503s trip the nous breaker, then the same port serves from
    the direct upstream with the direct key and a real model name."""

    def setUp(self):
        self.tmp = tempfile.mkdtemp()
        self.addCleanup(shutil.rmtree, self.tmp, ignore_errors=True)
        with open(os.path.join(self.tmp, "settings.json"), "w") as fh:
            json.dump({"env": {"ANTHROPIC_AUTH_TOKEN": "ds4-direct-key"}}, fh)
        patched = {"direct": dict(proxy.PROFILES["direct"], dir=self.tmp),
                   "nous": dict(proxy.PROFILES["nous"])}
        self._profiles = mock.patch.object(proxy, "PROFILES", patched)
        self._profiles.start()
        self.addCleanup(self._profiles.stop)
        self._w, self._r, self._rc = (proxy.FAILOVER_WINDOW,
                                      proxy.FAILOVER_RATE, proxy.FAILOVER_RECHECK)
        proxy.FAILOVER_WINDOW, proxy.FAILOVER_RATE, proxy.FAILOVER_RECHECK = 3, 1.0, 60
        self.addCleanup(setattr, proxy, "FAILOVER_WINDOW", self._w)
        self.addCleanup(setattr, proxy, "FAILOVER_RATE", self._r)
        self.addCleanup(setattr, proxy, "FAILOVER_RECHECK", self._rc)
        proxy._failover.clear()
        self.addCleanup(proxy._failover.clear)

    def test_requests_route_to_direct_after_tripping(self):
        flaky = helpers.FakeUpstream(
            {("POST", "/v1/messages"): (lambda b: (503, {}, b'{"error":"overloaded"}'))})
        good = helpers.FakeUpstream(
            {("POST", "/v1/messages"): (lambda b: (200, {}, b'{"ok":true}'))})
        self.addCleanup(flaky.close)
        self.addCleanup(good.close)
        nous = dict(proxy.PROFILES["nous"], upstream=flaky.url)
        # the failed-over path reads the direct profile's upstream
        proxy.PROFILES["direct"]["upstream"] = good.url
        srv = make_server(nous, flaky)
        self.addCleanup(srv.server_close)
        # window=3, rate=1.0 -> every failure is a strike, three open it
        for _ in range(3):
            status, _ = post(srv, "/v1/messages", SENTINEL)
            self.assertEqual(status, 503)
        self.assertTrue(proxy._failover["test"]["open"])
        # the next request is served by the direct upstream
        status, body = post(srv, "/v1/messages", SENTINEL)
        self.assertEqual(status, 200)
        self.assertEqual(json.loads(body), {"ok": True})
        sent = json.loads(good.requests[-1]["body"])
        self.assertEqual(sent["model"], "deepseek-v4-pro[1m]")   # ds4-xhigh -> pro
        self.assertNotIn("reasoning_effort", sent)
        self.assertEqual(good.requests[-1]["headers"].get("Authorization"),
                         "Bearer ds4-direct-key")

    def test_response_names_the_serving_upstream(self):
        """The client's base URL hides the gateway; the header reveals it."""
        fake = helpers.FakeUpstream(
            {("POST", "/v1/messages"): (lambda b: (200, {}, b'{"ok":true}'))})
        self.addCleanup(fake.close)
        nous = dict(proxy.PROFILES["nous"], upstream=fake.url)
        srv = make_server(nous, fake)
        self.addCleanup(srv.server_close)
        status, headers, _ = raw_post(srv, SENTINEL)
        self.assertEqual(status, 200)
        self.assertEqual(headers.get("X-DS4-Upstream"), fake.url)


if __name__ == "__main__":
    unittest.main()
