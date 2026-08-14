#!/usr/bin/env python3
"""One proxy for every DeepSeek V4 profile.

Each profile gets its own listener on its own port, so `settings.json` on each
side is unchanged and unaware this is shared. What varies between them is a row
in PROFILES, not a separate script.

Why any proxy exists at all:

  * V4 is in thinking mode by default and Claude Code cannot turn it off. It
    sends thinking={"type":"adaptive"}, which no provider serving this model
    implements. A main-loop request at max_tokens=32000 is unaffected; a small
    utility call is not, because the thinking block consumes the whole budget
    before the tool call is emitted. Measured on the direct endpoint at
    max_tokens=512 with a forced tool decision: 3 of 5 truncated, two of those
    with no tool_use block at all. The permission classifier behind
    `defaultMode: auto` is one of these calls, so the symptom is intermittent
    classifier errors. thinking={"type":"disabled"} clears it, and every
    endpoint here honours that spelling even though none honours its own.
  * Claude Code exposes model tiers but no per-tier effort knob, so the
    OpenRouter and Nous profiles map a `ds4-*` sentinel model name onto
    reasoning_effort.
  * OpenRouter needs a per-request zero-data-retention block.
  * The status line needs real pricing, which /__spend serves.

Run it with no arguments. It serves every profile in PROFILES whose directory
exists, and exits once none of them is in use.
"""
import ctypes, ctypes.util, collections, hmac, http.server, json, os, re, socket, subprocess, sys, threading, time, urllib.request, urllib.error

# Vision: translate image blocks into text descriptions before forwarding. The
# sibling module sits next to proxy.py (both in src/), so it imports without a
# path bootstrap — the launch agent and the tests both resolve it the same way.
import vision as _vision
import classifier as _classifier

HOME = os.path.expanduser("~")
VERBOSE = os.environ.get("DS4_VERBOSE") == "1" or os.environ.get("DS4_DEBUG") == "1"

# Gate the vision rewrite. Read at process startup (the launchd agent bakes
# DS4_* at install). 0 restores the old pass-through — image blocks forwarded
# unchanged.
VISION = os.environ.get("DS4_VISION", "1") == "1"

# Classifier routing: the auto-mode permission classifier (ds4-high + small
# max_tokens + thinking off) gates every tool call, so it defaults to the
# Anthropic subscription instead of DeepSeek — the security gate should live
# in a trusted boundary. Three routes, set by DS4_CLASSIFIER:
#   * anthropic (default) — forwarded to the subscription. "ds4" would bill
#     subscription tokens for the classifier on every tool call.
#   * zdr — forwarded to the or-ds4 (OpenRouter ZDR) route instead: no
#     subscription token spent, and ZDR keeps the intent off training. The
#     gate runs on DeepSeek V4 Flash via OpenRouter rather than Anthropic;
#     opt-in because that is a lower-trust boundary. Fails open to the
#     Anthropic route, then ds4, so auto mode never bricks.
#   * ds4 — old behavior: the classifier rides the profile's own upstream.
# DS4_CLASSIFIER_MODEL overrides the Anthropic model. Default is sonnet-5, NOT
# haiku: the profile advertises a 1M context window and Claude Code sizes the
# classifier transcript against it, so a 200K-window model (haiku) overflows
# on a long auto-mode session ("classifier transcript exceeded context
# window"). Sonnet 5 matches the 1M window the profiles claim.
# DS4_CLASSIFIER_TOKEN holds the subscription token (claude setup-token);
# without it the classifier fails open to ds4.
CLASSIFIER_ROUTE = os.environ.get("DS4_CLASSIFIER", "anthropic")
CLASSIFIER_MODEL = os.environ.get("DS4_CLASSIFIER_MODEL", "claude-sonnet-5")
# The or-ds4 route's model, defaulting to the openrouter profile's own.
ORDS4_CLASSIFIER_MODEL = os.environ.get("DS4_ORDS4_CLASSIFIER_MODEL", "")

# Below this many max_tokens, turn thinking off. Claude Code's utility calls
# arrive with a few hundred tokens of budget; the main loop arrives at 32000.
# Nothing observed lands in between, so the exact value is not delicate.
NOTHINK_BELOW = int(os.environ.get("DS4_NOTHINK_BELOW", "8192"))

# Exit once no profile is in use for this long. 0 disables and runs forever.
IDLE_EXIT = int(os.environ.get("DS4_IDLE_EXIT", "900"))

# Cloudflare 403s the stdlib's default urllib User-Agent ("error code: 1010"),
# which hits Nous Portal and intermittently OpenRouter. A curl UA is proven to
# pass, and is harmless on the endpoints that never cared.
UA = os.environ.get("DS4_UA", "curl/8.4.0")

# Sentinel -> (model family, default effort). The family selects the model
# (pro vs flash); the default effort applies only when neither /effort nor the
# per-profile effort-override says otherwise — /effort is meant to actually
# change how hard the model thinks, not be clobbered by the sentinel.
#   fable -> ds4-pro-xhigh   opus -> ds4-pro-medium
#   sonnet -> ds4-flash-xhigh   haiku -> ds4-flash-medium
EFFORT = {
    "ds4-pro-xhigh": ("pro", "xhigh"),
    "ds4-pro-medium": ("pro", "medium"),
    "ds4-flash-xhigh": ("flash", "xhigh"),
    "ds4-flash-medium": ("flash", "medium"),
}

# Transient upstream statuses to retry in the relay. A raw forward of any of
# these kills the whole claude -p process ("Execution error") and loses the
# worker's in-flight work; absorbing them here turns a blip into a success.
# 524 is Cloudflare's origin-timeout; 429/529 are rate limit / overload.
TRANSIENT_STATUS = {429, 500, 502, 503, 524, 529}
RETRY_ATTEMPTS = 3
RETRY_BACKOFF = 1.5          # seconds, scaled by attempt number

# Socket timeout for the upstream relay. A stalled origin (nous's Cloudflare
# 524 hangs up to its 100s relay window) would otherwise tie up a relay thread
# for minutes with no way to resolve, and the failover breaker only sees a
# strike once a request resolves — so the hang also delays tripping by minutes
# per strike. A socket timeout bounds both: reads that produce no data for this
# long count as a failure. A live stream always has data flowing, so this only
# fires on a real stall. 0 disables and restores the old no-timeout relay.
RELAY_TIMEOUT = int(os.environ.get("DS4_RELAY_TIMEOUT", "60"))

# Safe deployments set this in the socket-activation environment.  A
# self-bound loopback socket is not an ownership boundary: another local user
# can win the bind during a restart.  Keep manual development invocation
# available with the explicit default-off compatibility mode; install.sh turns
# it on for launchd.
REQUIRE_OWNED_SOCKET = os.environ.get("DS4_REQUIRE_OWNED_SOCKET", "0") == "1"

# Internal request contract.  Claude Code can carry this as a JSON field even
# when it cannot add a custom header.  It is removed before forwarding.
REQUIRE_ZDR_HEADER = "x-ds4-require-zdr"
REQUIRE_ZDR_FIELD = "ds4_require_zdr"


def _is_anthropic_model(name):
    """True for a literal Anthropic model id that the sentinel system missed."""
    n = name.lower()
    return any(k in n for k in ("sonnet", "opus", "haiku", "claude-"))


