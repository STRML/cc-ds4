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


class LocalSecurityContract(unittest.TestCase):
    def setUp(self):
        self.tmp, self.cfg = helpers.temp_profile()
        self.addCleanup(self.tmp.cleanup)
        self.cfg.update(require_client_auth=True, zdr=True)
        with open(os.path.join(self.cfg["dir"], "settings.json"), "w") as fh:
            json.dump({"env": {"ANTHROPIC_AUTH_TOKEN": "local-token"}}, fh)
        self.fake = helpers.FakeUpstream(
            {("POST", "/v1/messages"): (lambda b: (200, {}, b'{"ok":true}'))})
        self.addCleanup(self.fake.close)
        self.cfg["upstream"] = self.fake.url
        self.srv = make_server(self.cfg, self.fake)
        self.addCleanup(self.srv.server_close)

    def test_wrong_or_missing_local_credential_is_rejected(self):
        self.assertEqual(post(self.srv, "/v1/messages", SENTINEL)[0], 401)
        self.assertEqual(post(self.srv, "/v1/messages", SENTINEL,
                              {"content-type": "application/json",
                               "authorization": "Bearer wrong"})[0], 401)
        self.assertEqual(len(self.fake.requests), 0)

    def test_matching_local_credential_is_forwarded(self):
        status, _ = post(self.srv, "/v1/messages", SENTINEL,
                          {"content-type": "application/json",
                           "authorization": "Bearer local-token"})
        self.assertEqual(status, 200)

    def test_zdr_request_is_rejected_on_non_zdr_route(self):
        self.cfg["zdr"] = False
        status, body = post(self.srv, "/v1/messages",
                            dict(SENTINEL, ds4_require_zdr=True),
                            {"content-type": "application/json",
                             "authorization": "Bearer local-token"})
        self.assertEqual(status, 409)
        self.assertIn("requires ZDR", body)
        self.assertEqual(len(self.fake.requests), 0)

    def test_zdr_marker_is_not_forwarded(self):
        status, _ = post(self.srv, "/v1/messages",
                         dict(SENTINEL, ds4_require_zdr=True),
                         {"content-type": "application/json",
                          "authorization": "Bearer local-token"})
        self.assertEqual(status, 200)
        self.assertNotIn("ds4_require_zdr", json.loads(self.fake.requests[0]["body"]))


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


