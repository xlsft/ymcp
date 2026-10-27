package workspace

import "strings"

// Quote quotes s as one word for a POSIX shell. Words made only of
// characters that are never special are left as they are.
func Quote(s string) string {
	if s == "" {
		return "''"
	}
	safe := true
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case strings.ContainsRune("_-+./,:@%", r):
		default:
			safe = false
		}
		if !safe {
			break
		}
	}
	if safe {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// QuoteArgs quotes every element of argv and joins them with spaces, so a
// POSIX shell reads the line back as argv.
func QuoteArgs(argv []string) string {
	q := make([]string, len(argv))
	for i, a := range argv {
		q[i] = Quote(a)
	}
	return strings.Join(q, " ")
}

// cdScript is the start of a remote script that changes to dir; "" when dir
// is empty (the login directory). A leading ~ is the remote home; a
// relative dir is taken from there too, as ssh starts in it.
func cdScript(dir string) string {
	switch {
	case dir == "":
		return ""
	case dir == "~":
		return `cd "$HOME" && `
	case strings.HasPrefix(dir, "~/"):
		return `cd "$HOME"/` + Quote(dir[2:]) + " && "
	case strings.HasPrefix(dir, "-"):
		dir = "./" + dir
	}
	return "cd " + Quote(dir) + " && "
}
