"""The differential corpus: the request shapes that distinguish the proxies.

Each case is (label, method, path, headers, body_json). The harness fires the
whole corpus at a proxy, then compares (status, managed headers, body bytes)
and the recorded outbound request against the Python reference run.

Body JSON deliberately stays JSON in source; run_diff.py re-serializes with
Python's json.dumps() so both proxies receive byte-identical bodies. Raw-body
byte comparison (not decoded JSON) is the whole point of the harness — the
Go side must reproduce Python's exact serialization, including key order,
whitespace, and escaping.

The models below are the sentinels the proxy maps onto a profile's real model
id. NOTHINK_BELOW defaults to 8192, so the 32000-token cases keep thinking and
the 2048/512-token classifier cases get it disabled.
"""
import json

# Sentinel tiers map onto per-profile models via proxy.EFFORT. A sentinel is
# ds4-<family>-<effort>: the family half picks the model, the effort half the
# default reasoning effort. ds4-flash-xhigh at a small max_tokens is the
# auto-mode permission classifier; ds4-pro-xhigh is the main loop;
# ds4-flash-xhigh at a large max_tokens is a subagent.
MAIN_LOOP = {"model": "ds4-pro-xhigh", "max_tokens": 32000,
             "thinking": {"type": "adaptive"},
             "messages": [{"role": "user", "content": "hi"}]}
SUBAGENT = {"model": "ds4-flash-xhigh", "max_tokens": 32000,
            "thinking": {"type": "adaptive"},
            "messages": [{"role": "user", "content": "summarize the diff"},
                         {"role": "assistant", "content": "Done."},
                         {"role": "user", "content": "now the tests"}]}
CLASSIFIER = {"model": "ds4-flash-xhigh", "max_tokens": 2048,
              "thinking": {"type": "adaptive"},
              "messages": [{"role": "user", "content": "hi"}]}
# An assistant turn that holds a tool_use block but no thinking block. The
# direct profile's inject=True must insert a placeholder; a proxy that drops
# it would be caught by the outbound body bytes.
THINKING_INJECT = {"model": "ds4-pro-xhigh", "max_tokens": 32000,
                   "messages": [{"role": "user", "content": "run it"},
                                {"role": "assistant", "content": [
                                    {"type": "tool_use", "id": "toolu_01",
                                     "name": "Bash", "input": {"command": "ls"}}]}]}
NON_ASCII = {"model": "ds4-pro-xhigh", "max_tokens": 32000,
             "messages": [{"role": "user", "content": "café 日本語 😀 <>& 1.0"}]}
# A temperature above 2^53: too big for a float64's exact integer range, so
# JSON decoders with int/float blindness would mangle it (9007199254740992
# round-trips, 9007199254740993 does not).
BIG_INT = {"model": "ds4-pro-xhigh", "max_tokens": 32000, "temperature": 9007199254740993}
# Exponent floats. jsonpy formats float64 with strconv.FormatFloat(t, 'f', -1, 64)
# and Python json.dumps uses repr(float) — both spell 1.5e-07 as "1.5e-07" and
# 1e100 as "1e+100". A JSON round-trip that re-decodes to a float would mangle
# the exponent spelling (e.g. 1e-07 -> 1e-8), so the harness pins both proxies
# reproducing the original bytes.
EXPONENT = {"model": "ds4-pro-xhigh", "max_tokens": 32000,
            "temperature": 1.5e-07, "top_p": 1e100}
RETRY_503 = {"model": "ds4-flash-xhigh", "max_tokens": 32000,
             "messages": [{"role": "user", "content": "retry me"}]}
FAILOVER = {"model": "ds4-flash-xhigh", "max_tokens": 32000,
            "messages": [{"role": "user", "content": "failover me"}]}
AUTH_MISSING = {"model": "ds4-flash-xhigh", "max_tokens": 32000,
                "messages": [{"role": "user", "content": "who am i"}]}
