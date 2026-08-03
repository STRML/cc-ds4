"""Shared statusline machinery for the DeepSeek profiles.

Claude Code prices whatever model name you hand it against Anthropic's table. On a
DeepSeek profile that overstates cost by roughly two orders of magnitude — one
measured session showed $0.152731 against $0.002637 actual. This module recomputes
it from the session transcript at real rates and hands a corrected payload to cship.

Correct the payload, don't patch the output. `cost.total_cost_usd` and
`model.display_name` are read from stdin, so fixing them before cship renders is far
more robust than regexing ANSI-coloured text afterwards. `usage_limits` is the
exception: cship fetches it from Anthropic over OAuth rather than from stdin, so it
cannot be corrected at all, only dropped from the config.

Subclass Statusline and implement rates_for(). Override account() to add spend and
balance segments, and label() to change how the model is named.
"""
import json
import os
import re
import subprocess
import sys

# Tokyo Night, matching the shipped cship configs.
COST_NORMAL = "\x1b[38;2;169;177;214m"   # #a9b1d6
COST_WARN = "\x1b[38;2;224;175;104m"     # #e0af68  >= warn_threshold
COST_CRIT = "\x1b[1;38;2;247;118;142m"   # #f7768e  >= crit_threshold
RESET = "\x1b[0m"

WARN_AT = 0.25
CRIT_AT = 1.00
WEEK = 7 * 86400

# Claude Code's usage field names -> our rate keys.
USAGE_FIELDS = {
    "input_tokens": "prompt",
    "output_tokens": "completion",
    "cache_read_input_tokens": "input_cache_read",
    "cache_creation_input_tokens": "input_cache_write",
}


def base_model(name):
    """'deepseek-v4-pro[1m]' -> 'deepseek-v4-pro'.

    The bracket suffix is what tells Claude Code the context window is 1M; DeepSeek
    accepts and strips it. It is not part of the model's identity for pricing.
    """
    return re.sub(r"\[[^\]]*\]$", "", name or "").strip()


def money(v):
    """Dollars to 2dp, with a '<$0.01' floor.

    cship formats cost to 2 decimals unconditionally, which renders a whole DeepSeek
    session as '$0.00'. That is why cost is rendered here instead of by cship.
    """
    if 0 < v < 0.01:
        return "<$0.01"
    return f"${v:,.2f}"


def cost_segment(c):
    style = COST_CRIT if c >= CRIT_AT else COST_WARN if c >= WARN_AT else COST_NORMAL
    return f"{style}\N{MONEY BAG} {money(c)}{RESET}"


def harvest_usage(obj, by_model, fallback_model):
    """Accumulate one transcript record's usage into its model's bucket.

    Bucketing by model matters on the direct profile, where opus/fable reach v4-pro
    and sonnet/haiku reach v4-flash at 3x the price. A session that switched tiers
    would otherwise be costed entirely at whichever was active at render time.
    """
    usage = model = None
    if isinstance(obj, dict):
        msg = obj.get("message")
        if isinstance(msg, dict) and isinstance(msg.get("usage"), dict):
            usage, model = msg["usage"], msg.get("model")
        elif isinstance(obj.get("usage"), dict):
            usage, model = obj["usage"], obj.get("model")
    if not usage:
        return
    bucket = by_model.setdefault(base_model(model) or fallback_model, {})
    for field, rate_key in USAGE_FIELDS.items():
        n = usage.get(field)
        if isinstance(n, int) and n > 0:
            bucket[rate_key] = bucket.get(rate_key, 0) + n


