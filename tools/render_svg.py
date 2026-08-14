#!/usr/bin/env python3
"""Render the real status lines to an SVG for the README.

Runs both bars, captures their ANSI, and converts SGR colour runs to tspans, so
the image cannot drift from what the code actually prints. Regenerate with:

    python3 tools/render_svg.py > assets/statusline.svg
"""
import json, os, re, subprocess, sys

REPO = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
SGR = re.compile(r"\x1b\[([0-9;]*)m")
BG, FG = "#1a1b26", "#a9b1d6"          # Tokyo Night
CW, LH, PAD = 8.4, 30, 22              # char width, line height, padding


def payload(model_id):
    return json.dumps({
        "session_id": "readme", "transcript_path": "", "cwd": REPO,
        "model": {"id": model_id, "display_name": model_id},
        "workspace": {"current_dir": REPO, "project_dir": REPO},
        "cost": {"total_cost_usd": 9.42},
        "context_window": {"total_input_tokens": 84000, "total_output_tokens": 2000,
                           "context_window_size": 1048576, "used_percentage": 8},
    })


# Fixed demo figures. The image must not depend on a live proxy, a network call,
# or the author's actual account balance.
DEMO = {"week": 0.31, "week_partial": False, "remaining": 18.42}
DEMO_COST = 0.0487


def capture(kind, model_id):
    sys.path.insert(0, os.path.join(REPO, "src"))
    if kind == "direct":
        from statusline.direct import DirectStatusline as Cls
        sl = Cls()
    else:
        from statusline.openrouter import OpenRouterStatusline as Cls
        sl = Cls()
        sl._info = {"model": "deepseek/deepseek-v4-flash-0731"}
    sl.account = lambda: dict(DEMO)
    sl.session_tokens = lambda *a: {"demo": {}}
    sl.cost_of = lambda *a: DEMO_COST
    import io, contextlib
    buf = io.StringIO()
    with contextlib.redirect_stdout(buf):
        sl.run(payload(model_id))
    return buf.getvalue().rstrip("\n").splitlines()[-1]


def runs(line):
    """[(text, colour, bold)] from an ANSI line."""
    out, pos, fg, bold = [], 0, FG, False
    for m in SGR.finditer(line):
        if m.start() > pos:
            out.append((line[pos:m.start()], fg, bold))
        codes = [c for c in m.group(1).split(";") if c]
        i = 0
        while i < len(codes):
            c = codes[i]
            if c in ("0", ""):
                fg, bold = FG, False
            elif c == "1":
                bold = True
            elif c == "38" and codes[i+1:i+2] == ["2"]:
                fg = "#%02x%02x%02x" % tuple(int(x) for x in codes[i+2:i+5])
                i += 4
            i += 1
        pos = m.end()
    if pos < len(line):
        out.append((line[pos:], fg, bold))
    return [(t, c, b) for t, c, b in out if t]


def esc(s):
    return s.replace("&", "&amp;").replace("<", "&lt;").replace(">", "&gt;")


def main():
    lines = [capture("direct", "deepseek-v4-flash[1m]"),
             capture("openrouter", "ds4-pro-xhigh")]
    parsed = [runs(l) for l in lines]
    width = max(sum(len(t) for t, _, _ in p) for p in parsed) * CW + PAD * 2
    height = len(parsed) * LH + PAD * 2

    print(f'<svg xmlns="http://www.w3.org/2000/svg" width="{width:.0f}" '
          f'height="{height:.0f}" viewBox="0 0 {width:.0f} {height:.0f}" '
          f'font-family="ui-monospace,SFMono-Regular,Menlo,monospace" font-size="14">')
    print(f'<rect width="100%" height="100%" rx="8" fill="{BG}"/>')
    for row, p in enumerate(parsed):
        y = PAD + LH * row + 18
        x = PAD
        print(f'<text y="{y}" xml:space="preserve">', end="")
        for text, colour, bold in p:
            w = 'font-weight="600" ' if bold else ""
            print(f'<tspan x="{x:.1f}" {w}fill="{colour}">{esc(text)}</tspan>', end="")
            x += len(text) * CW
        print("</text>")
    print("</svg>")


if __name__ == "__main__":
    main()
