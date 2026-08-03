#!/usr/bin/env python3
"""Statusline for the Nous Portal profile (claude-nous).

Rates come from the effort proxy's GET /__spend. Nous Portal is a flat
subscription (optionally topped up with credits), but it exposes no public
credits/balance endpoint, so the weekly-spend and credit-balance tail segments
are omitted and only the session cost is shown.

No backend tag on the model name: Nous Portal is the only router behind this
profile, so "deepseek-v4-flash-0731" is already unambiguous. (OpenRouter uses
"or-" and the direct profile "ds-" for the same reason.)
"""
import json
import os
import sys
import urllib.request

# realpath, not abspath: install.sh symlinks this into the profile directory and
# abspath would resolve the bootstrap against the profile instead of the checkout.
sys.path.insert(0, os.path.dirname(os.path.dirname(os.path.realpath(__file__))))
from statusline.common import Statusline, selftest_payload  # noqa: E402

# deepseek/deepseek-v4-flash-0731 discounted (90%-off) rates, USD per token. Only a
# fallback — the proxy serves live figures pulled from Nous's /v1/models.
FALLBACK_RATES = {
    "prompt": 0.01e-6,
    "completion": 0.02e-6,
    "input_cache_read": 0.0,
}


class NousStatusline(Statusline):
    prefix = ""
    default_model = "deepseek/deepseek-v4-flash-0731"

    def __init__(self, profile_dir="~/.claude-nous", port=None, **kw):
        super().__init__(profile_dir, **kw)
        port = port or os.environ.get("NOUS_PROXY_PORT", "31502")
        self.spend_url = f"http://127.0.0.1:{port}/__spend"
        self._info = None

    def info(self):
        if self._info is None:
            try:
                with urllib.request.urlopen(self.spend_url, timeout=1.5) as r:
                    self._info = json.load(r)
            except Exception:
                self._info = {}
        return self._info

    def rates_for(self, model):
        return self.info().get("pricing") or FALLBACK_RATES

    def account(self):
        # Nous is a subscription with no public credits endpoint; no /__spend
        # week/remaining figures to surface.
        return {}

    def label(self, payload, account):
        m = payload.get("model")
        if not isinstance(m, dict):
            return
        sentinel = m.get("id") or ""
        tier = sentinel[4:] if sentinel.startswith("ds4-") else ""
        real = self.info().get("model")
        if real:
            m["display_name"] = f"{real.split('/')[-1]} {tier}".strip()
        else:
            # Proxy unreachable — keep the sentinel visible so the fault shows.
            m["display_name"] = f"{sentinel} (proxy?)"


def main():
    sl = NousStatusline()
    sl.run(selftest_payload("ds4-xhigh") if sys.stdin.isatty() else None)


if __name__ == "__main__":
    main()
