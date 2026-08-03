"""Tests for the money math and transcript accounting.

Everything here was verified by hand exactly once during development. These pin it
down so a refactor cannot quietly change what a session is reported to cost.

Run: python3 -m unittest discover -s tests -v
"""
import json
import os
import sys
import tempfile
import unittest
from unittest import mock

sys.path.insert(0, os.path.join(os.path.dirname(os.path.dirname(os.path.abspath(__file__))), "src"))

from statusline.common import Statusline, base_model, harvest_usage, money  # noqa: E402
from statusline.direct import RATES, DirectStatusline, week_spend  # noqa: E402
from statusline.openrouter import FALLBACK_RATES, OpenRouterStatusline  # noqa: E402
from statusline.nous import FALLBACK_RATES as NOUS_FALLBACK, NousStatusline  # noqa: E402

MILLION = 1_000_000


class Fixed(Statusline):
    """Statusline with a known rate table, for arithmetic tests."""
    default_model = "m"

    def rates_for(self, model):
        return {"prompt": 1e-6, "completion": 2e-6, "input_cache_read": 0.1e-6}


class TestMoney(unittest.TestCase):
    def test_sub_penny_floor(self):
        # cship renders 2dp unconditionally, which turns a real cost into "$0.00".
        self.assertEqual(money(0.002637), "<$0.01")
        self.assertEqual(money(0.0000001), "<$0.01")

    def test_zero_is_not_floored(self):
        # Zero spend is genuinely zero, and must not read as "<$0.01".
        self.assertEqual(money(0), "$0.00")

    def test_normal_and_thousands(self):
        self.assertEqual(money(0.01), "$0.01")
        self.assertEqual(money(9.54), "$9.54")
        self.assertEqual(money(1234.5), "$1,234.50")


class TestBaseModel(unittest.TestCase):
    def test_strips_context_suffix(self):
        # The [1m] suffix declares the window to Claude Code; it is not identity.
        self.assertEqual(base_model("deepseek-v4-pro[1m]"), "deepseek-v4-pro")
        self.assertEqual(base_model("deepseek-v4-flash"), "deepseek-v4-flash")

    def test_handles_missing(self):
        self.assertEqual(base_model(None), "")
        self.assertEqual(base_model(""), "")


class TestPublishedRates(unittest.TestCase):
    """Guards the transcribed price tables against typos and silent edits."""

    def test_deepseek_direct_matches_published(self):
        # https://api-docs.deepseek.com/quick_start/pricing, per million tokens.
        self.assertAlmostEqual(RATES["deepseek-v4-flash"]["prompt"] * MILLION, 0.14)
        self.assertAlmostEqual(RATES["deepseek-v4-flash"]["completion"] * MILLION, 0.28)
        self.assertAlmostEqual(RATES["deepseek-v4-flash"]["input_cache_read"] * MILLION, 0.0028)
        self.assertAlmostEqual(RATES["deepseek-v4-pro"]["prompt"] * MILLION, 0.435)
        self.assertAlmostEqual(RATES["deepseek-v4-pro"]["completion"] * MILLION, 0.87)
        self.assertAlmostEqual(RATES["deepseek-v4-pro"]["input_cache_read"] * MILLION, 0.003625)

    def test_pro_costs_about_3x_flash(self):
        f, p = RATES["deepseek-v4-flash"], RATES["deepseek-v4-pro"]
        self.assertAlmostEqual(p["prompt"] / f["prompt"], 3.107, places=2)

    def test_openrouter_fallback_matches_list_price(self):
        self.assertAlmostEqual(FALLBACK_RATES["prompt"] * MILLION, 0.09)
        self.assertAlmostEqual(FALLBACK_RATES["completion"] * MILLION, 0.18)


