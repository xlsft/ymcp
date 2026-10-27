package workspace

import (
	"strings"
	"unicode/utf8"
)

func trim(s string) string { return strings.TrimSpace(s) }

func join(args []string) string { return strings.Join(args, " ") }

// shorten cuts s to at most n runes, marking the cut.
func shorten(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	r := []rune(s)
	return string(r[:n]) + "..."
}

// isAbs reports whether p is an absolute path on either a POSIX or a Windows
// host, whatever system ymcp itself runs on.
func isAbs(p string) bool {
	if strings.HasPrefix(p, "/") || strings.HasPrefix(p, `\`) {
		return true
	}
	return len(p) > 2 && p[1] == ':' && (p[2] == '/' || p[2] == '\\')
}

// joinPath joins path elements with "/", which git accepts on every host.
func joinPath(base string, elems ...string) string {
	p := strings.TrimRight(base, `/\`)
	for _, e := range elems {
		p += "/" + strings.Trim(e, `/\`)
	}
	return p
}
