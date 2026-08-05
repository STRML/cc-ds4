"""Tests for the classifier -> Anthropic relay.

The classifier is the small ds4-high + small-max_tokens + thinking-off call
that gates every tool call in auto mode. These tests pin down its detection
and the shape it takes when forwarded to Anthropic. All network is mocked.
"""
import os
import sys
import unittest
from unittest import mock

sys.path.insert(0, "src")

import classifier as c


class TokenTest(unittest.TestCase):
    """DS4_CLASSIFIER_TOKEN is the auth source; missing/empty fails open."""

    def setUp(self):
        self._old = os.environ.get("DS4_CLASSIFIER_TOKEN")
        os.environ["DS4_CLASSIFIER_TOKEN"] = "sk-ant-oat01-test"

    def tearDown(self):
        if self._old is None:
            os.environ.pop("DS4_CLASSIFIER_TOKEN", None)
        else:
            os.environ["DS4_CLASSIFIER_TOKEN"] = self._old

    def test_token_reads_from_env(self):
        self.assertEqual(c.classifier_token(), "sk-ant-oat01-test")

    def test_empty_token_is_none(self):
        os.environ["DS4_CLASSIFIER_TOKEN"] = ""
        self.assertIsNone(c.classifier_token())

    def test_whitespace_token_is_none(self):
        os.environ["DS4_CLASSIFIER_TOKEN"] = "   "
        self.assertIsNone(c.classifier_token())

    def test_missing_env_is_none(self):
        os.environ.pop("DS4_CLASSIFIER_TOKEN", None)
        self.assertIsNone(c.classifier_token())


class DetectTest(unittest.TestCase):
    """The classifier signature is ds4-high + small max_tokens + thinking off."""

    def payload(self, **kw):
        # The classifier arrives with adaptive thinking (Claude Code sends it
        # on every request); the proxy's rewrite disables it at small
        # max_tokens, so the real inbound shape has thinking adaptive.
        p = {"model": "ds4-high", "max_tokens": 2112,
             "thinking": {"type": "adaptive", "display": "omitted"},
             "messages": [{"role": "user", "content": "hi"}]}
        p.update(kw)
        return p

    def test_classifier_signature_is_detected(self):
        # The classifier arrives with adaptive thinking; the proxy's rewrite
        # disables it at small max_tokens. So the detector keys on ds4-high +
        # small max_tokens, NOT on thinking-off.
        self.assertTrue(c.is_classifier(self.payload(), 8192))

    def test_classifier_with_adaptive_thinking_is_detected(self):
        self.assertTrue(c.is_classifier(
            self.payload(thinking={"type": "adaptive", "display": "omitted"}),
            8192))

    def test_main_loop_is_not_classifier(self):
        self.assertFalse(c.is_classifier(
            self.payload(model="ds4-xhigh", max_tokens=32000,
                         thinking={"type": "adaptive", "display": "omitted"}),
            8192))

    def test_subagent_is_not_classifier(self):
        # ds4-high but large max_tokens — the subagent tier
        self.assertFalse(c.is_classifier(
            self.payload(max_tokens=32000, thinking={"type": "adaptive"}), 8192))

    def test_max_tokens_above_threshold_is_not_classifier(self):
        self.assertFalse(c.is_classifier(self.payload(max_tokens=8193), 8192))

    def test_max_tokens_at_threshold_is_classifier(self):
        # the boundary is inclusive (matches proxy's NOTHINK_BELOW semantics)
        self.assertTrue(c.is_classifier(self.payload(max_tokens=8192), 8192))

    def test_non_integer_max_tokens_is_not_classifier(self):
        self.assertFalse(c.is_classifier(self.payload(max_tokens="big"), 8192))

    def test_non_dict_payload_is_not_classifier(self):
        self.assertFalse(c.is_classifier(None, 8192))
        self.assertFalse(c.is_classifier("nope", 8192))


