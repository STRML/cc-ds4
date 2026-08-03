#!/usr/bin/env python3
"""Statusline for the DeepSeek-direct profile (claude-ds4).

This profile's proxy is thinking_proxy.py, which serves no /__spend, so both
numbers come from DeepSeek's own API rather than from the proxy. That is what
differs from the OpenRouter sibling:

  * Rates are hardcoded. DeepSeek publishes no pricing endpoint — /models returns
    bare ids — so the table below is transcribed from their docs and has to be
    rechecked by hand.
  * Spend is integrated from a balance, not read as a usage counter. /user/balance
    goes down as you spend and up when you top up, so a naive difference would
    report a top-up as negative spend.
"""
import json
import os
import sys
import time
import urllib.request

# realpath, not abspath: install.sh symlinks this into the profile directory and
# abspath would resolve the bootstrap against the profile instead of the checkout.
sys.path.insert(0, os.path.dirname(os.path.dirname(os.path.realpath(__file__))))
from statusline.common import Statusline, WEEK, base_model, selftest_payload  # noqa: E402

BALANCE_URL = "https://api.deepseek.com/user/balance"
BALANCE_TTL = 60           # the bar renders constantly; don't hammer the endpoint
LEDGER_MIN_INTERVAL = 300  # sample at most every 5 minutes

# USD per token, from https://api-docs.deepseek.com/quick_start/pricing (their
# per-million figures / 1e6). Recheck when installing: their docs flag a coming
# peak/off-peak policy charging 2x these rates at peak.
RATES = {
    "deepseek-v4-flash": {
        "prompt": 0.14e-6,
        "completion": 0.28e-6,
        "input_cache_read": 0.0028e-6,
    },
    "deepseek-v4-pro": {
        "prompt": 0.435e-6,
        "completion": 0.87e-6,
        "input_cache_read": 0.003625e-6,
    },
}


def week_spend(rows, now):
    """(spend_over_7d, is_partial) from cumulative-spend ledger rows."""
    if not rows:
        return None, True
    latest = rows[-1][2]
    base = [s for t, _b, s in rows if t <= now - WEEK]
    if base:
        return max(0.0, latest - base[-1]), False
    return max(0.0, latest - rows[0][2]), True


class DirectStatusline(Statusline):
    prefix = "ds-"
    default_model = "deepseek-v4-flash"

    def __init__(self, profile_dir="~/.claude-ds4", **kw):
        super().__init__(profile_dir, **kw)
        self.ledger = os.path.join(self.profile, "spend-ledger.jsonl")
        self.balance_cache = os.path.join(self.profile, "balance-cache.json")

    def rates_for(self, model):
        return RATES.get(model) or RATES[self.default_model]

    def api_key(self):
        try:
            env = json.load(open(os.path.join(self.profile, "settings.json")))["env"]
            return env.get("ANTHROPIC_AUTH_TOKEN") or ""
        except Exception:
            return ""

    def balance(self):
        """Total USD balance. Cached to a file, since there is no daemon here."""
        now = time.time()
        try:
            c = json.load(open(self.balance_cache))
            if now - c["t"] < BALANCE_TTL:
                return c["balance"]
        except Exception:
            pass
        key = self.api_key()
        if not key:
            return None
        try:
            req = urllib.request.Request(BALANCE_URL)
            req.add_header("authorization", "Bearer " + key)
            with urllib.request.urlopen(req, timeout=1.5) as r:
                d = json.load(r)
            usd = next(b for b in d["balance_infos"] if b["currency"] == "USD")
            val = float(usd["total_balance"])
        except Exception:
            return None
        try:
            tmp = self.balance_cache + ".tmp"
            with open(tmp, "w") as fh:
                json.dump({"t": now, "balance": val}, fh)
            os.replace(tmp, self.balance_cache)
        except OSError:
            pass
        return val

    def ledger_rows(self):
        rows = []
        try:
            with open(self.ledger) as fh:
                for raw in fh:
                    try:
                        r = json.loads(raw)
                        rows.append(
                            (float(r["t"]), float(r["balance"]), float(r["spent"]))
                        )
                    except Exception:
                        continue
        except OSError:
            return []
        rows.sort()
        return rows

    def ledger_update(self, bal, now):
        """Append a sample carrying cumulative spend.

        Spend is integrated from the drops between samples. A rise means a top-up
        and contributes nothing, which is what keeps a top-up from reading as
        negative spend.
        """
        rows = self.ledger_rows()
        if rows and now - rows[-1][0] < LEDGER_MIN_INTERVAL:
            return rows
        prev_bal, prev_spent = (rows[-1][1], rows[-1][2]) if rows else (bal, 0.0)
        spent = prev_spent + max(0.0, prev_bal - bal)
        try:
            with open(self.ledger, "a") as fh:
                fh.write(json.dumps({"t": now, "balance": bal, "spent": spent}) + "\n")
        except OSError:
            return rows
        return rows + [(now, bal, spent)]

    def account(self):
        bal = self.balance()
        if bal is None:
            return {}
        now = time.time()
        week, partial = week_spend(self.ledger_update(bal, now), now)
        out = {"remaining": bal}
        if week is not None:
            out["week"] = week
            out["week_partial"] = partial
        return out


def main():
    sl = DirectStatusline()
    sl.run(selftest_payload("deepseek-v4-flash[1m]") if sys.stdin.isatty() else None)


if __name__ == "__main__":
    main()
