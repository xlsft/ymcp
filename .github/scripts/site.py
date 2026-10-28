#!/usr/bin/env python3
"""Builds the landing page from site/index.html.

    python3 .github/scripts/site.py site/index.html OUT_DIR

OUT_DIR holds what the Site workflow made: site.json and screens/NAME.html.
The page gets them built in, as the copy it shows until it has fetched the
fresh ones from the site branch, and the nav's loader (docs/loader-black.gif,
with its first frame as the still), and is written to OUT_DIR/index.html.
"""
import base64
import pathlib
import re
import sys

LOADER = pathlib.Path("docs/loader-black.gif")


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


def main():
    template, out = pathlib.Path(sys.argv[1]), pathlib.Path(sys.argv[2])
    page = template.read_text(encoding="utf-8")
    gif = LOADER.read_bytes()
    fills = {
        "LOADER": base64.b64encode(gif).decode(),
        "LOADER_STILL": base64.b64encode(first_frame(gif)).decode(),
        # Inside <script>, "</" would end the element early.
        "SITE_JSON": (out / "site.json").read_text(encoding="utf-8").strip().replace("</", "<\\/"),
    }
    for shot in (out / "screens").glob("*.html"):
        fills["SHOT_" + shot.stem] = shot.read_text(encoding="utf-8")

    def fill(m):
        if m.group(1) not in fills:
            raise SystemExit("%s: nothing to fill %s with" % (template, m.group(0)))
        return fills[m.group(1)]

    page = re.sub(r"__([A-Z]+(?:_[A-Za-z]+)*)__", fill, page)
    (out / "index.html").write_text(page, encoding="utf-8")
    print("%s: %d bytes" % (out / "index.html", len(page)))


if __name__ == "__main__":
    main()
