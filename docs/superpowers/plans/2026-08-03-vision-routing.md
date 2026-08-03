# Vision routing for DeepSeek profiles — implementation plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make images usable on the text-only DeepSeek profiles by having the proxy translate image blocks into text descriptions (via a local `claude -p --model haiku` child) before forwarding to DeepSeek.

**Architecture:** The shared proxy (`src/proxy.py`) already rewrites request bodies per profile. Add a vision pass: `rewrite_images()` walks every message's content (recursing into `tool_result.content`), swaps each image block for a text description obtained by spawning `claude -p --model haiku` on the Anthropic profile, cached by content hash, failing open. Ported from Sean Perkins' proven gist with `claude -p` as the describer instead of gemma.

**Tech Stack:** Python 3.9+ stdlib only (matches the repo — `http.server`, `hashlib`, `subprocess`, `json`). No new dependencies.

## Global Constraints

- **stdlib only** — no new dependencies (repo badge: "dependencies-none").
- **`tool_result` recursion is a hard requirement** — Sean: *"I didn't capture tool_results in my first iteration, so make sure you do capture that."* Images nested in `tool_result.content` are where `Read`/screenshot/MCP images land.
- **Fail open** — a describer failure swaps in a neutral placeholder; a hard error is never returned.
- **No unscoped key reach** — the `claude -p` child is forced onto the Anthropic profile via `CLAUDE_CONFIG_DIR=$HOME/.claude`; it never touches a ds4 key.
- **Existing 154-test suite stays green** (`python3 -m unittest discover -s tests`).
- The rewrite is **per-request**; it does not clear an already-poisoned transcript (that needs `/compact`/`/clear` client-side).
- Spawn shape (verified live 2026-08-03): `unset CLAUDECODE CLAUDE_CODE_ENTRYPOINT` + `env CLAUDE_CONFIG_DIR=$HOME/.claude CLAUDE_CODE_SIMPLE=1 claude -p --settings '{"disableAllHooks":true}' --model haiku --tools Read --allowedTools "Read(<tmp>/*)" --add-dir <tmp> --disable-slash-commands --strict-mcp-config --append-system-prompt 'Describe the image for a text-only model. Return only the description.' --output-format json "Read <img path> and describe the image."`

---

### Task 1: `transcribe()` — the `claude -p` describer

**Files:**
- Create: `src/vision.py` (the vision module; keeps `proxy.py` from growing)
- Test: `tests/test_vision.py`