def should_retry(tier):
    """True when a transient error on this request should be retried in-proxy.

    The main thread (ds4-pro-xhigh) has its own 10x-backoff retry, so retrying
    here would double up. Subagent tiers (everything else via
    CLAUDE_CODE_SUBAGENT_MODEL) die with "Execution error" on a raw forward, so
    they need the proxy guard.

    tier is the client-sent model, captured before any failover remap: the
    remap rewrites payload['model'] to the target's literal id, which would
    make every failed-over request look like a retryable subagent call.
    """
    return isinstance(tier, str) and tier != "ds4-pro-xhigh"


# ── circuit-breaker failover ────────────────────────────────────────────────
# A profile whose upstream has sustained bad stretches (nous's Cloudflare 524 /
# DeepSeek 503, ~10% of requests and clustering under load) can declare a
# "failover" target in PROFILES. Once transient errors are FAILOVER_RATE of the
# last FAILOVER_WINDOW requests, the breaker opens and that profile's requests
# are served by the target's upstream and key until a probe recovers. Knobs are
# read once at startup like every DS4_* var, so a change needs a proxy restart.
#
# Tuning: window=12, rate=0.25 (3 strikes in the last 12 requests) is the sweet
# spot against nous's observed ~10% sustained drip. The old 6/0.5 needed 3 of
# the last 6 and almost never tripped on a drip — 1.6% per window at a 10%
# error rate, one trip in ~24h. 12/0.25 trips ~11% per window on the same
# drip while a healthy ~3% upstream variance only false-trips ~0.5%.
FAILOVER_ENABLED = os.environ.get("DS4_FAILOVER", "1") == "1"
FAILOVER_WINDOW = int(os.environ.get("DS4_FAILOVER_WINDOW", "12"))
FAILOVER_RATE = float(os.environ.get("DS4_FAILOVER_RATE", "0.25"))
FAILOVER_RECHECK = int(os.environ.get("DS4_FAILOVER_RECHECK", "60"))
FAILOVER_PROBE_TIMEOUT = int(os.environ.get("DS4_FAILOVER_PROBE_TIMEOUT", "6"))
# A probe is a minimal POST /v1/messages (see _failover_probe) - the same path
# real requests ride, so a clean probe means completions recovered, not just
# the models list. Require N consecutive clean probes (spaced FAILOVER_RECHECK
# apart) before closing, so a genuinely recovered upstream survives the window
# and a still-bad one stays on the target.
FAILOVER_PROBES_TO_CLOSE = int(os.environ.get("DS4_FAILOVER_PROBES_TO_CLOSE", "3"))

# The failover target (direct) takes real model names and ignores
# reasoning_effort, so the ds4-* sentinel rewrite leaves behind must map onto
# one. The profiles' own qualified id (nous/openrouter both use
# deepseek/deepseek-v4-flash-0731) is included too: a request that already
# carries it — a /model-picker literal or a direct probe — would otherwise
# reach the target unchanged and 400 there, which only serves its own names.
# Flash only: the direct profile's own config runs flash for every tier,
# and the cost difference between flash and pro is what makes failover worth
# it — a pro main-loop request on the target would bill more than the nous
# trip it is trying to ride out (observed: 121 trips in 25h).
FAILOVER_MODEL = {
    # The failover target is openrouter (nous -> openrouter). This is the
    # safety net when rewrite() left a sentinel untouched on a failed-over
    # request. It mirrors the family split but pro has no usable OR host, so
    # both families resolve to flash here. Keep the base id: the suffix is
    # stripped before this lookup.
    "ds4-pro-xhigh": "deepseek/deepseek-v4-flash-0731:nitro",
    "ds4-pro-medium": "deepseek/deepseek-v4-flash-0731:nitro",
    "ds4-flash-xhigh": "deepseek/deepseek-v4-flash-0731:nitro",
    "ds4-flash-medium": "deepseek/deepseek-v4-flash-0731:nitro",
    "deepseek/deepseek-v4-flash-0731": "deepseek/deepseek-v4-flash-0731:nitro",
}

# name -> {"outcomes": deque(bool, maxlen=window), "open": bool,
#          "opened_at": float, "probed_at": float}; guarded by _lock.
_failover = {}


def _failover_state(name):
    """State for one profile. Caller holds _lock."""
    st = _failover.get(name)
    if st is None:
        st = {"outcomes": collections.deque(maxlen=FAILOVER_WINDOW),
              "open": False, "opened_at": 0.0, "probed_at": 0.0,
              "probes": 0}
        _failover[name] = st
    return st


def _failover_threshold():
    """Strikes in the window that trip the breaker (a floor of one)."""
    return max(1, int(FAILOVER_WINDOW * FAILOVER_RATE))


def _failover_probe(name, cfg):
    """A minimal /v1/messages POST probes the exact path real requests ride.

    A GET /v1/models was the old probe: it never measured completions health,
    so a models-ok / messages-still-503 split closed the circuit and the
    breaker re-tripped a few requests later (observed: close->trip gaps as
    small as 20 lines). The probe mirrors a tiny thinking-disabled request, so
    a clean probe means the completions endpoint has actually recovered.
    """
    # cfg is the profile being probed, so its model id is the one its own
    # requests use. Nous serves deepseek/deepseek-v4-flash-0731; a hardcoded
    # direct id (deepseek-v4-flash[1m]) 404s there, which would keep the
    # breaker open forever. Fall back to the direct id only for profiles with
    # no model (the direct profile itself).
    model = cfg["model"] or "deepseek-v4-flash[1m]"
    body = json.dumps({
        "model": model,
        "max_tokens": 1,
        "thinking": {"type": "disabled"},
        "messages": [{"role": "user", "content": "ping"}],
    }).encode()
    req = urllib.request.Request(cfg["upstream"].rstrip("/") + "/v1/messages",
                                 data=body, method="POST")
    req.add_header("authorization", "Bearer " + api_key(name, cfg))
    req.add_header("content-type", "application/json")
    req.add_header("user-agent", UA)
    try:
        with urllib.request.urlopen(req, timeout=FAILOVER_PROBE_TIMEOUT) as r:
            return r.status == 200
    except Exception:
        return False


