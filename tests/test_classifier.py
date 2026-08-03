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
        p = {"model": "ds4-high", "max_tokens": 2112,
             "thinking": {"type": "disabled"},
             "messages": [{"role": "user", "content": "hi"}]}
        p.update(kw)
        return p

    def test_classifier_signature_is_detected(self):
        self.assertTrue(c.is_classifier(self.payload(), 8192))

    def test_main_loop_is_not_classifier(self):
        self.assertFalse(c.is_classifier(
            self.payload(model="ds4-xhigh", max_tokens=32000,
                         thinking={"type": "adaptive", "display": "omitted"}),
            8192))

    def test_subagent_is_not_classifier(self):
        # ds4-high but large max_tokens and thinking on
        self.assertFalse(c.is_classifier(
            self.payload(max_tokens=32000, thinking={"type": "adaptive"}), 8192))

    def test_thinking_on_is_not_classifier(self):
        self.assertFalse(c.is_classifier(
            self.payload(thinking={"type": "adaptive"}), 8192))

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

    def test_anthropic_endpoint_returns_body_and_token(self):
        with mock.patch.object(c, "classifier_token", return_value="tok"):
            body, tok = c.anthropic_endpoint(
                {"model": "ds4-high", "max_tokens": 2112}, "claude-haiku-4-5")
        self.assertEqual(tok, "tok")
        self.assertEqual(body["model"], "claude-haiku-4-5")

    def test_anthropic_endpoint_returns_none_without_token(self):
        with mock.patch.object(c, "classifier_token", return_value=None):
            self.assertIsNone(c.anthropic_endpoint({}, "m"))


if __name__ == "__main__":
    unittest.main()
