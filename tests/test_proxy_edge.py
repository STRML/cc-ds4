"""Edge cases the first pass of proxy tests did not reach:
bounds, absence, malformed input, and the spend/ledger machinery.
"""
import copy
import json
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
                       b'{"data": {"endpoints": [{"pricing": {"prompt": "0.1", "completion": "0.2", "input_cache_read": "0.01"}}]}}'))})
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
