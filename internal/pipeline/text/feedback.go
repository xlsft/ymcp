package text

import (
	"regexp"
	"strconv"
	"strings"
)

// FeedbackItem is one piece of review feedback shown to the agent.
type FeedbackItem struct {
	N        int
	Source   string // youtrack, github, gitlab
	Author   string
	URL      string
	File     string
	Line     int
	Hunk     string
	Body     string
	Outdated bool
}

// RenderFeedback formats the items as a numbered list the agent can answer
// point by point:
//
//	[1] github · alice · https://…
//	  path/file.go:42
//	  @@ diff hunk
//	  comment text
//
// Parts that are empty are left out; the items are separated by a blank line.
func RenderFeedback(items []FeedbackItem) string {
	blocks := make([]string, 0, len(items))
	for _, it := range items {
		var lines []string
		head := "[" + strconv.Itoa(it.N) + "]"
		var meta []string
		for _, s := range []string{it.Source, it.Author, it.URL} {
			if s != "" {
				meta = append(meta, s)
			}
		}
		if len(meta) > 0 {
			head += " " + strings.Join(meta, " · ")
		}
		if it.Outdated {
			head += " (outdated)"
		}
		lines = append(lines, head)
		if it.File != "" {
			loc := it.File
			if it.Line > 0 {
				loc += ":" + strconv.Itoa(it.Line)
			}
			lines = append(lines, "  "+loc)
		}
		lines = appendIndented(lines, it.Hunk)
		lines = appendIndented(lines, it.Body)
		blocks = append(blocks, strings.Join(lines, "\n"))
	}
	return strings.Join(blocks, "\n\n")
}

// appendIndented adds the lines of s, indented by two spaces; blank lines get
// no trailing spaces.
func appendIndented(lines []string, s string) []string {
	s = strings.TrimRight(strings.ReplaceAll(s, "\r\n", "\n"), "\n")
	if strings.TrimSpace(s) == "" {
		return lines
	}
	for _, l := range strings.Split(s, "\n") {
		if strings.TrimSpace(l) == "" {
			lines = append(lines, "")
			continue
		}
		lines = append(lines, "  "+l)
	}
	return lines
}

// answerRe matches a line that opens the answer to feedback item n: "[2] …",
// optionally after a bullet ("- [2]") or in bold ("**[2]**").
var answerRe = regexp.MustCompile(`^[ \t]*(?:[-*+•][ \t]+)?(?:\*\*)?\[(\d+)\](?:\*\*)?(.*)$`)

// SplitAnswer splits an agent's answer to numbered feedback into the replies
// to each item. A reply starts on a line beginning with "[n]" for an n in ns
// and runs to the next such line; the text after the "[n]" on that line, minus
// a ":" or dash separator, is its first line. Text before the first reply,
// and text under numbers not in ns, goes to rest. A line of only "---" ends
// the replies: what follows (the agent's closing summary) goes to rest too.
// Replies and rest are trimmed; empty replies are dropped.
func SplitAnswer(answer string, ns []int) (parts map[int]string, rest string) {
	want := make(map[int]bool, len(ns))
	for _, n := range ns {
		want[n] = true
	}
	parts = map[int]string{}
	chunks := map[int][]string{}
	var restLines []string
	cur := 0
	inPart := false
	for _, line := range strings.Split(strings.ReplaceAll(answer, "\r\n", "\n"), "\n") {
		if inPart && strings.TrimSpace(line) == "---" {
			inPart = false
			continue
		}
		if m := answerRe.FindStringSubmatch(line); m != nil {
			if n, err := strconv.Atoi(m[1]); err == nil && want[n] {
				cur, inPart = n, true
				first := cleanSeparator(m[2])
				if first != "" {
					chunks[cur] = append(chunks[cur], first)
				}
				continue
			}
		}
		if inPart {
			chunks[cur] = append(chunks[cur], line)
		} else {
			restLines = append(restLines, line)
		}
	}
	for n, ls := range chunks {
		if s := strings.TrimSpace(strings.Join(ls, "\n")); s != "" {
			parts[n] = s
		}
	}
	return parts, strings.TrimSpace(strings.Join(restLines, "\n"))
}

// cleanSeparator drops the ":" or dash between "[n]" and the reply text.
func cleanSeparator(s string) string {
	s = strings.TrimSpace(s)
	switch {
	case strings.HasPrefix(s, ":**"): // "**[2]:** text"
		s = s[3:]
	case strings.HasPrefix(s, ":"):
		s = s[1:]
	case strings.HasPrefix(s, "- "), strings.HasPrefix(s, "– "), strings.HasPrefix(s, "— "):
		s = s[strings.Index(s, " ")+1:]
	}
	return strings.TrimSpace(s)
}