class Statusline:
    prefix = ""              # backend tag on the model name, e.g. "or-" / "ds-"
    default_model = ""       # used when a transcript record names no model

    def __init__(self, profile_dir, cship=None, cship_config=None):
        self.profile = os.path.expanduser(profile_dir)
        self.cship = os.path.expanduser(cship or "~/.cargo/bin/cship")
        self.cship_config = cship_config or os.path.join(self.profile, "cship.toml")
        self.state_dir = os.path.join(self.profile, "cost-state")
        self.dump = os.path.join(self.profile, "last-statusline-input.json")

    # --- pieces subclasses fill in -------------------------------------------

    def rates_for(self, model):
        """USD per token for one model: prompt / completion / input_cache_read."""
        raise NotImplementedError

    def account(self):
        """Optional {'week', 'week_partial', 'remaining'} for the tail segment."""
        return {}

    def label(self, payload, account):
        m = payload.get("model")
        if isinstance(m, dict):
            m["display_name"] = f"{self.prefix}{base_model(m.get('id'))}"

    # --- transcript accounting ------------------------------------------------

    def state_path(self, session_id):
        safe = re.sub(r"[^A-Za-z0-9_.-]", "_", session_id or "unknown")
        return os.path.join(self.state_dir, f"{safe}.json")

    def load_state(self, path):
        try:
            with open(path) as fh:
                s = json.load(fh)
            if isinstance(s, dict) and isinstance(s.get("offset"), int):
                s.setdefault("by_model", {})
                return s
        except (OSError, ValueError):
            pass
        return {"offset": 0, "by_model": {}}

    def save_state(self, path, state):
        try:
            os.makedirs(self.state_dir, exist_ok=True)
            tmp = path + ".tmp"
            with open(tmp, "w") as fh:
                json.dump(state, fh)
            os.replace(tmp, path)
        except OSError:
            pass

    def session_tokens(self, transcript, session_id, fallback_model):
        """Tokens per model for this session, read incrementally.

        State carries a byte offset so each render only parses newly appended lines,
        which keeps the cost constant as the transcript grows.
        """
        if not transcript or not os.path.exists(transcript):
            return None
        path = self.state_path(session_id)
        state = self.load_state(path)
        try:
            size = os.path.getsize(transcript)
        except OSError:
            return None
        # Replaced or truncated (e.g. compaction) — re-read from the top.
        if size < state["offset"]:
            state = {"offset": 0, "by_model": {}}
        if size == state["offset"]:
            return state["by_model"]

        by_model = {k: dict(v) for k, v in state["by_model"].items()}
        try:
            with open(transcript, "rb") as fh:
                fh.seek(state["offset"])
                chunk = fh.read()
                nl = chunk.rfind(b"\n")   # leave a trailing partial line for next time
                if nl == -1:
                    return by_model
                consumed = state["offset"] + nl + 1
                for raw in chunk[: nl + 1].splitlines():
                    if not raw.strip():
                        continue
                    try:
                        harvest_usage(json.loads(raw), by_model, fallback_model)
                    except ValueError:
                        continue
        except OSError:
            return state["by_model"]

        self.save_state(path, {"offset": consumed, "by_model": by_model})
        return by_model

    def cost_of(self, by_model):
        total = 0.0
        for model, tokens in by_model.items():
            rates = self.rates_for(model)
            if not rates:
                continue
            for key, n in tokens.items():
                rate = rates.get(key)
                if rate is None and key == "input_cache_write":
                    rate = rates.get("prompt")  # no separate write price published
                if rate:
                    total += n * rate
        return total

    # --- rendering -------------------------------------------------------------

    def tail_segment(self, cost, account):
        """' 💰 $0.05 · 📆 7d $0.31 · 💳 $18.42 left', omitting whatever is missing.

        Context size is deliberately absent: cship already renders it on its own line.
        """
        bits = []
        if cost is not None:
            bits.append(cost_segment(cost))
        week = account.get("week")
        if isinstance(week, (int, float)):
            mark = "~" if account.get("week_partial") else ""
            bits.append(f"\N{TEAR-OFF CALENDAR} 7d {mark}{money(week)}")
        remaining = account.get("remaining")
        if isinstance(remaining, (int, float)):
            bits.append(f"\N{CREDIT CARD} {money(remaining)} left")
        return ("  " + " \N{MIDDLE DOT} ".join(bits)) if bits else ""

    def render(self, data):
        cmd = [self.cship]
        if os.path.exists(self.cship_config):
            cmd += ["--config", self.cship_config]
        return subprocess.run(
            cmd, input=data, capture_output=True, text=True, timeout=10
        ).stdout

    def read_stdin_payload(self):
        data = sys.stdin.read()
        try:  # the only machine-readable view of Claude Code's resolved context
            with open(self.dump, "w") as fh:   # window, cost, and model name
                fh.write(data)
        except OSError:
            pass
        try:
            return data, json.loads(data)
        except ValueError:
            return data, {}

    def run(self, data=None):
        if data is None:
            data, payload = self.read_stdin_payload()
        else:
            try:
                payload = json.loads(data)
            except ValueError:
                payload = {}

        cost, account = None, {}
        if payload:
            try:
                current = base_model((payload.get("model") or {}).get("id")) \
                    or self.default_model
                by_model = self.session_tokens(
                    payload.get("transcript_path"), payload.get("session_id"), current
                )
                if by_model is not None:
                    cost = self.cost_of(by_model)
                account = self.account()
                # Drop Claude Code's Anthropic-priced figure so it cannot resurface
                # if the cship config is ever changed back to render $cship.cost.
                payload.pop("cost", None)
                self.label(payload, account)
                data = json.dumps(payload)
            except Exception:
                data = json.dumps(payload)

        try:
            out = self.render(data)
        except Exception:
            return
        # A failing cship returns nothing (None). The bar fails open: print a
        # blank line and exit 0, never crash.
        if not out:
            sys.stdout.write("\n")
            return
        try:
            out = out.rstrip("\n") + self.tail_segment(cost, account)
        except Exception:
            pass
        sys.stdout.write(out + "\n")


SELFTEST_PAYLOAD = {
    "session_id": "selftest",
    "transcript_path": "",
    "cost": {"total_cost_usd": 9.42},
    "context_window": {
        "total_input_tokens": 84000,
        "total_output_tokens": 2000,
        "context_window_size": 1048576,
        "used_percentage": 8,
    },
}


def selftest_payload(model_id):
    """A wrapper that swallows exceptions turns a NameError into a blank bar and
    exit 0, so every entry point ships a visible render test."""
    p = dict(SELFTEST_PAYLOAD)
    p["cwd"] = os.getcwd()
    p["model"] = {"id": model_id, "display_name": model_id}
    p["workspace"] = {"current_dir": os.getcwd(), "project_dir": os.getcwd()}
    return json.dumps(p)
