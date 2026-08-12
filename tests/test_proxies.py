"""Tests for the proxy's request rewriting, lifecycle, and symlinked invocation.

The rule these pin down is narrow but load-bearing: a small call must come out with
thinking disabled and a main-loop call must come out untouched. Getting the boundary
wrong in either direction is silent — too low and the classifier keeps truncating, too
high and the main loop loses reasoning on every turn.

Run: python3 -m unittest discover -s tests -v
"""
import copy
import http.server
import json
import os
import socket
import subprocess
import sys
import tempfile
import threading
import time
import unittest
import urllib.error
import urllib.request
from unittest import mock

SRC = os.path.join(os.path.dirname(os.path.dirname(os.path.abspath(__file__))), "src")
sys.path.insert(0, SRC)
sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))

import proxy  # noqa: E402
import helpers  # noqa: E402

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


class EnvVarMixin:
    """setenv with automatic restore, for tests that flip process-wide vars."""

    def setenv(self, key, val):
        old = os.environ.get(key)
        os.environ[key] = val
        self.addCleanup(self._restore_env, key, old)

    @staticmethod
    def _restore_env(key, old):
        if old is None:
            os.environ.pop(key, None)
        else:
            os.environ[key] = old


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
        keys = {"port", "dir", "upstream", "model", "zdr", "spend", "max_out",
                "inject", "failover"}
        for name, cfg in proxy.PROFILES.items():
            self.assertEqual(set(cfg), keys, name)

    def test_only_nous_declares_a_failover_target(self):
        """Nous sits behind Cloudflare with real bad stretches; direct is the
        fallback. The mechanism is config-driven, so another profile can add a
        target later without touching the breaker."""
        self.assertEqual(NOUS["failover"], "direct")
        self.assertIsNone(DIRECT["failover"])
        self.assertIsNone(OPENROUTER["failover"])

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
        """At the default threshold a clamp to 65536 lands above it, so thinking
        stays on. A raised NOTHINK_BELOW is what makes the two compose."""
        p = call(max_tokens=999999)
        proxy.rewrite(p, OPENROUTER)
        self.assertEqual(p["max_tokens"], OPENROUTER["max_out"])
        self.assertEqual(p["thinking"], ADAPTIVE)

    def test_clamp_and_thinking_disable_compose_when_threshold_is_raised(self):
        """NOTHINK_BELOW above max_out must disable thinking on a clamped call."""
        self.addCleanup(setattr, proxy, "NOTHINK_BELOW", proxy.NOTHINK_BELOW)
        proxy.NOTHINK_BELOW = 70000
        p = call(max_tokens=999999)
        proxy.rewrite(p, OPENROUTER)
        self.assertEqual(p["max_tokens"], OPENROUTER["max_out"])
        self.assertEqual(p["thinking"], DISABLED)


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


