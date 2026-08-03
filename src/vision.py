"""Describe images with a local `claude -p --model haiku` child.

DeepSeek V4 is text-only; this module turns image blocks into text the main
model can reason over. The describer is a minimal, hermetic `claude -p` call
on the machine's Anthropic profile (subscription credits, no new credential),
matching the recipe proven in cc-debate (`scripts/invoke-acpx.sh`).
"""
import hashlib, json, os, subprocess, tempfile, threading, time

# The Anthropic profile. The proxy may inherit CLAUDE_CONFIG_DIR/ANTHROPIC_*
# from the session that launched it; forcing this keeps --model haiku on
# Anthropic instead of routing back to DeepSeek (observed live).
ANTHROPIC_CONFIG_DIR = os.path.expanduser("~/.claude")

# Env vars that would route the child back to DeepSeek or another ds4 profile,
# or that carry the ds4 credential. Prefix-matching ANTHROPIC_* and
# CLAUDE_CODE_* is deliberate — a new profile var must not slip through.
_SCRUB_PREFIXES = ("ANTHROPIC_", "CLAUDE_CODE_", "DS4_")
_SCRUB_EXACT = ("CLAUDE_CONFIG_DIR", "CLAUDECODE")

# The absolute claude binary, resolved at import. Under launchd the bare name
# is not on PATH (the agent runs /usr/bin/python3 with only DS4_* env), so a
# bare "claude" would silently turn every image into a placeholder. If neither
# DS4_CLAUDE_BIN nor a PATH lookup finds it, the caller fails open.
def _resolve_claude():
    import shutil
    return shutil.which("claude")


CLAUDE_BIN = os.environ.get("DS4_CLAUDE_BIN") or _resolve_claude()

PROMPT = ("Describe the image for a text-only model. "
          "Return only the description.")
VISION_MODEL = "haiku"

# Bound the per-request describe work. Each child can take up to 120s, and the
# walk is serial, so a transcript with many images must not hold the request for
# minutes. Images beyond the budget become placeholders without spawning.
MAX_IMAGES_PER_REQUEST = 8

# Spawn shape verified live 2026-08-03: the child Reads a temp image file and
# returns a JSON result. `unset CLAUDECODE CLAUDE_CODE_ENTRYPOINT` is the
# nested-session guard — without it the child exits "cannot be launched inside
# another Claude Code session". The child runs with cwd = the temp dir, which
# contains ONLY the image, so it cannot Read the profile's settings.json or any
# inherited working dir (--allowedTools is a permission pre-approval, not a
# filesystem sandbox). --no-session-persistence keeps the child from writing a
# transcript that would retain the image bytes.
def _child_cmd(img_path):
    img_dir = os.path.dirname(img_path)
    return [
        CLAUDE_BIN, "-p",
        "--settings", '{"disableAllHooks":true}',
        "--model", VISION_MODEL,
        "--tools", "Read",
        "--allowedTools", f"Read({img_dir}/*)",
        "--add-dir", img_dir,
        "--disable-slash-commands",
        "--strict-mcp-config",
        "--append-system-prompt", PROMPT,
        "--no-session-persistence",
        "--output-format", "json",
        f"Read {img_path} and describe the image.",
    ]


def _env():
    """The child env: the parent env minus everything Claude/Anthropic/ds4.

    A child that inherits the parent session's ANTHROPIC_BASE_URL/AUTH_TOKEN
    routes to the text-only ds4 proxy (garbage descriptions) or, with those
    stripped, gets "Not logged in". But a 4-key whitelist (HOME/PATH/TERM/
    CLAUDE_CONFIG_DIR) is too sparse — the child needs the broader environment
    (XPC_SESSION_*, security's keychain access) to reach the Anthropic OAuth
    in the login keychain. Observed live: scrubbing the Claude/Anthropic/ds4/
    cmux families and keeping everything else lets `claude -p` auth via the
    keychain and hit real Anthropic Haiku.
    """
    return {k: v for k, v in os.environ.items()
            if not (any(k.startswith(p) for p in _SCRUB_PREFIXES)
                    or k in _SCRUB_EXACT
                    or k.startswith("CMUX")
                    or k in ("NODE_OPTIONS", "AI_AGENT"))}



