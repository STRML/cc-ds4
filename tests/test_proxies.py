"""Tests for the request rewriting both proxies do, and for symlinked invocation.

The rule these pin down is narrow but load-bearing: a small call must come out with
thinking disabled and a main-loop call must come out untouched. Getting the boundary
wrong in either direction is silent — too low and the classifier keeps truncating, too
high and the main loop loses reasoning on every turn.

Run: python3 -m unittest discover -s tests -v
"""
import os
import subprocess
import sys
import tempfile
import unittest

sys.path.insert(0, os.path.join(os.path.dirname(os.path.dirname(os.path.abspath(__file__))), "src"))

import thinking_proxy  # noqa: E402

DISABLED = {"type": "disabled"}
ADAPTIVE = {"type": "adaptive", "display": "omitted"}


def small(**kw):
    p = {"model": "deepseek-v4-flash", "max_tokens": 512, "thinking": dict(ADAPTIVE),
         "messages": [{"role": "user", "content": "hi"}]}
    p.update(kw)
    return p


class ThinkingRewrite(unittest.TestCase):
    """thinking_proxy.rewrite, the direct profile's only job."""

    def test_small_call_gets_thinking_disabled(self):
        p = small()
        thinking_proxy.rewrite(p)
        self.assertEqual(p["thinking"], DISABLED)

    def test_main_loop_call_is_untouched(self):
        p = small(max_tokens=32000)
        thinking_proxy.rewrite(p)
        self.assertEqual(p["thinking"], ADAPTIVE)

    def test_boundary_is_inclusive(self):
        at = small(max_tokens=thinking_proxy.NOTHINK_BELOW)
        above = small(max_tokens=thinking_proxy.NOTHINK_BELOW + 1)
        thinking_proxy.rewrite(at)
        thinking_proxy.rewrite(above)
        self.assertEqual(at["thinking"], DISABLED)
        self.assertEqual(above["thinking"], ADAPTIVE)

    def test_absent_max_tokens_is_left_alone(self):
        p = small()
        del p["max_tokens"]
        thinking_proxy.rewrite(p)
        self.assertEqual(p["thinking"], ADAPTIVE)

    def test_non_integer_max_tokens_does_not_raise(self):
        p = small(max_tokens="512")
        thinking_proxy.rewrite(p)
        self.assertEqual(p["thinking"], ADAPTIVE)

    def test_rewrite_reports_what_it_did(self):
        self.assertIn("thinking disabled", thinking_proxy.rewrite(small()))
        self.assertIsNone(thinking_proxy.rewrite(small(max_tokens=32000)))


class ThinkingInjection(unittest.TestCase):
    """The history repair. DeepSeek 400s an assistant tool_use with no thinking block."""

    def msgs(self):
        return [
            {"role": "user", "content": "read a.py"},
            {"role": "assistant", "content": [
                {"type": "tool_use", "id": "t1", "name": "get_file", "input": {}}]},
            {"role": "user", "content": [
                {"type": "tool_result", "tool_use_id": "t1", "content": "print(1)"}]},
        ]

    def test_bare_tool_use_gets_a_thinking_block(self):
        p = small(max_tokens=32000, messages=self.msgs())
        self.assertEqual(thinking_proxy.inject_missing_thinking(p), 1)
        self.assertEqual(p["messages"][1]["content"][0]["type"], "thinking")

    def test_existing_thinking_block_is_not_duplicated(self):
        m = self.msgs()
        m[1]["content"].insert(0, {"type": "thinking", "thinking": "x", "signature": "s"})
        p = small(max_tokens=32000, messages=m)
        self.assertEqual(thinking_proxy.inject_missing_thinking(p), 0)

    def test_string_content_is_skipped(self):
        p = small(max_tokens=32000, messages=[{"role": "assistant", "content": "plain text"}])
        self.assertEqual(thinking_proxy.inject_missing_thinking(p), 0)

    def test_assistant_message_without_tool_use_is_skipped(self):
        p = small(max_tokens=32000, messages=[
            {"role": "assistant", "content": [{"type": "text", "text": "hello"}]}])
        self.assertEqual(thinking_proxy.inject_missing_thinking(p), 0)

    def test_small_calls_skip_injection(self):
        """With thinking off the endpoint stops asking, so a placeholder is noise."""
        p = small(messages=self.msgs())
        thinking_proxy.rewrite(p)
        self.assertEqual(p["thinking"], DISABLED)
        self.assertEqual(p["messages"][1]["content"][0]["type"], "tool_use")


class EffortProxyParity(unittest.TestCase):
    """The OpenRouter proxy carries the same threshold under the same env var."""

    def test_same_default_threshold(self):
        import effort_proxy
        self.assertEqual(effort_proxy.NOTHINK_BELOW, thinking_proxy.NOTHINK_BELOW)

    def test_ports_do_not_collide(self):
        import effort_proxy
        self.assertNotEqual(effort_proxy.PORT, thinking_proxy.PORT)

    def test_ports_are_below_the_ephemeral_range(self):
        """Linux hands out 32768-60999 to outbound connections, so a fixed port at or
        above that can be taken before the proxy binds."""
        import effort_proxy
        for port in (effort_proxy.PORT, thinking_proxy.PORT):
            self.assertLess(port, 32768)
            self.assertGreater(port, 1024)


SRC = os.path.join(os.path.dirname(os.path.dirname(os.path.abspath(__file__))), "src")


class SymlinkedInvocation(unittest.TestCase):
    """install.sh symlinks these into the profile directory.

    Every one bootstraps sys.path off __file__. With abspath that resolves against the
    symlink's directory, not the checkout, and the import blows up. The status line
    fails open to a blank bar and exit 0, so nothing tells you it broke.
    """

    def run_via_symlink(self, target, args=()):
        with tempfile.TemporaryDirectory() as d:
            link = os.path.join(d, "ds4-linked.py")
            os.symlink(os.path.join(SRC, target), link)
            return subprocess.run([sys.executable, link, *args],
                                  capture_output=True, text=True, timeout=60, input="")

    def test_direct_statusline_imports_through_a_symlink(self):
        r = self.run_via_symlink("statusline/direct.py")
        self.assertNotIn("ModuleNotFoundError", r.stderr)

    def test_openrouter_statusline_imports_through_a_symlink(self):
        r = self.run_via_symlink("statusline/openrouter.py")
        self.assertNotIn("ModuleNotFoundError", r.stderr)

    def test_nous_statusline_imports_through_a_symlink(self):
        r = self.run_via_symlink("statusline/nous.py")
        self.assertNotIn("ModuleNotFoundError", r.stderr)

    def test_proxies_have_no_path_bootstrap_to_break(self):
        """Both proxies are single-file and stdlib-only. If that stops being true they
        need realpath too, so fail here rather than at someone's first launch."""
        for name in ("thinking_proxy.py", "effort_proxy.py"):
            with open(os.path.join(SRC, name)) as fh:
                self.assertNotIn("sys.path", fh.read(), name)


if __name__ == "__main__":
    unittest.main()