class EffortOverride(unittest.TestCase):
    """<profile>/effort-override pins the effort level per profile, mid-session.

    The file is the only state that survives a proxy restart, and it must stay
    off the per-request path: the mtime cache means a request is one stat plus
    a dict lookup unless the file changed. Invalid content must read as the
    tier default rather than go upstream, because OpenRouter accepts the
    parameter and DeepSeek drops unknown values without error.
    """

    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.cfg = dict(OPENROUTER, dir=self.tmp.name)

    def write(self, text):
        # Match the slash command's atomic same-directory replacement; this
        # also avoids a reader observing a partially written override.
        path = os.path.join(self.tmp.name, "effort-override")
        tmp = path + ".tmp"
        with open(tmp, "w") as fh:
            fh.write(text)
        os.replace(tmp, path)

    def rewrite(self, model="ds4-xhigh"):
        p = call(model=model, max_tokens=32000)
        proxy.rewrite(p, self.cfg)
        return p

    def test_absent_file_uses_the_tier_default(self):
        self.assertEqual(self.rewrite()["reasoning_effort"], "xhigh")

    def test_override_wins_over_the_tier_default(self):
        self.write("high\n")
        self.assertEqual(self.rewrite()["reasoning_effort"], "high")

    def test_every_valid_level_is_accepted(self):
        for level in proxy.EFFORT_LEVELS:
            self.write(level + "\n")
            self.assertEqual(self.rewrite()["reasoning_effort"], level, level)

    def test_invalid_level_is_never_sent_upstream(self):
        self.write("banana\n")
        self.assertEqual(self.rewrite()["reasoning_effort"], "xhigh")

    def test_empty_file_uses_the_tier_default(self):
        self.write("")
        self.assertEqual(self.rewrite()["reasoning_effort"], "xhigh")

    def test_whitespace_around_the_level_is_trimmed(self):
        self.write("  high  \n")
        self.assertEqual(self.rewrite()["reasoning_effort"], "high")

    def test_change_is_picked_up_without_a_restart(self):
        self.write("low\n")
        self.assertEqual(self.rewrite()["reasoning_effort"], "low")
        self.write("max\n")
        self.assertEqual(self.rewrite()["reasoning_effort"], "max")

    def test_coarse_timestamp_filesystem_never_serves_stale(self):
        """A cache entry whose (mtime, ctime, size) matches the current file
        must not shadow it. /ds4-effort writes via atomic replace, which
        changes the inode even when the clock tick is coarser than the gap
        between writes (ext2/ext3, FAT). Forge the exact stale tuple the real
        cache would hold there and prove the inode in the key wins."""
        self.write("low\n")
        self.assertEqual(self.rewrite()["reasoning_effort"], "low")
        path = os.path.join(self.tmp.name, "effort-override")
        self.write("max\n")
        st = os.stat(path)
        proxy._effort_cache[path] = (
            st.st_mtime_ns, st.st_ctime_ns, st.st_size, st.st_ino - 1, "low")
        self.assertEqual(self.rewrite()["reasoning_effort"], "max")

    def test_override_is_per_profile_not_global(self):
        """One process serves every profile, so the cache key is the file path."""
        self.write("low\n")
        other = tempfile.TemporaryDirectory()
        self.addCleanup(other.cleanup)
        p = call(model="ds4-xhigh", max_tokens=32000)
        proxy.rewrite(p, dict(NOUS, dir=other.name))
        self.assertEqual(p["reasoning_effort"], "xhigh")
        self.assertEqual(self.rewrite()["reasoning_effort"], "low")

    def test_direct_profile_ignores_the_override(self):
        self.write("max\n")
        p = call(model="ds4-xhigh", max_tokens=32000)
        proxy.rewrite(p, dict(DIRECT, dir=self.tmp.name))
        self.assertEqual(p["model"], "ds4-xhigh")
        self.assertNotIn("reasoning_effort", p)

    def test_valid_set_is_the_seven_openrouter_levels(self):
        self.assertEqual(
            proxy.EFFORT_LEVELS,
            ("max", "xhigh", "high", "medium", "low", "minimal", "none"))


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


class ZdrSwitch(EnvVarMixin, unittest.TestCase):
    """DS4_ZDR only ever disables the block, never enables it where unsupported."""

    def test_ds4_zdr_0_drops_the_openrouter_block(self):
        self.setenv("DS4_ZDR", "0")
        p = call(max_tokens=32000)
        proxy.rewrite(p, OPENROUTER)
        self.assertNotIn("provider", p)

    def test_ds4_zdr_1_cannot_inject_into_nous(self):
        self.setenv("DS4_ZDR", "1")
        p = call(max_tokens=32000)
        proxy.rewrite(p, NOUS)
        self.assertNotIn("provider", p)


class NitroVariant(unittest.TestCase):
    """or-ds4 rides OR's :nitro variant (sort=throughput) on the model id.

    The suffix goes on cfg['model']; exact-id consumers (pricing, the
    failover remap) must strip it back to the base id.
    """

    NITRO = "deepseek/deepseek-v4-flash-0731:nitro"
    BASE = "deepseek/deepseek-v4-flash-0731"

    def test_or_ds4_model_carries_nitro(self):
        self.assertEqual(OPENROUTER["model"], self.NITRO)

    def test_sentinel_tier_rewrites_to_nitro_model(self):
        p = call(model="ds4-high", max_tokens=32000)
        proxy.rewrite(p, OPENROUTER)
        self.assertEqual(p["model"], self.NITRO)

    def test_base_model_strips_nitro_and_keeps_zdr(self):
        self.assertEqual(proxy.base_model(OPENROUTER), self.BASE)
        p = call(model="ds4-high", max_tokens=32000)
        proxy.rewrite(p, OPENROUTER)
        self.assertEqual(p["provider"]["zdr"], True)
        self.assertEqual(p["provider"]["data_collection"], "deny")

    def test_base_model_is_identity_without_suffix(self):
        self.assertEqual(proxy.base_model(DIRECT), DIRECT["model"] or "")

    def test_nous_model_untouched_still_plain(self):
        p = call(model="ds4-high", max_tokens=32000)
        proxy.rewrite(p, NOUS)
        self.assertEqual(p["model"], NOUS["model"])
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


