package text

import (
	"crypto/rand"
	"encoding/hex"
	"regexp"
	"strconv"
	"strings"
)

// Thinking is the placeholder comment an agent posts while it works; the
// answer later replaces it in place.
const Thinking = "🧠 Thinking..."

// Mark identifies a message written by an agent run.
type Mark struct {
	Pipeline string
	Run      int
	Msg      string // short random id, see NewMsgID
}

// markerRe finds the marker anywhere in a text.
var markerRe = regexp.MustCompile(`<!--\s*ymcp:agent\s+pipeline=(\S+)\s+run=(\d+)\s+msg=([^\s>]+?)\s*-->`)

// prefixRe matches the visible prefix line that follows the marker. The
// mention is optional so a header written without one is still recognized.
var prefixRe = regexp.MustCompile(`^[ \t]*(?:\S+['’]s )?[Aa]gent #\d+ \([^\r\n]*\)[ \t]*(?:\r?\n|$)`)

// NewMsgID returns a short random message id.
func NewMsgID() string {
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic("text: no randomness: " + err.Error())
	}
	return hex.EncodeToString(b[:])
}

// MarkerLine returns the hidden marker, an HTML comment:
//
//	<!-- ymcp:agent pipeline=backend run=17 msg=3f9a -->
func MarkerLine(m Mark) string {
	return "<!-- ymcp:agent pipeline=" + m.Pipeline + " run=" + strconv.Itoa(m.Run) + " msg=" + m.Msg + " -->"
}

// ParseMarker finds the marker in the raw text of a comment, wherever it is
// and whatever the line endings or leading whitespace.
func ParseMarker(body string) (Mark, bool) {
	m := markerRe.FindStringSubmatch(body)
	if m == nil {
		return Mark{}, false
	}
	run, err := strconv.Atoi(m[2])
	if err != nil {
		return Mark{}, false
	}
	return Mark{Pipeline: m[1], Run: run, Msg: m[3]}, true
}

// Prefix returns the visible line of the header, for example
// "@xlsft's agent #17 (backend)". The mention is the "@login" the agent acts
// for; without one the line starts with "Agent".
func Prefix(mention string, m Mark) string {
	tail := "agent #" + strconv.Itoa(m.Run) + " (" + m.Pipeline + ")"
	if mention == "" {
		return "Agent #" + strconv.Itoa(m.Run) + " (" + m.Pipeline + ")"
	}
	return mention + "'s " + tail
}

// Decorate puts the header on body: the marker line, the prefix line, a blank
// line and the body. A header the body already has is replaced, so decorating
// twice, or decorating an edited message, leaves exactly one header.
func Decorate(body, mention string, m Mark) string {
	return MarkerLine(m) + "\n" + Prefix(mention, m) + "\n\n" + StripHeader(body)
}

// StripHeader returns body without the marker and prefix lines (and the blank
// lines after them). A marker in the middle of the text is not a header and
// stays.
func StripHeader(body string) string {
	loc := markerRe.FindStringIndex(body)
	if loc == nil || strings.TrimSpace(body[:loc[0]]) != "" {
		return body
	}
	rest := body[loc[1]:]
	rest = strings.TrimLeft(rest, " \t")
	rest = trimNewline(rest)
	if p := prefixRe.FindString(rest); p != "" {
		rest = rest[len(p):]
	}
	return strings.TrimLeft(rest, "\r\n")
}

func trimNewline(s string) string {
	if strings.HasPrefix(s, "\r\n") {
		return s[2:]
	}
	return strings.TrimPrefix(s, "\n")
}