def failover_effective(name, cfg):
    """(eff_cfg, eff_name) this request should hit, plus a close flag.

    Closed circuit: the profile's own cfg, one locked read of extra work.
    Open: the failover target's cfg — unless the recheck interval has lapsed,
    in which case one probe decides whether the profile has recovered. The
    probe happens on this request thread but outside the lock, and probed_at is
    reserved under the lock first, so concurrent requests never double-probe.

    A probe success does not close the circuit (a minimal ping never carries
    real load, and closing on a lull-pass is the close->503->reopen flap). It
    arms a trial: this request rides the profile's OWN upstream, and the relay
    closes the circuit only if that trial serves clean (see
    failover_trial_close).
    """
    target = cfg.get("failover")
    if not FAILOVER_ENABLED or not target:
        return cfg, name, False
    tcfg = PROFILES.get(target)
    if tcfg is None or not os.path.isdir(tcfg["dir"]):
        return cfg, name, False           # target not installed; failover is moot
    with _lock:
        st = _failover_state(name)
        if not st["open"]:
            return cfg, name, False
        if time.time() - st["probed_at"] < FAILOVER_RECHECK:
            return tcfg, f"{name}->{target}", False
        st["probed_at"] = time.time()     # this request holds the probe
    if _failover_probe(name, cfg):
        with _lock:
            st["probes"] += 1
            armed = st["probes"] >= FAILOVER_PROBES_TO_CLOSE
        if armed:
            # Route this request to the profile's OWN upstream as the trial.
            # The relay closes the circuit only if this real request serves
            # clean (failover_trial_close); a probe passing is not evidence
            # the upstream handles real load.
            print(f"  [{name}] failover: probe ok x{FAILOVER_PROBES_TO_CLOSE}, "
                  f"serving a real trial", flush=True)
            return cfg, name, True
        print(f"  [{name}] failover: probe ok ({st['probes']}/{FAILOVER_PROBES_TO_CLOSE}), "
              f"staying on {target}", flush=True)
        return tcfg, f"{name}->{target}", False
    with _lock:
        st["probes"] = 0                 # a failed probe resets the streak
    print(f"  [{name}] failover: probe failed, staying on {target}", flush=True)
    return tcfg, f"{name}->{target}", False


def failover_record(name, cfg, eff_cfg, up, last_err):
    """Feed one request's outcome into the breaker.

    Only the profile's own upstream outcomes count — a request the target served
    says nothing about the profile's upstream. A transient status or a
    connection failure is a strike; anything else is a hit. Tripping resets the
    probe clock so the fallback gets a quiet FAILOVER_RECHECK before the first
    re-probe.
    """
    if eff_cfg is not cfg or not cfg.get("failover"):
        return
    # up is a real response only when the request succeeded — possibly after
    # in-proxy retries, which leave a stale last_err. So read it first; None
    # means the loop never got one (exhausted retries, or a connection error).
    if up is None:
        if isinstance(last_err, urllib.error.HTTPError):
            bad = last_err.code in TRANSIENT_STATUS
        else:
            bad = True                    # non-HTTP network failure
    else:
        bad = up.status in TRANSIENT_STATUS
    with _lock:
        st = _failover_state(name)
        st["outcomes"].append(bad)
        if os.environ.get("DS4_FAILOVER_DEBUG") == "1":
            st_extra = (f"up={up.status if up else None} "
                        f"err={type(last_err).__name__ if last_err else None} "
                        f"code={last_err.code if isinstance(last_err, urllib.error.HTTPError) else '-'} "
                        f"bad={bad} window={sum(st['outcomes'])}/{len(st['outcomes'])}")
            print(f"  [{name}] failover-record: {st_extra}", flush=True)
        if not st["open"] and sum(st["outcomes"]) >= _failover_threshold():
            st["open"] = True
            st["opened_at"] = time.time()
            st["probed_at"] = time.time()
            st["probes"] = 0                 # fresh streak on the next recovery
            print(f"  [{name}] failover: {sum(st['outcomes'])} transient errors "
                  f"in the last {len(st['outcomes'])} requests, "
                  f"routing to {cfg['failover']}", flush=True)


def failover_trial_close(name):
    """Close the circuit after a clean trial request on the profile's own upstream.

    The half-open probe only tells us the upstream accepts a minimal ping — it
    says nothing about real load, and a lull-passed probe followed by the next
    heavy request 503ing is exactly the close->reopen flap this prevents. So a
    probe success only arms a trial; the first real request the profile's own
    upstream serves without a transient error is the evidence that closes the
    circuit. A trial that fails (transient error / connection failure) or never
    happens keeps the circuit open. Returns True when the circuit closed.
    """
    with _lock:
        st = _failover_state(name)
        if not st["open"]:
            return False
        st["open"] = False
        st["opened_at"] = 0.0
        st["outcomes"].clear()
        st["probes"] = 0
    print(f"  [{name}] failover: trial ok, closing circuit", flush=True)
    return True


# The full set /ds4-effort accepts. The tier map above only names the four
# Claude Code tiers; the override file may name any of these.
EFFORT_LEVELS = ("max", "xhigh", "high", "medium", "low", "minimal", "none")

# ZDR endpoints whose context is smaller than the 1M the profile advertises. A
# long session that routes here overflows the endpoint rather than the declared
# window. Recheck /api/v1/models/{id}/endpoints -> context_length when the
# provider list changes.
LOW_CONTEXT = ["Io Net"]

WEEK = 7 * 86400
LEDGER_MIN_INTERVAL = 300      # don't sample spend more than once every 5 min
CREDITS_TTL = 60               # /__spend is hit on every status line render

PROFILES = {
    "direct": {
        "port": 31500,
        "dir": f"{HOME}/.claude-ds4",
        "upstream": "https://api.deepseek.com/anthropic",
        # DeepSeek's own endpoint takes real model names and ignores
        # reasoning_effort, so no sentinel rewriting here. The tier map still
        # applies: opus/fable ride pro, sonnet/haiku ride flash.
        "model": None,
        # api.deepseek.com only accepts the bare ids — the versioned -0813/-0731
        # names are an OpenRouter convention and 400 here. Opus/fable ride pro,
        # sonnet/haiku ride flash. It ignores reasoning_effort, so effort=False.
        "effort": False,
        "family_models": {
            "pro": "deepseek-v4-pro",
            "flash": "deepseek-v4-flash",
        },
        "zdr_skip_models": [],
        "zdr": False,
        "spend": False,
        # The direct profile is the failover target for the 1M profiles. Its
        # endpoint counts input + completion against the same 1M cap, while
        # Claude Code budgets 131072 output against the advertised window — so
        # an uncapped failover session overflows at ~923K input and 400s
        # ("maximum context length"). Same cap as the other 1M profiles keeps
        # a failed-over request inside the endpoint's real limit.
        "max_out": 65536,
        # Only this endpoint requires an assistant tool_use message to replay its
        # thinking block. Claude Code 2.x does replay it, so this is a guard
        # against a path that drops it, not a fix for an observed failure.
        "inject": True,
        "failover": None,
    },
    "openrouter": {
        "port": 31501,
        "dir": f"{HOME}/.claude-or-ds4",
        "upstream": "https://openrouter.ai/api",
        # :nitro is OR's sort=throughput variant — the fastest providers. The
        # suffix rides the model id into every request; exact-id consumers
        # (pricing, failover remap) strip it back to the base id first.
        "model": "deepseek/deepseek-v4-flash-0731:nitro",
        # Opus/fable ride the pinned pro-0813, sonnet/haiku stay on flash-0731.
        # The :nitro suffix floats to the fastest provider for each. Never the
        # unversioned originals — OR has them but they'd bill differently.
        "effort": True,
        # pro-0813 has no usable host on OR (404), so the pro family falls back
        # to flash here; direct is the only place pro actually serves.
        "family_models": {
            "pro": "deepseek/deepseek-v4-flash-0731:nitro",
            "flash": "deepseek/deepseek-v4-flash-0731:nitro",
        },
        "zdr": True,
        # pro-0813's only host is DeepSeek itself, which rejects the ZDR block
        # (404 "no endpoints matching data policy"). Skip ZDR for it so the pro
        # tier can serve; the flash tiers keep ZDR. Env-overridable at install.
        "zdr_skip_models": ["deepseek/deepseek-v4-pro-0813"],
        "spend": True,
        # Smallest max_completion_tokens in the ZDR pool (DeepInfra, Io Net).
        "max_out": 65536,
        "inject": False,
        "failover": None,
    },
    "nous": {
        "port": 31502,
        "dir": f"{HOME}/.claude-nous",
        "upstream": "https://inference-api.nousresearch.com",
        "model": "deepseek/deepseek-v4-flash-0731",
        # Nous has no pro model (its /v1/models lists no deepseek at all), so
        # every tier falls back to the flash model above. A failed-over nous
        # request rides openrouter's tier_models (pro for opus/fable).
        "effort": True,
        "family_models": {
            "pro": "deepseek/deepseek-v4-flash-0731",
            "flash": "deepseek/deepseek-v4-flash-0731",
        },
        "zdr_skip_models": [],
        # Nous 403s any provider block, zdr or otherwise.
        "zdr": False,
        "spend": True,
        "max_out": 65536,
        "inject": False,
        # Nous sits behind Cloudflare and has real bad stretches (524/503).
        # When its transient errors pass the breaker threshold, its requests
        # are served by the openrouter profile (cheap flash) until a probe
        # recovers, not direct (billed per-token on api.deepseek.com and the
        # thing that emptied the balance). See the failover section below.
        "failover": "openrouter",
    },
}