# One billed child per (cache_dir, key) — concurrent threads that miss the same
# key in the SAME profile wait on the winner instead of spawning a stampede.
# Keyed by cache_dir too, because one proxy serves several profiles with
# separate caches: a cross-profile waiter would miss its own cache and placehold.
_lock = threading.Lock()
_inflight = {}


def transcribe(image_bytes, media_type, cache_dir):
    """Describe one image. Returns (text, fresh): fresh is 1 when a child ran,
    0 on a cache hit or failure (the caller substitutes the placeholder). Never
    returns a bare None and never a tuple with None as the count."""
    if not CLAUDE_BIN:
        return None, 0
    try:
        key = hash_key(image_bytes, media_type)
        hit = cache_get(cache_dir, key)
        if hit is not None:
            return hit, 0
        # single-flight: one billed child per (cache_dir, key)
        flight = (cache_dir, key)
        with _lock:
            if flight in _inflight:
                waiter = threading.Event()
                _inflight[flight].append(waiter)
            else:
                waiter = None
                _inflight[flight] = []
        if waiter is not None:
            waiter.wait(timeout=130)
            with _lock:
                if flight not in _inflight:
                    text = cache_get(cache_dir, key)
                    if text is not None:
                        return text, 0
            return None, 0
        try:
            with tempfile.TemporaryDirectory(
                    prefix="ds4-vision-", dir=tempfile.gettempdir()) as tmp:
                img_path = os.path.join(tmp, "image" + _ext(media_type))
                with open(img_path, "wb") as fh:
                    fh.write(image_bytes)
                # cwd = tmp: the child's only readable context is the image.
                # stdin=DEVNULL: an expired OAuth session must crash the child
                # (exit nonzero) rather than hang the proxy on an interactive
                # login prompt.
                r = subprocess.run(_child_cmd(img_path), env=_env(),
                                   cwd=tmp, stdin=subprocess.DEVNULL,
                                   capture_output=True, text=True, timeout=120)
            if r.returncode != 0:
                return None, 0
            text = _parse_result(r.stdout)
            if text is None:
                return None, 0
            cache_put(cache_dir, key, text)
            return text, 1
        finally:
            with _lock:
                waiters = _inflight.pop(flight, [])
            for w in waiters:
                w.set()
    except Exception:
        return None, 0


def _parse_result(stdout):
    """The child returns --output-format json on STDOUT. A deprecation warning
    can prefix the JSON, and the warning itself may contain a '{' (same-line or
    not) — so scan every '{' position with raw_decode and keep scanning when a
    decoded object lacks a usable result. A non-empty, non-error result is
    required."""
    dec = json.JSONDecoder()
    start = 0
    while True:
        i = stdout.find("{", start)
        if i < 0:
            return None
        try:
            obj, end = dec.raw_decode(stdout, i)
        except json.JSONDecodeError:
            start = i + 1
            continue
        if isinstance(obj, dict) and not obj.get("is_error"):
            text = obj.get("result")
            if isinstance(text, str) and text.strip():
                return text
        start = end
    return None


def _ext(media_type):
    return {"image/png": ".png", "image/jpeg": ".jpg",
            "image/gif": ".gif", "image/webp": ".webp"}.get(media_type, ".img")


def hash_key(image_bytes, media_type):
    """Content hash; the cache key. Hashes RAW bytes with a media-type prefix —
    never a stringified repr. Model + prompt are folded in so a describer or
    prompt change invalidates old entries."""
    salt = f"{VISION_MODEL}:{PROMPT}:".encode("utf-8")
    return hashlib.sha256(salt + media_type.encode("utf-8") + b":" + image_bytes).hexdigest()


CACHE_TTL = 30 * 86400   # 30 days


def cache_get(cache_dir, key):
    """Return the cached description if present and unexpired. A stale entry
    is deleted and treated as a miss — never served once then cleaned later."""
    try:
        path = os.path.join(cache_dir, key)
        if os.path.getmtime(path) < time.time() - CACHE_TTL:
            os.unlink(path)
            return None
        with open(path, "r", encoding="utf-8") as fh:
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


