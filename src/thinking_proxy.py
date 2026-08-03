#!/usr/bin/env python3
"""Turn off DeepSeek V4 thinking on Claude Code's small utility calls.

Claude Code sends thinking={"type":"adaptive","display":"omitted"} on every
request. DeepSeek does not implement "adaptive", so V4 stays in its default
thinking mode, and two rules of that mode break the small calls:

  1. A utility call spends its whole budget on the thinking block and gets cut
     off before the tool call comes out. Measured against a classifier-shaped
     forced decision at max_tokens=512: 3 of 5 returned stop_reason=max_tokens,
     two of those with no tool_use block at all. At 1024 and 2048, 0 of 5.
  2. tool_choice naming a specific tool is rejected outright while thinking is
     on: 400 "Thinking mode does not support this tool_choice". auto, none, and
     omitted are accepted.

The permission classifier behind `defaultMode: auto` is one of these calls, so
both failures read as intermittent: whether the tool call survives the budget
depends on how long the model thought that time.

Sending thinking={"type":"disabled"} clears both. The Anthropic-compatible
endpoint honours it, unlike reasoning_effort on the OpenAI-compatible one --
that difference is why the published workarounds all reach for a translating
proxy instead. Measured with it off: output 141-175 tokens instead of 386-512,
2.0s instead of 5.2s, and named tool_choice accepted.

Usage:
    python3 src/thinking_proxy.py &
    # then in ~/.claude-ds4/settings.json:
    #   "ANTHROPIC_BASE_URL": "http://127.0.0.1:31500"
"""
import http.server, json, os, urllib.request, urllib.error

UPSTREAM = os.environ.get("DS4_UPSTREAM", "https://api.deepseek.com/anthropic")
PORT = int(os.environ.get("DS4_PROXY_PORT", "31500"))
VERBOSE = os.environ.get("DS4_VERBOSE") == "1" or os.environ.get("DS4_DEBUG") == "1"

# Main-loop and subagent requests arrive with max_tokens=32000. Utility calls
# are an order of magnitude smaller. Nothing observed lands in between, so the
# exact value is not delicate -- it only has to separate the two populations.
NOTHINK_BELOW = int(os.environ.get("DS4_NOTHINK_BELOW", "8192"))

# Guard for a third rule: an assistant message carrying a tool_use must carry
# its thinking block too, or the request 400s with "The `content[].thinking` in
# the thinking mode must be passed back to the API." Claude Code 2.x does replay
# it -- verified on the wire, so this has not been seen to fire. It exists
# because a path that ever drops the block kills the session outright, and the
# repair is free: DeepSeek does not validate the signature.
INJECT = os.environ.get("DS4_INJECT_THINKING", "1") == "1"

DISABLED = {"type": "disabled"}
PLACEHOLDER = {"type": "thinking", "thinking": "(elided)", "signature": "ds4-proxy"}


def inject_missing_thinking(payload):
    """Count of assistant tool_use messages given a placeholder thinking block."""
    n = 0
    for m in payload.get("messages") or []:
        if m.get("role") != "assistant":
            continue
        blocks = m.get("content")
        if not isinstance(blocks, list):
            continue
        kinds = {b.get("type") for b in blocks if isinstance(b, dict)}
        if "tool_use" in kinds and "thinking" not in kinds:
            blocks.insert(0, dict(PLACEHOLDER))
            n += 1
    return n


def rewrite(payload):
    """Edit payload in place. Returns a log line, or None if left alone."""
    notes = []
    want = payload.get("max_tokens")
    if isinstance(want, int) and want <= NOTHINK_BELOW:
        payload["thinking"] = dict(DISABLED)
        notes.append(f"max_tokens={want} -> thinking disabled")
    # With thinking off the endpoint stops asking for the block, so only repair
    # the history on requests that still have it on.
    elif INJECT:
        n = inject_missing_thinking(payload)
        if n:
            notes.append(f"injected {n} missing thinking block(s)")
    return ", ".join(notes) or None


class Handler(http.server.BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"

    def log_message(self, *a):
        pass

    def do_POST(self):
        body = self.rfile.read(int(self.headers.get("content-length", 0)))
        try:
            payload = json.loads(body)
        except ValueError:
            payload = None

        if isinstance(payload, dict):
            note = rewrite(payload)
            body = json.dumps(payload).encode()
            if VERBOSE and note:
                print("  " + note, flush=True)

        req = urllib.request.Request(UPSTREAM.rstrip("/") + self.path, data=body, method="POST")
        for k, v in self.headers.items():
            if k.lower() not in ("host", "content-length", "accept-encoding"):
                req.add_header(k, v)
        req.add_header("content-length", str(len(body)))

        try:
            up = urllib.request.urlopen(req)
        except urllib.error.HTTPError as e:
            up = e
        except Exception as e:
            msg = json.dumps({"error": {"message": f"proxy upstream failure: {e}"}}).encode()
            self.send_response(502)
            self.send_header("content-type", "application/json")
            self.send_header("content-length", str(len(msg)))
            self.send_header("connection", "close")
            self.end_headers()
            self.wfile.write(msg)
            return

        if VERBOSE and up.status != 200:
            print(f"  <- {up.status}", flush=True)

        self.send_response(up.status)
        for k, v in up.headers.items():
            if k.lower() not in ("transfer-encoding", "content-encoding", "connection"):
                self.send_header(k, v)
        self.send_header("connection", "close")
        self.end_headers()
        while True:
            chunk = up.read(8192)
            if not chunk:
                break
            self.wfile.write(chunk)
            self.wfile.flush()
        up.close()


if __name__ == "__main__":
    print(f"ds4 thinking proxy on 127.0.0.1:{PORT} -> {UPSTREAM} "
          f"(no thinking at or below max_tokens={NOTHINK_BELOW})", flush=True)
    http.server.ThreadingHTTPServer(("127.0.0.1", PORT), Handler).serve_forever()
