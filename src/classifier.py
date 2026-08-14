"""Route the auto-mode permission classifier to a trusted boundary.

The classifier is the small flash-tier + small-max_tokens + thinking-off call
that gates every tool call in auto mode. It is already a valid Anthropic
Messages request, so the proxy forwards it to a provider that understands that
shape. Two routes are offered:

* **Anthropic subscription** (DS4_CLASSIFIER=anthropic, the default). Auth is a
  long-lived subscription token minted by `claude setup-token`, read from
  DS4_CLASSIFIER_TOKEN. The security gate lives in a trusted boundary.
* **or-ds4 / OpenRouter ZDR** (DS4_CLASSIFIER=zdr). The classifier is sent to
  the profile's OpenRouter upstream as a Messages request with the ZDR block
  forced on. No training, no subscription token spent — but the gate now runs
  on DeepSeek V4 Flash via OpenRouter instead of Anthropic. Opt-in.

The env reads are isolated behind classifier_token() / or_ds4_endpoint() so a
future token source (a keychain read once that path supports refresh) can slot
in without touching the relay.
"""
import os

# The Anthropic Messages endpoint the classifier is forwarded to. Overridable
# so tests can point it at a FakeUpstream without network.
CLASSIFIER_UPSTREAM = "https://api.anthropic.com/v1/messages"
ANTHROPIC_VERSION = "2023-06-01"
# The OpenRouter path the or-ds4 classifier rides. The or-ds4 profile's
# upstream is https://openrouter.ai/api, so the messages path is /v1/messages
# — a leading /api here would double up. OpenRouter accepts the Anthropic
# Messages shape here (the profile's main-loop and subagent requests ride it
# every day) and returns the same Anthropic-shaped stream Claude Code already
# parses, so the classifier body needs only a model swap.
ORDS4_PATH = "/v1/messages"


def classifier_token():
    """The subscription token, or None (fail open to ds4) when unset."""
    tok = os.environ.get("DS4_CLASSIFIER_TOKEN", "").strip()
    return tok or None


# The sentinels the classifier can arrive under: the flash family, whichever
# slot Claude Code maps its small fast model to. The pro family is the main
# loop and is never the classifier, so excluding it here costs nothing and
# keeps a 200k-token main-loop request from ever being mistaken for the gate.
# tests/test_classifier.py pins this against proxy.EFFORT so a future sentinel
# rename cannot silently orphan the detector.
CLASSIFIER_TIERS = ("ds4-flash-xhigh", "ds4-flash-medium")


def is_classifier(payload, nothink_below):
    """True when the request is the auto-mode permission classifier.

    The classifier is a flash-family sentinel + a small max_tokens. It arrives
    with adaptive thinking (the proxy's own rewrite disables thinking at small
    max_tokens, so requiring thinking-off here would never match — the relay
    runs before that rewrite). Subagents ride the same flash sentinel but at a
    much larger max_tokens, so the size threshold separates them. The threshold
    is passed in (proxy's NOTHINK_BELOW) so the detector stays
    config-independent.
    """
    if not isinstance(payload, dict):
        return False
    mt = payload.get("max_tokens")
    return (payload.get("model") in CLASSIFIER_TIERS
            and isinstance(mt, int)
            and mt <= nothink_below)


# The request keys that belong on an Anthropic Messages call. Everything else
# in the ds4 payload — provider (zdr block), metadata, reasoning_effort — is
# ds4-specific and must not leave the proxy. Whitelisting keeps a misdetected
# request from carrying ds4 body shape to Anthropic.
_ANTHROPIC_KEYS = ("model", "max_tokens", "thinking", "messages", "tools",
                   "tool_choice", "system", "stream", "temperature")


def classifier_body(payload, model):
    """The classifier request pointed at Anthropic.

    Builds a body from only the Anthropic-relevant keys, with model set to a
    real Anthropic id (haiku by default). The ds4-specific fields (provider,
    reasoning_effort, metadata) are dropped — Anthropic does not accept them on
    the subscription, and they must not carry ds4 body shape across.
    """
    body = {k: v for k, v in payload.items() if k in _ANTHROPIC_KEYS}
    body["model"] = model
    return body


def anthropic_endpoint(payload, model):
    """The Anthropic request the classifier should become, or None if it
    cannot be served (no token). None means the caller fails open to ds4."""
    tok = classifier_token()
    if not tok:
        return None
    return classifier_body(payload, model), tok


# The request keys that belong on an or-ds4 classifier call. The classifier
# body is Anthropic-shaped, so it carries ds4-specific keys (reasoning_effort,
# provider, metadata) that must not cross. The whitelist keeps the relay from
# shipping ds4 body shape to OpenRouter. max_tokens and thinking are kept:
# OpenRouter's /v1/messages accepts them and the classifier relies on both.
_ORDS4_KEYS = _ANTHROPIC_KEYS


def or_ds4_body(payload, model):
    """The classifier with the model swapped to the or-ds4 route's model.

    Same whitelist as the Anthropic body — the or-ds4 route is OpenRouter's
    /v1/messages, which accepts Anthropic body shape. Thinking is forced off:
    the classifier is always small, and no provider serving V4 implements
    Claude Code's `thinking: adaptive` (the proxy's own rewrite disables it
    below NOTHINK_BELOW). The ZDR provider block is injected by the relay, not
    here: the block is route-specific and must not leak into classifier.py.
    """
    body = {k: v for k, v in payload.items() if k in _ORDS4_KEYS}
    body["model"] = model
    body["thinking"] = {"type": "disabled"}
    return body


def or_ds4_endpoint(payload, model, upstream, key):
    """The OpenRouter ZDR classifier request, or None when not servable.

    upstream is the profile's OpenRouter base (e.g. https://openrouter.ai/api).
    Returns (body, full_url, bearer_key) so the relay can add the ZDR block and
    headers. None means the caller must fall back (the proxy decides where).
    """
    if not key:
        return None
    body = or_ds4_body(payload, model)
    return body, upstream.rstrip("/") + ORDS4_PATH, key