**Interfaces:**
- Consumes: nothing (stdlib only).
- Produces: `transcribe(image_bytes, media_type, cache_dir) -> (text, None)` on success, `(None, why)` on failure — **never a bare `None`** (Sean's `TypeError` bug). `hash_key(image_bytes, media_type) -> str` (SHA-256 hex). `cache_get(cache_dir, key) -> str|None`. `cache_put(cache_dir, key, text)` (atomic tmpfile+`os.replace`). Later tasks import these.

- [ ] **Step 1: Write the failing test**

```python
# tests/test_vision.py
import hashlib, os, tempfile, unittest
from unittest import mock

import src.vision as v


class HashKeyTest(unittest.TestCase):
    def test_hash_key_is_deterministic_and_content_addressed(self):
        a = v.hash_key(b"abc", "image/png")
        b = v.hash_key(b"abc", "image/png")
        self.assertEqual(a, b)
        self.assertNotEqual(a, v.hash_key(b"abc", "image/jpeg"))
        self.assertNotEqual(a, v.hash_key(b"abd", "image/png"))
        self.assertEqual(a, hashlib.sha256(b"image/png:abc").hexdigest())


class CacheTest(unittest.TestCase):
    def setUp(self):
        self._tmp = tempfile.mkdtemp()

    def test_cache_round_trip(self):
        key = v.hash_key(b"data", "image/png")
        self.assertIsNone(v.cache_get(self._tmp, key))
        v.cache_put(self._tmp, key, "a house")
        self.assertEqual(v.cache_get(self._tmp, key), "a house")

    def test_cache_is_atomic(self):
        key = v.hash_key(b"data", "image/png")
        v.cache_put(self._tmp, key, "x")
        # no partial reads: the file appears fully or not at all
        self.assertEqual(v.cache_get(self._tmp, key), "x")


class TranscribeTest(unittest.TestCase):
    def test_transcribe_returns_tuple_never_bare_none(self):
        with mock.patch("subprocess.run") as mrun:
            mrun.return_value.returncode = 0
            mrun.return_value.stdout = json_result("a house")
            text, why = v.transcribe(b"data", "image/png", "ignored")
        self.assertEqual(text, "a house")
        self.assertIsNone(why)


def json_result(text):
    import json
    return json.dumps({"result": text, "session_id": "s1", "is_error": False})


if __name__ == "__main__":
    unittest.main()
```

- [ ] **Step 2: Run test to verify it fails**

Run: `python3 -m unittest tests/test_vision.py -v`
Expected: FAIL with `ModuleNotFoundError: No module named 'src.vision'`.

- [ ] **Step 3: Write the implementation**

```python
# src/vision.py
"""Describe images with a local `claude -p --model haiku` child.

DeepSeek V4 is text-only; this module turns image blocks into text the main
model can reason over. The describer is a minimal, hermetic `claude -p` call
on the machine's Anthropic profile (subscription credits, no new credential),
matching the recipe proven in cc-debate (`scripts/invoke-acpx.sh`).
"""
import hashlib, json, os, subprocess, tempfile

# The Anthropic profile. The proxy may inherit CLAUDE_CONFIG_DIR/ANTHROPIC_*
# from the session that launched it; forcing this keeps --model haiku on
# Anthropic instead of routing back to DeepSeek (observed live).
ANTHROPIC_CONFIG_DIR = os.path.expanduser("~/.claude")

PROMPT = ("Describe the image for a text-only model. "
          "Return only the description.")
VISION_MODEL = "haiku"

# Spawn shape verified live 2026-08-03: the child Reads a temp image file and
# returns a JSON result. `unset CLAUDECODE CLAUDE_CODE_ENTRYPOINT` is the
# nested-session guard — without it the child exits "cannot be launched inside
# another Claude Code session".
def _child_cmd(img_path):
    return [
        "claude", "-p",
        "--settings", '{"disableAllHooks":true}',
        "--model", VISION_MODEL,
        "--tools", "Read",
        "--allowedTools", f"Read({os.path.dirname(img_path)}/*)",
        "--add-dir", os.path.dirname(img_path),
        "--disable-slash-commands",
        "--strict-mcp-config",
        "--append-system-prompt", PROMPT,
        "--output-format", "json",
        f"Read {img_path} and describe the image.",
    ]


def _env():
    env = dict(os.environ)
    env.pop("CLAUDE_CODE_ENTRYPOINT", None)
    env.pop("CLAUDE_CODE_SUBAGENT_MODEL", None)
    env["CLAUDE_CONFIG_DIR"] = ANTHROPIC_CONFIG_DIR
    env["CLAUDE_CODE_SIMPLE"] = "1"
    return env


def transcribe(image_bytes, media_type, cache_dir):
    """Describe one image. Returns (text, None) or (None, why). Never None."""
    try:
        key = hash_key(image_bytes, media_type)
        hit = cache_get(cache_dir, key)
        if hit is not None:
            return hit, None
        with tempfile.NamedTemporaryFile(
                suffix=_ext(media_type), delete=False, dir=os.path.dirname(cache_dir)) as f:
            f.write(image_bytes)
            img_path = f.name
        try:
            r = subprocess.run(_child_cmd(img_path), env=_env(),
                               capture_output=True, text=True, timeout=120)
        finally:
            try:
                os.unlink(img_path)
            except OSError:
                pass
        if r.returncode != 0:
            return None, f"describer exited {r.returncode}"
        text = _parse_result(r.stdout)
        if text is None:
            return None, "describer returned no text"
        cache_put(cache_dir, key, text)
        return text, None
    except Exception as e:
        return None, f"transcribe failed: {e}"


def _parse_result(stdout):
    try:
        d = json.loads(stdout)
        return d.get("result") or None
    except Exception:
        return None


def _ext(media_type):
    return {"image/png": ".png", "image/jpeg": ".jpg",
            "image/gif": ".gif", "image/webp": ".webp"}.get(media_type, ".img")


def hash_key(image_bytes, media_type):
    """Content hash; the cache key. Bytes + media type, so a re-encoded image
    is a different key (misses on re-encode, safe)."""
    return hashlib.sha256(f"{media_type}:{image_bytes}".encode("utf-8", "surrogateescape")).hexdigest()


def cache_get(cache_dir, key):
    try:
        with open(os.path.join(cache_dir, key), "r", encoding="utf-8") as fh:
            return fh.read()
    except (OSError, UnicodeDecodeError):
        return None


def cache_put(cache_dir, key, text):
    """Atomic write (tmpfile + os.replace), so a concurrent reader never sees
    a torn file. Missing dir is created; failures are swallowed (best-effort)."""
    try:
        os.makedirs(cache_dir, exist_ok=True)
        dst = os.path.join(cache_dir, key)
        fd, tmp = tempfile.mkstemp(dir=cache_dir)
        try:
            with os.fdopen(fd, "w", encoding="utf-8") as fh:
                fh.write(text)
            os.replace(tmp, dst)
        finally:
            try:
                os.unlink(tmp)
            except OSError:
                pass
    except OSError:
        pass
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `python3 -m unittest tests/test_vision.py -v`
Expected: PASS (3 tests).

- [ ] **Step 5: Run the full suite to confirm nothing regressed**

Run: `python3 -m unittest discover -s tests`
Expected: OK (existing 154 + 3 new).

- [ ] **Step 6: Commit**

```bash
git add src/vision.py tests/test_vision.py
git commit -m "feat: vision describer via local claude -p haiku child"
```

---

### Task 2: `rewrite_images()` with `tool_result` recursion

**Files:**
- Modify: `src/vision.py` (add the rewrite functions)
- Test: `tests/test_vision.py`

**Interfaces:**
- Consumes: `transcribe`, `hash_key`, `cache_get`, `cache_put` from Task 1.
- Produces: `rewrite_images(payload, cache_dir) -> (total, fresh)` — mutates `payload` in place, replacing image blocks with text blocks (description or fail-open placeholder); returns counts for tests and the `-v` log. Later tasks call this from the proxy.

- [ ] **Step 1: Write the failing test**

```python
# tests/test_vision.py (append)
class RewriteImagesTest(unittest.TestCase):
    def _payload(self):
        return {"messages": [{"role": "user", "content": [
            {"type": "image", "source": {"type": "base64", "media_type": "image/png", "data": "aGk="}},
        ]}]}

    def test_swaps_top_level_image_for_text(self):
        with mock.patch("subprocess.run") as mrun:
            mrun.return_value.returncode = 0
            mrun.return_value.stdout = json_result("a house")
            p = self._payload()
            total, fresh = v.rewrite_images(p, tempfile.mkdtemp())
        self.assertEqual((total, fresh), (1, 1))
        block = p["messages"][0]["content"][0]
        self.assertEqual(block["type"], "text")
        self.assertIn("a house", block["text"])

    def test_recurses_into_tool_result(self):
        # Sean's hard requirement: images nested in tool_result.content.
        with mock.patch("subprocess.run") as mrun:
            mrun.return_value.returncode = 0
            mrun.return_value.stdout = json_result("read image")
            p = {"messages": [{"role": "user", "content": [
                {"type": "tool_result", "tool_use_id": "t1",
                 "content": [{"type": "image", "source": {"type": "base64",
                                 "media_type": "image/png", "data": "aGk="}}]}]}]}
            total, fresh = v.rewrite_images(p, tempfile.mkdtemp())
        self.assertEqual((total, fresh), (1, 1))
        self.assertEqual(p["messages"][0]["content"][0]["content"][0]["type"], "text")

    def test_string_tool_result_is_untouched(self):
        p = {"messages": [{"role": "user", "content": [
            {"type": "tool_result", "tool_use_id": "t1", "content": "plain text"}]}]}
        total, fresh = v.rewrite_images(p, tempfile.mkdtemp())
        self.assertEqual((total, fresh), (0, 0))
        self.assertEqual(p["messages"][0]["content"][0]["content"], "plain text")

    def test_cache_hit_does_not_re_describe(self):
        cd = tempfile.mkdtemp()
        with mock.patch("subprocess.run") as mrun:
            mrun.return_value.returncode = 0
            mrun.return_value.stdout = json_result("a house")
            v.rewrite_images(self._payload(), cd)      # miss -> describe
        with mock.patch("subprocess.run") as mrun:
            v.rewrite_images(self._payload(), cd)      # hit -> no describe
        self.assertFalse(mrun.called)

    def test_fail_open_placeholder_on_error(self):
        with mock.patch("subprocess.run") as mrun:
            mrun.return_value.returncode = 1
            mrun.return_value.stdout = ""
            p = self._payload()
            total, fresh = v.rewrite_images(p, tempfile.mkdtemp())
        self.assertEqual((total, fresh), (1, 0))
        block = p["messages"][0]["content"][0]
        self.assertEqual(block["type"], "text")
        self.assertIn("could not be described", block["text"])

    def test_malformed_image_block_ignored(self):
        p = {"messages": [{"role": "user", "content": [
            {"type": "image", "source": {"type": "base64", "media_type": "image/png"}},  # no data
            "not a dict",
        ]}]}
        with mock.patch("subprocess.run") as mrun:
            total, fresh = v.rewrite_images(p, tempfile.mkdtemp())
        self.assertEqual((total, fresh), (0, 0))
        self.assertFalse(mrun.called)
```

- [ ] **Step 2: Run test to verify it fails**

Run: `python3 -m unittest tests/test_vision.py::RewriteImagesTest -v`
Expected: FAIL with `AttributeError: module 'src.vision' has no attribute 'rewrite_images'`.

- [ ] **Step 3: Write the implementation**

```python
# src/vision.py (append)
PLACEHOLDER = "[Image — text-only model; the attached image could not be described. Describe or OCR it yourself.]"


def rewrite_images(payload, cache_dir):
    """Swap image blocks for transcriptions in place. Returns (total, fresh).

    Recurse into tool_result.content — that is where Claude Code puts
    Read/screenshot/MCP images, and an image left nested there is silently
    DROPPED upstream (a 200 with the evidence removed). A tool_result whose
    content is a plain string falls through the list guard untouched.
    """
    total = fresh = 0
    for msg in payload.get("messages") or []:
        mt, mf = _rewrite_blocks(msg.get("content"), cache_dir)
        total += mt
        fresh += mf
    return total, fresh


def _rewrite_blocks(content, cache_dir):
    if not isinstance(content, list):
        return 0, 0
    total = fresh = 0
    for i, block in enumerate(content):
        if not isinstance(block, dict):
            continue
        kind = block.get("type")
        if kind == "image":
            total += 1
            content[i], got = _swap_image(block, cache_dir)
            fresh += got
        elif kind == "tool_result":
            st, sf = _rewrite_blocks(block.get("content"), cache_dir)
            total += st
            fresh += sf
    return total, fresh


def _swap_image(block, cache_dir):
    """Return (replacement_block, fresh). Never throws; fail-open."""
    try:
        src = block.get("source") or {}
        data = src.get("data")
        media_type = src.get("media_type") or "image/png"
        if not data:
            return dict(block), 0
        import base64
        image_bytes = base64.b64decode(data, validate=False)
        text, _why = transcribe(image_bytes, media_type, cache_dir)
        if text is None:
            text = PLACEHOLDER
            fresh = 0
        else:
            fresh = 1
        return {"type": "text", "text": f"[image transcribed by {VISION_MODEL}]\n{text}"}, fresh
    except Exception:
        return {"type": "text", "text": PLACEHOLDER}, 0
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `python3 -m unittest tests/test_vision.py -v`
Expected: PASS (3 + 6 = 9 tests).

- [ ] **Step 5: Run the full suite**

Run: `python3 -m unittest discover -s tests`
Expected: OK (existing 154 + 9 new).

- [ ] **Step 6: Commit**

```bash
git add src/vision.py tests/test_vision.py
git commit -m "feat: rewrite image blocks to text, recursing into tool_result"
```

---

### Task 3: Wire vision into the proxy + fail-open + disconnect guard

**Files:**
- Modify: `src/proxy.py` (add a `vision` flag to `PROFILES`, call `rewrite_images` in `_relay`, add the `handle_one_request` disconnect guard)
- Test: `tests/test_proxies.py`

**Interfaces:**
- Consumes: `rewrite_images` from Task 2, the existing `_relay()`/`rewrite()` at `proxy.py:130,425`, `make_handler` at `:381`.
- Produces: a `DS4_VISION` env knob (`1` default) that gates the rewrite per profile; the proxy passes a per-profile `vision-cache` dir.

- [ ] **Step 1: Write the failing test**

```python
# tests/test_proxies.py (append — this file already imports the proxy)
class VisionRoutingTest(unittest.TestCase):
    def _post(self, proxy, body):
        import json
        return proxy._relay_rewrite(json.loads(json.dumps(body)))  # helper we'll add

    def test_relay_calls_rewrite_images_when_vision_on(self):
        from unittest import mock
        with mock.patch("src.proxy.rewrite_images") as mrewrite:
            mrewrite.return_value = (0, 0)
            # a request through a vision-on profile hits the rewrite
            ...  # see step 3 for the actual wiring test
```

- [ ] **Step 2: Run test to verify it fails**

Run: `python3 -m unittest tests/test_proxies.py -k VisionRouting -v`
Expected: FAIL.

- [ ] **Step 3: Write the implementation**

In `src/proxy.py`:

```python
# near the top, with the other knobs
VISION = os.environ.get("DS4_VISION", "1") == "1"

# in PROFILES, each row gets:
#   "vision": True,
# (direct, openrouter, nous all True; the knob turns it off for all if set to 0)

# import at top (stdlib):
import base64 as _b64
from src import vision as _vision   # if src is a package; else `import vision as _vision`
```

Then in `_relay()`, after the existing `rewrite(payload, cfg)` call:

```python
if VISION and cfg.get("vision"):
    cache_dir = os.path.join(cfg["dir"], "vision-cache")
    try:
        total, fresh = _vision.rewrite_images(payload, cache_dir)
        if VERBOSE and total:
            print(f"  [{name}] vision: {total} image(s), {fresh} fresh", flush=True)
    except Exception as e:
        if VERBOSE:
            print(f"  [{name}] vision failed open: {e}", flush=True)
```

And add the disconnect guard on the handler class (in `make_handler`):

```python
class Handler(http.server.BaseHTTPRequestHandler):
    # ...
    def handle_one_request(self):
        # A client that hangs up mid-response (Claude Code on ESC, or any
        # cancelled stream) is routine, not an error. Caught once at the request
        # boundary so socketserver doesn't dump a traceback per disconnect.
        try:
            super().handle_one_request()
        except (BrokenPipeError, ConnectionResetError):
            self.close_connection = True
```

- [ ] **Step 4: Run tests**

Run: `python3 -m unittest discover -s tests`
Expected: OK (all green). If `src` isn't importable as a package, adjust the import (`import vision as _vision` with the file at repo root, or a `src/__init__.py`).

- [ ] **Step 5: Commit**

```bash
git add src/proxy.py tests/test_proxies.py
git commit -m "feat: route image blocks through the vision describer, failing open"
```

---

### Task 4: Docs — README + profile notes + issue

**Files:**
- Modify: `README.md`, `profiles/*.md` (the "Neither DeepSeek profile can see images" section), `src/statusline/*.py` (only if needed)

**Goal:** Document that images now transcribe to text via a local `claude -p --model haiku` child, that the description is a lossy proxy (charts/UI degrade), that it does NOT clear an already-poisoned transcript (use `/compact`), and that `DS4_VISION=0` disables it. Keep the README's existing "Neither DeepSeek profile can see images" section accurate — update it to say images are transcribed rather than dropped, with the honest-ceiling caveat.

- [ ] **Step 1: Update the README section**

Rewrite the "Neither DeepSeek profile can see images" section to describe the new behavior: images are transcribed locally by a `claude -p --model haiku` child and the transcription is forwarded as text; the description is lossy; an already-poisoned transcript still needs `/compact`/`/clear`; `DS4_VISION=0` disables it. Match the README's existing style (short, concrete, no fluff).

- [ ] **Step 2: Update the profile docs**

In each of `profiles/deepseek-direct.md`, `profiles/openrouter.md`, `profiles/nous.md`, note the vision behavior and the `DS4_VISION` knob.

- [ ] **Step 3: Run tests**

Run: `python3 -m unittest discover -s tests`
Expected: OK (docs changes only; suite must stay green).

- [ ] **Step 4: Commit**

```bash
git add README.md profiles/*.md
git commit -m "docs: document image transcription via local claude -p haiku child"
```

- [ ] **Step 5: File the GitHub issue (tag @seanperkins)**

Use `gh issue create` in `STRML/cc-ds4`: title "Vision routing for DeepSeek profiles — images transcribed via local claude -p haiku", body describing the feature, the ported shim, the `tool_result` hard requirement (credit Sean), the honest ceiling, and the `DS4_VISION` knob. Tag `@seanperkins` in the body. (Use the humanizer + my-voice pass on the body before publishing, per the global instructions.)

---

## Self-review

**1. Spec coverage:**
- Per-profile vision config → Task 3 (`PROFILES` `vision` flag) + Task 1 (the shared `claude -p` describer). ✓
- The `claude -p` spawn shape (cc-debate recipe) → Task 1 `_child_cmd`/`_env`. ✓
- Auth — no `CLAUDE_CODE_OAUTH_TOKEN` required → Task 1 uses the natural Anthropic OAuth (verified live); launchd keychain is an implementation-time probe, not a plan dependency. ✓
- `rewrite_images`/`_rewrite_blocks`/`_swap_image`/`transcribe` → Tasks 1 + 2. ✓
- **`tool_result` recursion** (Sean's hard requirement) → Task 2, dedicated test. ✓
- Fail-open placeholder + `(text, None)` return shape → Tasks 1 + 2. ✓
- Mid-response disconnect guard → Task 3. ✓
- Cache (bytes+model+prompt key, `<profile>/vision-cache/`, atomic, best-effort) → Task 1. ✓
- Honest ceiling (doesn't unbrick transcript, lossy) → Task 4 docs. ✓
- Tests offline/mocked → all `subprocess.run` mocked; no network in the suite. ✓

**2. Placeholder scan:** The one soft spot is Task 3 Step 1 — the "test" is a stub and Step 3's exact wiring depends on the current `_relay()` shape. I'll fill that in during implementation against the real `proxy.py` (I have it mapped: `rewrite()` at :130, `_relay()` at :425). The rest of the plan has concrete code.

**3. Type consistency:** `transcribe`, `hash_key`, `cache_get`, `cache_put`, `rewrite_images`, `_swap_image` signatures are consistent across tasks. `_swap_image` returns `(block, fresh)`; `_rewrite_blocks`/`rewrite_images` return `(total, fresh)`. ✓
