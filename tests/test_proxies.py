"""Tests for the proxy's request rewriting, lifecycle, and symlinked invocation.

The rule these pin down is narrow but load-bearing: a small call must come out with
thinking disabled and a main-loop call must come out untouched. Getting the boundary
wrong in either direction is silent — too low and the classifier keeps truncating, too
high and the main loop loses reasoning on every turn.

Run: python3 -m unittest discover -s tests -v
"""
import copy
import os
import subprocess
import sys
import tempfile
import time
import unittest

SRC = os.path.join(os.path.dirname(os.path.dirname(os.path.abspath(__file__))), "src")
sys.path.insert(0, SRC)

import proxy  # noqa: E402

DISABLED = {"type": "disabled"}
ADAPTIVE = {"type": "adaptive", "display": "omitted"}

DIRECT = proxy.PROFILES["direct"]
OPENROUTER = proxy.PROFILES["openrouter"]
NOUS = proxy.PROFILES["nous"]


def call(model="deepseek-v4-flash", **kw):
    p = {"model": model, "max_tokens": 512, "thinking": dict(ADAPTIVE),
         "messages": [{"role": "user", "content": "hi"}]}
    p.update(kw)
    return p


class ProfileTable(unittest.TestCase):
    """One process serves every profile, so the table has to stay coherent."""

    def test_ports_are_unique(self):
        ports = [c["port"] for c in proxy.PROFILES.values()]
        self.assertEqual(len(ports), len(set(ports)))

    def test_directories_are_unique(self):
        dirs = [c["dir"] for c in proxy.PROFILES.values()]
        self.assertEqual(len(dirs), len(set(dirs)))

    def test_ports_are_below_the_ephemeral_range(self):
        """Linux hands out 32768-60999 to outbound connections, so a fixed port at
        or above that can be taken before the proxy binds."""
        for cfg in proxy.PROFILES.values():
            self.assertLess(cfg["port"], 32768)
            self.assertGreater(cfg["port"], 1024)

    def test_every_profile_declares_every_key(self):
        keys = {"port", "dir", "upstream", "model", "zdr", "spend", "max_out", "inject"}
        for name, cfg in proxy.PROFILES.items():
            self.assertEqual(set(cfg), keys, name)

    def test_only_the_direct_endpoint_replays_thinking_blocks(self):
        """OpenRouter and Nous were measured accepting a bare tool_use history."""
        self.assertTrue(DIRECT["inject"])
        self.assertFalse(OPENROUTER["inject"])
        self.assertFalse(NOUS["inject"])


class ThinkingRewrite(unittest.TestCase):
    """The one rule every profile shares."""

    def test_small_call_gets_thinking_disabled(self):
        for name, cfg in proxy.PROFILES.items():
            p = call()
            proxy.rewrite(p, cfg)
            self.assertEqual(p["thinking"], DISABLED, name)

    def test_main_loop_call_keeps_thinking(self):
        for name, cfg in proxy.PROFILES.items():
            p = call(max_tokens=32000)
            proxy.rewrite(p, cfg)
            self.assertEqual(p["thinking"], ADAPTIVE, name)

    def test_boundary_is_inclusive(self):
        at = call(max_tokens=proxy.NOTHINK_BELOW)
        above = call(max_tokens=proxy.NOTHINK_BELOW + 1)
        proxy.rewrite(at, DIRECT)
        proxy.rewrite(above, DIRECT)
        self.assertEqual(at["thinking"], DISABLED)
        self.assertEqual(above["thinking"], ADAPTIVE)

    def test_absent_max_tokens_is_left_alone(self):
        p = call()
        del p["max_tokens"]
        proxy.rewrite(p, DIRECT)
        self.assertEqual(p["thinking"], ADAPTIVE)

    def test_non_integer_max_tokens_does_not_raise(self):
        p = call(max_tokens="512")
        proxy.rewrite(p, DIRECT)
        self.assertEqual(p["thinking"], ADAPTIVE)

    def test_a_clamped_call_still_keeps_thinking(self):
        """A clamp lands far above the threshold, so the two must not both fire."""
        p = call(max_tokens=999999)
        proxy.rewrite(p, OPENROUTER)
        self.assertEqual(p["max_tokens"], OPENROUTER["max_out"])
        self.assertEqual(p["thinking"], ADAPTIVE)


class EffortMapping(unittest.TestCase):
    """Sentinel model names are the only per-tier effort knob Claude Code exposes."""

    def test_sentinel_becomes_model_plus_effort(self):
        p = call(model="ds4-xhigh")
        proxy.rewrite(p, OPENROUTER)
        self.assertEqual(p["model"], OPENROUTER["model"])
        self.assertEqual(p["reasoning_effort"], "xhigh")

    def test_unknown_model_passes_through(self):
        p = call(model="something-else")
        proxy.rewrite(p, OPENROUTER)
        self.assertEqual(p["model"], "something-else")
        self.assertNotIn("reasoning_effort", p)

    def test_direct_profile_does_not_map(self):
        """DeepSeek's own endpoint takes real names and ignores reasoning_effort."""
        p = call(model="ds4-xhigh")
        proxy.rewrite(p, DIRECT)
        self.assertEqual(p["model"], "ds4-xhigh")
        self.assertNotIn("reasoning_effort", p)