class TestCostOf(unittest.TestCase):
    def setUp(self):
        self.sl = Fixed("/nonexistent")

    def test_arithmetic(self):
        cost = self.sl.cost_of({"m": {"prompt": MILLION, "completion": MILLION}})
        self.assertAlmostEqual(cost, 3.0)

    def test_cache_write_falls_back_to_prompt_rate(self):
        # DeepSeek publishes no separate cache-write price.
        cost = self.sl.cost_of({"m": {"input_cache_write": MILLION}})
        self.assertAlmostEqual(cost, 1.0)

    def test_empty_is_free(self):
        self.assertEqual(self.sl.cost_of({}), 0.0)

    def test_models_priced_separately(self):
        """The bug this prevents: costing a mixed session at one tier's rates."""
        sl = DirectStatusline("/nonexistent")
        mixed = {
            "deepseek-v4-flash": {"prompt": MILLION},
            "deepseek-v4-pro": {"prompt": MILLION},
        }
        self.assertAlmostEqual(sl.cost_of(mixed), 0.14 + 0.435)
        # Priced entirely as flash it would be 0.28; entirely as pro, 0.87.
        self.assertNotAlmostEqual(sl.cost_of(mixed), 0.28)
        self.assertNotAlmostEqual(sl.cost_of(mixed), 0.87)

    def test_unknown_model_uses_default(self):
        sl = DirectStatusline("/nonexistent")
        self.assertAlmostEqual(sl.cost_of({"who-knows": {"prompt": MILLION}}), 0.14)

    def test_real_session_figure(self):
        """Regression on a real measured session.

        0.066127 input + 0.117488 output + 0.107741 cache reads. Those 38.5M cache
        reads would have cost $5.39 at the miss rate, which is the single biggest
        cost lever on this profile.
        """
        sl = DirectStatusline("/nonexistent")
        tokens = {"prompt": 472338, "completion": 419600, "input_cache_read": 38479104}
        self.assertAlmostEqual(sl.cost_of({"deepseek-v4-flash": tokens}), 0.2913568112)
        uncached = tokens["input_cache_read"] * RATES["deepseek-v4-flash"]["prompt"]
        self.assertAlmostEqual(uncached, 5.38707456)


class TestHarvestUsage(unittest.TestCase):
    def test_buckets_by_model(self):
        by_model = {}
        harvest_usage({"message": {"model": "deepseek-v4-pro[1m]",
                                   "usage": {"input_tokens": 10}}}, by_model, "fb")
        harvest_usage({"message": {"model": "deepseek-v4-flash",
                                   "usage": {"input_tokens": 5}}}, by_model, "fb")
        self.assertEqual(by_model, {"deepseek-v4-pro": {"prompt": 10},
                                    "deepseek-v4-flash": {"prompt": 5}})

    def test_unnamed_model_uses_fallback(self):
        by_model = {}
        harvest_usage({"usage": {"output_tokens": 7}}, by_model, "fb")
        self.assertEqual(by_model, {"fb": {"completion": 7}})

    def test_ignores_records_without_usage(self):
        by_model = {}
        harvest_usage({"type": "user", "message": {"content": "hi"}}, by_model, "fb")
        harvest_usage("not a dict", by_model, "fb")
        self.assertEqual(by_model, {})

    def test_accumulates_across_records(self):
        by_model = {}
        for _ in range(3):
            harvest_usage({"message": {"model": "m", "usage": {"input_tokens": 4}}},
                          by_model, "fb")
        self.assertEqual(by_model["m"]["prompt"], 12)


class TestSessionTokens(unittest.TestCase):
    """The incremental read is the part most likely to break silently."""

    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.sl = Fixed(self.tmp.name)
        self.transcript = os.path.join(self.tmp.name, "t.jsonl")

    def tearDown(self):
        self.tmp.cleanup()

    def append(self, n, model="m"):
        with open(self.transcript, "a") as fh:
            fh.write(json.dumps(
                {"message": {"model": model, "usage": {"input_tokens": n}}}) + "\n")

    def test_missing_transcript(self):
        self.assertIsNone(self.sl.session_tokens("/no/such", "s", "m"))

    def test_reads_then_resumes_without_double_counting(self):
        self.append(10)
        self.assertEqual(self.sl.session_tokens(self.transcript, "s", "m")["m"]["prompt"], 10)
        # Re-render with no new data must not re-add.
        self.assertEqual(self.sl.session_tokens(self.transcript, "s", "m")["m"]["prompt"], 10)
        self.append(5)
        self.assertEqual(self.sl.session_tokens(self.transcript, "s", "m")["m"]["prompt"], 15)

    def test_partial_trailing_line_is_not_consumed(self):
        self.append(10)
        with open(self.transcript, "a") as fh:
            fh.write('{"message": {"model": "m", "usage": {"input_toke')
        self.assertEqual(self.sl.session_tokens(self.transcript, "s", "m")["m"]["prompt"], 10)
        with open(self.transcript, "a") as fh:
            fh.write('ns": 7}}}\n')
        self.assertEqual(self.sl.session_tokens(self.transcript, "s", "m")["m"]["prompt"], 17)

    def test_truncation_rereads_from_scratch(self):
        self.append(10)
        self.append(10)
        self.assertEqual(self.sl.session_tokens(self.transcript, "s", "m")["m"]["prompt"], 20)
        with open(self.transcript, "w") as fh:   # compaction replaces the file
            fh.write("")
        self.append(3)
        self.assertEqual(self.sl.session_tokens(self.transcript, "s", "m")["m"]["prompt"], 3)

    def test_malformed_lines_are_skipped(self):
        with open(self.transcript, "a") as fh:
            fh.write("not json\n\n")
        self.append(4)
        self.assertEqual(self.sl.session_tokens(self.transcript, "s", "m")["m"]["prompt"], 4)


