# Go Proxy Rewrite — Design

**Date:** 2026-08-06
**Status:** Revised (Round 3)
**Goal:** Replace the Python proxy with a Go implementation that is **binary compatible** with the Python proxy's HTTP contract — identical status codes, managed response headers, and byte-identical bodies for relayed responses (see the Phase A contract for exactly what is byte-parity vs normalized) — proven by a differential test suite before the swap.

## Two-phase structure (resolves the Round-2 contradiction)

Round-2 review (6 seats, unanimous REVISE) found that security hardening contradicts the differential-oracle premise: you cannot both byte-match Python and behave differently from Python on the exact paths you harden. This spec is therefore **two phases**:

- **Phase A — the port.** Go matches Python's behavior byte-for-byte on the *unchanged* surface (relay, rewrite, failover, classifier, vision, auth, sidecar — as they are today). The differential gate proves this against the **frozen Python oracle**.
- **Phase B — the hardening (separate migration).** Built **directly in Go with Go-specific integration tests** — NOT backported into Python. The whole reason for this rewrite is that Python's `socketserver` can't express saturation policy, resettable deadlines, or fail-closed classification cleanly; forcing those into a deprecated oracle risks regressing the oracle itself. Once Phase A is green and Go handles production traffic, Go is the source of truth and Phase B adds the hardening on top, validated by Go integration tests with their own expected results.

**Phase A is the only differential surface.** The frozen Python oracle governs Phase A; Phase B hardening is Go-native with Go-specific tests. There is no Python-first migration, and no two-oracle complication.

## Context

The Python proxy (`src/proxy.py`) relays Claude Code Messages requests to DeepSeek V4 providers. It has three hard-won behaviors: request rewriting (sentinel model → real model + effort, thinking-disable below a max_tokens threshold, ZDR block injection, missing-thinking-block injection), a failover circuit breaker (nous → direct on sustained transient errors), and classifier routing (auto-mode permission calls → Anthropic subscription, or or-ds4 ZDR route).

A prior diagnosis found proxy-side defects that a Go rewrite addresses: unbounded thread-per-request (GIL contention, no cap) is fixed by construction with goroutines; the classifier `urlopen` with **no timeout** (a hung classifier blocks a thread with no deadline) is **bounded only in Phase B** — Phase A deliberately ports Python's no-timeout classifier behavior byte-for-byte (see the two-phase structure).

## Phase A — Compatibility Contract

### What binary compatibility means

For the same request bytes, the Go proxy must return the same observable HTTP behavior as Python:

| Layer | Contract |
|---|---|
| Status code | identical for relayed and generated responses, including the 409 ZDR refusal, 502 upstream failure, 200/error paths |
| Error JSON shape | proxy-generated errors use `{"error":{"message":...}}`; **relayed upstream error bodies are raw bytes, passed through unchanged** (Python preserves `{"error":"no"}` byte-for-byte) |
| Headers | preserve upstream headers except `transfer-encoding`/`content-encoding`/`connection`; add `x-ds4-upstream` and `connection: close`. **Do not synthesize `Content-Length`** (Python's `_stream` never sets one). Go must set `DisableCompression` so `net/http` doesn't auto-gzip/decompress and alter the body |
| Body bytes | **byte-identical relay of the upstream body** (Python's `_stream` is a pure `read(8192)`→`write` passthrough; SSE framing is owned by the upstream) |
| Rewrite semantics | sentinel→model, effort, thinking-disable, ZDR block, vision rewrite, thinking injection (into the **outbound request** `messages`, not the response) |
| Sidecar GETs | `/__spend` only, **unauthenticated as today**. `/v1/models` is upstream pricing, not a proxy endpoint (proxy.py:741 404s it) |

### What binary compatibility does NOT mean

- **TCP chunk boundaries.** Go's http server chunks differently than Python's socketserver. Not tested.
- **Relayed SSE framing is `transfer-encoding: chunked` in Go, not close-delimited.** Python's `_stream` sends `connection: close` with no `Content-Length` and close-delimits; Go's http server, with no Content-Length on a flushing handler, emits `transfer-encoding: chunked` (chunk-size wire bytes included). The CLI tolerates both identically, so this is a **documented, accepted transport deviation** — the differential gate normalizes `transfer-encoding`/`connection` and asserts the body stream is byte-identical, not the wire framing.
- **Locally generated 502 bodies.** Python embeds platform-specific exception text; Go differs. Compare **status + error shape** for proxy-generated failures; **exact bytes** only for upstream-replayed responses.
- **Generated `Date`/`Server` headers.** Normalize header case/order and ignore generated `Date`/`Server` in the diff.

### Phase A behaviors to port exactly as Python has them

These are the Round-2 "contradictions" — Phase A ports Python's *current* behavior; Phase B changes them.

| Behavior | Python today | Phase A (port) | Phase B (deferred) |
|---|---|---|---|
| Classifier fail-open | non-400 → falls through to ds4 (proxy.py:790,996) | **port as-is** (fail-open preserved) | fail-closed default; explicit `DS4_CLASSIFIER=ds4` only |
| Redirects | `urlopen` follows redirects (proxy.py:873) | **port as-is** (redirects followed) | `CheckRedirect` → no follow (SSRF fix) |
| Request size | no cap (proxy.py:764) | **port as-is** | `MaxBytesReader` → 413 |
| Semaphore | none | **no admission semaphore in Phase A** (match Python's unbounded accept) | bounded + 503 saturation policy |
| Classifier timeout | none (proxy.py:950,987) | **no classifier timeout in Phase A** (match Python) | resettable inactivity deadline + absolute ceiling |
| `/__spend` auth | unauthenticated GET (proxy.py:737-744) | **port as-is** | authenticate + statuslines send credential |

**Each Phase B row is a separate Go-side migration, validated by Go integration tests** — not backported into Python. Python stays frozen as the Phase A oracle; Phase B adds the hardening on top of the shipped Go proxy.

## Architecture

### Go proxy (`src/go/`)

```
src/go/
  go.mod
  main.go           # profile table (same ports/upstreams), server bootstrap
  proxy.go          # handler, rewrite(), relay, stream-back
  breaker.go        # failover circuit breaker (window/rate/probes)
  classifier.go     # classifier routing: anthropic / zdr / ds4, fail-open chain
  vision.go         # image -> text via local claude child, hash-cached
  sockets.go        # launchd socket activation (fd inheritance), idle-exit
```

- **net/http** with goroutine-per-request.
- **`http.Transport` is explicitly configured** (not `DefaultTransport`): `MaxIdleConns`/`MaxIdleConnsPerHost` scaled to the expected concurrency, `DisableCompression: true`, so parallel goroutines don't churn TIME_WAIT connections or alter response bodies.
- **The relay idle-timeout is on the UPSTREAM connection, mirroring Python.** Python's `RELAY_TIMEOUT` is a `socket.settimeout()` on the upstream `urlopen` (proxy.py:868) whose documented purpose is the stalled-origin case ("reads that produce no data for this long count as a failure", proxy.py:92-98). Go must apply the same idle deadline to the **upstream** connection — e.g. a per-`DialContext` connection wrapper that resets `SetReadDeadline()`/`SetWriteDeadline()` on each successful `Read()`/`Write()`, refreshing to `now + RELAY_TIMEOUT` so the clock only runs while the stream is idle. This is NOT `http.Server.ReadTimeout`/`WriteTimeout` (absolute, would sever SSE) and NOT a server-side `net.Listener` on the accepted client connection (client-side deadlines never fire while a handler goroutine is blocked reading upstream — that would silently drop the exact behavior `RELAY_TIMEOUT` exists for). When `DS4_RELAY_TIMEOUT=0`, no deadline is set. The differential corpus adds a **stall case**: a fake upstream that sends one chunk then goes silent must produce a 502 within the timeout, matching Python. (An absolute request ceiling is a Phase B item — antigravity's slowloris point.)
- **Idle-exit shutdown test:** an explicit test proves that after the idle-exit decision is made, the server refuses to accept/start new work — `http.Server.Shutdown(ctx)` drains in-flight connections while rejecting new accepts, and the test asserts no request begins after the decision.
- **Context propagation** from the incoming request to the upstream HTTP client, classifier relays, and vision child — a client disconnect cancels the upstream SSE stream.
- **Graceful shutdown with a session-aware idle-exit.** `http.Server.Shutdown(ctx)` drains active connections, but **`http.Server` alone cannot implement Python's `sessions_live`/`claude_running` semantics** — it would idle-exit the instant connections drop to zero, even while a Claude session is logically mid-flight between requests. Go ports Python's **out-of-band lifecycle poll** (proxy.py:643-693): a periodic check of `.ds4-sessions` entries and `claude` processes indicates whether a session is logically live, and idle-exit is deferred while one is. **A session-token WaitGroup is the wrong mechanism** — it requires explicit Add/Done from code that knows about sessions, so it would drop sessions started externally (the launcher writes the PID file out of band) and idle-exit between a session's requests, exactly the failure the poll exists to prevent. Shutdown = `http.Server.Shutdown(ctx)` once the poll allows it, with an explicit test that no request begins after the idle-exit decision. No admission semaphore and no separate HTTP connection tracker in Phase A (both contradict the "no semaphore" boundary); the session poll is out-of-band state, not connection tracking.
- Same PROFILES table as Python (ports 31500/31501/31502, upstreams, zdr, max_out, failover target), **generated from Python's `PROFILES` dict at build time** (see Source of truth below) rather than a hand-copied table.
- Classifier relay is **one generic helper** `relayClassifier(body, endpoint, auth, headers)` (Python duplicates `_relay_or_ds4`/`_relay_anthropic`; Go does not). **Classifier requests use the request context without a deadline** in Phase A (match Python's no-timeout); ordinary ds4 relay requests use the idle-timeout wrapper. Distinct retry/no-retry boundaries preserved: the main relay retries per `ds4-high`/`ds4-xhigh`; classifier and vision child calls do **not** carry the main retry policy.
- **Client auth preserved exactly as Python does it**: POST-only check, constant-time compare, `DS4_KEY_<PROFILE>` fallback (proxy.py:498,752-757). `/__spend` GET stays unauthenticated.
- **Malformed bodies are safety-tested, not parity-tested.** Python's `int(Content-Length)` behavior (raises on invalid, empty body on chunked/no-length) is an implementation accident, not a contract. Go writes one safety test: malformed or absent length must not panic and must not reach the upstream. Do not reproduce Python's exception behavior.
- **Retries rewind the body.** Retain the `[]byte` body and construct a fresh `http.Request` per attempt (not a `GetBody` abstraction). A 503→200 test must not send an empty attempt two.
- **JSON re-serialization replicates Python's `json.dumps` byte-for-byte.** Python always re-serializes the entire payload with `json.dumps(payload)` (proxy.py:836) using default separators (`', '` / `': '`) and `ensure_ascii=True`. Go must produce the **same bytes** for the rewritten payload: parse with order preservation (a JSON order-preserving decoder — e.g. `json.Decoder` with token-stream re-emission, not `map[string]any` which reorders keys and coerces numbers), apply the rewrite, and serialize with Python-matching separators and ASCII escaping. `UseNumber`-equivalent for numeric fidelity (Python preserves big ints; float64 mangles `9007199254740993`). **Byte-slice mutation (sjson/jsonparser) is wrong here** — it would preserve client whitespace/escaping where Python emits normalized `json.dumps` bytes, breaking the raw-byte parity the gate asserts.
- Socket activation: **one small platform adapter used by `main.go`** — macOS: obtain the single configured listener via cgo `launch_activate_socket`; non-macOS: bind normally. `http.Server` handles serving/shutdown. No separate "collect every named fd" machinery unless the plist defines multiple sockets.

### Differential harness (`tests/diff/`)

The suite that proves Phase A compatibility. **Not golden-record** — Python stays the living oracle.

```
tests/diff/
  run_diff.py       # orchestrator: boots both proxies + fake upstream, fires corpus
  fake_upstream.py  # canned SSE responses + an OUTBOUND REQUEST RECORDER
  corpus.py         # the request corpus (below)
```

**Method:** a fake upstream serves canned SSE responses **and records every outbound request** (method, path, headers, auth, retry count/order, raw body bytes). The harness fires identical requests at both proxies and asserts:

1. **Response** — identical status codes, **response headers (managed set, normalized case/order, generated `Date`/`Server` ignored)**, and byte-for-byte body for relayed responses; status + error shape for proxy-generated failures.
2. **Outbound request** — the two proxies must send the **same rewritten outbound request** (same URL, headers, auth policy, raw body bytes, retry count). This proves `rewrite()` parity — a response-only comparison cannot. **Compare raw body bytes, not decoded JSON**, so whitespace/order/escaping divergence is caught (the recorder may also decode for diagnostics, but the parity assertion is on bytes).

The recorder must handle **three separate endpoints** (auditor): the classifier traffic to the Anthropic/`or-ds4` upstream, and the profile traffic to the DeepSeek upstream, must each have their own recorder so the complete route sequence (classifier attempt → fail-open → profile relay) is compared independently. Extend the existing `tests/helpers.py` `FakeUpstream` (which already inspects forwarded requests) rather than creating a parallel fake.

**How both proxies are pointed at the fakes:** `PROFILES` hardcodes the real upstream URLs and `src/proxy.py` has no upstream-override env, and the spec forbids mutating the frozen oracle (Source of truth below). The harness therefore boots the Python proxy **in-process via a test shim** that imports `proxy`, patches `PROFILES` upstreams to the fake URLs, and calls `serve()` — mirroring how the existing test suite already imports and mutates `proxy.PROFILES` (tests/test_proxies.py:659-740). The Go proxy is given the same fake upstreams via its build-time-generated config pointing at the fakes. Neither source file is edited; the override lives entirely in the harness.

**Corpus (Phase A — unchanged behavior only):**
- main-loop tier (`ds4-xhigh`), subagent tier (`ds4-high`), classifier tier (small `max_tokens` → thinking disabled)
- vision requests (image blocks → placeholders), nested `tool_result`, failure → placeholder
- thinking-injection cases (placeholder inserted into outbound request history)
- error paths: 429/503/524 upstream, 401, retry-then-forward — **including the `ds4-xhigh` no-retry vs `ds4-high` retry distinction** (503→200 must return 503 for xhigh, 200 for high)
- **upstream stall:** fake upstream sends one chunk then goes silent → must 502 within `RELAY_TIMEOUT`, matching Python (the exact behavior `RELAY_TIMEOUT` exists for)
- failover transition (nous → direct) — **independent fakes per proxy** (a shared stateful fake is consumed in arrival order)
- classifier statuses: 400 (terminal), 401/503 (fail-open, as Python does). **No classifier "timeout" case in Phase A** — Python's classifier `urlopen` has no timeout, so a timeout is not exercisable against the frozen oracle; use a connection-refusal if that branch needs exercising
- client auth: missing/wrong/correct bearer on POST; prove no upstream request on failure
- ZDR: marker from header or JSON, removed before forwarding; 409 on unsupported route
- retries: `ds4-high` makes two upstream calls on 429→200; `ds4-xhigh` returns the first 429. **Go must rewind the request body on retry** (fresh `http.Request` per attempt from the retained `[]byte` — see Architecture) so attempt two isn't empty
- **JSON number fidelity:** `UseNumber` or equivalent — Python preserves big ints, Go's float64 mangles `9007199254740993`. Add a large-integer corpus case
- redirects: **assert both proxies follow them identically** (Phase A parity; no-follow is Phase B)
- `/__spend` — **status + JSON shape only** (not byte-parity-gated; spend is stateful filesystem/time logic — see Testing Strategy), **unauthenticated**, with **isolated profile dirs + deterministic pricing + identical ledger seeds + controlled time**
- **no over-capacity / no oversized-body cases in Phase A** (those are Phase B Go-only tests)

**Phase B deltas are Go-native integration tests, not differential assertions.** The frozen Python oracle governs Phase A only. Phase B hardening (fail-closed classifier, no-follow redirect, size limits, saturation policy, authenticated `/__spend`) is tested against Go invariants with their own expected results — 503 on saturation, 413 on oversize, original-3xx on redirect, retryable-error on classifier failure with no ds4 contact.

### Profile schema

The complete schema ported (not a subset): `dir`, `spend`, `inject`, `zdr`, `max_out`, `failover`, plus `port`, `upstream`, `model`, and the `FAILOVER_MODEL` map (proxy.py:812). `DS4_PORT_*` overrides preserved. `spend` determines whether `/__spend` is 200 or 404; `inject` makes direct-profile tool histories different. **Profile availability matches Python**: serve only profiles whose config directories exist (proxy.py:1123-1128), and `--ports` emits only those (proxy.py:1133-1137) — Go must not create launchd sockets for unavailable profiles.

### Source of truth (Phase A boundary)

- **Go's config is generated from Python's table at build time** (antigravity: don't mutate the legacy oracle's config loading right before using it to validate a rewrite). A small build step emits the Go profile table from `src/proxy.py`'s `PROFILES` dict — no hand-copied `main.go` table, and Python's `proxy.py` stays untouched as the frozen oracle. `install.sh` consumes the ports via the Go binary's `--ports` (no new `jq`/Python dependency). A later consolidation to a shared declarative file is a post-cutover cleanup, not a Phase A prerequisite.
- **"Byte-for-byte status" is a misnomer — it means identical status codes.** Byte parity applies to relayed bodies and recorded outbound requests; status codes are compared as integers.
- **Go owns runtime proxy configuration after cutover.** `src/go/main.go` holds the config generated from Python's `PROFILES` at build time.
- **Python's table remains a frozen differential oracle** until archived.
- `--ports` is consumed by **`install.sh` only** (both now and after cutover).
- **`ds4-run` retains its launcher metadata** (`sentinel`, `dir`) — not forced to invoke the Go binary.
- **Statuslines retain their environment/default-port logic** — untouched in Phase A (they make unauthenticated `/__spend` calls; that's the current contract).
- Fix the known path bug separately: `skills/ds4-skill-family/bin/ds4-effort:16` (`DIR="$HOME/.claude-$PROFILE"`) maps `openrouter` → `~/.claude-openrouter`, which is wrong — the real openrouter dir is `~/.claude-or-ds4`. The installed slash command `src/commands/ds4-effort.md` resolves the dir correctly; the skill copy is the one to fix.

## Testing Strategy

1. **Differential suite gates the Phase A swap.** Go only takes over the port when the corpus passes byte-identical vs Python, including the outbound-request recorder.
2. Python's existing **280 tests** stay green during transition (they're the Python regression net). **Inventory them first**: identify which are Python-only regression tests vs contract tests that must be rewritten against Go before the flip — the old tests must not falsely serve as the production gate.
3. **Go breaker/vision/concurrency unit tests run BEFORE the swap** — table-driven breaker suite (threshold crossing, recovery, concurrent transitions) + `go test -race`. The diff harness can't prove thread-safety, so the race detector does.
4. **Test porting is a precondition of the swap, not deferred.** Port the breaker/vision/classifier unit tests to Go before the atomic flip, or formalize the diff harness as a permanent component. Keep Python and its tests until the Go proxy is stable through one explicit migration checkpoint; retire or port in a separate change.
5. **`/__spend` is not byte-parity-gated.** It's stateful filesystem/time logic, not the core HTTP relay. Go gets focused spend unit tests + one endpoint compatibility test (status, JSON shape, unauthenticated access). Detailed ledger/pricing tests stay at implementation level.

## Rollout

1. Build Go proxy side-by-side (`src/go/`), tests (`tests/diff/`).
2. Run differential suite until green. Python proxy stays live as oracle.
3. **Cutover is an operational sequence, not "one atomic commit".** Build and validate the Go binary → install it alongside the current one → update `install.sh` (both `--ports` calls at :84/:339, plist `ProgramArguments` at :355) → update the launchd plist → reload → verify the listener and differential smoke → keep rollback instructions. Source-control "same change" is not runtime atomicity. **Go toolchain preflight is an explicit dry/preflight phase before any write** — build validation runs before `install.sh` mutates profile files (install.sh:127,195 currently write first), and a failed build must leave profile files and plist unchanged (test this). Non-swallowed bootstrap failure (install.sh:435-442 currently prints inside an `if` and still exits 0).
4. **No "Python backup on another port".** Keep the Python *source* as the differential oracle and rollback target; Go is the only production listener.
5. Rewrite `install.sh` to build/install the Go binary; update `ds4-proxy-kickstart.sh` (Linux/no-launchd startup path — `python3 src/proxy.py &` in profile guides must become the Go binary), README, codemaps, profile guides, and the classifier-routing design spec. **Enumerate the exact stale claims**: CLAUDE.md:29, README.md:461-462, :495-498, codemaps/architecture.md:30-33,:51-70, profiles/*.md `python3 src/proxy.py` commands.
6. **Phase B tracked separately** — the credential/hardening migration (fail-closed classifier, no-follow redirect, size limits, saturation policy, authenticated `/__spend`, Keychain) is a distinct spec, built **in Go with Go integration tests** (Python stays frozen as the Phase A oracle). Idle-exit compatibility: Python's `sessions_live`/`claude_running` lifecycle semantics (not just active HTTP connections) must be ported with parity tests — Go must not exit between a session's requests.

## Out of Scope (Phase A)

- Phase B hardening (see two-phase structure above)
- Protocol changes / new features
- TCP chunk-boundary parity (not testable, not needed)
- Changing the classifier route or failover topology (as-is port)
- The statusline (untouched in Phase A)