class ProviderRouting(unittest.TestCase):
    def test_zdr_block_is_injected_where_supported(self):
        p = call(max_tokens=32000)
        proxy.rewrite(p, OPENROUTER)
        self.assertEqual(p["provider"]["zdr"], True)
        self.assertEqual(p["provider"]["data_collection"], "deny")
        self.assertIn("Io Net", p["provider"]["ignore"])

    def test_low_context_endpoint_is_not_listed_twice(self):
        p = call(max_tokens=32000, provider={"ignore": ["Io Net"]})
        proxy.rewrite(p, OPENROUTER)
        self.assertEqual(p["provider"]["ignore"].count("Io Net"), 1)

    def test_nous_gets_no_provider_block(self):
        """Nous 403s any provider block at all."""
        p = call(max_tokens=32000)
        proxy.rewrite(p, NOUS)
        self.assertNotIn("provider", p)


class ThinkingInjection(unittest.TestCase):
    """DeepSeek 400s an assistant tool_use carrying no thinking block."""

    def msgs(self):
        return [
            {"role": "user", "content": "read a.py"},
            {"role": "assistant", "content": [
                {"type": "tool_use", "id": "t1", "name": "get_file", "input": {}}]},
            {"role": "user", "content": [
                {"type": "tool_result", "tool_use_id": "t1", "content": "print(1)"}]},
        ]

    def test_bare_tool_use_gets_a_thinking_block(self):
        p = call(max_tokens=32000, messages=self.msgs())
        proxy.rewrite(p, DIRECT)
        self.assertEqual(p["messages"][1]["content"][0]["type"], "thinking")

    def test_existing_thinking_block_is_not_duplicated(self):
        m = self.msgs()
        m[1]["content"].insert(0, {"type": "thinking", "thinking": "x", "signature": "s"})
        p = call(max_tokens=32000, messages=m)
        self.assertEqual(proxy.inject_missing_thinking(p), 0)

    def test_string_content_is_skipped(self):
        p = call(messages=[{"role": "assistant", "content": "plain text"}])
        self.assertEqual(proxy.inject_missing_thinking(p), 0)

    def test_assistant_message_without_tool_use_is_skipped(self):
        p = call(messages=[{"role": "assistant",
                            "content": [{"type": "text", "text": "hello"}]}])
        self.assertEqual(proxy.inject_missing_thinking(p), 0)

    def test_small_calls_skip_injection(self):
        """With thinking off the endpoint stops asking, so a placeholder is noise."""
        p = call(messages=self.msgs())
        proxy.rewrite(p, DIRECT)
        self.assertEqual(p["thinking"], DISABLED)
        self.assertEqual(p["messages"][1]["content"][0]["type"], "tool_use")

    def test_profiles_that_do_not_need_it_do_not_get_it(self):
        for cfg in (OPENROUTER, NOUS):
            p = call(max_tokens=32000, messages=self.msgs())
            proxy.rewrite(p, cfg)
            self.assertEqual(p["messages"][1]["content"][0]["type"], "tool_use")


class SessionTokens(unittest.TestCase):
    """Liveness gate for the idle exit.

    Too eager and a live session loses its proxy mid-turn, which is the
    connection-refused failure the launch agent exists to prevent. Too lax and the
    proxy never exits, which defeats running on demand at all.
    """

    def setUp(self):
        self.dir = tempfile.mkdtemp()
        os.mkdir(os.path.join(self.dir, ".ds4-sessions"))
        self.cfg = dict(DIRECT, dir=self.dir)

    def tokens(self):
        return os.path.join(self.dir, ".ds4-sessions")

    def token(self, name):
        open(os.path.join(self.tokens(), str(name)), "w").close()

    def test_missing_directory_means_no_sessions(self):
        self.assertFalse(proxy.sessions_live(dict(DIRECT, dir="/nonexistent")))

    def test_empty_directory_means_no_sessions(self):
        self.assertFalse(proxy.sessions_live(self.cfg))

    def test_our_own_pid_counts_as_live(self):
        self.token(os.getpid())
        self.assertTrue(proxy.sessions_live(self.cfg))

    def test_dead_pid_is_not_live_and_gets_cleared(self):
        dead = subprocess.Popen([sys.executable, "-c", "pass"])
        dead.wait()
        self.token(dead.pid)
        self.assertFalse(proxy.sessions_live(self.cfg))
        self.assertEqual(os.listdir(self.tokens()), [])

    def test_one_live_token_outvotes_dead_ones(self):
        dead = subprocess.Popen([sys.executable, "-c", "pass"])
        dead.wait()
        self.token(dead.pid)
        self.token(os.getpid())
        self.assertTrue(proxy.sessions_live(self.cfg))

    def test_non_numeric_names_are_left_alone(self):
        """Nothing here may delete a file it did not create."""
        open(os.path.join(self.tokens(), "44242.json"), "w").close()
        open(os.path.join(self.tokens(), "notes.txt"), "w").close()
        self.assertFalse(proxy.sessions_live(self.cfg))
        self.assertEqual(sorted(os.listdir(self.tokens())), ["44242.json", "notes.txt"])

    def test_tokens_never_live_in_claude_codes_own_directory(self):
        """<profile>/sessions holds Claude Code's state. The reaper must not see it."""
        with open(os.path.join(SRC, "proxy.py")) as fh:
            src = fh.read()
        self.assertIn('".ds4-sessions"', src)
        self.assertNotIn('os.path.join(cfg["dir"], "sessions")', src)