DISABLED = {"type": "disabled"}
PLACEHOLDER = {"type": "thinking", "thinking": "(elided)", "signature": "ds4-proxy"}

_last_seen = time.time()
_lock = threading.Lock()
_cache = {}                    # (name, kind) -> cached value
_effort_cache = {}             # path -> (mtime_ns, ctime_ns, size, ino, level|None)
_inflight = 0                  # relayed requests open; idle_watch must not exit while nonzero


# ── request rewriting ────────────────────────────────────────────────────────

def effort_override(cfg):
    """Per-profile effort pin from <profile>/effort-override, or None.

    One line, one of EFFORT_LEVELS; /ds4-effort is the writer. A file survives
    a proxy restart, and the stat-keyed cache keeps the read off the
    per-request path: a request is one stat plus a dict lookup unless the file
    changed. The key also carries the inode: the command writes via an atomic
    replace, which allocates a fresh inode even on filesystems whose clock
    tick is coarser than the gap between writes (ext2/ext3, FAT), where
    mtime/ctime alone would go stale. An absent file, or one holding anything
    outside EFFORT_LEVELS, reads as None (tier default) — OpenRouter accepts
    the parameter and DeepSeek drops unknown values without error, so an
    invalid level must fail here rather than vanish upstream.
    """
    path = os.path.join(cfg["dir"], "effort-override")
    try:
        st = os.stat(path)
    except OSError:
        return None
    with _lock:
        hit = _effort_cache.get(path)
        if hit and hit[:4] == (st.st_mtime_ns, st.st_ctime_ns, st.st_size, st.st_ino):
            return hit[4]
    level = None
    try:
        with open(path, encoding="utf-8") as fh:
            raw = fh.read().strip()
        if raw in EFFORT_LEVELS:
            level = raw
    except OSError:
        pass
    with _lock:
        _effort_cache[path] = (st.st_mtime_ns, st.st_ctime_ns, st.st_size, st.st_ino, level)
    return level

def inject_missing_thinking(payload):
    """Count of assistant tool_use messages given a placeholder thinking block.

    DeepSeek 400s on a history where one is missing, and does not validate the
    signature, so a placeholder is enough.
    """
    n = 0
    for m in payload.get("messages") or []:
        if not isinstance(m, dict) or m.get("role") != "assistant":
            continue
        blocks = m.get("content")
        if not isinstance(blocks, list):
            continue
        kinds = {b.get("type") for b in blocks if isinstance(b, dict)}
        if "tool_use" in kinds and "thinking" not in kinds:
            blocks.insert(0, dict(PLACEHOLDER))
            n += 1
    return n


def rewrite(payload, cfg):
    """Edit payload in place for one profile. Returns a log line, or None."""
    notes = []

    tier = payload.get("model")
    # Sentinel encodes (model family, default effort): ds4-pro-* / ds4-flash-*.
    # The family picks the model (pro vs flash) on this profile; the default
    # effort only applies when the client didn't send one. /effort sends
    # reasoning_effort in the body — honor it so the knob actually changes how
    # hard the model thinks instead of being clobbered by the sentinel.
    family, default_effort = EFFORT.get(tier, (None, None)) if isinstance(tier, str) else (None, None)
    model = (cfg.get("family_models") or {}).get(family) or cfg["model"]
    if model:
        client_effort = payload.get("reasoning_effort")
        if client_effort in EFFORT_LEVELS:
            # /effort or an explicit request — it wins over the sentinel default.
            payload["model"] = model
            payload["reasoning_effort"] = client_effort
            notes.append(f"{tier} -> {model} effort={client_effort}")
        elif default_effort and cfg.get("effort", True):
            payload["model"] = model
            payload["reasoning_effort"] = effort_override(cfg) or default_effort
            notes.append(f"{tier} -> {model} effort={payload['reasoning_effort']}")
        elif default_effort:
            # The profile injects no effort (direct ignores reasoning_effort);
            # just swap the sentinel for the model id.
            payload["model"] = model
            notes.append(f"{tier} -> {model}")
        elif isinstance(tier, str) and _is_anthropic_model(tier):
            # A literal Anthropic model (sonnet, claude-sonnet-4-5, opus, ...)
            # bypassed the sentinel system and would bill real Anthropic rates on
            # this profile's upstream. The /model picker can expose these via
            # gateway discovery; rewrite defensively so nothing leaks.
            payload["model"] = model
            notes.append(f"{tier} -> {model} (literal Anthropic model)")

    # DS4_ZDR only ever disables ZDR, never enables it on a profile whose table
    # row does not support it (Nous 403s any provider block at all). Some
    # models have no ZDR-capable host (pro-0813's only endpoint is DeepSeek
    # itself, which rejects the block), so a profile can list model prefixes to
    # skip ZDR for — a configurable escape hatch, not a silent default.
    skip_zdr = cfg.get("zdr_skip_models") or ()
    if (cfg["zdr"] and os.environ.get("DS4_ZDR", "1") != "0"
            and not any(payload.get("model", "").startswith(p) for p in skip_zdr)):
        prov = payload.get("provider")
        if not isinstance(prov, dict):
            prov = {}
        prov["zdr"] = True
        prov["data_collection"] = "deny"
        ignore = [p for p in prov.get("ignore", []) if p not in LOW_CONTEXT]
        prov["ignore"] = ignore + LOW_CONTEXT
        payload["provider"] = prov

    want = payload.get("max_tokens")
    if isinstance(want, int):
        if cfg["max_out"] and want > cfg["max_out"]:
            payload["max_tokens"] = cfg["max_out"]
            notes.append(f"clamped max_tokens {want} -> {cfg['max_out']}")
        # Decide thinking from the post-clamp value so a NOTHINK_BELOW raised
        # above max_out still disables thinking on a clamped request.
        if payload["max_tokens"] <= NOTHINK_BELOW:
            payload["thinking"] = dict(DISABLED)
            notes.append(f"max_tokens={payload['max_tokens']} -> thinking disabled")

    # With thinking off the endpoint stops asking for the block, so only repair
    # a history on requests that still have thinking on.
    if cfg["inject"] and payload.get("thinking") != DISABLED:
        n = inject_missing_thinking(payload)
        if n:
            notes.append(f"injected {n} missing thinking block(s)")

    return ", ".join(notes) or None