class ZdrBypassRegressionTest(unittest.TestCase):
    """Issue #29: the per-request ZDR marker must not be bypassed by the
    classifier branch.

    A classifier-matched request (ds4-high + max_tokens <= NOTHINK_BELOW)
    that also carries the per-request ZDR marker must be served by its ZDR
    route (rewrite() injects the block), not rerouted to the Anthropic
    subscription — the classifier relays build a fresh body that cannot
    carry the ZDR provider block. The marker is a routing demand.
    """

    def _cfg(self, fake):
        # openrouter is the ZDR-capable route; point it at the fake. The
        # classifier relays fail open to the same fake, so a bug that lets
        # the classifier branch run would be visible as an Anthropic-shaped
        # (model-swapped) relay to the fake.
        return dict(proxy.PROFILES["openrouter"], upstream=fake.url)

    def _classifier_payload(self, **kw):
        p = {"model": "ds4-high", "max_tokens": 2112,
             "thinking": {"type": "adaptive", "display": "omitted"},
             "messages": [{"role": "user", "content": "hi"}]}
        p.update(kw)
        return p

    def test_zdr_marker_stays_on_route_not_classifier_relay(self):
        from unittest import mock
        fake = helpers.FakeUpstream(
            {("POST", "/v1/messages"): (lambda b: (200, {}, b'{"ok":true}'))})
        self.addCleanup(fake.close)
        srv = make_server(self._cfg(fake), fake)
        self.addCleanup(srv.server_close)
        with mock.patch.object(proxy, "CLASSIFIER_ROUTE", "anthropic"), \
             mock.patch.object(proxy._classifier, "CLASSIFIER_UPSTREAM",
                               fake.url + "/v1/messages"), \
             mock.patch.object(proxy._classifier, "classifier_token",
                               return_value="sk-ant-oat01-test"):
            status, body = post(srv, "/v1/messages",
                                self._classifier_payload(ds4_require_zdr=True))
        self.assertEqual(status, 200)
        self.assertEqual(json.loads(body), {"ok": True})
        sent = json.loads(fake.requests[0]["body"])
        # served by the openrouter route — model not swapped to an Anthropic id,
        # and the ZDR provider block injected by rewrite()
        self.assertEqual(sent["model"], proxy.PROFILES["openrouter"]["model"])
        self.assertEqual(sent["provider"]["zdr"], True)
        self.assertNotIn("ds4_require_zdr", sent)

    def test_zdr_marker_bypasses_zdr_classifier_route_too(self):
        """The marker must also gate the DS4_CLASSIFIER=zdr classifier branch.

        That branch's or-ds4 relay forces ZDR, but its fail-open falls through
        to the Anthropic subscription, which cannot carry the block. A
        ZDR-demanding classifier request must instead be served by the route's
        own rewrite (ZDR injected), not enter the classifier branch at all.
        """
        from unittest import mock
        fake = helpers.FakeUpstream(
            {("POST", "/v1/messages"): (lambda b: (200, {}, b'{"ok":true}'))})
        self.addCleanup(fake.close)
        srv = make_server(self._cfg(fake), fake)
        self.addCleanup(srv.server_close)
        tmp = tempfile.mkdtemp()
        self.addCleanup(shutil.rmtree, tmp, ignore_errors=True)
        # or-ds4 present and keyed so the classifier branch would serve if reached.
        with mock.patch.object(proxy, "CLASSIFIER_ROUTE", "zdr"), \
             mock.patch.object(proxy, "PROFILES",
                               {"openrouter": dict(proxy.PROFILES["openrouter"],
                                                   dir=tmp, upstream=fake.url),
                                "nous": proxy.PROFILES["nous"]}), \
             mock.patch.object(proxy._classifier, "classifier_token",
                               return_value="sk-ant-test"):
            with open(os.path.join(tmp, "settings.json"), "w") as fh:
                json.dump({"env": {"ANTHROPIC_AUTH_TOKEN": "sk-or-key"}}, fh)
            status, body = post(srv, "/v1/messages",
                                self._classifier_payload(ds4_require_zdr=True))
        self.assertEqual(status, 200)
        sent = json.loads(fake.requests[0]["body"])
        # served by the route's own rewrite — reasoning_effort is injected here
        # but the or-ds4 classifier body whitelists it out
        self.assertEqual(sent["model"], proxy.PROFILES["openrouter"]["model"])
        self.assertEqual(sent["reasoning_effort"], "high")
        self.assertEqual(sent["provider"]["zdr"], True)


