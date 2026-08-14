#!/usr/bin/env python3
"""Freeze the Python proxy's rewrite output as a golden file for Go.

The differential harness compared a live Python proxy against the Go one. That
worked while both existed; once Python is deleted the oracle goes with it. This
script captures what the oracle actually produced -- the exact bytes rewrite()
emits for every corpus case on every profile -- so the assertions outlive the
implementation that justified them.

Run it against the Python tree BEFORE deleting src/proxy.py:

    python3 tests/diff/dump_golden.py > src/go/internal/proxy/testdata/rewrite_golden.json

The Go side replays it in TestRewriteMatchesPythonGolden. A mismatch there means
the Go rewrite drifted from behavior that shipped for months, which is worth a
deliberate decision and a regenerated golden, not a silent edit.
"""
import json
import os
import sys

HERE = os.path.dirname(os.path.abspath(__file__))
sys.path.insert(0, os.path.dirname(HERE))                             # tests/
sys.path.insert(0, os.path.join(os.path.dirname(HERE), "..", "src"))  # src/

import corpus  # noqa: E402  (path set above)
import proxy   # noqa: E402


def main():
    out = []
    for label, method, path, _headers, body in corpus.cases():
        if method != "POST" or body is None:
            continue
        for profile in ("direct", "openrouter", "nous"):
            payload = json.loads(body)
            note = proxy.rewrite(payload, proxy.PROFILES[profile])
            out.append({
                "case": label,
                "profile": profile,
                # The input rides along with its output. Transcribing corpus
                # bodies into the Go test by hand put the two halves out of
                # sync on the first non-ASCII case; emitting both keeps them a
                # matched pair by construction.
                "body": body.decode("utf-8"),
                # Exactly the call proxy.py:984 makes to build the outbound
                # body, defaults included. ensure_ascii defaults to True, so
                # non-ASCII goes on the wire as \uXXXX; passing
                # ensure_ascii=False here would freeze a golden the proxy never
                # actually sent and fail Go for matching reality.
                "want": json.dumps(payload),
                "note": note,
            })
    json.dump(out, sys.stdout, indent=2, ensure_ascii=False)
    sys.stdout.write("\n")


if __name__ == "__main__":
    main()
