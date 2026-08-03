#!/usr/bin/env python3
"""Statusline for the OpenRouter profile (claude-or-ds4).

Rates, weekly spend, and credit balance all come from the effort proxy's GET
/__spend, so this file hardcodes nothing that OpenRouter can change under it. The
fallback rates below are only used when the proxy is unreachable.
"""
import json
import os
import sys
import urllib.request

# realpath, not abspath: install.sh symlinks this into the profile directory and
# abspath would resolve the bootstrap against the profile instead of the checkout.
sys.path.insert(0, os.path.dirname(os.path.dirname(os.path.realpath(__file__))))
from statusline.common import Statusline, base_model, selftest_payload  # noqa: E402

# deepseek/deepseek-v4-flash-0731 list prices, USD per token. Only a fallback —
# the proxy serves live figures pulled from OpenRouter's endpoints API.
FALLBACK_RATES = {
    "prompt": 0.09e-6,
    "completion": 0.18e-6,
    "input_cache_read": 0.018e-6,
}


class OpenRouterStatusline(Statusline):
    # "or-" names the backend, not the endpoint that served the request. OpenRouter
    # re-routes between providers request to request, so a live provider name would
    # flicker between DeepInfra, Novita, and SiliconFlow while nothing meaningful
    # changed. The serving provider is available if ever wanted: it appears as
    # `provider` in response bodies and in the SSE message_start event.
    prefix = "or-"
    default_model = "deepseek/deepseek-v4-flash-0731"

    def __init__(self, profile_dir="~/.claude-or-ds4", port=None, **kw):
        super().__init__(profile_dir, **kw)
        port = port or os.environ.get("DS4_PROXY_PORT", "31501")
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
        i = self.info()
        return {k: i[k] for k in ("week", "week_partial", "remaining") if k in i}

    def label(self, payload, account):
        m = payload.get("model")
        if not isinstance(m, dict):
            return
        sentinel = m.get("id") or ""
        tier = sentinel[4:] if sentinel.startswith("ds4-") else ""
        real = self.info().get("model")
        if real:
            m["display_name"] = f"{self.prefix}{real.split('/')[-1]} {tier}".strip()
        else:
            # Proxy unreachable — keep the sentinel visible so the fault shows.
            m["display_name"] = f"{self.prefix}{sentinel} (proxy?)"


def main():
    sl = OpenRouterStatusline()
    sl.run(selftest_payload("ds4-xhigh") if sys.stdin.isatty() else None)


if __name__ == "__main__":
    main()
