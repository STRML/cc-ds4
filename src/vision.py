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
    env = {k: v for k, v in os.environ.items()
           if not (any(k.startswith(p) for p in _SCRUB_PREFIXES)
                   or k in _SCRUB_EXACT)}
    env["CLAUDE_CONFIG_DIR"] = ANTHROPIC_CONFIG_DIR
    env["CLAUDE_CODE_SIMPLE"] = "1"
    return env


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
