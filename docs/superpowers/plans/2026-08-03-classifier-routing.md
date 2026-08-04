# Classifier routing to Anthropic — implementation plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Route the auto-mode permission classifier (the small `ds4-high` + `max_tokens≈2112` + thinking-disabled call) to the Anthropic subscription instead of DeepSeek, by having the proxy forward the already-valid Anthropic request to `api.anthropic.com` with a `claude setup-token` subscription token. Fail open to ds4 on any error. Default on, per-profile opt-out.

**Architecture:** The shared proxy (`src/proxy.py`) already rewrites and relays per profile. Add a classifier pass: detect the signature, rewrite `model` to an Anthropic id, strip `reasoning_effort`, forward the body to `api.anthropic.com/v1/messages` with `Authorization: Bearer <token>`, relay the reply. Auth = a long-lived subscription token from `claude setup-token`, read from `DS4_CLASSIFIER_TOKEN`. Everything else (main loop, subagents, vision) untouched.

**Tech Stack:** Python 3.9+ stdlib only (matches the repo). No new dependencies.

## Global Constraints

- **stdlib only** — no new dependencies.
- **Fail open** — any failure on the Anthropic path falls through to the ds4 path; a hard error is never returned (except a forwarded Anthropic 400).
- **No new credential surface** — a long-lived token from `claude setup-token`, read from `DS4_CLASSIFIER_TOKEN`. Held in process memory, never logged.
- **Only the classifier moves** — main loop (`ds4-xhigh`) and subagents (`ds4-high` + thinking on / large `max_tokens`) stay on the ds4 path.
- **Existing suite stays green** (`python3 -m unittest discover -s tests`).
- Design doc: `docs/superpowers/specs/2026-08-03-classifier-routing-design.md`.

---

### Task 1: the token reader

**Files:**
- Create: `src/classifier.py` (sibling of `vision.py`; keeps `proxy.py` from growing)
- Test: `tests/test_classifier.py`

**Interfaces:**
- `classifier_token() -> str | None` — returns the subscription token from
  `DS4_CLASSIFIER_TOKEN`. `None` when unset/empty → caller fails open to ds4.
  (A future token source — e.g. a keychain read once that path supports
  refresh — can slot in behind this one function.)
- The env read is isolated in one function so tests can set/clear it.

- [ ] **Step 1: Write the failing test**

```python
# tests/test_classifier.py
import os, sys, unittest

sys.path.insert(0, "src")
import classifier as c


class TokenTest(unittest.TestCase):
    def setUp(self):
        self._old = os.environ.get("DS4_CLASSIFIER_TOKEN")
        os.environ["DS4_CLASSIFIER_TOKEN"] = "sk-ant-oat01-test"

    def tearDown(self):
        if self._old is None:
            os.environ.pop("DS4_CLASSIFIER_TOKEN", None)
        else:
            os.environ["DS4_CLASSIFIER_TOKEN"] = self._old

    def test_token_reads_from_env(self):
        self.assertEqual(c.classifier_token(), "sk-ant-oat01-test")

    def test_empty_token_is_none(self):
        os.environ["DS4_CLASSIFIER_TOKEN"] = ""
        self.assertIsNone(c.classifier_token())

    def test_missing_env_is_none(self):
        os.environ.pop("DS4_CLASSIFIER_TOKEN", None)
        self.assertIsNone(c.classifier_token())
```

- [ ] **Step 2: Make it pass**

```python
# src/classifier.py
"""Route the auto-mode permission classifier to the Anthropic subscription.

The classifier is the small `ds4-high` + small-max_tokens + thinking-off call
that gates every tool call in auto mode. proxy.py forwards it to
api.anthropic.com instead of DeepSeek. Auth is a long-lived subscription
token minted by `claude setup-token`, read from DS4_CLASSIFIER_TOKEN. The
env read is isolated behind classifier_token() so a future token source can
slot in.
"""
import os


def classifier_token():
    """The subscription token, or None (fail open to ds4) when unset."""
    tok = os.environ.get("DS4_CLASSIFIER_TOKEN", "").strip()
    return tok or None
```

---

### Task 2: classifier detection

**Files:**
- `src/classifier.py` (add a detector)
- Test: `tests/test_classifier.py`

**Interface:**
- `is_classifier(payload, nothink_below) -> bool` — True when the request is the permission classifier: `model == "ds4-high"` **and** `max_tokens` is an int `≤ NOTHINK_BELOW` (proxy passes the threshold in).

The `thinking` value is deliberately **not** part of the signature. The classifier arrives with adaptive thinking (Claude Code sends it on every request); the proxy's rewrite disables it at small max_tokens, and the classifier relay runs *before* that rewrite. Requiring thinking-off would never match.

The detector lives in `classifier.py`, taking the threshold as an argument so it stays independent of proxy config.

- [ ] **Step 1: failing tests**

