# Test inventory — Python vs Go

The Python proxy is being superseded by the Go rewrite (`src/go/`). The
differential harness (`tests/diff/run_diff.py`) proves byte-for-byte parity on
the Phase A surface. This file names which tests are **Python-only regression**
(belongs to the old implementation, retired with it) vs **contract** (the
behavior contract both proxies must satisfy, ported to Go).

## Differential harness (the swap gate)

- `tests/diff/run_diff.py` — boots the Python oracle + the Go binary at the
  same fake upstreams and asserts identical status, headers, and body bytes.
  **This is the gate**: the Go proxy only takes over when it is GREEN.
- `tests/diff/corpus.py` — the Phase A corpus (10 cases).
- `tests/diff/fake_upstream.py` — canned upstreams (SSE, retry, failover).

## Python-only regression tests (retire with src/proxy.py)

These test the Python implementation's internals; the Go rewrite ports the
behavior but not the code, so these are not the swap gate:

- `test_proxies.py` — PROFILES table, failover breaker internals, `--ports`.
- `test_proxy_http.py` — relay/rewrite/auth behavior against a fake upstream.
- `test_proxy_socket.py` — launchd socket activation.
- `test_classifier.py` — classifier routing internals.
- `test_vision.py` — image->text child process.
- `test_install.py` / `test_install.sh` — install.sh behavior.

These stay green while the Python proxy is the production agent (Phase A). They
are NOT ported to Go; the Go equivalents live in `src/go/internal/*`.

## Contract tests (must be re-asserted in Go before the swap)

The differential harness corpus is the canonical contract check. The Go unit
suites that back it up:

- `src/go/internal/proxy` — rewrite parity, relay/retry, auth, spend shape,
  classifier whitelist, failover breaker (race-tested).
- `src/go/internal/jsonpy` — ensure_ascii JSON byte-parity with CPython.
- `src/go/internal/profiles` — PROFILES generation + `--ports` parity.
- `src/go/internal/relay` — upstream idle-deadline wrapper.

## Rule

Before the Go proxy flips into production: the differential harness must be
GREEN, `src/go` must pass `go test -race ./...`, and the Python-only tests
stay as the Python proxy's regression net until it is archived.
