#!/usr/bin/env python3
"""Generates a music clip with Google Lyria 3 through OpenRouter.

    OPENROUTER_API_KEY=… python3 gen.py PROMPT_FILE OUT_BASENAME

Writes OUT_BASENAME.<ext> (whatever the stream says the audio is) and
OUT_BASENAME.json with the request and any text the model sent back.
nE.mp3 is prompt.txt through google/lyria-3-clip-preview.
"""
import base64
import json
import os
import sys
import urllib.request

MODEL = os.environ.get("LYRIA_MODEL", "google/lyria-3-clip-preview")
FORMAT = os.environ.get("LYRIA_FORMAT", "wav")


def main():
    prompt, out = open(sys.argv[1]).read(), sys.argv[2]
    body = {
        "model": MODEL,
        "modalities": ["text", "audio"],
        "audio": {"format": FORMAT},
        "stream": True,
        "messages": [{"role": "user", "content": prompt}],
    }
    req = urllib.request.Request(
        "https://openrouter.ai/api/v1/chat/completions",
        data=json.dumps(body).encode(),
        headers={"Authorization": "Bearer " + os.environ["OPENROUTER_API_KEY"], "Content-Type": "application/json"},
    )
    chunks, text = [], []
    try:
        resp = urllib.request.urlopen(req, timeout=600)
    except urllib.error.HTTPError as e:
        raise SystemExit("HTTP %d: %s" % (e.code, e.read().decode()[:800]))
    for raw in resp:
        line = raw.decode().strip()
        if not line.startswith("data:"):
            continue
        data = line[5:].strip()
        if data == "[DONE]":
            break
        msg = json.loads(data)
        if "error" in msg:
            raise SystemExit("error: %s" % json.dumps(msg["error"])[:800])
        for ch in msg.get("choices", []):
            d = ch.get("delta", {})
            a = d.get("audio") or {}
            if a.get("data"):
                chunks.append(a["data"])
            if a.get("transcript"):
                text.append(a["transcript"])
            if d.get("content"):
                text.append(d["content"])
    audio = b"".join(base64.b64decode(c) for c in chunks)
    kinds = {b"RIFF": "wav", b"ID3": "mp3", b"fLaC": "flac", b"OggS": "ogg"}
    kind = next((v for k, v in kinds.items() if audio.startswith(k)), "mp3" if audio[:2] == b"\xff\xfb" else "bin")
    with open("%s.%s" % (out, kind), "wb") as f:
        f.write(audio)
    with open(out + ".json", "w") as f:
        json.dump({"model": MODEL, "prompt": prompt, "text": "".join(text)}, f, indent=2)
    print("%s.%s: %d bytes; text: %r" % (out, kind, len(audio), "".join(text)[:200]))


if __name__ == "__main__":
    main()