class ClassifierOrDS4RelayTest(unittest.TestCase):
    """The zdr classifier routes to the or-ds4 (OpenRouter) route.

    DS4_CLASSIFIER=zdr sends the classifier to the openrouter profile's
    upstream as a Messages request with the ZDR block forced on, using the
    or-ds4 API key. It fails open to the Anthropic route (then ds4) when the
    or-ds4 route can't serve. The subagent tier never reaches or-ds4.
    """

    def _cfg(self, fake, classifier="zdr"):
        # The ds4 relay forwards to cfg["upstream"]; point it at the fake so
        # the ds4 path stays offline too.
        return dict(proxy.PROFILES["nous"], classifier=classifier,
                    upstream=fake.url)

    def _classifier_payload(self, **kw):
        p = {"model": "ds4-high", "max_tokens": 2112,
             "thinking": {"type": "adaptive", "display": "omitted"},
             "messages": [{"role": "user", "content": "hi"}]}
        p.update(kw)
        return p

    def _with_or_ds4(self, profile_dir, upstream=None):
        """Patch PROFILES so openrouter resolves to a temp profile dir.

        upstream points the or-ds4 route at a FakeUpstream when given (else the
        real OpenRouter URL, which a relay test must never reach).
        """
        with open(os.path.join(profile_dir, "settings.json"), "w") as fh:
            json.dump({"env": {"ANTHROPIC_AUTH_TOKEN": "sk-or-key"}}, fh)
        oc = dict(proxy.PROFILES["openrouter"], dir=profile_dir)
        if upstream:
            oc["upstream"] = upstream
        return mock.patch.object(proxy, "PROFILES",
                                 {"openrouter": oc, "nous": proxy.PROFILES["nous"]})

    def test_classifier_goes_to_or_ds4_with_zdr(self):
        from unittest import mock
        fake = helpers.FakeUpstream(
            {("POST", "/v1/messages"): (lambda b: (200, {}, b'{"ok":true}'))})
        self.addCleanup(fake.close)
        srv = make_server(self._cfg(fake), fake)
        self.addCleanup(srv.server_close)
        tmp = tempfile.mkdtemp()
        self.addCleanup(shutil.rmtree, tmp, ignore_errors=True)
        with mock.patch.object(proxy, "CLASSIFIER_ROUTE", "zdr"), \
             self._with_or_ds4(tmp, upstream=fake.url):
            status, body = post(srv, "/v1/messages", self._classifier_payload())
        self.assertEqual(status, 200)
        self.assertEqual(json.loads(body), {"ok": True})
        req = fake.requests[0]
        sent = json.loads(req["body"])
        self.assertEqual(sent["model"], proxy.PROFILES["openrouter"]["model"])
        self.assertEqual(sent["thinking"], {"type": "disabled"})
        self.assertEqual(sent["provider"]["zdr"], True)
        self.assertNotIn("reasoning_effort", sent)
        self.assertEqual(req["headers"].get("Authorization"), "Bearer sk-or-key")
        self.assertEqual(req["headers"].get("Anthropic-Version"), "2023-06-01")
        self.assertIn("/v1/messages", req["path"])

    def test_classifier_zdr_without_or_ds4_key_fails_open_to_anthropic(self):
        from unittest import mock
        fake = helpers.FakeUpstream(
            {("POST", "/v1/messages"): (lambda b: (200, {}, b'{"ok":true}'))})
        self.addCleanup(fake.close)
        srv = make_server(self._cfg(fake), fake)
        self.addCleanup(srv.server_close)
        tmp = tempfile.mkdtemp()
        self.addCleanup(shutil.rmtree, tmp, ignore_errors=True)
        # or-ds4 profile dir exists but has no key.
        with open(os.path.join(tmp, "settings.json"), "w") as fh:
            json.dump({"env": {}}, fh)
        oc = dict(proxy.PROFILES["openrouter"], dir=tmp)
        with mock.patch.object(proxy, "CLASSIFIER_ROUTE", "zdr"), \
             mock.patch.object(proxy, "PROFILES",
                               {"openrouter": oc, "nous": proxy.PROFILES["nous"]}), \
             mock.patch.object(proxy._classifier, "classifier_token",
                               return_value="sk-ant-test"), \
             mock.patch.object(proxy._classifier, "CLASSIFIER_UPSTREAM",
                               fake.url + "/v1/messages"):
            status, body = post(srv, "/v1/messages", self._classifier_payload())
        self.assertEqual(status, 200)
        sent = json.loads(fake.requests[0]["body"])
        # fell through to the Anthropic relay (the fake) — NOT the or-ds4 body
        self.assertEqual(sent["model"], "claude-sonnet-5")
        self.assertEqual(fake.requests[0]["headers"].get("Authorization"),
                         "Bearer sk-ant-test")

    def test_classifier_zdr_falls_back_to_ds4_when_all_routes_offline(self):
        from unittest import mock
        fake = helpers.FakeUpstream(
            {("POST", "/v1/messages"): (lambda b: (200, {}, b'{"ok":true}'))})
        self.addCleanup(fake.close)
        srv = make_server(self._cfg(fake), fake)
        self.addCleanup(srv.server_close)
        tmp = tempfile.mkdtemp()
        self.addCleanup(shutil.rmtree, tmp, ignore_errors=True)
        # or-ds4 absent (no profile), Anthropic token absent -> ds4 relay
        with mock.patch.object(proxy, "CLASSIFIER_ROUTE", "zdr"), \
             mock.patch.object(proxy, "PROFILES",
                               {"nous": proxy.PROFILES["nous"]}), \
             mock.patch.object(proxy._classifier, "classifier_token",
                               return_value=None):
            status, _ = post(srv, "/v1/messages", self._classifier_payload())
        self.assertEqual(status, 200)
        sent = json.loads(fake.requests[0]["body"])
        self.assertEqual(sent["model"], proxy.PROFILES["nous"]["model"])

    def test_subagent_request_never_goes_to_or_ds4(self):
        from unittest import mock
        fake = helpers.FakeUpstream(
            {("POST", "/v1/messages"): (lambda b: (200, {}, b'{"ok":true}'))})
        self.addCleanup(fake.close)
        srv = make_server(self._cfg(fake), fake)
        self.addCleanup(srv.server_close)
        tmp = tempfile.mkdtemp()
        self.addCleanup(shutil.rmtree, tmp, ignore_errors=True)
        with mock.patch.object(proxy, "CLASSIFIER_ROUTE", "zdr"), \
             self._with_or_ds4(tmp):
            post(srv, "/v1/messages",
                 self._classifier_payload(max_tokens=32000))
        sent = json.loads(fake.requests[0]["body"])
        # a subagent (large max_tokens) is NOT the classifier -> stays on ds4
        self.assertEqual(sent["model"], proxy.PROFILES["nous"]["model"])
        self.assertEqual(fake.requests[0]["headers"].get("Authorization"), None)


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


