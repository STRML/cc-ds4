import hashlib, json, os, subprocess, sys, tempfile, unittest
from unittest import mock

sys.path.insert(0, os.path.join(os.path.dirname(__file__), "..", "src"))
import vision as v


class HashKeyTest(unittest.TestCase):
    def test_hash_key_is_deterministic_and_content_addressed(self):
        a = v.hash_key(b"abc", "image/png")
        b = v.hash_key(b"abc", "image/png")
        self.assertEqual(a, b)
        self.assertNotEqual(a, v.hash_key(b"abc", "image/jpeg"))
        self.assertNotEqual(a, v.hash_key(b"abd", "image/png"))
        # hashes RAW bytes with a model+prompt salt — a describer change
        # invalidates entries. Salt = f"{VISION_MODEL}:{PROMPT}:".
        salt = f"{v.VISION_MODEL}:{v.PROMPT}:".encode("utf-8")
        self.assertEqual(a, hashlib.sha256(salt + b"image/png:abc").hexdigest())


class CacheTest(unittest.TestCase):
    def setUp(self):
        self._tmp = tempfile.mkdtemp()

    def test_cache_round_trip(self):
        key = v.hash_key(b"data", "image/png")
        self.assertIsNone(v.cache_get(self._tmp, key))
        v.cache_put(self._tmp, key, "a house")
        self.assertEqual(v.cache_get(self._tmp, key), "a house")


class TranscribeTest(unittest.TestCase):
    # CLAUDE_BIN is resolved at import; the tests must patch it so a machine
    # without claude on PATH still exercises the mocked subprocess.
    def _mock_bin(self):
        return mock.patch.object(v, "CLAUDE_BIN", "/fake/claude")

    def test_transcribe_returns_tuple_never_bare_none(self):
        with tempfile.TemporaryDirectory() as cd, self._mock_bin(), \
             mock.patch("subprocess.run") as mrun:
            mrun.return_value.returncode = 0
            mrun.return_value.stdout = json_result("a house")
            text, fresh = v.transcribe(b"data", "image/png", cd)
        self.assertEqual(text, "a house")
        self.assertEqual(fresh, 1)       # fresh=1: a child ran

    def test_transcribe_failure_returns_none_fresh_zero(self):
        with tempfile.TemporaryDirectory() as cd, self._mock_bin(), \
             mock.patch("subprocess.run") as mrun:
            mrun.return_value.returncode = 1
            mrun.return_value.stdout = ""
            text, fresh = v.transcribe(b"data", "image/png", cd)
        self.assertIsNone(text)
        self.assertEqual(fresh, 0)       # caller substitutes the placeholder

    def test_missing_claude_returns_none(self):
        # No claude on PATH / no DS4_CLAUDE_BIN -> fail open, no child.
        with tempfile.TemporaryDirectory() as cd, \
             mock.patch.object(v, "CLAUDE_BIN", None), \
             mock.patch("subprocess.run") as mrun:
            text, fresh = v.transcribe(b"data", "image/png", cd)
        self.assertIsNone(text)
        self.assertEqual(fresh, 0)
        mrun.assert_not_called()

    def test_parse_result_skips_warning_prefix(self):
        # A Node deprecation warning before the JSON must not lose the parse.
        self.assertEqual(v._parse_result('npm warn\n{"result":"a house","is_error":false}'),
                         "a house")
        # A same-line warning containing '{' must not skip the result.
        self.assertEqual(v._parse_result('warn {"code":"W"}\n{"result":"a house","is_error":false}'),
                         "a house")
        self.assertIsNone(v._parse_result('{"result":"","is_error":false}'))
        self.assertIsNone(v._parse_result('{"result":"x","is_error":true}'))

    def test_child_gets_no_session_persistence_and_devnull_stdin(self):
        with tempfile.TemporaryDirectory() as cd, self._mock_bin(), \
             mock.patch("subprocess.run") as mrun:
            mrun.return_value.returncode = 0
            mrun.return_value.stdout = json_result("a house")
            v.transcribe(b"data", "image/png", cd)
        cmd = mrun.call_args.args[0]
        self.assertIn("--no-session-persistence", cmd)
        self.assertEqual(mrun.call_args.kwargs["stdin"], subprocess.DEVNULL)


