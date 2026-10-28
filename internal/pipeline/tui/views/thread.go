package views

import (
	"cmp"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/xlsft/ymcp/internal/pipeline/text"
	"github.com/xlsft/ymcp/internal/pipeline/tui/termimg"
	"github.com/xlsft/ymcp/internal/pipeline/tui/theme"
	"github.com/xlsft/ymcp/internal/pipeline/vcs"
)

// threadItem is one message of a run card's thread: a YouTrack comment, a
// pull request comment, or a comment of ours still being posted.
type threadItem struct {
	author  string // login
	name    string // full name, "" when unknown
	label   string // agent header ("@ilya's agent #12 (backend)"); "" for people
	working bool   // the agent's Working… placeholder
	body    string // without the agent header
	at      time.Time
	edited  bool
	where   string // file:line of a line comment, "review" for a review
	pending bool   // posted by us, not confirmed yet
	files   []Attachment
}

// agentHeader recognises an agent's message by its marker and returns the
// header to show and the body without it.
func agentHeader(raw, author string) (label, body string, ok bool) {
	mk, ok := text.ParseMarker(raw)
	if !ok {
		return "", raw, false
	}
	mention := ""
	if author != "" {
		mention = "@" + author
	}
	return text.Prefix(mention, mk), text.StripHeader(raw), true
}

// isWorking reports whether an agent's message is the working placeholder:
// the loader alone, or the loader with "Working..." as older runs wrote it.
func isWorking(body string) bool {
	b := strings.TrimSpace(body)
	if b == text.Working {
		return true
	}
	rest := strings.TrimSpace(mdImage.ReplaceAllString(b, ""))
	return (rest == "" || rest == "Working...") && strings.Contains(b, "working.gif")
}

func issueItem(c IssueComment) threadItem {
	it := threadItem{author: c.Author, name: c.AuthorName, at: c.Created, edited: !c.Updated.IsZero() && c.Updated.After(c.Created.Add(time.Second)), files: c.Files}
	it.label, it.body, _ = agentHeader(c.Text, c.Author)
	it.working = it.label != "" && isWorking(it.body)
	return it
}

func changeItem(c vcs.Comment) threadItem {
	it := threadItem{author: c.Author, at: c.Created, edited: !c.Updated.IsZero() && c.Updated.After(c.Created.Add(time.Second))}
	it.label, it.body, _ = agentHeader(c.Body, c.Author)
	it.working = it.label != "" && isWorking(it.body)
	switch {
	case c.File != "" && c.Line > 0:
		it.where = c.File + ":" + strconv.Itoa(c.Line)
	case c.File != "":
		it.where = c.File
	case c.Kind == vcs.CommentReview:
		it.where = "review"
	}
	if c.Outdated && it.where != "" {
		it.where += " (outdated)"
	}
	return it
}

// lines renders an item at width w: a header line, then the body indented,
// then the pictures attached to it. pics draws pictures; nil reduces them
// to their names.
func (it threadItem) lines(w int, now time.Time, frame int, pics *pictures) []string {
	when := agoText(now, it.at)
	var head string
	switch {
	case it.pending:
		head = theme.Bold.Render(it.author) + theme.Dim.Render(" · sending…")
	case it.label != "":
		head = theme.Dim.Render("◆ " + it.label + " · " + when)
	default:
		who := it.author
		if it.name != "" && it.name != it.author {
			who = it.name + theme.Dim.Render(" @"+it.author)
		}
		head = theme.Bold.Render(who) + theme.Dim.Render(" · "+when)
	}
	if it.where != "" {
		head += theme.Dim.Render(" · " + it.where)
	}
	if it.edited && !it.pending {
		head += theme.Dim.Render(" · edited")
	}
	out := []string{cut(head, w)}
	gutter := "  "
	if it.label != "" {
		gutter = theme.Dim.Render("│ ")
	}
	if it.working {
		if pics != nil && pics.mode == termimg.Kitty {
			for _, l := range pics.lines("", workingURL, max(w-2, 1), loaderRows) {
				out = append(out, gutter+l)
			}
			return out
		}
		return append(out, gutter+theme.Accent.Render(spinner[frame%len(spinner)])+" "+theme.Dim.Render("working…"))
	}
	var draw pictureLines
	if pics != nil {
		draw = func(alt, src string, w int) []string {
			return pics.lines(cmp.Or(alt, pictureName("", src)), resolve(src, it.files, pics.files), w, pictureRows)
		}
	}
	body := mdLinesWith(it.body, max(w-2, 1), draw)
	shown := map[string]bool{}
	for _, src := range pictureRefs(it.body, it.files) {
		shown[src] = true
	}
	for _, f := range it.files {
		if !shown[f.URL] {
			if len(body) > 0 {
				body = append(body, "")
			}
			body = append(body, pics.file(f, max(w-2, 1))...)
		}
	}
	if len(body) == 0 {
		body = []string{theme.Dim.Render("(empty)")}
	}
	if drawn(body[0]) { // a picture first: a line of air under the header
		out = append(out, "")
	}
	for _, l := range body {
		out = append(out, gutter+l)
	}
	return out
}

// pictureLines draws a picture of a text, w cells wide at most.
type pictureLines func(alt, src string, w int) []string

