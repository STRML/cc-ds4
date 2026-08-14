# Test inventory

The proxy is Go. What is left in this directory tests the Python that remains:
the status line and the install script.

## Python (`python3 -m unittest discover -s tests -q`, 85 tests)

- `test_statusline.py`, `test_statusline_edge.py` — transcript accounting, the
  cost maths, and the label the bar renders.
- `test_install.py`, `test_install.sh` — install.sh: argument parsing, symlinks,
  the settings.json rewrite, the sentinel migration, stale-symlink cleanup.
- `test_render_svg.py` — status line SVG rendering.
- `helpers.py` — shared fixtures.

## Go (`cd src/go && go test ./...`)

- `internal/proxy` — rewrite, relay and retry, auth, the failover breaker and
  its trial close, the classifier routes, the ZDR gate, spend, vision, idle
  exit. Race-tested.
- `internal/jsonpy` — CPython `json.dumps` byte parity: number spelling,
  ensure_ascii escaping, key order, malformed input.
- `internal/profiles` — the profile table, `Served`, and the `--ports` output
  install.sh parses.
- `internal/sockets` — launchd socket activation and the plain-bind fallback.
- `internal/relay` — the upstream idle-deadline dial wrapper.

## The golden file

`src/go/internal/proxy/testdata/rewrite_golden.json` holds what the Python
proxy emitted for the differential corpus on its last run, replayed by
`TestRewriteMatchesPythonGolden`. It is frozen: the harness that produced it
(`tests/diff/`) compared two implementations, and there is one now.

`tests/diff/dump_golden.py` is kept as the recipe that wrote it. It does not run
against this tree, and restoring the proxy alone is not enough: it also does
`import corpus`, and `tests/diff/corpus.py` went in the same deletion.
Regenerating means restoring BOTH `src/proxy.py` and `tests/diff/corpus.py`
from history. It stays because a golden whose provenance cannot be reproduced
is one nobody can ever justify changing — and a recipe that names half its
inputs is the same problem wearing a different hat.

## Rule

A test that cannot fail is worse than no test, because it is trusted. This repo
has already shipped two: a differential harness that never rebuilt the binary it
compared against, and a corpus whose inputs both sides passed through untouched.
When a test guards something that matters, break the code on purpose once and
confirm it goes red.
