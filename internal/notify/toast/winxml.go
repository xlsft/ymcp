package toast

import (
	"net/url"
	"strings"

	"github.com/xlsft/ymcp/internal/notify"
)

// The Windows toast is built from go-toast's text/template, which pastes
// values into XML unescaped: the title and body inside CDATA sections, the
// icon and launch URL inside double-quoted attributes. When the COM call
// fails, go-toast falls back to PowerShell with that XML in an expandable
// here-string, where $ and ` would be interpreted. These helpers make text
// safe for both.

// winToast holds the escaped values for go-toast's Notification.
type winToast struct {
	Title, Body string // for CDATA sections
	Icon        string // for an attribute
	Launch      string // for an attribute; empty when there is nothing to open
	Silent      bool
}

func windowsToast(e notify.Event, icon string, sound bool) winToast {
	t := winToast{
		Title:  cdataText(e.Heading()),
		Body:   cdataText(e.Body),
		Icon:   xmlAttr(icon),
		Silent: !sound,
	}
	if openable(e.URL) {
		t.Launch = xmlAttr(e.URL)
	}
	return t
}

// openable reports whether u is an absolute http(s) URL, the only kind a
// click opens.
func openable(u string) bool {
	p, err := url.Parse(u)
	return err == nil && (p.Scheme == "http" || p.Scheme == "https") && p.Host != ""
}

// xmlAttr escapes s for a double-quoted XML attribute, also hiding $ and `
// from PowerShell as character references.
func xmlAttr(s string) string {
	return attrReplacer.Replace(xmlChars(s))
}

var attrReplacer = strings.NewReplacer(
	"&", "&amp;", `"`, "&quot;", "<", "&lt;", ">", "&gt;", "'", "&apos;",
	"$", "&#36;", "`", "&#96;",
)

// cdataText makes s safe inside <![CDATA[…]]>: characters that would end the
// section (">" of "]]>") or mean something to PowerShell ($, `, and " of a
// closing "@) are moved out of it as character references.
func cdataText(s string) string {
	return cdataReplacer.Replace(xmlChars(s))
}

var cdataReplacer = strings.NewReplacer(
	">", "]]>&gt;<![CDATA[",
	"$", "]]>&#36;<![CDATA[",
	"`", "]]>&#96;<![CDATA[",
	`"`, "]]>&#34;<![CDATA[",
)

// xmlChars drops characters XML 1.0 doesn't allow (most control
// characters), which would make Windows reject the whole toast.
func xmlChars(s string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r == '\t' || r == '\n' || r == '\r':
			return r
		case r < 0x20, r == 0xFFFE, r == 0xFFFF, r >= 0xD800 && r <= 0xDFFF:
			return -1
		}
		return r
	}, s)
}
