#!/usr/bin/env python3
"""Builds the landing page and what goes with it from site/.

    python3 .github/scripts/site.py site/index.html OUT_DIR

OUT_DIR holds what the Site workflow made: site.json and screens/NAME.html.
The page gets them built in, as the copy it shows until it has fetched the
fresh ones from the site branch, the figures its meta tags and structured
data quote, and the nav's loader (docs/loader-black.gif, with its first
frame as the still); it is written to OUT_DIR/index.html. Next to it go
robots.txt, sitemap.xml, the icons, the promo video from docs/promo and its
poster, CNAME and .nojekyll for GitHub Pages, and, drawn by headless Chrome
($CHROME, google-chrome by default), the PNG icons and og.png from
site/og.html.
"""
import base64
import json
import os
import pathlib
import re
import shutil
import subprocess
import sys

LOADER = pathlib.Path("docs/loader-black.gif")
# The promo video the hero plays, and its poster, published as promo.*.
PROMO = {"promo.webm": "docs/promo/ymcp-promo.webm", "promo.mp4": "docs/promo/ymcp-promo.mp4", "promo.jpg": "docs/promo/poster.jpg"}
SITE = "https://ymcp.su/"


def first_frame(gif):
    """The first frame of a GIF, as a GIF of its own."""
    flags = gif[10]
    i = 13 + (3 * (2 << (flags & 7)) if flags & 0x80 else 0)
    out, control = bytearray(gif[:i]), b""
    while i < len(gif):
        block = gif[i]
        if block == 0x21:  # an extension; keep the graphic control one
            j = i + 2
            while gif[j]:
                j += gif[j] + 1
            j += 1
            if gif[i + 1] == 0xF9:
                control = gif[i:j]
            i = j
        elif block == 0x2C:  # the image: its descriptor, colour table and data
            f = gif[i + 9]
            j = i + 10 + (3 * (2 << (f & 7)) if f & 0x80 else 0) + 1
            while gif[j]:
                j += gif[j] + 1
            j += 1
            return bytes(out + control + gif[i:j] + b"\x3b")
        else:
            break
    raise SystemExit("%s: no image in the GIF" % LOADER)


def features(data):
    """What the structured data lists the app can do, from the site's data."""
    t = data["tools"]
    return [
        "%d MCP tools: the %d of YouTrack's own MCP server, same names, and %d extras" % (t["total"], t["builtIn"], t["extras"]),
        "Signs in with a YouTrack login and password: nothing to set up in YouTrack, no webhooks, OAuth client or API keys",
        "Works without admin rights",
        "Installs into %d MCP clients: %s" % (len(data["clients"]), ", ".join(c["name"] for c in data["clients"])),
        "Prompts: " + ", ".join(p["name"] for p in data["prompts"]),
        "Pipelines hand YouTrack issues to coding agents: " + ", ".join(a["about"] for a in data["agents"]),
        "Publishes work as " + ", ".join(s["name"] for s in data["strategies"]),
    ]


def fill_in(path, fills):
    def fill(m):
        if m.group(1) not in fills:
            raise SystemExit("%s: nothing to fill %s with" % (path, m.group(0)))
        return fills[m.group(1)]

    return re.sub(r"__([A-Z]+(?:_[A-Za-z]+)*)__", fill, path.read_text(encoding="utf-8"))


def draw(page, png, w, h):
    """Draws an HTML file at w×h CSS pixels into png."""
    chrome = os.environ.get("CHROME", "google-chrome")
    subprocess.run([chrome, "--headless=new", "--disable-gpu", "--hide-scrollbars",
                    "--force-device-scale-factor=1", "--virtual-time-budget=5000",
                    "--window-size=%d,%d" % (w, h), "--screenshot=%s" % png.resolve(),
                    page.resolve().as_uri()], check=True, capture_output=True)
    print("%s: %dx%d" % (png, w, h))


def main():
    template, out = pathlib.Path(sys.argv[1]), pathlib.Path(sys.argv[2])
    src = template.parent
    data = json.loads((out / "site.json").read_text(encoding="utf-8"))
    gif = LOADER.read_bytes()
    fills = {
        "LOADER": base64.b64encode(gif).decode(),
        "LOADER_STILL": base64.b64encode(first_frame(gif)).decode(),
        # Inside <script>, "</" would end the element early.
        "SITE_JSON": (out / "site.json").read_text(encoding="utf-8").strip().replace("</", "<\\/"),
        "FEATURES": json.dumps(features(data), ensure_ascii=False).replace("</", "<\\/"),
        "TOOLS": str(data["tools"]["total"]),
        "EXTRAS": str(data["tools"]["extras"]),
        "CLIENTS": str(len(data["clients"])),
    }
    for shot in (out / "screens").glob("*.html"):
        fills["SHOT_" + shot.stem] = shot.read_text(encoding="utf-8")

    page = fill_in(template, fills)
    (out / "index.html").write_text(page, encoding="utf-8")
    print("%s: %d bytes" % (out / "index.html", len(page)))

    (out / "robots.txt").write_text("User-agent: *\nAllow: /\n\nSitemap: %ssitemap.xml\n" % SITE)
    # GitHub Pages serves the site branch at ymcp.su, as it is (no Jekyll).
    (out / "CNAME").write_text(SITE.split("/")[2] + "\n")
    (out / ".nojekyll").write_text("")
    (out / "sitemap.xml").write_text(
        '<?xml version="1.0" encoding="UTF-8"?>\n'
        '<urlset xmlns="http://www.sitemaps.org/schemas/sitemap/0.9">\n'
        "  <url><loc>%s</loc></url>\n</urlset>\n" % SITE)
    for name, path in PROMO.items():
        shutil.copy(path, out / name)
    icon = (src / "favicon.svg").read_text(encoding="utf-8")
    (out / "favicon.svg").write_text(icon, encoding="utf-8")

    # The pictures: icons from the SVG, the Open Graph card from og.html.
    work = out / ".draw"
    work.mkdir(exist_ok=True)
    for name, size in (("favicon-48.png", 48), ("apple-touch-icon.png", 180)):
        holder = work / (name + ".html")
        holder.write_text("<!doctype html><style>html,body{margin:0}svg{display:block;width:%dpx;height:%dpx}</style>%s"
                          % (size, size, icon), encoding="utf-8")
        draw(holder, out / name, size, size)
    og = work / "og.html"
    og.write_text(fill_in(src / "og.html", fills), encoding="utf-8")
    draw(og, out / "og.png", 1200, 630)
    for f in work.iterdir():
        f.unlink()
    work.rmdir()


if __name__ == "__main__":
    main()