PLACEHOLDER = "[Image omitted: no usable description was available.]"


def rewrite_images(payload, cache_dir):
    """Swap image blocks for transcriptions in place. Returns (total, fresh).

    Recurse into tool_result.content — that is where Claude Code puts
    Read/screenshot/MCP images, and an image left nested there is silently
    DROPPED upstream (a 200 with the evidence removed). A tool_result whose
    content is a plain string falls through the list guard untouched. A
    messages entry that is not a dict is skipped, matching proxy.rewrite.
    """
    messages = payload.get("messages")
    if not isinstance(messages, list):
        return 0, 0   # a non-list messages value must not crash the walker
    total = fresh = 0
    budget = [MAX_IMAGES_PER_REQUEST]
    for msg in messages:
        if not isinstance(msg, dict):
            continue
        mt, mf = _rewrite_blocks(msg.get("content"), cache_dir, budget)
        total += mt
        fresh += mf
    return total, fresh


def _rewrite_blocks(content, cache_dir, budget=None):
    """Return (total, fresh). Walk serially — a single request is bounded by
    MAX_IMAGES_PER_REQUEST so a huge transcript can't hold the request for
    minutes (each child can take up to 120s). Images beyond the budget are
    placeholdered without spawning a child."""
    if budget is None:
        budget = [MAX_IMAGES_PER_REQUEST]
    if not isinstance(content, list):
        return 0, 0
    total = fresh = 0
    for i, block in enumerate(content):
        if not isinstance(block, dict):
            continue
        kind = block.get("type")
        if kind == "image":
            total += 1
            if budget[0] <= 0:
                content[i] = {"type": "text", "text": PLACEHOLDER}
                continue
            budget[0] -= 1
            content[i], got = _swap_image(block, cache_dir)
            fresh += got
        elif kind == "tool_result":
            st, sf = _rewrite_blocks(block.get("content"), cache_dir, budget)
            total += st
            fresh += sf
    return total, fresh


def _swap_image(block, cache_dir):
    """Return (replacement_block, fresh). Never throws; fail-open. Every image
    — including a malformed one — becomes a text block (description or
    placeholder), so nothing image-shaped reaches the text-only upstream."""
    try:
        src = block.get("source")
        if not isinstance(src, dict) or src.get("type") != "base64":
            return {"type": "text", "text": PLACEHOLDER}, 0   # URL sources unsupported
        data = src.get("data")
        media_type = src.get("media_type")
        if not isinstance(data, str) or not data or not isinstance(media_type, str) or not media_type:
            return {"type": "text", "text": PLACEHOLDER}, 0   # missing/invalid metadata is malformed
        import base64
        try:
            image_bytes = base64.b64decode(data, validate=True)
        except Exception:
            return {"type": "text", "text": PLACEHOLDER}, 0
        if not image_bytes:               # strict decode yielded nothing
            return {"type": "text", "text": PLACEHOLDER}, 0
        text, fresh = transcribe(image_bytes, media_type, cache_dir)
        if text is None:
            text = PLACEHOLDER
            fresh = 0
        return {"type": "text", "text": f"[image transcribed by {VISION_MODEL}]\n{text}"}, fresh
    except Exception:
        return {"type": "text", "text": PLACEHOLDER}, 0


def placeholder_remaining(payload):
    """Replace any image block still present with the fail-open placeholder.
    Called on the exception path so the upstream never receives an image block,
    even if the describer walk throws mid-request. Guards a non-list messages
    value exactly like rewrite_images."""
    messages = payload.get("messages")
    if not isinstance(messages, list):
        return
    for msg in messages:
        if not isinstance(msg, dict):
            continue
        _scrub_blocks(msg.get("content"))


def _scrub_blocks(content):
    if not isinstance(content, list):
        return
    for i, block in enumerate(content):
        if not isinstance(block, dict):
            continue
        if block.get("type") == "image":
            content[i] = {"type": "text", "text": PLACEHOLDER}
        elif block.get("type") == "tool_result":
            _scrub_blocks(block.get("content"))
