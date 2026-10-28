#!/usr/bin/env python3
"""Traces docs/loader-black.gif into loader.json for the outro.

    python3 trace_loader.py docs/loader-black.gif docs/promo/loader.json

For each of the GIF's 50 frames (48x48, 25 fps): where its Y and its bar
are, as a centre and a size, so the page can redraw them as vectors at
any size and move them smoothly between frames.
"""
import json
import subprocess
import sys

W = H = 48


def main():
    gif, out = sys.argv[1], sys.argv[2]
    data = subprocess.run(["ffmpeg", "-v", "error", "-i", gif, "-f", "rawvideo", "-pix_fmt", "rgba", "-"],
                          check=True, capture_output=True).stdout
    n = len(data) // (W * H * 4)

    def lum(f, x, y):
        i = ((f * H + y) * W + x) * 4
        r, _, _, a = data[i:i + 4]
        return r / 255 if a > 128 else 0

    frames = []
    for f in range(n):
        on = {(x, y): lum(f, x, y) for y in range(H) for x in range(W) if lum(f, x, y) > 0.35}
        seen, parts = set(), []
        for p in on:
            if p in seen:
                continue
            stack, part = [p], []
            seen.add(p)
            while stack:
                x, y = stack.pop()
                part.append((x, y))
                for dx in (-1, 0, 1):
                    for dy in (-1, 0, 1):
                        q = (x + dx, y + dy)
                        if q in on and q not in seen:
                            seen.add(q)
                            stack.append(q)
            parts.append(part)
        parts = sorted(parts, key=len, reverse=True)[:2]

        def stat(part):
            xs, ys = [x for x, _ in part], [y for _, y in part]
            weight = sum(on[p] for p in part)
            return {"cx": round(sum(x * on[(x, y)] for x, y in part) / weight + 0.5, 3),
                    "cy": round(sum(y * on[(x, y)] for x, y in part) / weight + 0.5, 3),
                    "w": max(xs) - min(xs) + 1, "h": max(ys) - min(ys) + 1}

        a, b = stat(parts[0]), stat(parts[1])
        # The bar is the wider and flatter of the two.
        bar, y = (a, b) if a["w"] / a["h"] > b["w"] / b["h"] else (b, a)
        frames.append({"y": y, "bar": bar})
    with open(out, "w") as f:
        json.dump(frames, f)
    print(out, len(frames), "frames")


if __name__ == "__main__":
    main()