class FailoverStallRelayTest(unittest.TestCase):
    """The full failure the breaker exists for: the upstream hangs (nous's 524
    stall), the relay times out, the strikes trip the circuit, and the next
    requests come from the failover target. Then, when the stall clears, a
    probe closes the circuit and nous serves again.
    """

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
        self._w, self._r = proxy.FAILOVER_WINDOW, proxy.FAILOVER_RATE
        self._rc, self._rt = proxy.FAILOVER_RECHECK, proxy.RELAY_TIMEOUT
        self._pc = proxy.FAILOVER_PROBES_TO_CLOSE
        # window=4, rate=0.5 -> two strikes trip; a 1s relay timeout resolves
        # each hang fast instead of blocking the test for minutes.
        proxy.FAILOVER_WINDOW, proxy.FAILOVER_RATE = 4, 0.5
        proxy.FAILOVER_RECHECK, proxy.RELAY_TIMEOUT = 60, 1
        proxy.FAILOVER_PROBES_TO_CLOSE = 1            # keep single-probe close here
        self.addCleanup(setattr, proxy, "FAILOVER_WINDOW", self._w)
        self.addCleanup(setattr, proxy, "FAILOVER_RATE", self._r)
        self.addCleanup(setattr, proxy, "FAILOVER_RECHECK", self._rc)
        self.addCleanup(setattr, proxy, "RELAY_TIMEOUT", self._rt)
        self.addCleanup(setattr, proxy, "FAILOVER_PROBES_TO_CLOSE", self._pc)
        proxy._failover.clear()
        self.addCleanup(proxy._failover.clear)

    def _hanging(self):
        """An upstream that accepts the request but never answers on time."""
        import time
        return helpers.FakeUpstream(
            {("POST", "/v1/messages"): (lambda b: (time.sleep(5) or (200, {}, b'{}')))})

    def test_hang_times_out_trips_the_breaker_and_routes_to_direct(self):
        hanging = self._hanging()
        good = helpers.FakeUpstream(
            {("POST", "/v1/messages"): (lambda b: (200, {}, b'{"ok":true}'))})
        self.addCleanup(hanging.close)
        self.addCleanup(good.close)
        nous = dict(proxy.PROFILES["nous"], upstream=hanging.url)
        proxy.PROFILES["direct"]["upstream"] = good.url
        srv = make_server(nous, hanging)
        self.addCleanup(srv.server_close)
        # two stalls -> two strikes -> circuit opens
        for _ in range(2):
            status, body = post(srv, "/v1/messages", SENTINEL)
            self.assertEqual(status, 502)
            self.assertIn("proxy upstream failure", body)
        self.assertTrue(proxy._failover["test"]["open"])
        # the next request is served by the direct upstream, direct key swapped in
        status, body = post(srv, "/v1/messages", SENTINEL)
        self.assertEqual(status, 200)
        self.assertEqual(json.loads(body), {"ok": True})
        self.assertEqual(good.requests[-1]["headers"].get("Authorization"),
                         "Bearer ds4-direct-key")

    def test_breaker_recovers_when_the_stalled_upstream_comes_back(self):
        hanging = self._hanging()
        good = helpers.FakeUpstream(
            {("POST", "/v1/messages"): (lambda b: (200, {}, b'{"ok":true}'))})
        self.addCleanup(hanging.close)
        self.addCleanup(good.close)
        nous = dict(proxy.PROFILES["nous"], upstream=hanging.url)
        proxy.PROFILES["direct"]["upstream"] = good.url
        srv = make_server(nous, hanging)
        self.addCleanup(srv.server_close)
        for _ in range(2):
            post(srv, "/v1/messages", SENTINEL)
        self.assertTrue(proxy._failover["test"]["open"])
        # the stall clears: nous answers promptly again
        hanging.set_route("POST", "/v1/messages",
                          lambda b: (200, {}, b'{"ok":"nous"}'))
        with proxy._lock:
            proxy._failover_state("test")["probed_at"] = 0.0   # recheck due now
        # the next request probes nous via /v1/messages, it succeeds, the
        # circuit closes
        status, body = post(srv, "/v1/messages", SENTINEL)
        self.assertEqual(status, 200)
        # the probe was a minimal messages ping, not the real request
        probe = next(r for r in hanging.requests
                     if r["method"] == "POST" and r["path"] == "/v1/messages"
                     and json.loads(r["body"]).get("messages") == [
                         {"role": "user", "content": "ping"}])
        self.assertEqual(probe["path"], "/v1/messages")
        p = json.loads(probe["body"])
        self.assertEqual(p["max_tokens"], 1)
        self.assertEqual(p["thinking"], {"type": "disabled"})
        # the probe uses the profile's own model id (nous serves the -0731
        # id; a hardcoded direct id would 404 there and never close)
        self.assertEqual(p["model"], "deepseek/deepseek-v4-flash-0731")
        self.assertEqual(json.loads(body), {"ok": "nous"})
        self.assertFalse(proxy._failover["test"]["open"])
        # and nous keeps serving
        status, body = post(srv, "/v1/messages", SENTINEL)
        self.assertEqual(json.loads(body), {"ok": "nous"})


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
        self._pc = proxy.FAILOVER_PROBES_TO_CLOSE
        proxy.FAILOVER_WINDOW, proxy.FAILOVER_RATE, proxy.FAILOVER_RECHECK = 3, 1.0, 60
        proxy.FAILOVER_PROBES_TO_CLOSE = 1
        self.addCleanup(setattr, proxy, "FAILOVER_WINDOW", self._w)
        self.addCleanup(setattr, proxy, "FAILOVER_RATE", self._r)
        self.addCleanup(setattr, proxy, "FAILOVER_RECHECK", self._rc)
        self.addCleanup(setattr, proxy, "FAILOVER_PROBES_TO_CLOSE", self._pc)
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
        self.assertEqual(sent["model"], "deepseek-v4-flash[1m]")   # ds4-xhigh -> flash
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


