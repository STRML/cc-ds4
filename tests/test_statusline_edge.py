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
        # Main's review-fix added the "nous-" backend tag (was empty).
        self.assertEqual(p["model"]["display_name"], "nous-deepseek-v4-flash-0731 low")

    def test_sentinel_without_proxy_keeps_proxy_marker(self):
        sl = NousStatusline("/nonexistent")
        sl._info = {}
        p = {"model": {"id": "ds4-high"}}
        sl.label(p, {})
        # Main's review-fix added the "nous-" backend tag (was empty).
        self.assertEqual(p["model"]["display_name"], "nous-ds4-high (proxy?)")

    def test_base_model_strips_nested_suffix(self):
        self.assertEqual(base_model("a[1m]"), "a")
        self.assertEqual(base_model("a"), "a")
        self.assertEqual(base_model(""), "")


class InfoMemoization(unittest.TestCase):
    def test_openrouter_info_is_fetched_once(self):
        sl = OpenRouterStatusline("/nonexistent")
        # urlopen is patched (not _info), so after info() runs, _info is {}
        # and stays cached. Both calls must return {} and urlopen must run once.
        with mock.patch("urllib.request.urlopen",
                        side_effect=OSError("no network")) as urlopen:
            self.assertEqual(sl.info(), {})
            self.assertEqual(sl.info(), {})   # second call: still cached {}
            self.assertEqual(urlopen.call_count, 1)   # memoized, not refetched
        self.assertEqual(sl._info, {})   # remains cached after the patch ends


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
