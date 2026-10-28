#!/usr/bin/env python3
"""Puts the promo page together in docs/promo/build.

    python3 docs/promo/build.py FRAMES_DIR FONT_DIR

FRAMES_DIR holds what the TestPromo harnesses and gen_agent.py wrote
(board/, setup/, wizard/, agent/). The page gets them as HTML through
.github/scripts/screens.py, the banner's Y, the traced loader, and the
JetBrains Mono Nerd Font from FONT_DIR, as the site's screenshots do.
"""
import importlib.util
import json
import pathlib
import re
import shutil
import sys

HERE = pathlib.Path(__file__).resolve().parent
ROOT = HERE.parent.parent


def main():
    frames_dir, fonts = pathlib.Path(sys.argv[1]), pathlib.Path(sys.argv[2])
    build = HERE / "build"
    build.mkdir(exist_ok=True)

    spec = importlib.util.spec_from_file_location("screens", ROOT / ".github/scripts/screens.py")
    screens = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(screens)
    data = {}
    for shot in ("board", "setup", "wizard", "agent"):
        frames = []
        for line in (frames_dir / shot / (shot + ".txt")).read_text().split("\n"):
            if line.strip():
                sec, name = line.split()
                frames.append([float(sec), screens.to_html((frames_dir / shot / name).read_text(encoding="utf-8").rstrip("\n"))])
        data[shot] = frames
    (build / "frames.js").write_text("window.FRAMES = " + json.dumps(data) + ";\nwindow.TERM = "
                                     + json.dumps({"fg": screens.FG, "bg": screens.BG}) + ";\n")

    banner = (ROOT / "docs/banner.svg").read_text()
    paths = {"y": re.search(r'<path d="(M504\.75[^"]*)"', banner).group(1)}
    page = (HERE / "promo.html").read_text()
    page = page.replace("__LOADER__", (HERE / "loader.json").read_text()).replace("__PATHS__", json.dumps(paths))
    (build / "index.html").write_text(page)
    for name in ("Regular", "Bold", "Italic"):
        shutil.copy(fonts / ("JetBrainsMonoNerdFontMono-%s.ttf" % name), build)
    print(build / "index.html", ", ".join("%s %d" % (k, len(v)) for k, v in data.items()))


if __name__ == "__main__":
    main()
