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

    The classifier is ds4-high + a small max_tokens + thinking already off.
    Subagents also run at ds4-high but carry a large max_tokens with thinking
    on, so they fall through. The threshold is passed in (proxy's
    NOTHINK_BELOW) so the detector stays config-independent.
    """
    if not isinstance(payload, dict):
        return False
    mt = payload.get("max_tokens")
    return (payload.get("model") == "ds4-high"
            and isinstance(mt, int)
            and mt <= nothink_below
            and payload.get("thinking") == {"type": "disabled"})


def classifier_body(payload, model):
    """A copy of the classifier request pointed at Anthropic.

    The model becomes a real Anthropic id (haiku by default). The
    reasoning_effort the ds4 rewrite added is dropped — Anthropic does not
    accept it on the subscription. Everything else (messages, tools,
    max_tokens, thinking) is untouched: the body is already Anthropic-shaped.
    """
    body = dict(payload)
    body["model"] = model
    body.pop("reasoning_effort", None)
    return body


def anthropic_endpoint(payload, model):
    """The Anthropic request the classifier should become, or None if it
    cannot be served (no token). None means the caller fails open to ds4."""
    tok = classifier_token()
    if not tok:
        return None
    return classifier_body(payload, model), tok