# ── spend reporting, for the status line ─────────────────────────────────────

def api_key(name, cfg):
    """Server-side key, read from the profile's settings.json.

    One process serves every profile, so the key must come from the profile's
    own file, never from a process-wide variable that any profile can reach.
    DS4_KEY_<NAME> is a per-profile override on top of that file.
    """
    try:
        with open(os.path.join(cfg["dir"], "settings.json")) as fh:
            k = json.load(fh)["env"].get("ANTHROPIC_AUTH_TOKEN")
            if k:
                return k
    except Exception:
        pass
    return os.environ.get(f"DS4_KEY_{name.upper()}", "")


def client_token(name, cfg):
    """Expected credential for a local client.

    It is deliberately the profile key, not a process-wide token.  Empty keys
    are a configuration error for production profiles; test/custom handlers
    without a configured profile key retain the pre-auth compatibility path.
    """
    return api_key(name, cfg)


def request_requires_zdr(headers, payload):
    """Return and remove the proxy-local per-request ZDR signal."""
    header = headers.get(REQUIRE_ZDR_HEADER, "").strip().lower()
    required = header in ("1", "true", "yes")
    if isinstance(payload, dict) and payload.pop(REQUIRE_ZDR_FIELD, False) is True:
        required = True
    return required


def client_auth_required(name, cfg):
    """Only listeners created by serve() opt into the production boundary."""
    return cfg.get("require_client_auth", False)


def get_json(name, cfg, path, timeout=6):
    req = urllib.request.Request(cfg["upstream"].rstrip("/") + path)
    req.add_header("authorization", "Bearer " + api_key(name, cfg))
    req.add_header("user-agent", UA)
    with urllib.request.urlopen(req, timeout=timeout) as r:
        return json.load(r)


def base_model(cfg):
    """A profile's published model id, with any OR variant suffix (:nitro)
    stripped. Variable-alias pricing, failover remap, and any other consumer
    that matches on the exact upstream id must use this, not cfg['model'].
    """
    m = cfg["model"] or ""
    return m.split(":")[0] if ":" in m else m


def pricing(name, cfg):
    """Per-token rates, cached forever.

    OpenRouter serves per-deployment data under /v1/models/{id}/endpoints, and
    the cheapest endpoint is where ZDR routing mostly lands, so it is the right
    estimate there. Nous has no such sub-resource and publishes the price on the
    model itself in /v1/models, already discounted. Same key names either way.
    The variant suffix (":nitro") is not a published id — look up the base.
    """
    model = base_model(cfg)
    with _lock:
        if _cache.get((name, "pricing")):
            return _cache[(name, "pricing")]
    try:
        d = get_json(name, cfg, f"/v1/models/{model}/endpoints")["data"]
        p = min(d["endpoints"], key=lambda e: float(e["pricing"]["prompt"]))["pricing"]
    except Exception:
        try:
            d = get_json(name, cfg, "/v1/models")["data"]
            p = next(m["pricing"] for m in d if m.get("id") == model)
        except Exception:
            return None
    out = {k: float(p[k]) for k in ("prompt", "completion", "input_cache_read") if k in p}
    with _lock:
        _cache[(name, "pricing")] = out
    return out


def credits(name, cfg):
    """(total_credits, total_usage), cached. Absent on providers without it."""
    now = time.time()
    with _lock:
        entry = _cache.get((name, "credits"))
    if entry and now - entry[0] < CREDITS_TTL:
        return entry[1]
    try:
        d = get_json(name, cfg, "/v1/credits")["data"]
        val = (float(d["total_credits"]), float(d["total_usage"]))
    except Exception:
        return None
    with _lock:
        _cache[(name, "credits")] = (now, val)
    return val


def ledger_append(cfg, usage, now):
    """Sample lifetime usage so a rolling 7-day figure survives restarts."""
    ledger = os.path.join(cfg["dir"], "spend-ledger.jsonl")
    try:
        last = 0.0
        if os.path.exists(ledger):
            with open(ledger, "rb") as fh:
                fh.seek(0, 2)
                fh.seek(max(0, fh.tell() - 4096))
                tail = fh.read().splitlines()
            for raw in reversed(tail):
                try:
                    last = json.loads(raw)["t"]
                    break
                except Exception:
                    continue
        if now - last < LEDGER_MIN_INTERVAL:
            return
        with open(ledger, "a") as fh:
            fh.write(json.dumps({"t": now, "usage": usage}) + "\n")
    except OSError:
        pass


def week_spend(cfg, usage, now):
    """(spend_over_7d, is_partial). Partial = ledger is younger than a week."""
    try:
        rows = []
        with open(os.path.join(cfg["dir"], "spend-ledger.jsonl")) as fh:
            for raw in fh:
                try:
                    r = json.loads(raw)
                    rows.append((float(r["t"]), float(r["usage"])))
                except Exception:
                    continue
    except OSError:
        return None, True
    if not rows:
        return None, True
    rows.sort()
    base = [u for t, u in rows if t <= now - WEEK]
    if base:
        return max(0.0, usage - base[-1]), False
    return max(0.0, usage - rows[0][1]), True


def spend(name, cfg):
    now = time.time()
    out = {"model": cfg["model"], "zdr": cfg["zdr"]}
    p = pricing(name, cfg)
    if p:
        out["pricing"] = p
    c = credits(name, cfg)
    if c:
        total, usage = c
        out["remaining"] = total - usage
        out["usage"] = usage
        ledger_append(cfg, usage, now)
        wk, partial = week_spend(cfg, usage, now)
        if wk is not None:
            out["week"] = wk
            out["week_partial"] = partial
    return out


# ── lifecycle ────────────────────────────────────────────────────────────────

def sessions_live(cfg):
    """True if any registered session PID is alive. Clears tokens that are not.

    The directory is .ds4-sessions, never sessions: the latter is Claude Code's
    own state directory and nothing here may reap a file it did not create.
    """
    d = os.path.join(cfg["dir"], ".ds4-sessions")
    try:
        names = os.listdir(d)
    except OSError:
        return False
    live = False
    for n in names:
        try:
            pid = int(n)
        except ValueError:
            continue
        try:
            os.kill(pid, 0)
        except ProcessLookupError:
            try:
                os.unlink(os.path.join(d, n))
            except OSError:
                pass
            continue
        except OSError:
            pass          # PermissionError and friends still mean it exists
        live = True
    return live