class BodyTest(unittest.TestCase):
    """The classifier body becomes a real Anthropic request."""

    def test_classifier_body_swaps_model_and_drops_effort(self):
        p = {"model": "ds4-high", "max_tokens": 2112,
             "reasoning_effort": "high",
             "thinking": {"type": "disabled"},
             "messages": [{"role": "user", "content": "hi"}]}
        out = c.classifier_body(p, "claude-haiku-4-5")
        self.assertEqual(out["model"], "claude-haiku-4-5")
        self.assertNotIn("reasoning_effort", out)
        self.assertEqual(out["max_tokens"], 2112)
        self.assertEqual(out["thinking"], {"type": "disabled"})
        self.assertEqual(out["messages"], [{"role": "user", "content": "hi"}])
        # the original is untouched
        self.assertEqual(p["model"], "ds4-high")
        self.assertIn("reasoning_effort", p)

    def test_classifier_body_without_effort_is_unchanged_elsewhere(self):
        p = {"model": "ds4-high", "max_tokens": 2112,
             "thinking": {"type": "disabled"}, "messages": []}
        out = c.classifier_body(p, "claude-haiku-4-5")
        self.assertEqual(out["max_tokens"], 2112)
        self.assertEqual(out["messages"], [])
        self.assertNotIn("reasoning_effort", out)

    def test_classifier_body_drops_ds4_specific_fields(self):
        # provider (zdr block), metadata, and reasoning_effort are ds4 body
        # shape and must not cross to Anthropic.
        p = {"model": "ds4-high", "max_tokens": 2112,
             "thinking": {"type": "disabled"},
             "reasoning_effort": "high",
             "provider": {"zdr": True, "data_collection": "deny"},
             "metadata": {"project": "/Users/samuelreed/git/oss/cc-ds4"},
             "messages": [{"role": "user", "content": "hi"}]}
        out = c.classifier_body(p, "claude-haiku-4-5")
        for key in ("provider", "metadata", "reasoning_effort"):
            self.assertNotIn(key, out, f"ds4 field {key} leaked to Anthropic")
        self.assertEqual(out["messages"], [{"role": "user", "content": "hi"}])

    def test_anthropic_endpoint_returns_body_and_token(self):
        with mock.patch.object(c, "classifier_token", return_value="tok"):
            body, tok = c.anthropic_endpoint(
                {"model": "ds4-high", "max_tokens": 2112}, "claude-haiku-4-5")
        self.assertEqual(tok, "tok")
        self.assertEqual(body["model"], "claude-haiku-4-5")

    def test_anthropic_endpoint_returns_none_without_token(self):
        with mock.patch.object(c, "classifier_token", return_value=None):
            self.assertIsNone(c.anthropic_endpoint({}, "m"))


class OrDS4BodyTest(unittest.TestCase):
    """The zdr classifier becomes an or-ds4 Messages request.

    The or-ds4 route is OpenRouter's /v1/messages, which accepts the same
    Anthropic Messages shape the classifier already has — so the body is the
    Anthropic whitelist with the model swapped and thinking forced off. The
    ZDR provider block is injected by the relay, not here.
    """

    def payload(self, **kw):
        p = {"model": "ds4-high", "max_tokens": 2112,
             "thinking": {"type": "adaptive", "display": "omitted"},
             "reasoning_effort": "high",
             "provider": {"zdr": True, "data_collection": "deny"},
             "metadata": {"project": "/x"},
             "messages": [{"role": "user", "content": "hi"}]}
        p.update(kw)
        return p

    def test_or_ds4_body_swaps_model_and_forces_thinking_off(self):
        out = c.or_ds4_body(self.payload(), "deepseek/deepseek-v4-flash-0731")
        self.assertEqual(out["model"], "deepseek/deepseek-v4-flash-0731")
        self.assertEqual(out["thinking"], {"type": "disabled"})
        self.assertEqual(out["max_tokens"], 2112)
        self.assertEqual(out["messages"], [{"role": "user", "content": "hi"}])
        # ds4-specific fields must not cross to OpenRouter.
        for key in ("provider", "metadata", "reasoning_effort"):
            self.assertNotIn(key, out, f"ds4 field {key} leaked to or-ds4")
        # the original is untouched
        self.assertEqual(self.payload()["model"], "ds4-high")

    def test_or_ds4_body_keeps_tools_and_system(self):
        out = c.or_ds4_body(
            self.payload(tools=[{"type": "function", "function": {"name": "x"}}],
                         system="sys"), "m")
        self.assertEqual(out["tools"], [{"type": "function", "function": {"name": "x"}}])
        self.assertEqual(out["system"], "sys")

    def test_or_ds4_endpoint_builds_url_and_key(self):
        ep = c.or_ds4_endpoint(self.payload(), "m",
                               "https://openrouter.ai/api", "sk-or-test")
        body, url, key = ep
        self.assertEqual(url, "https://openrouter.ai/api/v1/messages")
        self.assertEqual(key, "sk-or-test")
        self.assertEqual(body["model"], "m")

    def test_or_ds4_endpoint_none_without_key(self):
        self.assertIsNone(c.or_ds4_endpoint(self.payload(), "m",
                                            "https://openrouter.ai/api", ""))
        self.assertIsNone(c.or_ds4_endpoint(self.payload(), "m",
                                            "https://openrouter.ai/api", None))

    def test_or_ds4_endpoint_strips_trailing_slash_on_upstream(self):
        ep = c.or_ds4_endpoint(self.payload(), "m", "https://openrouter.ai/api/", "k")
        self.assertEqual(ep[1], "https://openrouter.ai/api/v1/messages")


if __name__ == "__main__":
    unittest.main()
