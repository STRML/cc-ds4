"""Socket activation: the inherited-fd path launchd drives.

launchd is not available under CI, so launch_activate_socket is stubbed and a
real listening socket stands in for the fd it would hand over. That covers
everything except launchd itself: the fd is served rather than rebound, several
fds for one key all get served, and serve() falls back to binding when nothing
hands it anything.
"""
import socket
import sys
import unittest
import unittest.mock as mock
import urllib.error
import urllib.request

sys.path.insert(0, "tests")
sys.path.insert(0, "src")

import proxy  # noqa: E402
import helpers  # noqa: E402


def listening_fd():
    """A bound, listening socket the way launchd would hand one over.

    Returns (fd, port). The port is read before detach(), because detaching
    invalidates the Python object even though the fd stays open.
    """
    s = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
    s.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
    s.bind(("127.0.0.1", 0))
    s.listen(5)
    port = s.getsockname()[1]
    # detach so the fd outlives this object and ownership passes to the server,
    # which is exactly the handoff launchd performs.
    return s.detach(), port


def get(port, path="/__spend"):
    try:
        with urllib.request.urlopen(f"http://127.0.0.1:{port}{path}", timeout=5) as r:
            return r.status
    except urllib.error.HTTPError as e:
        return e.code


class InheritedFd(unittest.TestCase):
    def test_server_on_fd_serves_the_fd_it_was_given(self):
        fd, want = listening_fd()
        srv = proxy.server_on_fd(fd, proxy.make_handler("direct", proxy.PROFILES["direct"]))
        self.addCleanup(srv.server_close)
        # The port is the one already bound, not a fresh one.
        self.assertEqual(srv.server_address[1], want)

    def test_server_on_fd_answers_requests(self):
        fd, port = listening_fd()
        srv = proxy.server_on_fd(fd, proxy.make_handler("direct", proxy.PROFILES["direct"]))
        self.addCleanup(srv.server_close)
        import threading
        threading.Thread(target=srv.serve_forever, daemon=True).start()
        self.addCleanup(srv.shutdown)
        # Any status at all means the inherited fd is being accepted on.
        self.assertIsNotNone(get(port))


class Serve(unittest.TestCase):
    def test_uses_the_inherited_fd_instead_of_binding(self):
        fd, port = listening_fd()
        cfg = dict(proxy.PROFILES["direct"], port=1)   # a port bind() would reject
        with mock.patch.object(proxy, "launchd_sockets", return_value=[fd]):
            self.assertTrue(proxy.serve("direct", cfg))
        # Binding cfg["port"]=1 would have failed, so serving proves the fd won.
        self.assertIsNotNone(get(port))

    def test_serves_every_fd_for_one_key(self):
        fd_a, port_a = listening_fd()
        fd_b, port_b = listening_fd()
        cfg = dict(proxy.PROFILES["direct"], port=1)
        with mock.patch.object(proxy, "launchd_sockets", return_value=[fd_a, fd_b]):
            self.assertTrue(proxy.serve("direct", cfg))
        # An fd nobody accepts on hangs rather than refuses, so both must answer.
        for p in (port_a, port_b):
            self.assertIsNotNone(get(p))

    def test_falls_back_to_binding_when_nothing_is_inherited(self):
        free = socket.socket()
        free.bind(("127.0.0.1", 0))
        port = free.getsockname()[1]
        free.close()
        cfg = dict(proxy.PROFILES["direct"], port=port)
        with mock.patch.object(proxy, "launchd_sockets", return_value=[]):
            self.assertTrue(proxy.serve("direct", cfg))
        self.assertIsNotNone(get(port))

    def test_bind_failure_is_reported_not_raised(self):
        cfg = dict(proxy.PROFILES["direct"], port=1)
        with mock.patch.object(proxy, "launchd_sockets", return_value=[]):
            self.assertFalse(proxy.serve("direct", cfg))


class LaunchdSockets(unittest.TestCase):
    def test_returns_empty_when_not_launched_by_launchd(self):
        # The suite is not a launchd job, so this is ESRCH on macOS and a
        # missing symbol on Linux. Both have to mean "bind it yourself".
        self.assertEqual(proxy.launchd_sockets("direct"), [])

    def test_returns_empty_when_libc_has_no_such_symbol(self):
        fake = mock.Mock(spec=[])          # no launch_activate_socket attribute
        with mock.patch("ctypes.CDLL", return_value=fake):
            self.assertEqual(proxy.launchd_sockets("direct"), [])


class PortsFlag(unittest.TestCase):
    def test_emits_name_and_port_for_each_served_profile(self):
        import io
        import contextlib
        out = io.StringIO()
        with mock.patch("os.path.isdir", return_value=True), \
             mock.patch.object(sys, "argv", ["proxy.py", "--ports"]), \
             contextlib.redirect_stdout(out):
            proxy.main()
        lines = [l.split() for l in out.getvalue().splitlines() if l.strip()]
        self.assertEqual({p[0] for p in lines}, set(proxy.PROFILES))
        for name, port in lines:
            self.assertEqual(int(port), proxy.PROFILES[name]["port"])

    def test_override_is_honoured(self):
        import io
        import contextlib
        out = io.StringIO()
        with mock.patch("os.path.isdir", return_value=True), \
             helpers.EnvGuard({"DS4_PORT_DIRECT": "41999"}), \
             mock.patch.object(sys, "argv", ["proxy.py", "--ports"]), \
             contextlib.redirect_stdout(out):
            proxy.main()
        self.assertIn("direct 41999", out.getvalue())

    def test_junk_override_fails_loudly(self):
        import io
        import contextlib
        # install.sh interpolates this straight into the plist, so a junk value
        # has to die here rather than produce unparseable XML.
        with mock.patch("os.path.isdir", return_value=True), \
             helpers.EnvGuard({"DS4_PORT_DIRECT": "31500</string><key>x</key>"}), \
             mock.patch.object(sys, "argv", ["proxy.py", "--ports"]), \
             contextlib.redirect_stdout(io.StringIO()):
            with self.assertRaises(ValueError):
                proxy.main()


if __name__ == "__main__":
    unittest.main()