class TestLedger(unittest.TestCase):
    """DeepSeek reports a balance, so spend has to be integrated from the drops."""

    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.sl = DirectStatusline(self.tmp.name)

    def tearDown(self):
        self.tmp.cleanup()

    def write(self, rows):
        with open(self.sl.ledger, "w") as fh:
            for t, b, s in rows:
                fh.write(json.dumps({"t": t, "balance": b, "spent": s}) + "\n")

    def test_first_sample_spends_nothing(self):
        rows = self.sl.ledger_update(10.0, 1000.0)
        self.assertEqual(rows[-1][2], 0.0)

    def test_drop_accumulates(self):
        self.write([(0.0, 10.0, 0.0)])
        rows = self.sl.ledger_update(9.25, 1000.0)
        self.assertAlmostEqual(rows[-1][2], 0.75)

    def test_topup_is_not_negative_spend(self):
        """A top-up raises the balance; naive differencing would report < 0."""
        self.write([(0.0, 2.0, 5.0)])
        rows = self.sl.ledger_update(50.0, 1000.0)
        self.assertAlmostEqual(rows[-1][2], 5.0)   # unchanged, not -48

    def test_spend_after_topup_still_counts(self):
        self.write([(0.0, 2.0, 5.0), (1000.0, 50.0, 5.0)])
        rows = self.sl.ledger_update(49.0, 2000.0)
        self.assertAlmostEqual(rows[-1][2], 6.0)

    def test_sampling_is_rate_limited(self):
        self.write([(1000.0, 10.0, 0.0)])
        rows = self.sl.ledger_update(9.0, 1001.0)   # 1s later
        self.assertEqual(len(rows), 1)

    def test_week_spend_partial_then_full(self):
        week, partial = week_spend([(0.0, 10.0, 0.0), (100.0, 9.0, 1.0)], 200.0)
        self.assertAlmostEqual(week, 1.0)
        self.assertTrue(partial)          # ledger younger than 7 days

        now = 10 * 86400
        rows = [(0.0, 10.0, 0.0), (now - 86400, 9.0, 1.0)]
        week, partial = week_spend(rows, now)
        self.assertAlmostEqual(week, 1.0)
        self.assertFalse(partial)

    def test_week_spend_empty(self):
        self.assertEqual(week_spend([], 100.0), (None, True))


class TestLabels(unittest.TestCase):
    def test_direct_prefixes_and_strips_suffix(self):
        p = {"model": {"id": "deepseek-v4-pro[1m]"}}
        DirectStatusline("/nonexistent").label(p, {})
        self.assertEqual(p["model"]["display_name"], "ds-deepseek-v4-pro")

    def test_openrouter_marks_a_dead_proxy(self):
        sl = OpenRouterStatusline("/nonexistent")
        sl._info = {}          # proxy unreachable
        p = {"model": {"id": "ds4-xhigh"}}
        sl.label(p, {})
        self.assertIn("(proxy?)", p["model"]["display_name"])

    def test_openrouter_shows_real_slug_and_tier(self):
        sl = OpenRouterStatusline("/nonexistent")
        sl._info = {"model": "deepseek/deepseek-v4-flash-0731"}
        p = {"model": {"id": "ds4-xhigh"}}
        sl.label(p, {})
        self.assertEqual(p["model"]["display_name"], "or-deepseek-v4-flash-0731 xhigh")