// ago is a relative time: "just now", "12m ago", "3h ago", or a date for
// anything older than a week.
func agoText(now, t time.Time) string {
	if t.IsZero() {
		return ""
	}
	d := now.Sub(t)
	switch {
	case d < 10*time.Second:
		return "just now"
	case d < 7*24*time.Hour:
		return short(d) + " ago"
	case t.Year() == now.Year():
		return t.Local().Format("Jan 2")
	}
	return t.Local().Format("Jan 2, 2006")
}

var (
	mdImage   = regexp.MustCompile(`!\[([^\]]*)\]\(([^)\s]*)[^)]*\)`)
	mdLink    = regexp.MustCompile(`\[([^\]]+)\]\(([^)\s]+)[^)]*\)`)
	mdBold    = regexp.MustCompile(`\*\*([^*\n]+)\*\*|__([^_\n]+)__`)
	mdComment = regexp.MustCompile(`(?s)<!--.*?-->`)
	mdOwnLine = regexp.MustCompile(`(?ms)^[ \t]*<!--.*?-->[ \t]*(?:\n|\z)`)
	mdHeading = regexp.MustCompile(`^#{1,6}\s+`)
	mdList    = regexp.MustCompile(`^(\s*)(?:[-*+]|\d+[.)])\s+`)
	mdRule    = regexp.MustCompile(`^\s*(?:-{3,}|\*{3,}|_{3,})\s*$`)
)

// mdLines renders markdown as plain wrapped text: headings and bold in
// bold, list bullets, quotes and code blocks indented, images and links
// reduced to their text, HTML comments dropped; runs of blank lines become
// one.
func mdLines(s string, w int) []string { return mdLinesWith(s, w, nil) }

// mdLinesWith is mdLines with a picture alone on its line drawn by draw
// when it is not nil.
func mdLinesWith(s string, w int, draw pictureLines) []string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = mdOwnLine.ReplaceAllString(s, "")
	s = mdComment.ReplaceAllString(s, "")
	s = strings.Trim(s, "\n")
	if strings.TrimSpace(s) == "" {
		return nil
	}
	var out []string
	inCode := false
	for _, line := range strings.Split(s, "\n") {
		t := strings.TrimSpace(line)
		if strings.HasPrefix(t, "```") || strings.HasPrefix(t, "~~~") {
			inCode = !inCode
			continue
		}
		if inCode {
			line = strings.ReplaceAll(line, "\t", "    ")
			for _, l := range wrap(line, max(w-2, 1)) {
				out = append(out, theme.Dim.Render("▏ ")+l)
			}
			continue
		}
		switch {
		case t == "":
			out = append(out, "")
		case mdRule.MatchString(t):
			out = append(out, theme.Dim.Render(strings.Repeat("─", min(w, 12))))
		case mdHeading.MatchString(t):
			out = append(out, wrap(theme.Bold.Render(mdInline(mdHeading.ReplaceAllString(t, ""))), w)...)
		case strings.HasPrefix(t, ">"):
			for _, l := range wrap(mdInline(strings.TrimSpace(strings.TrimLeft(t, ">"))), max(w-2, 1)) {
				out = append(out, theme.Dim.Render("│ ")+l)
			}
		case mdList.MatchString(line):
			m := mdList.FindStringSubmatch(line)
			indent := strings.Repeat(" ", min(len(strings.ReplaceAll(m[1], "\t", "  ")), 8))
			bullet := "• "
			if d := strings.TrimSpace(line[len(m[1]):len(m[0])]); d != "" && d[0] >= '0' && d[0] <= '9' {
				bullet = d + " "
			}
			body := wrap(mdInline(line[len(m[0]):]), max(w-len(indent)-len(bullet), 1))
			for i, l := range body {
				if i == 0 {
					out = append(out, indent+bullet+l)
				} else {
					out = append(out, indent+strings.Repeat(" ", len(bullet))+l)
				}
			}
		case draw != nil && mdImageLine(t):
			m := mdImage.FindStringSubmatch(t)
			out = append(out, draw(m[1], m[2], w)...)
		default:
			out = append(out, wrap(mdInline(line), w)...)
		}
	}
	// One blank line at most between paragraphs, none at the ends.
	var clean []string
	for _, l := range out {
		if strings.TrimSpace(l) == "" && (len(clean) == 0 || strings.TrimSpace(clean[len(clean)-1]) == "") {
			continue
		}
		clean = append(clean, l)
	}
	for len(clean) > 0 && strings.TrimSpace(clean[len(clean)-1]) == "" {
		clean = clean[:len(clean)-1]
	}
	return clean
}

// mdImageLine reports whether a line is a picture alone, with YouTrack's
// {width=…} after it if any.
func mdImageLine(t string) bool {
	loc := mdImage.FindStringIndex(t)
	if loc == nil || loc[0] != 0 {
		return false
	}
	rest := strings.TrimSpace(t[loc[1]:])
	return rest == "" || strings.HasPrefix(rest, "{") && strings.HasSuffix(rest, "}")
}

// inline reduces inline markdown: images to [image], links to their text,
// bold to bold.
func mdInline(s string) string {
	s = mdImage.ReplaceAllStringFunc(s, func(m string) string {
		alt := strings.TrimSpace(mdImage.FindStringSubmatch(m)[1])
		if alt == "" {
			return "[image]"
		}
		return "[image: " + alt + "]"
	})
	s = mdLink.ReplaceAllString(s, "$1")
	s = mdBold.ReplaceAllStringFunc(s, func(m string) string {
		sub := mdBold.FindStringSubmatch(m)
		return theme.Bold.Render(sub[1] + sub[2])
	})
	return s
}