class FailoverBugTest(unittest.TestCase):
    """Regression tests for the two breaker bugs found in #17:
    500 was not in TRANSIENT_STATUS so 500s never tripped the breaker;
    a single clean probe closed the circuit and caused flapping."""

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
        self._pc = proxy.FAILOVER_PROBES_TO_CLOSE
        proxy.FAILOVER_WINDOW, proxy.FAILOVER_RATE = 4, 0.5
        proxy.FAILOVER_RECHECK = 60
        proxy.FAILOVER_PROBES_TO_CLOSE = 2  # need 2 probes to close
        self.addCleanup(setattr, proxy, "FAILOVER_WINDOW", self._w)
        self.addCleanup(setattr, proxy, "FAILOVER_RATE", self._r)
        self.addCleanup(setattr, proxy, "FAILOVER_RECHECK", self._rc)
        self.addCleanup(setattr, proxy, "FAILOVER_PROBES_TO_CLOSE", self._pc)
        proxy._failover.clear()
        self.addCleanup(proxy._failover.clear)

    def test_bug1_500s_trip_the_breaker(self):
        """500 is now in TRANSIENT_STATUS — two 500s trip the breaker."""
        server500 = helpers.FakeUpstream(
            {("POST", "/v1/messages"): (lambda b: (500, {}, b'{"error":"internal"}'))})
        good = helpers.FakeUpstream(
            {("POST", "/v1/messages"): (lambda b: (200, {}, b'{"ok":true}'))})
        self.addCleanup(server500.close)
        self.addCleanup(good.close)
        nous = dict(proxy.PROFILES["nous"], upstream=server500.url)
        proxy.PROFILES["direct"]["upstream"] = good.url
        srv = make_server(nous, server500)
        self.addCleanup(srv.server_close)
        # window=4, rate=0.5 → two 500s trip
        for _ in range(2):
            status, body = post(srv, "/v1/messages", SENTINEL)
            self.assertEqual(status, 500)
            self.assertIn("internal", body)      # the upstream 500 is relayed
        self.assertTrue(proxy._failover["test"]["open"])
        # the next request is served by direct (failover did its job on 500s)
        status, body = post(srv, "/v1/messages", SENTINEL)
        self.assertEqual(status, 200)
        self.assertEqual(json.loads(body), {"ok": True})

    def test_bug2_circuit_stays_open_after_single_probe(self):
        """A single clean probe does NOT close the circuit; the circuit closes
        only when the profile's own upstream serves a real request clean
        (PROBES_TO_CLOSE=2 arms the trial)."""
        flaky = helpers.FakeUpstream(
            {("POST", "/v1/messages"): (lambda b: (503, {}, b'{}'))})
        good = helpers.FakeUpstream(
            {("POST", "/v1/messages"): (lambda b: (200, {}, b'{"ok":true}'))})
        self.addCleanup(flaky.close)
        self.addCleanup(good.close)
        nous = dict(proxy.PROFILES["nous"], upstream=flaky.url)
        proxy.PROFILES["direct"]["upstream"] = good.url
        srv = make_server(nous, flaky)
        self.addCleanup(srv.server_close)
        # trip the breaker
        for _ in range(2):
            post(srv, "/v1/messages", SENTINEL)
        self.assertTrue(proxy._failover["test"]["open"])
        # force recheck now; probe succeeds -> probes=1, circuit stays open,
        # the request itself is still served by the target (direct)
        with mock.patch.object(proxy, "_failover_probe", return_value=True):
            with proxy._lock:
                proxy._failover_state("test")["probed_at"] = 0.0
            status, body = post(srv, "/v1/messages", SENTINEL)
        self.assertTrue(proxy._failover["test"]["open"])
        self.assertEqual(proxy._failover["test"]["probes"], 1)
        self.assertEqual(json.loads(body), {"ok": True})   # direct served it
        # second probe (force recheck again): clean, probes=2 — arms a trial.
        # nous is still 503ing, so the trial request fails and the circuit
        # stays open.
        with mock.patch.object(proxy, "_failover_probe", return_value=True):
            with proxy._lock:
                proxy._failover_state("test")["probed_at"] = 0.0
            status, _ = post(srv, "/v1/messages", SENTINEL)
        self.assertTrue(proxy._failover["test"]["open"])
        self.assertEqual(proxy._failover["test"]["probes"], 0)   # trial reset it
        # nous recovers: two more clean probes re-arm the trial (probes=1,
        # then probes=2 arms), and the trial serves clean, closing the circuit
        flaky.set_route("POST", "/v1/messages",
                        lambda b: (200, {}, b'{"ok":"nous"}'))
        for _ in range(2):
            with mock.patch.object(proxy, "_failover_probe", return_value=True):
                with proxy._lock:
                    proxy._failover_state("test")["probed_at"] = 0.0
                post(srv, "/v1/messages", SENTINEL)
        self.assertFalse(proxy._failover["test"]["open"])

    def test_bug2_failed_probe_resets_the_streak(self):
        """A failed probe zeroes the probe counter; fresh probes are needed."""
        flaky = helpers.FakeUpstream(
            {("POST", "/v1/messages"): (lambda b: (503, {}, b'{}'))})
        good = helpers.FakeUpstream(
            {("POST", "/v1/messages"): (lambda b: (200, {}, b'{"ok":true}'))})
        self.addCleanup(flaky.close)
        self.addCleanup(good.close)
        nous = dict(proxy.PROFILES["nous"], upstream=flaky.url)
        proxy.PROFILES["direct"]["upstream"] = good.url
        srv = make_server(nous, flaky)
        self.addCleanup(srv.server_close)
        # trip
        for _ in range(2):
            post(srv, "/v1/messages", SENTINEL)
        self.assertTrue(proxy._failover["test"]["open"])
        # force a recheck whose probe FAILS -> streak stays at 0, still open
        with mock.patch.object(proxy, "_failover_probe", return_value=False):
            with proxy._lock:
                proxy._failover_state("test")["probed_at"] = 0.0
            post(srv, "/v1/messages", SENTINEL)
        self.assertTrue(proxy._failover["test"]["open"])
        self.assertEqual(proxy._failover["test"]["probes"], 0)
        # now probes succeed: one clean probe gets probes=1 (still open)
        with mock.patch.object(proxy, "_failover_probe", return_value=True):
            with proxy._lock:
                proxy._failover_state("test")["probed_at"] = 0.0
            post(srv, "/v1/messages", SENTINEL)
        self.assertEqual(proxy._failover["test"]["probes"], 1)
        self.assertTrue(proxy._failover["test"]["open"])
        # second consecutive clean probe arms a trial, but nous is still
        # 503ing — the trial fails and keeps the circuit open, zeroing the
        # streak (the probe can never close alone; only a clean real request
        # on the profile's own upstream does).
        with mock.patch.object(proxy, "_failover_probe", return_value=True):
            with proxy._lock:
                proxy._failover_state("test")["probed_at"] = 0.0
            post(srv, "/v1/messages", SENTINEL)
        self.assertTrue(proxy._failover["test"]["open"])
        self.assertEqual(proxy._failover["test"]["probes"], 0)
        # nous recovers: two clean probes re-arm, and the trial serves clean
        flaky.set_route("POST", "/v1/messages",
                        lambda b: (200, {}, b'{"ok":"nous"}'))
        for _ in range(2):
            with mock.patch.object(proxy, "_failover_probe", return_value=True):
                with proxy._lock:
                    proxy._failover_state("test")["probed_at"] = 0.0
                post(srv, "/v1/messages", SENTINEL)
        self.assertFalse(proxy._failover["test"]["open"])


if __name__ == "__main__":
    unittest.main()
