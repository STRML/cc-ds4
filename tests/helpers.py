"""Shared scaffolding for the cc-ds4 test suite.

This held FakeUpstream, FakeChip, EnvGuard and a _Handler for exercising the
Python proxy's HTTP layer with no network. That proxy is gone, and so are they.
What is left is what the status line tests still use.
"""


def temp_profile():
    """Return (profile_dir, direct-shaped cfg dict) with an ephemeral dir."""
    import tempfile
    tmp = tempfile.TemporaryDirectory()
    cfg = {"dir": tmp.name, "port": 0, "upstream": "http://127.0.0.1:1",
           "model": None, "zdr": False, "spend": False, "max_out": None,
           "inject": True}
    return tmp, cfg