class ApiKeySource(EnvVarMixin, unittest.TestCase):
    """The key is per-profile: settings.json first, then a namespaced override."""

    def setUp(self):
        self.dir = tempfile.mkdtemp()
        self.cfg = dict(DIRECT, dir=self.dir)
        self.setenv("DS4_KEY_DIRECT", "ds4-key-env")

    def write_settings(self, token):
        with open(os.path.join(self.dir, "settings.json"), "w") as fh:
            json.dump({"env": {"ANTHROPIC_AUTH_TOKEN": token}}, fh)

    def test_settings_token_is_used(self):
        self.write_settings("ds4-key-settings")
        self.assertEqual(proxy.api_key("direct", self.cfg), "ds4-key-settings")

    def test_settings_token_wins_over_env_override(self):
        self.write_settings("ds4-key-settings")
        self.assertEqual(proxy.api_key("direct", self.cfg), "ds4-key-settings")

    def test_env_override_is_the_fallback(self):
        self.assertEqual(proxy.api_key("direct", self.cfg), "ds4-key-env")

    def test_missing_key_is_empty_string(self):
        self.setenv("DS4_KEY_DIRECT", "")
        self.assertEqual(proxy.api_key("direct", self.cfg), "")


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

    def test_does_not_match_a_directory_that_has_this_one_as_prefix(self):
        """A backup at <marker>-backup is a different profile, not this one."""
        self.assertFalse(proxy.claude_running(
            dict(DIRECT, dir=self.MARKER + "-backup")))


class ServeTolerance(unittest.TestCase):
    """One busy port must not stop the other profiles from binding."""

    def test_a_busy_port_returns_false_and_does_not_raise(self):
        holder = socket.socket()
        holder.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
        holder.bind(("127.0.0.1", 0))
        holder.listen(1)
        self.addCleanup(holder.close)
        busy = holder.getsockname()[1]
        self.assertFalse(proxy.serve("direct", dict(DIRECT, port=busy)))

    def test_a_free_port_returns_true(self):
        probe = socket.socket()
        probe.bind(("127.0.0.1", 0))
        port = probe.getsockname()[1]
        probe.close()
        self.assertTrue(proxy.serve("direct", dict(DIRECT, port=port)))


class _SlowUpstream:
    """Responds to a POST, then dribbles the body so the relay stays open."""

    def __init__(self, hold=5):
        self.hold = hold
        self.srv = http.server.ThreadingHTTPServer(
            ("127.0.0.1", 0), self._handler)
        self.port = self.srv.server_address[1]
        threading.Thread(target=self.srv.serve_forever, daemon=True).start()

    def close(self):
        self.srv.shutdown()
        self.srv.server_close()

    def _handler(self, *a, **kw):
        return _SlowHandler(self.hold, *a, **kw)