```python
class DetectTest(unittest.TestCase):
    def payload(self, **kw):
        # The classifier arrives with adaptive thinking.
        p = {"model": "ds4-high", "max_tokens": 2112,
             "thinking": {"type": "adaptive", "display": "omitted"},
             "messages": [{"role": "user", "content": "hi"}]}
        p.update(kw)
        return p

    def test_classifier_signature_is_detected(self):
        self.assertTrue(c.is_classifier(self.payload(), 8192))

    def test_classifier_with_adaptive_thinking_is_detected(self):
        self.assertTrue(c.is_classifier(self.payload(), 8192))

    def test_main_loop_is_not_classifier(self):
        self.assertFalse(c.is_classifier(
            self.payload(model="ds4-xhigh", max_tokens=32000,
                         thinking={"type": "adaptive", "display": "omitted"}), 8192))

    def test_subagent_is_not_classifier(self):
        # ds4-high but large max_tokens — the subagent tier
        self.assertFalse(c.is_classifier(
            self.payload(max_tokens=32000), 8192))

    def test_max_tokens_above_threshold_is_not_classifier(self):
        self.assertFalse(c.is_classifier(self.payload(max_tokens=8193), 8192))

    def test_non_integer_max_tokens_is_not_classifier(self):
        self.assertFalse(c.is_classifier(self.payload(max_tokens="big"), 8192))
```

- [ ] **Step 2: make it pass**

```python
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
```

---

### Task 3: the classifier relay

**Files:**
- `src/classifier.py` (add `classifier_body` + a relay helper)
- `src/proxy.py` (hook into `_relay`)
- Test: `tests/test_classifier.py` + `tests/test_proxy_http.py`

**Interface:**
- `classifier_body(payload, model) -> dict` — a shallow copy of the payload with `model` set to the Anthropic id and `reasoning_effort` removed. Everything else (messages, tools, max_tokens, thinking) unchanged.

The actual HTTP forward happens in `proxy.py`'s `_relay`, reusing the existing streaming relay. The new code:

1. Before the ds4 `rewrite`, check `is_classifier(payload, NOTHINK_BELOW)` and the profile route.
2. If classifier + route `anthropic`: build the Anthropic body, forward to `https://api.anthropic.com/v1/messages` with the `DS4_CLASSIFIER_TOKEN`. On any failure (network error, 5xx, missing token), **restore the ds4 shape** and fall through to the existing relay. A **400 from Anthropic** is forwarded as-is (a malformed request from Claude Code shouldn't be masked by failing open — see the design doc's failure handling).
3. The existing relay logic is unchanged for everything else.

To keep `_relay` readable, add a module-level helper in `classifier.py`:

```python
def anthropic_endpoint(payload, model):
    """The Anthropic request the classifier should become, or None if it
    cannot be served (no token). None means the caller fails open to ds4."""
    tok = classifier_token()
    if not tok:
        return None
    body = classifier_body(payload, model)
    return body, tok
```

- [ ] **Step 1: failing tests (classifier_body + relay)**

```python
class BodyTest(unittest.TestCase):
    def test_classifier_body_swaps_model_and_drops_effort(self):
        p = {"model": "ds4-high", "max_tokens": 2112,
             "reasoning_effort": "high",
             "thinking": {"type": "disabled"},
             "messages": [{"role": "user", "content": "hi"}]}
        out = c.classifier_body(p, "claude-haiku-4-5")
        self.assertEqual(out["model"], "claude-haiku-4-5")
        self.assertNotIn("reasoning_effort", out)
        self.assertEqual(out["max_tokens"], 2112)
        self.assertEqual(out["thinking"], {"type": "disabled"})
        # the original is untouched
        self.assertEqual(p["model"], "ds4-high")

    def test_anthropic_endpoint_returns_none_without_token(self):
        with mock.patch.object(c, "classifier_token", return_value=None):
            self.assertIsNone(c.anthropic_endpoint({}, "m"))
```

(Relay integration tests live in `test_proxy_http.py` — see Task 4.)

- [ ] **Step 2: make it pass**

```python
def classifier_body(payload, model):
    """A copy of the classifier request pointed at Anthropic."""
    body = dict(payload)
    body["model"] = model
    body.pop("reasoning_effort", None)
    return body


def anthropic_endpoint(payload, model):
    tok = classifier_token()
    if not tok:
        return None
    return classifier_body(payload, model), tok
```

---

### Task 4: wire the relay into the proxy

**Files:**
- `src/proxy.py`
- Test: `tests/test_proxy_http.py`

Add to `proxy.py`:

1. **Config**: a module constant `CLASSIFIER_ROUTE = os.environ.get("DS4_CLASSIFIER", "anthropic")`, `CLASSIFIER_MODEL = os.environ.get("DS4_CLASSIFIER_MODEL", "claude-sonnet-5")`, and the import `classifier as _classifier`. No per-profile `classifier` row — one process serves every profile and the classifier is the same call on all of them, so a single global env switch is the opt-out (matching how `VISION` gates the vision rewrite). Sonnet 5 (not haiku) is the default because the profiles advertise a 1M context window and a 200K-window classifier overflows a long auto-mode session.

2. **In `_relay`**, before the existing `rewrite(payload, cfg)`:

