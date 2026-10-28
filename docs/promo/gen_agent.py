#!/usr/bin/env python3
"""Draws the agent scene of the promo: a Claude Code session, 120x34, in the
look of a real one recorded with ymcp (rules, ❯, ⏺, ⎿, the spinner line and
the mode line, in its colours), working on YPC-34 through the YouTrack tools.

    python3 gen_agent.py OUT_DIR

Writes agent-NNNN.ans and agent.txt ("seconds file"), times from 0.
"""
import pathlib
import sys

W, H = 120, 34
ESC = "\x1b["


def fg(hexc, s, bold=False):
    r, g, b = int(hexc[0:2], 16), int(hexc[2:4], 16), int(hexc[4:6], 16)
    return "%s%s38;2;%d;%d;%dm%s%s0m" % (ESC, "1;" if bold else "", r, g, b, s, ESC)


WHITE, GREY, DIM, ORANGE, PURPLE, GREEN, RULE = "ffffff", "999999", "6b6b6b", "d77757", "af87ff", "4eba65", "505050"
SPIN = "✻✶✳✢·✢✳✶"

PROMPT = "Take YPC-34 from YouTrack and fix it. Comment on the issue when you're done."

# The session: (seconds, kind, …). Tool calls show as in Claude Code:
# ⏺ name (MCP)(args), then ⎿ the result.
STEPS = [
    (0.95, "say", "I'll read the issue and what was said about it first."),
    (1.20, "mcp", "get_issue", 'issueId: "YPC-34"', "YPC-34 Rate limit the login endpoint · Open · Major"),
    (1.45, "mcp", "get_issue_comments", 'issueId: "YPC-34"', "1 comment: “Keep the counters in memory for now; Redis can come later.”"),
    (1.70, "mcp", "search_issues", 'query: "project: YPC login #Unresolved"', "2 issues"),
    (1.95, "tool", "Write", "internal/auth/ratelimit.go", "Wrote 118 lines to internal/auth/ratelimit.go"),
    (2.20, "tool", "Bash", "go test ./... -race", "ok    example.com/backend/internal/auth    0.41s"),
    (2.45, "mcp", "update_issue", 'issueId: "YPC-34", customFields: {"State": "Fixed"}', "YPC-34 · State: Fixed"),
    (2.70, "mcp", "add_issue_comment", 'issueId: "YPC-34", text: "Limited POST /login to 5 attempts a minute per IP…"', "Comment added to YPC-34"),
    (2.95, "mcp", "log_work", 'issueId: "YPC-34", durationMinutes: 25', "25m logged on YPC-34"),
    (3.30, "done", "Done. YPC-34 is fixed: 5 attempts a minute per IP, then 429 with Retry-After. I said so on the issue."),
]


def cut(s, n):
    return s if len(s) <= n else s[: n - 1] + "…"


def history(t):
    out = ["", fg(WHITE, "❯ ") + fg(GREY, PROMPT), ""]
    for step in STEPS:
        if t < step[0]:
            break
        kind = step[1]
        if kind == "say":
            out += [fg(WHITE, "⏺") + " " + step[2], ""]
        elif kind == "done":
            out += [fg(WHITE, "⏺") + " " + step[2], ""]
        else:
            name, args, result = step[2], step[3], step[4]
            finished = t >= step[0] + 0.18
            dot = fg(GREEN if finished else GREY, "⏺")
            if kind == "mcp":
                head = fg(WHITE, "youtrack - " + name, True) + fg(DIM, " (MCP)") + fg(GREY, "(" + cut(args, 80) + ")")
            else:
                head = fg(WHITE, name, True) + fg(GREY, "(" + args + ")")
            out.append(dot + " " + head)
            out.append("  " + fg(GREY, "⎿  ") + (fg(GREY, cut(result, 100)) if finished else fg(DIM, "Running…")))
            out.append("")
    return out


def screen(t, typed):
    lines = history(t) if typed is None else [""]
    busy = typed is None and t < STEPS[-1][0]
    status = ""
    if busy:
        secs = int((t - 0.85) * 9) + 1  # the session runs faster than the clock here
        tokens = 0.2 + (t - 0.85) * 3.1
        status = fg(ORANGE, SPIN[int(t * 12) % len(SPIN)] + " Working…") + fg(DIM, " (%ds · ↓ %.1fk tokens · esc to interrupt)" % (secs, tokens))
    box = typed if typed is not None else ""
    rule = fg(RULE, "─" * W)
    mode = "  " + fg(PURPLE, "⏵⏵ accept edits on (shift+tab to cycle)") + (fg(DIM, " · esc to interrupt") if busy else "")
    bottom = [status, "", rule, fg(WHITE, "❯ ") + box + (fg(WHITE, "█") if typed is not None else ""), rule, mode]
    room = H - len(bottom)
    if len(lines) > room:
        lines = lines[len(lines) - room:]
    lines += [""] * (room - len(lines))
    return "\n".join(lines + bottom)


def main():
    out = pathlib.Path(sys.argv[1])
    out.mkdir(parents=True, exist_ok=True)
    frames = []
    # The prompt, typed fast, then sent at 0.85 s.
    for i in range(len(PROMPT) + 1):
        frames.append((0.05 + i * 0.0095, screen(0, PROMPT[:i])))
    t = 0.85
    while t <= 4.05:
        frames.append((t, screen(t, None)))
        t += 1 / 24
    lst = []
    for i, (sec, text) in enumerate(frames):
        name = "agent-%04d.ans" % i
        (out / name).write_text(text, encoding="utf-8")
        lst.append("%.4f %s" % (sec, name))
    (out / "agent.txt").write_text("\n".join(lst) + "\n")
    print(len(frames), "frames")


if __name__ == "__main__":
    main()