# The -medium half of the sentinel. Without this the corpus only ever exercises
# xhigh, so a family/effort decode that ignored the effort half would pass.
MEDIUM_EFFORT = {"model": "ds4-flash-medium", "max_tokens": 32000,
                 "messages": [{"role": "user", "content": "think less"}]}
# The pro family. On or-ds4 and nous it resolves to the same flash id as the
# flash family, so this pins that both proxies agree on the collapse rather
# than one of them inventing a pro model the upstream does not serve.
PRO_FAMILY = {"model": "ds4-pro-medium", "max_tokens": 32000,
              "messages": [{"role": "user", "content": "the big one"}]}
# A client-sent reasoning_effort, which is what /ds4-effort puts in the body.
# It must win over the sentinel's default on both sides; a proxy that let the
# sentinel clobber it would send "medium" here instead of "max".
CLIENT_EFFORT = {"model": "ds4-flash-medium", "reasoning_effort": "max",
                 "max_tokens": 32000,
                 "messages": [{"role": "user", "content": "think hard"}]}
# An effort level outside the accepted set. Both proxies must drop it and fall
# back to the sentinel default rather than forwarding it, because OpenRouter
# accepts the parameter and DeepSeek ignores unknown values silently.
BAD_EFFORT = {"model": "ds4-flash-medium", "reasoning_effort": "banana",
              "max_tokens": 32000,
              "messages": [{"role": "user", "content": "typo"}]}


def _fmt(obj):
    """Compact, key-ordered JSON, no ASCII escaping — the Python reference
    bytes both proxies must reproduce."""
    return json.dumps(obj, ensure_ascii=False, sort_keys=True,
                      separators=(",", ":")).encode("utf-8")


def cases():
    """The full corpus as (label, method, path, headers, body_bytes).

    Built in a function rather than a module constant so every case holds its
    own body bytes: bodies are immutable bytes (never shared and mutated by a
    rewrite), and the harness can re-fire the list for each proxy.
    """
    post = {"authorization": "Bearer client-token",
            "content-type": "application/json"}
    return [
        ("main-loop", "POST", "/v1/messages", dict(post), _fmt(MAIN_LOOP)),
        ("subagent", "POST", "/v1/messages", dict(post), _fmt(SUBAGENT)),
        ("classifier", "POST", "/v1/messages", dict(post), _fmt(CLASSIFIER)),
        ("thinking-inject", "POST", "/v1/messages", dict(post), _fmt(THINKING_INJECT)),
        ("non-ascii", "POST", "/v1/messages", dict(post), _fmt(NON_ASCII)),
        ("big-int", "POST", "/v1/messages", dict(post), _fmt(BIG_INT)),
        ("exponent-float", "POST", "/v1/messages", dict(post), _fmt(EXPONENT)),
        ("retry-503", "POST", "/v1/messages", dict(post), _fmt(RETRY_503)),
        ("failover", "POST", "/v1/messages", dict(post), _fmt(FAILOVER)),
        ("auth-missing", "POST", "/v1/messages", {}, _fmt(AUTH_MISSING)),
        ("medium-effort", "POST", "/v1/messages", dict(post), _fmt(MEDIUM_EFFORT)),
        ("pro-family", "POST", "/v1/messages", dict(post), _fmt(PRO_FAMILY)),
        ("client-effort", "POST", "/v1/messages", dict(post), _fmt(CLIENT_EFFORT)),
        ("bad-effort", "POST", "/v1/messages", dict(post), _fmt(BAD_EFFORT)),
        # /__spend is compared as status + JSON shape only: its body carries
        # timing-dependent numbers and is not byte-comparable.
        ("spend", "GET", "/__spend", {}, None),
    ]


# The header names a proxy may set or rewrite on the way through. Everything
# else is compared byte-for-byte (and most are omitted anyway — the harness
# only keeps these, plus x-ds4-upstream which names the serving origin).
MANAGED_HEADERS = {"content-type", "x-ds4-upstream"}
