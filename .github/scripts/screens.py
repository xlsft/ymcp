#!/usr/bin/env python3
"""Turns the TUI screens TestScreens writes into pictures.

    YMCP_SCREENS=screens go test -run TestScreens ./internal/pipeline/tui/views
    python3 .github/scripts/screens.py screens FONT_DIR OUT_DIR

Each NAME.ans becomes OUT_DIR/NAME.png, drawn by headless Chrome ($CHROME,
google-chrome by default) at twice the size, in JetBrains Mono from
FONT_DIR (JetBrainsMono-{Regular,Bold,Italic}.woff2), and OUT_DIR/NAME.html,
the same screen as text.
"""
import html
import math
import os
import pathlib
import re
import subprocess
import sys

# A dark terminal palette for the 16 named colours; true colours pass through.
PALETTE = [
    "#1d1919", "#e0574f", "#7fbf6a", "#e0b25a", "#6c9ee0", "#c58ad8", "#5fbfbf", "#d8d0cf",
    "#6b5f5e", "#ff7a70", "#9edc87", "#f5cf7a", "#8fb8f5", "#dca6ee", "#7fd8d8", "#ffffff",
]
FG, BG = "#e9e2e1", "#141111"
FONT_PX, LINE, PAD = 14, 1.2, 24
CELL = 0.6  # JetBrains Mono's advance, in em


def c256(n):
    if n < 16:
        return PALETTE[n]
    if n < 232:
        n -= 16
        steps = [0, 95, 135, 175, 215, 255]
        return "#%02x%02x%02x" % (steps[n // 36], steps[(n // 6) % 6], steps[n % 6])
    v = 8 + (n - 232) * 10
    return "#%02x%02x%02x" % (v, v, v)


class Pen:
    """The SGR state: what the next text is drawn with."""

    def __init__(self):
        self.reset()

    def reset(self):
        self.fg = self.bg = None
        self.bold = self.faint = self.italic = self.under = self.rev = self.strike = False

    def css(self):
        fg, bg = self.fg, self.bg
        if self.rev:
            fg, bg = (bg or BG), (fg or FG)
        s = []
        if fg:
            s.append("color:" + fg)
        if bg:
            s.append("background:" + bg)
        if self.bold:
            s.append("font-weight:700")
        if self.faint:
            s.append("opacity:.5")
        if self.italic:
            s.append("font-style:italic")
        deco = [d for d, on in (("underline", self.under), ("line-through", self.strike)) if on]
        if deco:
            s.append("text-decoration:" + " ".join(deco))
        return ";".join(s)

    def sgr(self, params):
        p = [int(x) if x else 0 for x in params.split(";")] if params else [0]
        i = 0
        while i < len(p):
            n = p[i]
            if n == 0:
                self.reset()
            elif n == 1:
                self.bold = True
            elif n == 2:
                self.faint = True
            elif n == 3:
                self.italic = True
            elif n == 4:
                self.under = True
            elif n == 7:
                self.rev = True
            elif n == 9:
                self.strike = True
            elif n == 22:
                self.bold = self.faint = False
            elif n == 23:
                self.italic = False
            elif n == 24:
                self.under = False
            elif n == 27:
                self.rev = False
            elif n == 29:
                self.strike = False
            elif 30 <= n <= 37:
                self.fg = PALETTE[n - 30]
            elif 90 <= n <= 97:
                self.fg = PALETTE[n - 82]
            elif 40 <= n <= 47:
                self.bg = PALETTE[n - 40]
            elif 100 <= n <= 107:
                self.bg = PALETTE[n - 92]
            elif n == 39:
                self.fg = None
            elif n == 49:
                self.bg = None
            elif n in (38, 48) and i + 1 < len(p):
                col = None
                if p[i + 1] == 2 and i + 4 < len(p):
                    col = "#%02x%02x%02x" % tuple(p[i + 2:i + 5])
                    i += 4
                elif p[i + 1] == 5 and i + 2 < len(p):
                    col = c256(p[i + 2])
                    i += 2
                if n == 38:
                    self.fg = col
                else:
                    self.bg = col
            i += 1


# SGR, then OSC (hyperlinks; dropped), then any other CSI (dropped), then text.
TOKEN = re.compile(r"\x1b\[([0-9;]*)m|\x1b\][^\x07\x1b]*(?:\x07|\x1b\\)|\x1b\[[0-9;?]*[A-Za-z]|([^\x1b]+)")


def to_html(text):
    pen, out = Pen(), []
    for m in TOKEN.finditer(text):
        if m.group(1) is not None:
            pen.sgr(m.group(1))
        elif m.group(2) is not None:
            chunk, css = html.escape(m.group(2)), pen.css()
            out.append('<span style="%s">%s</span>' % (css, chunk) if css else chunk)
    return "".join(out)


def visible(line):
    return re.sub(r"\x1b\[[0-9;?]*[A-Za-z]|\x1b\][^\x07\x1b]*(?:\x07|\x1b\\)", "", line)


def page(body, fonts):
    faces = "".join(
        "@font-face{font-family:'JetBrains Mono';src:url('%s') format('woff2');font-weight:%s;font-style:%s}"
        % ((fonts / ("JetBrainsMono-%s.woff2" % name)).resolve().as_uri(), weight, style)
        for name, weight, style in (("Regular", 400, "normal"), ("Bold", 700, "normal"), ("Italic", 400, "italic")))
    return ("<!doctype html><meta charset='utf-8'><style>%s"
            "html,body{margin:0;background:%s}"
            "pre{margin:0;padding:%dpx;color:%s;font:%dpx/%s 'JetBrains Mono',monospace;"
            "white-space:pre;font-variant-ligatures:none}</style><pre>%s</pre>"
            % (faces, BG, PAD, FG, FONT_PX, LINE, body))


def main():
    src, fonts, out = (pathlib.Path(a) for a in sys.argv[1:4])
    chrome = os.environ.get("CHROME", "google-chrome")
    out.mkdir(parents=True, exist_ok=True)
    for ans in sorted(src.glob("*.ans")):
        text = ans.read_text(encoding="utf-8").rstrip("\n")
        lines = text.split("\n")
        cols = max(len(visible(l)) for l in lines)
        body = to_html(text)
        (out / (ans.stem + ".html")).write_text(body, encoding="utf-8")
        shot = src / (ans.stem + ".page.html")
        shot.write_text(page(body, fonts), encoding="utf-8")
        w = math.ceil(cols * CELL * FONT_PX) + 2 * PAD
        h = math.ceil(len(lines) * LINE * FONT_PX) + 2 * PAD
        png = out / (ans.stem + ".png")
        subprocess.run([chrome, "--headless=new", "--disable-gpu", "--hide-scrollbars",
                        "--force-device-scale-factor=2", "--virtual-time-budget=3000",
                        "--window-size=%d,%d" % (w, h), "--screenshot=%s" % png.resolve(),
                        shot.resolve().as_uri()], check=True, capture_output=True)
        print("%s %dx%d" % (png, w * 2, h * 2))


if __name__ == "__main__":
    main()
