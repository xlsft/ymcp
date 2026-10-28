#!/usr/bin/env python3
"""Tempo and beat grid of a clip, from its kick drum.

    python3 beats.py FILE...

Decodes through ffmpeg, low-passed to the kick, takes the energy rise every
5 ms, finds the tempo by autocorrelation (100–150 BPM, refined to 0.1 BPM)
and the beat phase that lands on the most energy. Also says where the drums
come in and where the loudest change (the drop) is.
"""
import array
import math
import subprocess
import sys

SR, HOP = 11025, 55  # 5 ms hops


def envelope(path, lowpass):
    pcm = subprocess.run(["ffmpeg", "-v", "error", "-i", path, "-ac", "1", "-ar", str(SR), "-af", "lowpass=f=%d" % lowpass,
                          "-f", "f32le", "-"], check=True, capture_output=True).stdout
    x = array.array("f", pcm)
    rms = [math.sqrt(sum(v * v for v in x[i:i + HOP]) / HOP) for i in range(0, len(x) - HOP, HOP)]
    rise = [max(0.0, rms[i] - rms[i - 1]) for i in range(1, len(rms))]
    return rms, [0.0] + rise


def score(env, period, phase):
    s, n, t = 0.0, 0, phase
    while t < len(env) - 1:
        i = int(t)
        s += max(env[i], env[i + 1]); n += 1
        t += period
    return s / max(n, 1)


def main():
    fps = SR / HOP
    for path in sys.argv[1:]:
        rms, env = envelope(path, 150)
        dur = len(rms) / fps
        best = max(((score(env, 60 * fps / b, ph), b) for b in [x / 2 for x in range(200, 301)]
                    for ph in [p * 0.5 for p in range(int(2 * 60 * fps / b))]), key=lambda v: v[0])
        bpm = best[1]
        fine = [bpm + d / 10 for d in range(-6, 7)]
        cand = []
        for b in fine:
            per = 60 * fps / b
            for k in range(int(per * 4)):
                ph = k / 4
                cand.append((score(env, per, ph), b, ph))
        s, bpm, ph = max(cand)
        beat = 60 / bpm
        first = ph / fps
        # Drums in: first beat whose kick energy is a third of the median beat's.
        per = 60 * fps / bpm
        beats = [first + i * beat for i in range(int((dur - first) / beat) + 1)]
        energy = [max(rms[int(b * fps):int(b * fps) + 8] or [0]) for b in beats]
        med = sorted(energy)[len(energy) // 2]
        drums_in = next((b for b, e in zip(beats, energy) if e > med / 3), None)
        # Drop: the biggest jump in full-band loudness over one bar.
        full, _ = envelope(path, 8000)
        bar = int(4 * beat * fps)
        loud = [sum(full[i:i + bar]) / bar for i in range(0, len(full) - bar, int(beat * fps))]
        jumps = [(loud[i + 1] - loud[i], i + 1) for i in range(len(loud) - 1)]
        drop = max(jumps)[1] * beat if jumps else None
        print("%s: %.1f BPM (beat %.3f s, bar %.3f s), grid starts %.3f s, drums by %.2f s, drop near %.2f s, length %.2f s"
              % (path, bpm, beat, 4 * beat, first % beat, drums_in or -1, drop or -1, dur))


if __name__ == "__main__":
    main()
