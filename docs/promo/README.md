# Promo video

[`ymcp-promo.mp4`](ymcp-promo.mp4): 24.65 s, 1920×1080 at 60 fps, 12 bars of a 118 BPM track.

| Bars | Scene | Caption |
|---|---|---|
| 0 | The backlog grows, new issues on top | Save your backlog from overgrowing |
| 1 | `ymcp setup`: the login and password, then the MCP clients ticked | Easy connection |
| 2 | The pipeline wizard: an ssh host, then every agent | Anywhere |
| 3–4 | An agent works on an issue through the YouTrack tools | Directly |
| 5 | The pipeline board gets going | Or via pipelines |
| 6–7 | A run asks for a permission; its card opens, `/approve` | Don't miss anything |
| 8–9 | The backlog drains | Issues in, solutions out |
| 10–11 | The outro, the site's Open Graph card with the loader | |

Everything on screen but the agent session and the outro is the real TUI: the
`TestPromo` harnesses drive the board, `ymcp setup` and the wizard on made-up
data, and their frames go through `.github/scripts/screens.py`, as the site's
screenshots do. The agent scene (`gen_agent.py`) is drawn in the look of a
Claude Code session with ymcp; the outro redraws `docs/loader-black.gif` as
vectors (`trace_loader.py` → `loader.json`).

## Rebuild

From the repository root, with Go, Python 3, Node, ffmpeg and Chrome (`$CHROME`):

```sh
docs/promo/make.sh FONT_DIR
```

`FONT_DIR` holds `JetBrainsMonoNerdFontMono-{Regular,Bold,Italic}.ttf` from the
JetBrains Mono Nerd Font v3.5.1, the one the Site workflow downloads. The script
writes the frames, the page and the parts to `docs/promo/build/` and the video
to `docs/promo/ymcp-promo.mp4`, in about five minutes.

| File | |
|---|---|
| `promo.html` | The video as a page: the shots, the camera, the captions, the outro |
| `render.js` | Draws it frame by frame; the camera gets motion blur, the terminal does not |
| `build.py` | Fills the page in from the frames, the banner and the loader |
| `gen_agent.py` | The agent session's frames |
| `trace_loader.py` | Where the loader's Y and bar are in each GIF frame |
| `music/` | `nE.mp3`, made by Google Lyria 3 (`google/lyria-3-clip-preview` on OpenRouter) from `prompt.txt` with `gen.py`; `beats.py` measures its tempo and beat grid |

The camera keys and caption times are in bars of the track, in `promo.html`;
the harnesses time their scenes in eighths of it.