```python
if (CLASSIFIER_ROUTE == "anthropic"
        and _classifier.is_classifier(payload, NOTHINK_BELOW)):
    ep = _classifier.anthropic_endpoint(payload, CLASSIFIER_MODEL)
    if ep is not None:
        body2, token = ep
        ok = _relay_to_anthropic(body2, token, CLASSIFIER_UPSTREAM)
        if ok:
            return
        # fall through to ds4 on any failure
```

Where `_relay_to_anthropic` is a small method that posts the body to
`https://api.anthropic.com/v1/messages` with the bearer token, streams the
reply back, and returns True on success / False on failure. It mirrors the
existing relay's streaming loop.

A **400 from Anthropic** is forwarded as-is (not failed-open) — see the design
doc's failure handling.

3. The ds4 `rewrite` runs only when the classifier relay did not take over. The
   classifier body already has `thinking` disabled and a real Anthropic model,
   so the ds4 `NOTHINK_BELOW` and sentinel logic must NOT touch it — hence the
   early return.

- [ ] **Step 1: failing integration tests** (in `tests/test_proxy_http.py`)

```python
class ClassifierRelayTest(unittest.TestCase):
    def test_classifier_goes_to_anthropic(self):
        srv = make_server(dict(proxy.PROFILES["nous"], classifier="anthropic"), None)
        # mock urlopen: assert the request hits api.anthropic.com with the token
        with mock.patch("urllib.request.urlopen") as mu:
            mu.return_value = helpers.fake_response(200, '{"ok":true}')
            with mock.patch.object(proxy._classifier, "classifier_token", return_value="tok"):
                status, body = post(srv, "/v1/messages",
                                    {"model": "ds4-high", "max_tokens": 2112,
                                     "thinking": {"type": "disabled"},
                                     "messages": []})
        self.assertEqual(status, 200)
        req = mu.call_args[0][0]
        self.assertIn("api.anthropic.com", req.full_url)
        self.assertEqual(req.get_header("Authorization"), "Bearer tok")

    def test_classifier_fails_open_to_ds4_without_token(self):
        srv = make_server(dict(proxy.PROFILES["nous"], classifier="anthropic"), None)
        with mock.patch.object(proxy._classifier, "classifier_token", return_value=None), \
             mock.patch("urllib.request.urlopen") as mu:
            mu.return_value = helpers.fake_response(200, "{}")
            status, _ = post(srv, "/v1/messages",
                             {"model": "ds4-high", "max_tokens": 2112,
                              "thinking": {"type": "disabled"}, "messages": []})
        self.assertEqual(status, 200)
        # the ds4 upstream was hit, not anthropic
        self.assertIn("inference-api.nousresearch.com", mu.call_args[0][0].full_url)

    def test_classifier_opt_out_stays_on_ds4(self):
        srv = make_server(dict(proxy.PROFILES["nous"], classifier="ds4"), None)
        with mock.patch("urllib.request.urlopen") as mu:
            mu.return_value = helpers.fake_response(200, "{}")
            post(srv, "/v1/messages",
                 {"model": "ds4-high", "max_tokens": 2112,
                  "thinking": {"type": "disabled"}, "messages": []})
        self.assertIn("inference-api.nousresearch.com", mu.call_args[0][0].full_url)

    def test_subagent_request_never_goes_to_anthropic(self):
        srv = make_server(dict(proxy.PROFILES["nous"], classifier="anthropic"), None)
        with mock.patch.object(proxy._classifier, "classifier_token", return_value="tok"), \
             mock.patch("urllib.request.urlopen") as mu:
            mu.return_value = helpers.fake_response(200, "{}")
            post(srv, "/v1/messages",
                 {"model": "ds4-high", "max_tokens": 32000,
                  "thinking": {"type": "adaptive"}, "messages": []})
        self.assertIn("inference-api.nousresearch.com", mu.call_args[0][0].full_url)
```

> These use `helpers.fake_response` if present; if not, a small in-test stub
> object with `.status`, `.headers`, `.read()`, `.close()`.

- [ ] **Step 2: make them pass** (wire the relay, keep the existing suite green)

---

### Task 5: install.sh env + docs note

**Files:**
- `install.sh` (document the new DS4_* knobs; no functional change required —
  the defaults need nothing baked)
- `README.md` (short note under the classifier section)
- Test: `tests/test_install.py` only if the env table requires it

Add to the README's classifier discussion a sentence that the classifier now
routes to the Anthropic subscription, that this is the trusted boundary, and
that `DS4_CLASSIFIER=ds4` opts out.

Update the install.sh comment block listing DS4_* knobs to include
`DS4_CLASSIFIER`, `DS4_CLASSIFIER_MODEL`.

---

## Verification

- `python3 -m unittest discover -s tests -v` — all green, including the 178 pre-existing.
- `python3 -c "import src.classifier"` — imports clean on 3.9.
- Manual smoke (optional, after merge): with a live session in auto mode, confirm
  the proxy log shows a classifier request forwarded to Anthropic, not ds4.

## Not in scope (follow-ups, not this PR)

- Routing subagents to Anthropic (deliberately excluded — legitimate work on
  the cheap tier).
- A vision-native or Anthropic-native profile.
- Making the loopback listener authenticated (pre-existing limitation).
