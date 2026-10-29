#!/usr/bin/env python3
"""The human-readable changelog: what each release changed, for its users.

    python3 .github/scripts/changelog.py notes TAG   the release's What's new
    python3 .github/scripts/changelog.py markdown    CHANGELOG.md, every release

A feat, fix or perf commit says what it changes for users in a Changelog:
trailer, one sentence (continued on lines that start with a space):

    Changelog: Answers given in the TUI reach the run at once.

A squashed pull request carries the trailers of its commits, each counted
under the type of the commit it came with. An entry goes under Added (feat),
Fixed (fix), Changed (perf) or Breaking changes (type!: or BREAKING CHANGE);
a trailer that starts with "Changed: ", "Removed: ", "Deprecated: " or
"Security: " goes there instead. A feat, fix or perf commit without a trailer
counts its subject. Releases up to v0.21.0, made before the trailers, are
written out in docs/changelog-history.md.

The Release workflow puts a release's notes above GoReleaser's list of
commits; the Site workflow publishes CHANGELOG.md and the changelog page.
"""
import pathlib
import re
import subprocess
import sys

REPO = "https://github.com/xlsft/ymcp"
HISTORY = pathlib.Path("docs/changelog-history.md")
SECTIONS = ["Breaking changes", "Added", "Changed", "Deprecated", "Removed", "Fixed", "Security"]
BY_TYPE = {"feat": "Added", "fix": "Fixed", "perf": "Changed"}
HEAD = re.compile(r"^(?:\* )?([a-z]+)(?:\([^)]*\))?(!)?: (.+)$")
TRAILER = re.compile(r"^Changelog: *(.*)$")
PREFIX = re.compile(r"^(%s): +(.+)$" % "|".join(s for s in SECTIONS if s != "Breaking changes"))


def git(*args):
    return subprocess.run(["git", *args], check=True, capture_output=True, text=True).stdout


def tags():
    """[(tag, date)] of the v* tags, oldest first."""
    out = git("for-each-ref", "--sort=v:refname", "--format=%(refname:short)\t%(creatordate:short)", "refs/tags/v*")
    return [tuple(line.split("\t")) for line in out.splitlines() if re.match(r"^v\d+\.\d+\.\d+\t", line)]


def history():
    """{tag: {section: [entry]}} from docs/changelog-history.md."""
    notes, tag, section = {}, None, None
    if not HISTORY.exists():
        return notes
    for line in HISTORY.read_text(encoding="utf-8").splitlines():
        if line.startswith("## "):
            tag, section = line[3:].strip(), None
            notes[tag] = {}
        elif line.startswith("### ") and tag:
            section = line[4:].strip()
            notes[tag][section] = []
        elif line.startswith("- ") and section:
            notes[tag][section].append(line[2:].strip())
        elif line.strip() and section and notes[tag][section]:
            notes[tag][section][-1] += " " + line.strip()
    return notes


def entries(message):
    """[(section, entry)] a commit message adds."""
    found, kind, breaking, said, fallback = [], None, False, False, None
    lines = message.splitlines()
    for i, line in enumerate(lines):
        head = HEAD.match(line)
        if head and (i == 0 or line.startswith("* ")):
            kind, breaking = head.group(1), bool(head.group(2))
            if i == 0 and (kind in BY_TYPE or breaking):
                fallback = (kind, breaking, re.sub(r" \(#\d+\)$", "", head.group(3)))
            continue
        if line.startswith("BREAKING CHANGE"):
            breaking = True
            continue
        trailer = TRAILER.match(line)
        if not trailer:
            continue
        said = True
        text = trailer.group(1).strip()
        for more in lines[i + 1:]:
            if not more.startswith((" ", "\t")) or not more.strip():
                break
            text += " " + more.strip()
        if text:
            found.append((section(kind, breaking, text), strip(text)))
    if not said and fallback:
        kind, breaking, text = fallback
        found.append((section(kind, breaking, text), strip(text if text.endswith(".") else text + ".")))
    return found


def section(kind, breaking, text):
    if breaking:
        return "Breaking changes"
    prefix = PREFIX.match(text)
    if prefix:
        return prefix.group(1)
    return BY_TYPE.get(kind, "Changed")


def strip(text):
    prefix = PREFIX.match(text)
    text = prefix.group(2) if prefix else text
    return text[:1].upper() + text[1:]


def from_commits(rev_range):
    notes = {}
    out = git("log", "--reverse", "--format=%B%x1e", rev_range)
    for message in out.split("\x1e"):
        for sec, entry in entries(message.strip()):
            notes.setdefault(sec, []).append(entry)
    return notes


def releases():
    """[(tag, date, {section: [entry]})], newest first."""
    known, written = tags(), history()
    out, prev = [], None
    for tag, date in known:
        notes = written.get(tag)
        if notes is None:
            notes = from_commits("%s..%s" % (prev, tag) if prev else tag)
        out.append((tag, date, notes))
        prev = tag
    return out[::-1]


def sections(notes, level):
    text = ""
    for sec in SECTIONS:
        if notes.get(sec):
            text += "%s %s\n\n" % ("#" * level, sec) + "".join("- %s\n" % e for e in notes[sec]) + "\n"
    return text


def notes_for(tag):
    """The What's new of a release that is tagged but may not be published."""
    for t, _, notes in releases():
        if t == tag:
            return "## What's new\n\n" + (sections(notes, 3) or "Nothing users would notice.\n\n")
    raise SystemExit("no tag %s" % tag)


def markdown():
    text = ("# Changelog\n\nWhat each release of ymcp changed, newest first. The versions follow "
            "[Semantic Versioning](https://semver.org); every release, with its binaries and the list of its "
            "commits, is on [GitHub](%s/releases).\n\n" % REPO)
    links = ""
    for tag, date, notes in releases():
        text += "## [%s] - %s\n\n" % (tag[1:], date) + sections(notes, 3)
        links += "[%s]: %s/releases/tag/%s\n" % (tag[1:], REPO, tag)
    return text + links


def main():
    if sys.argv[1:2] == ["notes"] and len(sys.argv) == 3:
        sys.stdout.write(notes_for(sys.argv[2]))
    elif sys.argv[1:] == ["markdown"]:
        sys.stdout.write(markdown())
    else:
        raise SystemExit(__doc__)


if __name__ == "__main__":
    main()