class TestNousStatusline(unittest.TestCase):
    def test_label_carries_the_backend_prefix(self):
        # All three profiles reach the same model family, so the bar has to name
        # which backend is being spent against: "ds-", "or-", "nous-".
        sl = NousStatusline("/nonexistent")
        sl._info = {"model": "deepseek/deepseek-v4-flash-0731"}
        p = {"model": {"id": "ds4-xhigh"}}
        sl.label(p, {})
        self.assertEqual(p["model"]["display_name"], "nous-deepseek-v4-flash-0731 xhigh")

    def test_every_profile_tags_its_backend(self):
        """A missing prefix on one bar makes two profiles indistinguishable."""
        from statusline.direct import DirectStatusline
        from statusline.openrouter import OpenRouterStatusline
        self.assertEqual(DirectStatusline.prefix, "ds-")
        self.assertEqual(OpenRouterStatusline.prefix, "or-")
        self.assertEqual(NousStatusline.prefix, "nous-")

    def test_label_marks_dead_proxy(self):
        sl = NousStatusline("/nonexistent")
        sl._info = {}
        p = {"model": {"id": "ds4-xhigh"}}
        sl.label(p, {})
        self.assertIn("(proxy?)", p["model"]["display_name"])

    def test_account_is_empty(self):
        # Nous is a subscription with no public credits endpoint.
        sl = NousStatusline("/nonexistent")
        sl._info = {"model": "deepseek/deepseek-v4-flash-0731"}
        self.assertEqual(sl.account(), {})

    def test_fallback_matches_discounted_list_price(self):
        # 90%-off of the $0.10/$0.20 per-million prices.
        self.assertAlmostEqual(NOUS_FALLBACK["prompt"] * 1e6, 0.01)
        self.assertAlmostEqual(NOUS_FALLBACK["completion"] * 1e6, 0.02)


class TestTailSegment(unittest.TestCase):
    def setUp(self):
        self.sl = Fixed("/nonexistent")

    def test_empty_when_nothing_known(self):
        self.assertEqual(self.sl.tail_segment(None, {}), "")

    def test_omits_missing_pieces(self):
        out = self.sl.tail_segment(0.5, {})
        self.assertIn("$0.50", out)
        self.assertNotIn("7d", out)
        self.assertNotIn("left", out)

    def test_partial_week_is_marked(self):
        out = self.sl.tail_segment(None, {"week": 0.3, "week_partial": True})
        self.assertIn("7d ~$0.30", out)
        out = self.sl.tail_segment(None, {"week": 0.3, "week_partial": False})
        self.assertIn("7d $0.30", out)

    def test_cost_thresholds_change_colour(self):
        cheap = self.sl.tail_segment(0.10, {})
        warn = self.sl.tail_segment(0.30, {})
        crit = self.sl.tail_segment(2.00, {})
        self.assertNotEqual(cheap[:12], warn[:12])
        self.assertNotEqual(warn[:12], crit[:12])


class TestSpendPort(unittest.TestCase):
    """The statuslines must read the same port env names as proxy.py.

    proxy.py reads DS4_PORT_<NAME>. These statuslines used to read stale names
    (DS4_PROXY_PORT / NOUS_PROXY_PORT), so overriding a port moved the listener
    without moving the reader and the bar silently lost its spend segments.
    """
    PORT_KEYS = ("DS4_PORT_OPENROUTER", "DS4_PROXY_PORT",
                 "DS4_PORT_NOUS", "NOUS_PROXY_PORT")

    def make(self, cls, **env):
        # Clear the sibling port vars so "unset" really means unset, not "still
        # leaking from the test process environment".
        with mock.patch.dict(os.environ, env):
            for k in self.PORT_KEYS:
                if k not in env:
                    os.environ.pop(k, None)
            return cls("/nonexistent")

    def test_openrouter_new_name_wins_over_old(self):
        sl = self.make(OpenRouterStatusline, DS4_PORT_OPENROUTER="31999",
                       DS4_PROXY_PORT="31998")
        self.assertEqual(sl.spend_url, "http://127.0.0.1:31999/__spend")

    def test_openrouter_old_name_still_works(self):
        sl = self.make(OpenRouterStatusline, DS4_PROXY_PORT="31998")
        self.assertEqual(sl.spend_url, "http://127.0.0.1:31998/__spend")

    def test_openrouter_defaults_when_neither_set(self):
        sl = self.make(OpenRouterStatusline)
        self.assertEqual(sl.spend_url, "http://127.0.0.1:31501/__spend")

    def test_nous_new_name_wins_over_old(self):
        sl = self.make(NousStatusline, DS4_PORT_NOUS="31997", NOUS_PROXY_PORT="31996")
        self.assertEqual(sl.spend_url, "http://127.0.0.1:31997/__spend")

    def test_nous_old_name_still_works(self):
        sl = self.make(NousStatusline, NOUS_PROXY_PORT="31996")
        self.assertEqual(sl.spend_url, "http://127.0.0.1:31996/__spend")

    def test_nous_defaults_when_neither_set(self):
        sl = self.make(NousStatusline)
        self.assertEqual(sl.spend_url, "http://127.0.0.1:31502/__spend")


if __name__ == "__main__":
    unittest.main()