def claude_running(cfg):
    """True if a live claude process is using this profile.

    The check that does not depend on the launcher: `ps -E` prints each
    process's environment and Claude Code is started with CLAUDE_CONFIG_DIR set,
    so a session is visible however it was launched. Session tokens stay as a
    second signal because `ps -E` is macOS spelling. A boundary match keeps a
    backup directory with this one as a prefix from pinning the proxy up.
    """
    try:
        out = subprocess.run(["ps", "-E", "-ax", "-o", "command="],
                             stdout=subprocess.PIPE, stderr=subprocess.DEVNULL,
                             timeout=10).stdout.decode("utf8", "replace")
    except Exception:
        return False
    return bool(re.search(re.escape("CLAUDE_CONFIG_DIR=" + cfg["dir"]) + r"(?=\s|$)", out))


def anything_in_use(served):
    return any(sessions_live(cfg) or claude_running(cfg) for cfg in served.values())


def idle_watch(served):
    # Poll no slower than half the timeout, so a short DS4_IDLE_EXIT is honoured
    # promptly instead of being rounded up to the poll interval.
    every = max(1, min(30, IDLE_EXIT // 2))
    while IDLE_EXIT > 0:
        time.sleep(every)
        with _lock:
            open_req = _inflight
        if time.time() - _last_seen < IDLE_EXIT or anything_in_use(served) or open_req:
            continue
        print(f"no profile in use and idle for {IDLE_EXIT}s, exiting", flush=True)
        os._exit(0)


# ── serving ──────────────────────────────────────────────────────────────────

def make_handler(name, cfg):
    class Handler(http.server.BaseHTTPRequestHandler):
        protocol_version = "HTTP/1.1"

        def log_message(self, *a):
            pass

        def _json(self, status, obj):
            msg = json.dumps(obj).encode()
            self.send_response(status)
            self.send_header("content-type", "application/json")
            self.send_header("content-length", str(len(msg)))
            self.send_header("connection", "close")
            self.end_headers()
            self.wfile.write(msg)

        def _touch(self):
            global _last_seen
            _last_seen = time.time()

        def _count(self, n):
            global _inflight
            with _lock:
                _inflight += n

        def do_GET(self):
            self._touch()
            self._count(1)
            try:
                if self.path != "/__spend" or not cfg["spend"]:
                    self._json(404, {"error": {"message": "not found"}})
                    return
                self._json(200, spend(name, cfg))
            finally:
                self._count(-1)

        def do_POST(self):
            self._touch()
            self._count(1)
            try:
                expected = client_token(name, cfg)
                supplied = self.headers.get("authorization", "")
                if (client_auth_required(name, cfg)
                        and (not expected or not hmac.compare_digest(
                            supplied, "Bearer " + expected))):
                    self._json(401, {"error": {"message": "invalid proxy client credential"}})
                    return
                self._relay()
            finally:
                self._count(-1)

        def _relay(self):
            body = self.rfile.read(int(self.headers.get("content-length", 0)))
            try:
                payload = json.loads(body)
            except ValueError:
                payload = None

            eff_cfg = cfg
            eff_name = name
            trial = False
            # The client-sent tier, before the failover remap rewrites
            # payload["model"] to the target's literal id. should_retry must
            # see this original tier or a failed-over main-loop request would
            # be retried in-proxy on top of the main thread's own 10x-backoff
            # retry. Default None: a non-JSON body never reaches should_retry.
            orig_tier = payload.get("model") if isinstance(payload, dict) else None
            if isinstance(payload, dict):
                requires_zdr = request_requires_zdr(self.headers, payload)
                if requires_zdr:
                    if not cfg["zdr"] or os.environ.get("DS4_ZDR", "1") == "0":
                        self._json(409, {"error": {"message": "request requires ZDR, but this route cannot enforce it"}})
                        return
                # The classifier is the small ds4-high call that gates every
                # tool call in auto mode. It is already an Anthropic-shaped
                # request, so we forward it before the ds4 rewrite touches it
                # (the ds4 sentinel/effort logic must not see it). The route is
                # DS4_CLASSIFIER: anthropic -> subscription, zdr -> or-ds4
                # (OpenRouter, ZDR on), ds4 -> this profile's own upstream.
                # Fail open to ds4 on any failure so auto mode never bricks.
                # A request that demands ZDR is excluded: its ZDR provider
                # block is injected by rewrite() on this route, and the
                # classifier relays rebuild the body from a whitelist that
                # cannot carry it. The marker is a routing demand — a
                # ZDR-demanding classifier request stays on its ZDR route.
                if (not requires_zdr
                        and CLASSIFIER_ROUTE in ("anthropic", "zdr")
                        and _classifier.is_classifier(payload, NOTHINK_BELOW)):
                    if CLASSIFIER_ROUTE == "zdr":
                        ep = self._or_ds4_endpoint(payload)
                        if ep is not None:
                            body2, url, key = ep
                            if self._relay_or_ds4(body2, url, key):
                                return
                    ep = _classifier.anthropic_endpoint(payload, CLASSIFIER_MODEL)
                    if ep is not None:
                        body2, token = ep
                        if self._relay_anthropic(body2, token):
                            return

                eff_cfg, eff_name, trial = failover_effective(name, cfg)
                # An armed trial is a request routed back to the profile's OWN
                # upstream once the half-open probes have passed. The target
                # serving a request is not evidence the profile recovered — only
                # the profile's own upstream handling a real request is. The
                # relay closes the circuit on this request's success below.

                note = rewrite(payload, eff_cfg)

                # The failover target (direct) takes real model names and
                # ignores reasoning_effort, so a ds4-* sentinel the rewrite left
                # untouched maps onto one of its models.
                if eff_cfg is not cfg and isinstance(payload.get("model"), str):
                    # A :nitro variant suffix is not a FAILOVER_MODEL key; the
                    # direct target 400s on it. Match on the base id.
                    base = payload["model"].split(":")[0]
                    payload["model"] = FAILOVER_MODEL.get(base, payload["model"])

                # vision: replace image blocks with text descriptions before
                # the body is serialized, so the upstream never receives an
                # image-shaped block. (When DS4_VISION=0 this is skipped and
                # the old pass-through — image blocks forwarded unchanged — is
                # restored, deliberately.)
                if VISION:
                    cache_dir = os.path.join(cfg["dir"], "vision-cache")
                    try:
                        total, fresh = _vision.rewrite_images(payload, cache_dir)
                        if VERBOSE and total:
                            print(f"  [{eff_name}] vision: {total} image(s), {fresh} fresh",
                                  flush=True)
                    except Exception as e:
                        # Fail open, but never forward an image block: replace
                        # any that remain with the placeholder so the request
                        # is total.
                        _vision.placeholder_remaining(payload)
                        if VERBOSE:
                            print(f"  [{eff_name}] vision failed open: {e}", flush=True)

                body = json.dumps(payload).encode()
                if VERBOSE and note:
                    print(f"  [{eff_name}] {note}", flush=True)

            req = urllib.request.Request(
                eff_cfg["upstream"].rstrip("/") + self.path, data=body, method="POST")
            failover_auth = eff_cfg is not cfg
            for k, v in self.headers.items():
                low = k.lower()
                # On a failed-over request the client's Authorization is this
                # profile's key, which the target rejects — drop it and add the
                # target's own below.
                if low in ("host", "content-length", "accept-encoding",
                           REQUIRE_ZDR_HEADER) \
                        or (failover_auth and low == "authorization"):
                    continue
                req.add_header(k, v)
            req.add_header("user-agent", UA)   # Cloudflare-safe; overrides the client's
            req.add_header("content-length", str(len(body)))
            if failover_auth:
                req.add_header("authorization",
                               "Bearer " + api_key(cfg["failover"], eff_cfg))

            # Transient upstream errors (rate limit / overload / cloudflare
            # timeout) kill a claude -p subagent when forwarded raw: the worker
            # dies with "Execution error" and loses whatever it was about to
            # send. The main thread has its own 10x-backoff retry, so only
            # retry subagent-tier requests here (the model sentinel tells which
            # tier: subagents default to ds4-high via CLAUDE_CODE_SUBAGENT_MODEL,
            # the main loop to ds4-xhigh via ANTHROPIC_MODEL). Non-transient
            # statuses pass through unchanged.
            do_retry = should_retry(orig_tier)
            open_kw = {} if RELAY_TIMEOUT <= 0 else {"timeout": RELAY_TIMEOUT}
            up = None
            last_err = None
            for attempt in range(RETRY_ATTEMPTS if do_retry else 1):
                try:
                    up = urllib.request.urlopen(req, **open_kw)
                    break
                except urllib.error.HTTPError as e:
                    last_err = e
                    if e.code not in TRANSIENT_STATUS or attempt + 1 >= RETRY_ATTEMPTS:
                        break
                    if VERBOSE:
                        print(f"  [{eff_name}] <- {e.code}, retrying {attempt + 1}/{RETRY_ATTEMPTS}",
                              flush=True)
                    time.sleep(RETRY_BACKOFF * (attempt + 1))
                except Exception as e:
                    last_err = e
                    break

            failover_record(name, cfg, eff_cfg, up, last_err)

            # A clean trial — the armed request that rode the profile's OWN
            # upstream after the half-open probes passed — is the evidence the
            # probe cannot carry: the upstream handled a real request, so the
            # circuit closes. A transient response or connection failure keeps
            # it open (and resets the probe streak so the next recovery starts
            # fresh).
            if trial and up is not None and up.status not in TRANSIENT_STATUS:
                failover_trial_close(name)
            elif trial and (up is None or up.status in TRANSIENT_STATUS):
                with _lock:
                    _failover_state(name)["probes"] = 0

            if up is None:
                if isinstance(last_err, urllib.error.HTTPError):
                    up = last_err      # exhausted retries; forward the transient error
                else:
                    msg = json.dumps(
                        {"error": {"message": f"proxy upstream failure: {last_err}"}}).encode()
                    self.send_response(502)
                    self.send_header("content-type", "application/json")
                    self.send_header("content-length", str(len(msg)))
                    self.send_header("x-ds4-upstream", eff_cfg["upstream"])
                    self.send_header("connection", "close")
                    self.end_headers()
                    self.wfile.write(msg)
                    return

            if VERBOSE and up.status != 200:
                print(f"  [{eff_name}] <- {up.status}", flush=True)

            self._stream(up, eff_cfg["upstream"])

        def _or_ds4_endpoint(self, payload):
            """The or-ds4 (OpenRouter ZDR) classifier request, or None.

            Reads the openrouter profile from PROFILES; None when the profile
            isn't installed, has no key, or is mid-failover (the breaker would
            otherwise send the classifier to a dead upstream on a security
            gate). The model defaults to the profile's own; the ZDR block is
            forced on in _relay_or_ds4.
            """
            ocfg = PROFILES.get("openrouter")
            if not ocfg or not os.path.isdir(ocfg["dir"]):
                return None
            okey = api_key("openrouter", ocfg)
            if not okey:
                return None
            if failover_effective("openrouter", ocfg)[0] is not ocfg:
                return None
            model = ORDS4_CLASSIFIER_MODEL or ocfg["model"]
            return _classifier.or_ds4_endpoint(payload, model, ocfg["upstream"], okey)

        def _relay_or_ds4(self, body, url, key):
            """Forward a classifier request to the or-ds4 (OpenRouter ZDR) route.

            The ZDR block is forced on here — an or-ds4 classifier that silently
            lost ZDR would violate the reason this route exists. Returns True
            when the request was fully handled, False when it failed and the
            caller should fall through to the ds4 relay. A 400 is streamed as-is
            — the classifier's shape being rejected means Claude Code sent
            something unexpected, and failing open would mask it.
            """
            # or_ds4_endpoint builds a plain messages body; the ZDR block is
            # OpenRouter-specific and stays out of classifier.py.
            body["provider"] = {"zdr": True, "data_collection": "deny"}
            raw = json.dumps(body).encode()
            req = urllib.request.Request(url, data=raw, method="POST")
            req.add_header("authorization", "Bearer " + key)
            req.add_header("anthropic-version", _classifier.ANTHROPIC_VERSION)
            req.add_header("content-type", "application/json")
            req.add_header("content-length", str(len(raw)))
            req.add_header("user-agent", UA)
            try:
                up = urllib.request.urlopen(req)
            except urllib.error.HTTPError as e:
                if e.code == 400:
                    self._stream(e, url)
                    return True
                if VERBOSE:
                    print(f"  [{name}] classifier(or-ds4) <- {e.code}, failing open to ds4",
                          flush=True)
                return False
            except Exception as e:
                if VERBOSE:
                    print(f"  [{name}] classifier(or-ds4) upstream failure: {e}, failing open to ds4",
                          flush=True)
                return False
            if VERBOSE:
                print(f"  [{name}] classifier -> or-ds4 {up.status}", flush=True)
            self._stream(up, url)
            return True

        def _relay_anthropic(self, body, token):
            """Forward a classifier request to the Anthropic subscription.

            Returns True when the request was fully handled (the reply was
            streamed back), False when it failed and the caller should fall
            through to the ds4 relay. A 400 from Anthropic is streamed as-is —
            the classifier's shape is Anthropic's own, so a 400 means Claude
            Code sent something unexpected and failing open would mask it.
            """
            raw = json.dumps(body).encode()
            req = urllib.request.Request(
                _classifier.CLASSIFIER_UPSTREAM, data=raw, method="POST")
            req.add_header("authorization", "Bearer " + token)
            req.add_header("anthropic-version", _classifier.ANTHROPIC_VERSION)
            req.add_header("content-type", "application/json")
            req.add_header("content-length", str(len(raw)))
            req.add_header("user-agent", UA)
            try:
                up = urllib.request.urlopen(req)
            except urllib.error.HTTPError as e:
                # A 400 is Anthropic rejecting the request shape — relay it so
                # Claude Code sees the real error (headers + body, like the
                # ds4 error-relay). Anything else fails open to ds4.
                if e.code == 400:
                    self._stream(e, _classifier.CLASSIFIER_UPSTREAM)
                    return True
                if VERBOSE:
                    print(f"  [{name}] classifier <- {e.code}, failing open to ds4", flush=True)
                return False
            except Exception as e:
                if VERBOSE:
                    print(f"  [{name}] classifier upstream failure: {e}, failing open to ds4",
                          flush=True)
                return False
            if VERBOSE:
                print(f"  [{name}] classifier -> Anthropic {up.status}", flush=True)
            self._stream(up, _classifier.CLASSIFIER_UPSTREAM)
            return True

        def _stream(self, up, via):
            """Stream an upstream response back to the client, then close it.

            via is the upstream that served it. The client can't tell from its
            base URL (the proxy is anonymous to it) — the error template even
            names 127.0.0.1 — so the real gateway rides out on a header.
            """
            self.send_response(up.status)
            for k, v in up.headers.items():
                if k.lower() not in ("transfer-encoding", "content-encoding", "connection"):
                    self.send_header(k, v)
            self.send_header("x-ds4-upstream", via)
            self.send_header("connection", "close")
            self.end_headers()
            try:
                while True:
                    chunk = up.read(8192)
                    if not chunk:
                        break
                    self.wfile.write(chunk)
                    self.wfile.flush()
            finally:
                up.close()

    return Handler


def launchd_sockets(name):
    """The fds launchd bound for this Sockets key, or [] if there are none.

    Under socket activation launchd owns the listener: it binds the port at load
    and hands the already-listening fd to whichever process it starts on the
    first connection. That is what lets this process idle-exit without the port
    going away, and what stops launchd reaping the job as demandless.

    launch_activate_socket is the only supported way to collect those fds and
    CPython has no binding for it, hence ctypes. A nonzero return is the normal
    path when nothing launched us: ESRCH means "not a launchd job", ENOENT means
    the plist has no socket by this name. Both mean "bind it yourself".
    """
    try:
        libc = ctypes.CDLL(ctypes.util.find_library("c"), use_errno=True)
        activate = libc.launch_activate_socket
    except (OSError, AttributeError):
        return []
    activate.restype = ctypes.c_int
    activate.argtypes = [ctypes.c_char_p,
                         ctypes.POINTER(ctypes.POINTER(ctypes.c_int)),
                         ctypes.POINTER(ctypes.c_size_t)]
    libc.free.argtypes = [ctypes.c_void_p]
    libc.free.restype = None

    fds = ctypes.POINTER(ctypes.c_int)()
    count = ctypes.c_size_t(0)
    if activate(name.encode(), ctypes.byref(fds), ctypes.byref(count)) != 0:
        return []
    try:
        return [fds[i] for i in range(count.value)]
    finally:
        # The array is malloc'd for us and documented as the caller's to free.
        libc.free(ctypes.cast(fds, ctypes.c_void_p))


def server_on_fd(fd, handler):
    """An HTTP server on an fd launchd already bound and listened on.

    bind_and_activate=False keeps TCPServer from binding a port of its own, but
    it still constructs a throwaway socket in __init__, so that one is closed
    before the inherited fd takes its place.

    AF_INET is hardcoded because install.sh only ever writes SockNodeName
    127.0.0.1. macOS has no SO_DOMAIN, so there is nothing to detect it from.
    """
    srv = http.server.ThreadingHTTPServer(("127.0.0.1", 0), handler,
                                          bind_and_activate=False)
    srv.socket.close()
    srv.socket = socket.socket(socket.AF_INET, socket.SOCK_STREAM, fileno=fd)
    srv.server_address = srv.socket.getsockname()
    return srv


def serve(name, cfg):
    """Bind one listener. False on bind failure so the rest still get served."""
    port = int(os.environ.get(f"DS4_PORT_{name.upper()}", cfg["port"]))
    # Mark only the real profile listeners. Tests and embedders can construct a
    # handler with a custom cfg without having to mint a credential.
    cfg = dict(cfg, require_client_auth=True)
    handler = make_handler(name, cfg)
    inherited = launchd_sockets(name)
    try:
        if inherited:
            # One key can yield several fds. All of them are already listening,
            # so an fd nobody accepts on is a port that hangs instead of
            # refusing - serve every one rather than picking the first.
            srvs = [server_on_fd(fd, handler) for fd in inherited]
            # launchd's plist is authoritative once it owns the socket, so
            # report where we actually are rather than where we meant to be.
            port = srvs[0].server_address[1]
            origin = "launchd"
        else:
            if REQUIRE_OWNED_SOCKET:
                print(f"  {name:<11} :{port} FAILED: no OS-owned socket (socket activation required)",
                      file=sys.stderr, flush=True)
                return False
            srvs = [http.server.ThreadingHTTPServer(("127.0.0.1", port), handler)]
            origin = "self-bound"
    except OSError as e:
        print(f"  {name:<11} :{port} FAILED to bind: {e}", file=sys.stderr, flush=True)
        return False
    print(f"  {name:<11} :{port} -> {cfg['upstream']} ({origin})", flush=True)
    for srv in srvs:
        threading.Thread(target=srv.serve_forever, daemon=True).start()
    return True


def main():
    # An absent profile directory means that profile is not installed here.
    # Binding its port anyway would be a lie to anyone checking with nc.
    served = {n: c for n, c in PROFILES.items() if os.path.isdir(c["dir"])}
    if not served:
        raise SystemExit("no profile directories found; nothing to serve")

    # install.sh needs the same name/port pairs to write the plist's Sockets
    # block, and the socket keys have to match what serve() asks launchd for.
    # Emitting them from here keeps PROFILES the only place ports are declared.
    if "--ports" in sys.argv:
        for name, cfg in served.items():
            # int() so a junk DS4_PORT_* override fails here rather than being
            # interpolated into the plist install.sh builds from this output.
            print(f"{name} {int(os.environ.get(f'DS4_PORT_{name.upper()}', cfg['port']))}")
        return

    print(f"ds4 proxy: no thinking at or below max_tokens={NOTHINK_BELOW}, "
          f"idle exit {IDLE_EXIT}s", flush=True)
    # The classifier routes to Anthropic by default, but that only works if a
    # subscription token is present. Failing open to ds4 is the documented
    # safety valve (the classifier must not brick auto mode), but a silent
    # fallback on a security gate would hide a misconfiguration — so warn.
    if (CLASSIFIER_ROUTE == "anthropic"
            and _classifier.classifier_token() is None):
        print("  WARNING: classifier routed to Anthropic but "
              "DS4_CLASSIFIER_TOKEN is unset — the classifier will fail open "
              "to ds4. Set it (claude setup-token) and re-run install.sh.",
              file=sys.stderr, flush=True)
    # The zdr route needs the or-ds4 profile installed with a key; without it
    # the classifier silently falls back to Anthropic (then ds4). Warn so the
    # opt-in ZDR gate never quietly becomes something else.
    if CLASSIFIER_ROUTE == "zdr":
        ocfg = PROFILES.get("openrouter")
        if not ocfg or not os.path.isdir(ocfg["dir"]) or not api_key("openrouter", ocfg):
            print("  WARNING: classifier routed to or-ds4 (zdr) but the "
                  "openrouter profile has no API key — the classifier will "
                  "fail open to Anthropic. Install/configure or-ds4, or set "
                  "DS4_CLASSIFIER to 'anthropic' or 'ds4'.",
                  file=sys.stderr, flush=True)
    bound = [serve(name, cfg) for name, cfg in served.items()]
    if not any(bound):
        raise SystemExit("no profile bound; nothing to serve")

    threading.Thread(target=idle_watch, args=(served,), daemon=True).start()
    while True:
        time.sleep(3600)


if __name__ == "__main__":
    main()
