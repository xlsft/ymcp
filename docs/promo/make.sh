#!/bin/sh
# Rebuilds docs/promo/ymcp-promo.mp4 from the real TUI. Run from the
# repository root; needs Go, Python 3, Node, ffmpeg and Chrome ($CHROME):
#
#   docs/promo/make.sh FONT_DIR
#
# FONT_DIR holds JetBrainsMonoNerdFontMono-{Regular,Bold,Italic}.ttf, the
# JetBrains Mono Nerd Font the Site workflow downloads (v3.5.1).
set -eu
fonts=${1:?usage: docs/promo/make.sh FONT_DIR}
here=docs/promo
frames=$here/build/frames
mkdir -p "$frames"

# The frames: the board, ymcp setup and the pipeline wizard, drawn by the
# TUI's own code on made-up data, and the agent session.
YMCP_PROMO=$PWD/$frames go test -count=1 -run '^TestPromo$' ./internal/pipeline/tui/views ./internal/pipeline/tui/wizard ./cmd/ymcp
python3 $here/gen_agent.py "$frames/agent"
python3 $here/build.py "$frames" "$fonts"

# The track: nE cut at 12 bars (24.65 s at 118 BPM), fading out over the last.
ffmpeg -y -loglevel error -i $here/music/nE.mp3 -af "atrim=0:24.65,afade=t=out:st=22.61:d=2.04" \
  -c:a libmp3lame -b:a 320k $here/build/track.mp3

# Four quarters at once, each frame the mean of 8 subframes, then joined.
(cd $here && npm ci --silent)
parts=$here/build/parts
mkdir -p "$parts"
node $here/render.js video 0 6.1667 "$parts/p1.mp4" 8 &
node $here/render.js video 6.1667 12.3333 "$parts/p2.mp4" 8 &
node $here/render.js video 12.3333 18.5 "$parts/p3.mp4" 8 &
node $here/render.js video 18.5 24.65 "$parts/p4.mp4" 8 &
wait
printf "file 'p1.mp4'\nfile 'p2.mp4'\nfile 'p3.mp4'\nfile 'p4.mp4'\n" > "$parts/list.txt"
ffmpeg -y -loglevel error -f concat -safe 0 -i "$parts/list.txt" -i $here/build/track.mp3 \
  -map 0:v -map 1:a -c:v copy -c:a aac -b:a 320k -shortest -movflags +faststart $here/ymcp-promo.mp4
echo "$here/ymcp-promo.mp4"
