"""Route the auto-mode permission classifier to the Anthropic subscription.

The classifier is the small `ds4-high` + small-max_tokens + thinking-off call
that gates every tool call in auto mode. It is already a valid Anthropic
Messages request, so the proxy forwards it to api.anthropic.com instead of
DeepSeek. Auth is a long-lived subscription token minted by `claude
setup-token`, read from DS4_CLASSIFIER_TOKEN.

The env read is isolated behind classifier_token() so a future token source
(a keychain read once that path supports refresh) can slot in without touching
the relay.
"""
import os

# The Anthropic Messages endpoint the classifier is forwarded to. Overridable
# so tests can point it at a FakeUpstream without network.
CLASSIFIER_UPSTREAM = "https://api.anthropic.com/v1/messages"
ANTHROPIC_VERSION = "2023-06-01"


def classifier_token():
    """The subscription token, or None (fail open to ds4) when unset."""
    tok = os.environ.get("DS4_CLASSIFIER_TOKEN", "").strip()
    return tok or None


def is_classifier(payload, nothink_below):
    """True when the request is the auto-mode permission classifier.

    The classifier is ds4-high + a small max_tokens. It arrives with adaptive
    thinking (the proxy's own rewrite disables thinking at small max_tokens,
    so requiring thinking-off here would never match — the relay runs before
    that rewrite). Subagents also run at ds4-high but at a much larger
    max_tokens, so the size threshold separates them. The threshold is passed
    in (proxy's NOTHINK_BELOW) so the detector stays config-independent.
    """
    if not isinstance(payload, dict):
        return False
    mt = payload.get("max_tokens")
    return (payload.get("model") == "ds4-high"
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
