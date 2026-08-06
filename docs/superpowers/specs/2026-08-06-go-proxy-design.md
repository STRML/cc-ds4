# Go Proxy Rewrite — Design

**Date:** 2026-08-06
**Status:** Approved (design)
**Goal:** Replace the Python proxy with a Go implementation that is **byte-for-byte binary compatible** with the Python proxy's HTTP contract, proven by a differential test suite before the swap.

## Context

The Python proxy (`src/proxy.py`) relays Claude Code Messages requests to DeepSeek V4 providers. It has three hard-won behaviors: request rewriting (sentinel model → real model + effort, thinking-disable below a max_tokens threshold, ZDR block injection, missing-thinking-block injection), a failover circuit breaker (nous → direct on sustained transient errors), and classifier routing (auto-mode permission calls → Anthropic subscription, or or-ds4 ZDR route).

A prior diagnosis found proxy-side defects that a Go rewrite fixes by construction: unbounded thread-per-request (GIL contention, no cap), and a classifier `urlopen` with **no timeout** (a hung classifier blocks a thread with no deadline). The Go proxy uses goroutines with per-request deadlines and a bounded in-flight semaphore.

**Scope boundary:** This is a concurrency + timeout fix, not a protocol change. The wire contract is unchanged. The classifier route, failover breaker, vision rewrite, effort override, and socket activation all port over.

## Compatibility Contract

### What binary compatibility means

For the same request bytes, the Go proxy must return the same:

| Layer | Contract |
|---|---|
| Status code | identical, including the 409 ZDR refusal, 502 upstream failure, 200/error paths |
| Error JSON shape | `{"error":{"message":...}}` |
| Headers | `x-ds4-upstream`, `connection: close`; `content-length` set; never pass through `transfer-encoding`/`content-encoding`/`connection` from upstream |
| Body bytes | identical SSE frames (`event:`/`data:` lines) including injected placeholder `thinking` blocks |
| Rewrite semantics | sentinel→model, effort, thinking-disable, ZDR block, vision rewrite, thinking injection |
| Sidecar GETs | `/v1/models`, `/__spend` (statusline + CLI poll these) |

### What binary compatibility does NOT mean

**TCP chunk boundaries.** Go's http server chunks at different byte counts than Python's socketserver, even with identical content. Invisible to HTTP semantics and the CLI. Do not test it.

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
  effort.go         # per-profile effort-override file, read per request
  sockets.go        # launchd socket activation (fd inheritance), idle-exit
  config.go         # constants: NOTHINK_BELOW, TRANSIENT_STATUS, FAILOVER_*
```

- **net/http** with goroutine-per-request, bounded in-flight semaphore (default ~256), per-request read/write deadlines.
- Same PROFILES table as Python (ports 31500/31501/31502, upstreams, zdr, max_out, failover target).
- Classifier relays and upstream relay both get the same deadline bound — the no-timeout classifier `urlopen` defect is structurally absent.
- Socket activation: launchd hands listening fds; Go collects via the same `launch_activate_socket` ctypes path or `SOCKET_FD` env. Idle-exit (`DS4_IDLE_EXIT`) preserved.

### Differential harness (`tests/diff/`)

The suite that proves compatibility. **Not golden-record** — Python stays the living oracle.

```
tests/diff/
  run_diff.py       # orchestrator: boots both proxies + fake upstream, fires corpus
  fake_upstream.py  # canned SSE responses
  corpus.py         # the request corpus (below)
```

**Method:** a fake upstream returns canned SSE responses. The harness fires identical requests at both proxies and asserts byte-for-byte equality of status + headers + body.

**Corpus:**
- main-loop tier (`ds4-xhigh`), subagent tier (`ds4-high`), classifier tier (small `max_tokens` → thinking disabled)
- vision requests (image blocks → placeholders)
- thinking-injection cases (missing thinking blocks)
- error paths: 429/503/524 upstream, 401, retry-then-forward
- failover transition (nous → direct)
- `/v1/models`, `/__spend`

**Live smoke:** run `claude -p` against each proxy on the same prompt, diff the resulting transcript JSONL. The "absolutely" layer.

## Behavior to Port (Risk List)

The codemap names these; a port drifts silently without them:

- `rewrite()` — sentinel→model + effort, thinking-disable below `NOTHINK_BELOW`, ZDR block, max_tokens clamp
- Failover breaker — window/rate/probes, consecutive-clean-probe recovery (500+ regression tests)
- Classifier routing — anthropic/zdr/ds4 + fail-open chain (zdr→anthropic→ds4)
- Vision child-process rewriting (env scrubbed), content-hash cache
- Thinking injection (direct profile, `PLACEHOLDER` block)
- Socket activation + idle-exit
- Sidecar GETs (`/v1/models`, `/__spend`)

## Testing Strategy

1. **Differential suite gates the swap.** Go only takes over the port when the full corpus passes byte-identical vs Python.
2. Python's existing 269 tests stay green during transition (they're the Python proxy's regression net).
3. After the swap, port the relevant Python unit tests to Go where they assert behavior the diff corpus can't reach (breaker internals, vision cache).

## Rollout

1. Build Go proxy side-by-side (`src/go/`), tests (`tests/diff/`).
2. Run differential suite until green. Python proxy stays live as oracle.
3. When green: flip the launchd plist to the Go binary, keep Python as a backup route on another port.
4. Observe a nightly of real load; on no drift, remove `src/proxy.py`.
5. Rewrite `install.sh` to build/install the Go binary; update `ds4-proxy-kickstart.sh`.

## Out of Scope

- Protocol changes / new features
- TCP chunk-boundary parity (not testable, not needed)
- Changing the classifier route or failover topology
- The statusline (separate Python code, untouched)
