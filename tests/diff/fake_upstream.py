"""Canned upstreams for the differential harness.

Each shape returns the same bytes no matter which proxy asked, so a divergence
in status, headers, or body bytes is attributable to the proxy, never to the
upstream. The default handler answers /v1/messages with the same minimal SSE
stream Claude Code parses; retry serves a 503-then-200 sequence and failover
serves two upstreams per proxy (see run_diff.py).
"""
import json

# A minimal Anthropic-shaped SSE stream. The proxy streams these bytes back
# untouched, so both proxies must return byte-identical bodies. 'message_start'
# and 'message_delta' are deliberately the exact bytes a real upstream sends —
# whitespace and ordering included — so the harness catches a proxy that
# re-serializes the stream.
SSE_BODY = (b'event: message_start\n'
            b'data: {"type":"message_start","message":{"id":"msg_01",'
            b'"type":"message","role":"assistant","model":"deepseek/'
            b'deepseek-v4-flash-0731","content":[],"stop_reason":null,'
            b'"stop_sequence":null,"usage":{"input_tokens":5,"output_tokens":1}}}\n\n'
            b'event: content_block_start\n'
            b'data: {"type":"content_block_start","index":0,"content_block":'
            b'{"type":"text","text":""}}\n\n'
            b'event: content_block_delta\n'
            b'data: {"type":"content_block_delta","index":0,"delta":'
            b'{"type":"text_delta","text":"Hello"}}\n\n'
            b'event: content_block_stop\n'
            b'data: {"type":"content_block_stop","index":0}\n\n'
            b'event: message_delta\n'
            b'data: {"type":"message_delta","delta":{"stop_reason":"end_turn",'
            b'"stop_sequence":null},"usage":{"output_tokens":5}}\n\n'
            b'event: message_stop\n'
            b'data: {"type":"message_stop"}\n\n')

_JSON = {"content-type": "application/json"}
_SSE = {"content-type": "text/event-stream"}


def ok(*_):
    """200 with the canned SSE stream (drop-in handler signature)."""
    return 200, dict(_SSE), SSE_BODY


def _j(status, obj):
    return status, dict(_JSON), json.dumps(obj).encode()


def sse_ok(b):
    """200 text/event-stream, ignoring the request body."""
    return ok(b)


def retry_503_then_200(attempts=3, backoff=0.0):
    """A handler that 503s the first call and 200s the rest.

    The proxy's own retry loop re-drives the same URL, so each attempt arrives
    as a fresh HTTP request and this handler answers by arrival count. The
    harness asserts on retry_count, so a proxy that fails to retry (one
    request) or retries too many (more than attempts) is caught.
    """
    state = {"n": 0}
    try:
        import time
        sleep = time.sleep
    except Exception:                                   # pragma: no cover
        def sleep(_):                                   # noqa: E306
            pass

    def handler(body):
        state["n"] += 1
        n = state["n"]
        if n == 1:
            return 503, dict(_JSON), json.dumps(
                {"error": {"message": "overloaded", "type": "overloaded_error"}}).encode()
        if n <= attempts:
            if backoff:
                sleep(backoff)
            return ok(body)
        return _j(429, {"error": {"message": "too many attempts"}})

    return handler


def messages_503(*_):
    """Always 503 — trips the failover breaker."""
    return _j(503, {"error": {"message": "overloaded", "type": "overloaded_error"}})