class LauncherIndependentLiveness(unittest.TestCase):
    """The proxy must notice a session started without the launcher.

    This is the failure that kept biting in practice: a shell holding an old
    launcher runs claude without writing a token, the idle timer is never held off,
    and the proxy exits under a session that is still open.
    """

    MARKER = "/tmp/ds4-liveness-probe"

    @staticmethod
    def ps_exposes_env():
        """`ps -E` is a capability, not a given: it is macOS spelling, and a
        restricted sandbox can deny exec of ps outright. Skip rather than fail,
        since the proxy already degrades to session tokens when it is missing."""
        env = dict(os.environ, DS4_PS_PROBE="1")
        try:
            out = subprocess.run(["ps", "-E", "-ax", "-o", "command="],
                                 stdout=subprocess.PIPE, stderr=subprocess.DEVNULL,
                                 env=env, timeout=10).stdout
        except Exception:
            return False
        return b"DS4_PS_PROBE=1" in out

    def setUp(self):
        if not self.ps_exposes_env():
            self.skipTest("ps -E does not expose process environments here")
        env = dict(os.environ, CLAUDE_CONFIG_DIR=self.MARKER)
        self.proc = subprocess.Popen(
            [sys.executable, "-c", "import time; time.sleep(30)"], env=env)
        self.addCleanup(self.proc.wait)
        self.addCleanup(self.proc.kill)

    def test_matches_a_process_using_this_profile(self):
        cfg = dict(DIRECT, dir=self.MARKER)
        deadline = time.time() + 10
        while time.time() < deadline and not proxy.claude_running(cfg):
            time.sleep(0.2)
        self.assertTrue(proxy.claude_running(cfg))

    def test_does_not_match_an_unrelated_profile(self):
        self.assertFalse(proxy.claude_running(dict(DIRECT, dir="/tmp/ds4-nothing-here")))

    def test_in_use_is_the_union_across_profiles(self):
        served = {"a": dict(DIRECT, dir="/tmp/ds4-nothing-here"),
                  "b": dict(DIRECT, dir=self.MARKER)}
        self.assertTrue(proxy.anything_in_use(served))


class SymlinkedInvocation(unittest.TestCase):
    """install.sh symlinks the status lines into the profile directory.

    Each bootstraps sys.path off __file__. With abspath that resolves against the
    symlink's directory rather than the checkout, and the import blows up. The
    status line fails open to a blank bar and exit 0, so nothing tells you.
    """

    def run_via_symlink(self, target):
        with tempfile.TemporaryDirectory() as d:
            link = os.path.join(d, "ds4-linked.py")
            os.symlink(os.path.join(SRC, target), link)
            return subprocess.run([sys.executable, link], capture_output=True,
                                  text=True, timeout=60, input="")

    def test_direct_statusline(self):
        self.assertNotIn("ModuleNotFoundError",
                         self.run_via_symlink("statusline/direct.py").stderr)

    def test_openrouter_statusline(self):
        self.assertNotIn("ModuleNotFoundError",
                         self.run_via_symlink("statusline/openrouter.py").stderr)

    def test_nous_statusline(self):
        self.assertNotIn("ModuleNotFoundError",
                         self.run_via_symlink("statusline/nous.py").stderr)

    def test_the_proxy_has_no_path_bootstrap_to_break(self):
        """It is symlinked too, and is single-file and stdlib-only so it needs none."""
        with open(os.path.join(SRC, "proxy.py")) as fh:
            self.assertNotIn("sys.path", fh.read())


class RewriteIsInPlace(unittest.TestCase):
    def test_rewrite_does_not_mutate_the_profile_config(self):
        before = copy.deepcopy(proxy.PROFILES)
        for cfg in proxy.PROFILES.values():
            proxy.rewrite(call(model="ds4-low"), cfg)
            proxy.rewrite(call(max_tokens=999999), cfg)
        self.assertEqual(proxy.PROFILES, before)


if __name__ == "__main__":
    unittest.main()