class _SlowHandler(http.server.BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"

    def __init__(self, hold, *a, **kw):
        self.hold = hold
        super().__init__(*a, **kw)

    def do_POST(self):
        self.rfile.read(int(self.headers.get("content-length", 0)))
        body = b"y" * 100000
        self.send_response(200)
        self.send_header("content-type", "application/json")
        self.send_header("content-length", str(len(body)))
        self.end_headers()
        self.wfile.write(body[:8192])
        self.wfile.flush()
        time.sleep(self.hold)
        self.wfile.write(body[8192:])
        self.wfile.flush()

    def log_message(self, *a):
        pass


class InflightIdle(unittest.TestCase):
    """idle_watch must not exit while a request is still being relayed.

    The relay is held open against a slow local upstream, and IDLE_EXIT is
    cranked down to 1 so idle_watch wants to leave. os._exit is patched to
    raise, because a broken fix exiting mid-relay would otherwise just kill
    the whole test process with a clean exit status.
    """

    def setUp(self):
        self.tmpdir = tempfile.mkdtemp()
        self._idle = proxy.IDLE_EXIT
        proxy.IDLE_EXIT = 1
        self.addCleanup(setattr, proxy, "IDLE_EXIT", self._idle)
        proxy._inflight = 0
        self.addCleanup(setattr, proxy, "_inflight", 0)
        self._exit = os._exit

        def boom(code):
            raise AssertionError(f"idle_watch exited ({code}) with a relay open")

        os._exit = boom
        self.addCleanup(setattr, os, "_exit", self._exit)

    def test_idle_watch_survives_a_streamed_relay(self):
        up = _SlowUpstream()
        self.addCleanup(up.close)
        cfg = dict(DIRECT, dir=self.tmpdir,
                   upstream=f"http://127.0.0.1:{up.port}")
        srv = http.server.ThreadingHTTPServer(
            ("127.0.0.1", 0), proxy.make_handler("direct", cfg))
        proxy_port = srv.server_address[1]
        threading.Thread(target=srv.serve_forever, daemon=True).start()
        self.addCleanup(srv.server_close)

        client_done = threading.Event()

        def client():
            try:
                req = urllib.request.Request(
                    f"http://127.0.0.1:{proxy_port}/v1/messages",
                    data=json.dumps(call()).encode(), method="POST")
                urllib.request.urlopen(req, timeout=15).read()
            finally:
                client_done.set()

        threading.Thread(target=client, daemon=True).start()
        deadline = time.time() + 10
        while time.time() < deadline and proxy._inflight < 1:
            time.sleep(0.05)
        self.assertGreaterEqual(proxy._inflight, 1)

        watched = threading.Thread(target=proxy.idle_watch,
                                   args=({"direct": cfg},), daemon=True)
        watched.start()
        time.sleep(2.2)   # two idle_watch polls at IDLE_EXIT=1
        self.assertTrue(watched.is_alive(),
                        "idle_watch exited while a relay was still open")

        proxy.IDLE_EXIT = self._idle   # stop idle_watch wanting to leave
        self.assertTrue(client_done.wait(15))
        self.assertEqual(proxy._inflight, 0)
        watched.join(2)
        self.assertTrue(watched.is_alive())


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


class RetryGuard(unittest.TestCase):
    """Transient upstream errors are retried for subagents, not the main thread.

    The main loop (ds4-xhigh) has its own 10x-backoff retry; retrying in the
    proxy would double up. Subagents (ds4-high/max/low via
    CLAUDE_CODE_SUBAGENT_MODEL) die with "Execution error" on a raw forward, so
    the proxy absorbs the transient error for them.
    """

    def test_subagent_tiers_are_retried(self):
        for tier in ("ds4-high", "ds4-low", "ds4-max"):
            self.assertTrue(proxy.should_retry(tier), tier)

    def test_main_thread_tier_is_not_retried(self):
        # the main loop sends ds4-xhigh (ANTHROPIC_MODEL)
        self.assertFalse(proxy.should_retry("ds4-xhigh"))

    def test_unknown_model_is_retried(self):
        # defensive: a tier we do not recognize is a subagent tier
        self.assertTrue(proxy.should_retry("ds4-sonnet"))

    def test_failover_remap_does_not_retry_the_main_loop(self):
        # The failover remap rewrites payload["model"] to the target's literal
        # id (deepseek-v4-flash[1m]). should_retry is called with the
        # client-sent tier, so a failed-over main-loop request stays exempt
        # from in-proxy retry - it must not double up with the main thread's
        # own 10x-backoff retry.
        self.assertTrue(proxy.should_retry("ds4-high"))
        self.assertFalse(proxy.should_retry("ds4-xhigh"))

    def test_absent_tier_is_not_retried(self):
        self.assertFalse(proxy.should_retry(None))
        self.assertFalse(proxy.should_retry({}))


class AnthropicModelGuard(unittest.TestCase):
    """Literal Anthropic models (sonnet/opus/haiku) must not bill on the profile.

    The /model picker can expose real upstream models via gateway discovery,
    which bypass the ds4-* sentinel system. The proxy rewrites them to the
    profile's deepseek model defensively.
    """

    def test_literal_sonnet_is_rewritten(self):
        p = call(model="sonnet")
        proxy.rewrite(p, NOUS)
        self.assertEqual(p["model"], NOUS["model"])

    def test_claude_sonnet_id_is_rewritten(self):
        p = call(model="claude-sonnet-4-5")
        proxy.rewrite(p, OPENROUTER)
        self.assertEqual(p["model"], OPENROUTER["model"])

    def test_unknown_sentinel_passes_through(self):
        p = call(model="ds4-something")
        proxy.rewrite(p, NOUS)
        self.assertEqual(p["model"], "ds4-something")


class _OkUp:
    status = 200


class FailoverBreaker(unittest.TestCase):
    """Circuit-breaker failover routes a flaky profile to its declared target.

    The breaker opens once transient errors hit the threshold in a recent
    window, serves from the target until a probe recovers, and never feeds a
    target-served outcome back into the window.
    """

    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        patched = {
            "direct": dict(proxy.PROFILES["direct"], dir=self.tmp.name),
            "nous": dict(proxy.PROFILES["nous"]),
        }
        self._profiles = mock.patch.object(proxy, "PROFILES", patched)
        self._profiles.start()
        self.addCleanup(self._profiles.stop)
        self.nous = dict(proxy.PROFILES["nous"])
        self._w, self._r, self._rc = (proxy.FAILOVER_WINDOW,
                                      proxy.FAILOVER_RATE, proxy.FAILOVER_RECHECK)
        self._pc = proxy.FAILOVER_PROBES_TO_CLOSE
        proxy.FAILOVER_WINDOW, proxy.FAILOVER_RATE, proxy.FAILOVER_RECHECK = 4, 0.5, 60
        proxy.FAILOVER_PROBES_TO_CLOSE = 2
        self.addCleanup(setattr, proxy, "FAILOVER_WINDOW", self._w)
        self.addCleanup(setattr, proxy, "FAILOVER_RATE", self._r)
        self.addCleanup(setattr, proxy, "FAILOVER_RECHECK", self._rc)
        self.addCleanup(setattr, proxy, "FAILOVER_PROBES_TO_CLOSE", self._pc)
        proxy._failover.clear()
        self.addCleanup(proxy._failover.clear)

    def state(self):
        with proxy._lock:
            return proxy._failover_state("nous")

    # ── choosing where a request goes ────────────────────────────────────────

    def test_circuit_starts_closed(self):
        eff, name, trial = proxy.failover_effective("nous", self.nous)
        self.assertIs(eff, self.nous)
        self.assertEqual(name, "nous")
        self.assertFalse(trial)

    def test_open_routes_to_target_within_recheck(self):
        st = self.state()
        st["open"] = True
        st["opened_at"] = time.time()
        st["probed_at"] = time.time()
        eff, name, trial = proxy.failover_effective("nous", self.nous)
        self.assertIs(eff, proxy.PROFILES["direct"])
        self.assertEqual(name, "nous->direct")
        self.assertFalse(trial)

    def test_disabled_never_fails_over(self):
        st = self.state()
        st["open"] = True
        with mock.patch.object(proxy, "FAILOVER_ENABLED", False):
            eff, _, _ = proxy.failover_effective("nous", self.nous)
        self.assertIs(eff, self.nous)

    def test_missing_target_never_fails_over(self):
        proxy.PROFILES["direct"]["dir"] = "/nonexistent/ds4-direct"
        st = self.state()
        st["open"] = True
        eff, _, _ = proxy.failover_effective("nous", self.nous)
        self.assertIs(eff, self.nous)

    # ── the half-open probe ──────────────────────────────────────────────────

    def test_probe_success_arms_a_trial_that_rides_the_profile(self):
        fake = helpers.FakeUpstream(
            {("POST", "/v1/messages"): (lambda b: (200, {}, b'{"ok":true}'))})
        self.addCleanup(fake.close)
        self.nous["upstream"] = fake.url
        st = self.state()
        st["open"] = True
        st["opened_at"] = time.time() - 9999
        st["probed_at"] = 0.0
        # PROBES_TO_CLOSE=2: one clean probe keeps it open (still on target)
        eff, name, trial = proxy.failover_effective("nous", self.nous)
        self.assertIs(eff, proxy.PROFILES["direct"])
        self.assertEqual(name, "nous->direct")
        self.assertFalse(trial)
        self.assertTrue(self.state()["open"])
        self.assertEqual(self.state()["probes"], 1)
        # a second consecutive clean probe arms a trial: the NEXT request rides
        # the profile's OWN upstream so its outcome closes the circuit
        st["probed_at"] = 0.0
        eff, name, trial = proxy.failover_effective("nous", self.nous)
        self.assertIs(eff, self.nous)
        self.assertEqual(name, "nous")
        self.assertTrue(trial)
        self.assertTrue(self.state()["open"])     # not closed by the probe
        self.assertEqual(self.state()["probes"], 2)
        # a clean real request on the profile's upstream is what closes it
        proxy.failover_trial_close("nous")
        self.assertFalse(self.state()["open"])
        self.assertEqual(self.state()["probes"], 0)

    def test_probe_failure_stays_on_target(self):
        fake = helpers.FakeUpstream(
            {("POST", "/v1/messages"): (lambda b: (503, {}, b'{"error":"overloaded"}'))})
        self.addCleanup(fake.close)
        self.nous["upstream"] = fake.url
        st = self.state()
        st["open"] = True
        st["opened_at"] = time.time() - 9999
        st["probed_at"] = 0.0
        eff, name, _ = proxy.failover_effective("nous", self.nous)
        self.assertIs(eff, proxy.PROFILES["direct"])
        self.assertEqual(name, "nous->direct")
        self.assertTrue(self.state()["open"])

    def test_probe_is_throttled_to_the_recheck_interval(self):
        fake = helpers.FakeUpstream(
            {("POST", "/v1/messages"): (lambda b: (503, {}, b'{"error":"overloaded"}'))})
        self.addCleanup(fake.close)
        self.nous["upstream"] = fake.url
        st = self.state()
        st["open"] = True
        st["opened_at"] = time.time() - 9999
        st["probed_at"] = 0.0
        eff, _, _ = proxy.failover_effective("nous", self.nous)   # probe fails
        self.assertIs(eff, proxy.PROFILES["direct"])
        # a second request inside the recheck window must not probe again
        eff, _, _ = proxy.failover_effective("nous", self.nous)
        self.assertIs(eff, proxy.PROFILES["direct"])
        self.assertEqual(len(fake.requests), 1)

    def test_probe_body_uses_the_profile_own_model_id(self):
        # The probe must send the profile's model id: nous serves
        # deepseek/deepseek-v4-flash-0731, and a hardcoded direct id would
        # 404 there and keep the breaker open forever.
        fake = helpers.FakeUpstream(
            {("POST", "/v1/messages"): (lambda b: (200, {}, b'{"ok":true}'))})
        self.addCleanup(fake.close)
        self.nous["upstream"] = fake.url
        self.assertTrue(proxy._failover_probe("nous", self.nous))
        body = json.loads(fake.requests[0]["body"])
        self.assertEqual(body["model"], "deepseek/deepseek-v4-flash-0731")
        # a profile with no model (direct) falls back to the direct id
        fake2 = helpers.FakeUpstream(
            {("POST", "/v1/messages"): (lambda b: (200, {}, b'{"ok":true}'))})
        self.addCleanup(fake2.close)
        direct = dict(proxy.PROFILES["direct"], upstream=fake2.url)
        self.assertTrue(proxy._failover_probe("direct", direct))
        body2 = json.loads(fake2.requests[0]["body"])
        self.assertEqual(body2["model"], "deepseek-v4-flash[1m]")

    # ── feeding the window ──────────────────────────────────────────────────

    def _record(self, bad):
        if bad:
            proxy.failover_record("nous", self.nous, self.nous, None, None)
        else:
            proxy.failover_record("nous", self.nous, self.nous, _OkUp(), None)

    def test_transient_http_error_is_a_strike(self):
        # up is None at record time for an exhausted retry; last_err is the error
        e = urllib.error.HTTPError("http://x", 524, "timeout", {}, None)
        proxy.failover_record("nous", self.nous, self.nous, None, e)
        self.assertEqual(list(self.state()["outcomes"]), [True])

    def test_non_transient_error_is_not_a_strike(self):
        e = urllib.error.HTTPError("http://x", 400, "bad", {}, None)
        proxy.failover_record("nous", self.nous, self.nous, None, e)
        self.assertEqual(list(self.state()["outcomes"]), [False])

    def test_retried_request_that_succeeds_is_not_a_strike(self):
        """In-proxy retries leave a stale last_err; a response means it worked."""
        e = urllib.error.HTTPError("http://x", 503, "overload", {}, None)
        proxy.failover_record("nous", self.nous, self.nous, _OkUp(), e)
        self.assertEqual(list(self.state()["outcomes"]), [False])

    def test_connection_failure_is_a_strike(self):
        self._record(bad=True)
        self.assertEqual(list(self.state()["outcomes"]), [True])

    def test_success_is_not_a_strike(self):
        self._record(bad=False)
        self.assertEqual(list(self.state()["outcomes"]), [False])

    def test_strikes_trip_the_circuit_at_threshold(self):
        for _ in range(2):     # window=4, rate=0.5 -> threshold 2
            self._record(bad=True)
        st = self.state()
        self.assertTrue(st["open"])
        # the probe clock resets on open so the fallback gets a quiet window
        self.assertGreaterEqual(st["probed_at"], st["opened_at"])

    def test_a_good_run_never_trips(self):
        for _ in range(20):
            self._record(bad=False)
        self.assertFalse(self.state()["open"])

    def test_target_served_requests_are_not_recorded(self):
        proxy.failover_record("nous", self.nous, proxy.PROFILES["direct"], None, None)
        self.assertEqual(len(self.state()["outcomes"]), 0)

    def test_profiles_without_a_target_are_ignored(self):
        direct = dict(proxy.PROFILES["direct"])     # failover is None
        proxy.failover_record("direct", direct, direct, None, None)
        self.assertNotIn("direct", proxy._failover)


class FailoverDripTuning(unittest.TestCase):
    """The shipped breaker tuning must catch a sustained drip, not just a burst.

    Nous's upstream sits around a ~10% transient error rate that spikes to
    25%+ under load. window=12/rate=0.25 (3 strikes in the last 12) trips ~11%
    per window at a 10% drip — versus the old 6/0.5 which needed 3 of 6 and
    tripped 1.6% per window, i.e. basically never. These tests pin that a drip
    trips while a healthy run does not.
    """

    def setUp(self):
        self.nous = dict(proxy.PROFILES["nous"])     # declares failover: direct
        self._w, self._r = proxy.FAILOVER_WINDOW, proxy.FAILOVER_RATE
        proxy.FAILOVER_WINDOW, proxy.FAILOVER_RATE = 12, 0.25
        self.addCleanup(setattr, proxy, "FAILOVER_WINDOW", self._w)
        self.addCleanup(setattr, proxy, "FAILOVER_RATE", self._r)
        proxy._failover.clear()
        self.addCleanup(proxy._failover.clear)

    def _state(self):
        with proxy._lock:
            return proxy._failover_state("nous")

    def _feed(self, outcomes):
        for bad in outcomes:
            if bad:
                proxy.failover_record("nous", self.nous, self.nous, None, None)
            else:
                proxy.failover_record("nous", self.nous, self.nous, _OkUp(), None)

    def test_three_strikes_in_twelve_trip_the_breaker(self):
        # a 25% drip: 9 good, then 3 failures within the 12-window
        self._feed([False] * 9 + [True] * 3)
        self.assertTrue(self._state()["open"])

    def test_healthy_variance_never_trips(self):
        # ~3% variance: one failure in a full window never trips
        self._feed([False] * 11 + [True])
        self.assertFalse(self._state()["open"])

    def test_drip_trips_even_interleaved(self):
        # 25% failures interleaved across the window still add to 3 strikes
        seq = [False, False, False, True, False, True, False, False, False, False, True, False]
        self._feed(seq)
        self.assertTrue(self._state()["open"])

    # ── the model map ────────────────────────────────────────────────────────

    def test_failover_model_maps_every_sentinel_to_flash(self):
        # Every tier maps to flash: the direct profile runs flash for all tiers
        # and no failover path may bill pro (the cost difference is the point
        # of the fallback).
        for tier in ("ds4-max", "ds4-xhigh", "ds4-high", "ds4-low"):
            self.assertEqual(proxy.FAILOVER_MODEL[tier], "deepseek-v4-flash[1m]")

    def test_failover_model_maps_the_profiles_own_qualified_id(self):
        # A request that already carries the profile's own id (nous/openrouter:
        # deepseek/deepseek-v4-flash-0731) reaches the direct target unchanged
        # and 400s there. It must remap onto the same flash literal, exactly
        # like a sentinel. The bare direct id stays put so real direct traffic
        # is untouched.
        self.assertEqual(proxy.FAILOVER_MODEL["deepseek/deepseek-v4-flash-0731"],
                         "deepseek-v4-flash[1m]")
        self.assertNotIn("deepseek-v4-flash[1m]", proxy.FAILOVER_MODEL)


if __name__ == "__main__":
    unittest.main()