class RewriteImagesTest(unittest.TestCase):
    def _payload(self):
        return {"messages": [{"role": "user", "content": [
            {"type": "image", "source": {"type": "base64", "media_type": "image/png", "data": "aGk="}},
        ]}]}

    def _mock_bin(self):
        return mock.patch.object(v, "CLAUDE_BIN", "/fake/claude")

    def test_swaps_top_level_image_for_text(self):
        with self._mock_bin(), mock.patch("subprocess.run") as mrun:
            mrun.return_value.returncode = 0
            mrun.return_value.stdout = json_result("a house")
            p = self._payload()
            total, fresh = v.rewrite_images(p, tempfile.mkdtemp())
        self.assertEqual((total, fresh), (1, 1))
        block = p["messages"][0]["content"][0]
        self.assertEqual(block["type"], "text")
        self.assertIn("a house", block["text"])

    def test_recurses_into_tool_result(self):
        # Sean's hard requirement: images nested in tool_result.content.
        with self._mock_bin(), mock.patch("subprocess.run") as mrun:
            mrun.return_value.returncode = 0
            mrun.return_value.stdout = json_result("read image")
            p = {"messages": [{"role": "user", "content": [
                {"type": "tool_result", "tool_use_id": "t1",
                 "content": [{"type": "image", "source": {"type": "base64",
                                 "media_type": "image/png", "data": "aGk="}}]}]}]}
            total, fresh = v.rewrite_images(p, tempfile.mkdtemp())
        self.assertEqual((total, fresh), (1, 1))
        self.assertEqual(p["messages"][0]["content"][0]["content"][0]["type"], "text")

    def test_string_tool_result_is_untouched(self):
        p = {"messages": [{"role": "user", "content": [
            {"type": "tool_result", "tool_use_id": "t1", "content": "plain text"}]}]}
        total, fresh = v.rewrite_images(p, tempfile.mkdtemp())
        self.assertEqual((total, fresh), (0, 0))
        self.assertEqual(p["messages"][0]["content"][0]["content"], "plain text")

    def test_cache_hit_does_not_re_describe(self):
        cd = tempfile.mkdtemp()
        with self._mock_bin(), mock.patch("subprocess.run") as mrun:
            mrun.return_value.returncode = 0
            mrun.return_value.stdout = json_result("a house")
            v.rewrite_images(self._payload(), cd)      # miss -> describe
        with self._mock_bin(), mock.patch("subprocess.run") as mrun:
            v.rewrite_images(self._payload(), cd)      # hit -> no describe
        self.assertFalse(mrun.called)

    def test_fail_open_placeholder_on_error(self):
        with self._mock_bin(), mock.patch("subprocess.run") as mrun:
            mrun.return_value.returncode = 1
            mrun.return_value.stdout = ""
            p = self._payload()
            total, fresh = v.rewrite_images(p, tempfile.mkdtemp())
        self.assertEqual((total, fresh), (1, 0))
        block = p["messages"][0]["content"][0]
        self.assertEqual(block["type"], "text")
        self.assertIn("no usable description", block["text"])

    def test_malformed_image_block_becomes_placeholder(self):
        # A malformed image left in the request is NOT fail-open for a text-only
        # upstream — it becomes the placeholder and counts as handled.
        p = {"messages": [{"role": "user", "content": [
            {"type": "image", "source": {"type": "base64", "media_type": "image/png"}},  # no data
        ]}]}
        with self._mock_bin(), mock.patch("subprocess.run") as mrun:
            total, fresh = v.rewrite_images(p, tempfile.mkdtemp())
        self.assertEqual((total, fresh), (1, 0))
        self.assertFalse(mrun.called)
        self.assertIn("no usable description", p["messages"][0]["content"][0]["text"])

    def test_non_dict_message_is_skipped(self):
        # Mirrors proxy.rewrite's existing guard (test_proxy_edge.py:87).
        p = {"messages": [
            "bogus",
            {"role": "user", "content": [
                {"type": "image", "source": {"type": "base64",
                 "media_type": "image/png", "data": "aGk="}}]},
        ]}
        with self._mock_bin(), mock.patch("subprocess.run") as mrun:
            mrun.return_value.returncode = 0
            mrun.return_value.stdout = json_result("a house")
            total, fresh = v.rewrite_images(p, tempfile.mkdtemp())
        self.assertEqual((total, fresh), (1, 1))
        self.assertEqual(p["messages"][0], "bogus")
        self.assertEqual(p["messages"][1]["content"][0]["type"], "text")

    def test_CLAUDECODE_inherited_does_not_break_child(self):
        # The nested-session guard must scrub CLAUDECODE from the child env.
        old = os.environ.get("CLAUDECODE")
        os.environ["CLAUDECODE"] = "1"
        try:
            with self._mock_bin(), mock.patch("subprocess.run") as mrun:
                mrun.return_value.returncode = 0
                mrun.return_value.stdout = json_result("a house")
                v.rewrite_images(self._payload(), tempfile.mkdtemp())
            env = mrun.call_args.kwargs.get("env", {})
            self.assertNotIn("CLAUDECODE", env)
            self.assertNotIn("CLAUDECODE", str(env))
        finally:
            if old is None:
                os.environ.pop("CLAUDECODE", None)
            else:
                os.environ["CLAUDECODE"] = old

    def test_missing_media_type_is_placeholder_not_png(self):
        # A JPEG payload with valid base64 but no media_type is malformed.
        p = {"messages": [{"role": "user", "content": [
            {"type": "image", "source": {"type": "base64", "data": "aGk="}}]}]}  # no media_type
        with self._mock_bin(), mock.patch("subprocess.run") as mrun:
            total, fresh = v.rewrite_images(p, tempfile.mkdtemp())
        self.assertEqual((total, fresh), (1, 0))
        self.assertFalse(mrun.called)                    # no child invoked
        self.assertIn("no usable description", p["messages"][0]["content"][0]["text"])

    def test_url_image_source_is_placeholder(self):
        # URL sources are unsupported — they become the placeholder.
        p = {"messages": [{"role": "user", "content": [
            {"type": "image", "source": {"type": "url", "url": "https://x/i.png"}}]}]}
        with self._mock_bin(), mock.patch("subprocess.run") as mrun:
            total, fresh = v.rewrite_images(p, tempfile.mkdtemp())
        self.assertEqual((total, fresh), (1, 0))
        self.assertFalse(mrun.called)


def json_result(text):
    return json.dumps({"result": text, "session_id": "s1", "is_error": False})


if __name__ == "__main__":
    unittest.main()
