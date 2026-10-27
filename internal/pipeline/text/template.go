// Package text holds the text handling of the ymcp pipeline runner: prompt
// templates, branch-name slugs, the header that marks a comment as written by
// an agent, and the numbered feedback the agent answers.
//
// It has no dependencies on the rest of ymcp so every other pipeline package
// can use it.
package text

import (
	"fmt"
	"strconv"
	"strings"
	"unicode/utf8"
)

// Vars are the variables a template can use, keyed by their full dotted name:
// "issue.id", "issue.fields.Priority", "feedback".
type Vars map[string]string

// none is rendered for a variable that is missing or empty.
const none = "none"

// trimmedSuffix is appended by the trim filter when it cuts a value.
const trimmedSuffix = "… [trimmed]"

// maxSlug is the longest slug Slug returns.
const maxSlug = 50

type filter struct {
	name string
	n    int    // trim
	arg  string // default
}

// Render expands {{ name }} tags in tmpl. A tag is a variable name followed by
// any number of filters separated by "|":
//
//	slug          Slug of the value
//	trim N        the first N characters, "… [trimmed]" appended when cut
//	default "x"   x when the value is empty
//
// Whitespace inside the braces is free. A variable that is missing or empty
// renders as "none" unless the tag has a default filter. A name containing
// spaces can be written in quotes: {{ "issue.fields.Story points" }}. An
// unknown filter or a malformed tag is an error naming its position; there is
// no other syntax, and text outside tags is copied as is.
func Render(tmpl string, v Vars) (string, error) {
	var b strings.Builder
	i := 0
	for {
		open := strings.Index(tmpl[i:], "{{")
		if open < 0 {
			b.WriteString(tmpl[i:])
			return b.String(), nil
		}
		open += i
		b.WriteString(tmpl[i:open])
		closeAt := strings.Index(tmpl[open+2:], "}}")
		if closeAt < 0 {
			return "", errAt(tmpl, open, "tag is not closed, expected }}")
		}
		closeAt += open + 2
		name, filters, err := parseTag(tmpl, open+2, closeAt)
		if err != nil {
			return "", err
		}
		b.WriteString(eval(v[name], filters))
		i = closeAt + 2
	}
}

func eval(val string, filters []filter) string {
	hasDefault := false
	for _, f := range filters {
		if f.name == "default" {
			hasDefault = true
		}
	}
	if isEmpty(val) && !hasDefault {
		return none
	}
	for _, f := range filters {
		switch f.name {
		case "slug":
			val = Slug(val)
		case "trim":
			if utf8.RuneCountInString(val) > f.n {
				val = string([]rune(val)[:f.n]) + trimmedSuffix
			}
		case "default":
			if isEmpty(val) {
				val = f.arg
			}
		}
	}
	if isEmpty(val) && !hasDefault {
		return none
	}
	return val
}

func isEmpty(s string) bool { return strings.TrimSpace(s) == "" }

// parseTag parses tmpl[i:end], the inside of one tag; positions in errors are
// offsets into tmpl.
func parseTag(tmpl string, i, end int) (name string, filters []filter, err error) {
	skip := func() {
		for i < end && isSpace(tmpl[i]) {
			i++
		}
	}
	// word reads up to the next space or pipe.
	word := func() string {
		start := i
		for i < end && !isSpace(tmpl[i]) && tmpl[i] != '|' {
			i++
		}
		return tmpl[start:i]
	}

	skip()
	if i >= end {
		return "", nil, errAt(tmpl, i, "empty tag")
	}
	if tmpl[i] == '"' || tmpl[i] == '\'' {
		if name, i, err = quoted(tmpl, i, end); err != nil {
			return "", nil, err
		}
	} else {
		if name = word(); name == "" {
			return "", nil, errAt(tmpl, i, "missing variable name")
		}
	}
	for {
		skip()
		if i >= end {
			return name, filters, nil
		}
		if tmpl[i] != '|' {
			return "", nil, errAt(tmpl, i, "unexpected %q, expected | or }}", word())
		}
		i++
		skip()
		fpos := i
		fname := word()
		if fname == "" {
			return "", nil, errAt(tmpl, fpos, "missing filter name after |")
		}
		f := filter{name: fname}
		switch fname {
		case "slug":
		case "trim":
			skip()
			apos := i
			arg := word()
			if arg == "" {
				return "", nil, errAt(tmpl, apos, "trim needs a number of characters")
			}
			n, convErr := strconv.Atoi(arg)
			if convErr != nil || n < 0 {
				return "", nil, errAt(tmpl, apos, "trim needs a non-negative number, got %q", arg)
			}
			f.n = n
		case "default":
			skip()
			if i >= end || (tmpl[i] != '"' && tmpl[i] != '\'') {
				return "", nil, errAt(tmpl, i, `default needs a quoted value, like default "none"`)
			}
			if f.arg, i, err = quoted(tmpl, i, end); err != nil {
				return "", nil, err
			}
		default:
			return "", nil, errAt(tmpl, fpos, "unknown filter %q", fname)
		}
		filters = append(filters, f)
	}
}

// quoted reads the quoted string starting at tmpl[i] and returns it with the
// offset after the closing quote. \\, \", \' and \n are escapes.
func quoted(tmpl string, i, end int) (string, int, error) {
	q := tmpl[i]
	start := i
	i++
	var b strings.Builder
	for i < end {
		c := tmpl[i]
		switch {
		case c == q:
			return b.String(), i + 1, nil
		case c == '\\' && i+1 < end:
			i++
			switch tmpl[i] {
			case 'n':
				b.WriteByte('\n')
			default:
				b.WriteByte(tmpl[i])
			}
			i++
		default:
			b.WriteByte(c)
			i++
		}
	}
	return "", 0, errAt(tmpl, start, "string is not closed")
}

func isSpace(c byte) bool { return c == ' ' || c == '\t' || c == '\n' || c == '\r' }

// errAt builds a template error at byte offset pos, shown as line and column.
func errAt(tmpl string, pos int, format string, args ...any) error {
	if pos > len(tmpl) {
		pos = len(tmpl)
	}
	line := 1 + strings.Count(tmpl[:pos], "\n")
	col := 1 + utf8.RuneCountInString(tmpl[strings.LastIndex(tmpl[:pos], "\n")+1:pos])
	return fmt.Errorf("template: line %d, column %d: %s", line, col, fmt.Sprintf(format, args...))
}

// translit maps lower-case Russian and Ukrainian letters to Latin.
var translit = map[rune]string{
	'а': "a", 'б': "b", 'в': "v", 'г': "g", 'д': "d", 'е': "e", 'ё': "yo", 'ж': "zh",
	'з': "z", 'и': "i", 'й': "y", 'к': "k", 'л': "l", 'м': "m", 'н': "n", 'о': "o",
	'п': "p", 'р': "r", 'с': "s", 'т': "t", 'у': "u", 'ф': "f", 'х': "kh", 'ц': "ts",
	'ч': "ch", 'ш': "sh", 'щ': "shch", 'ъ': "", 'ы': "y", 'ь': "", 'э': "e", 'ю': "yu",
	'я': "ya", 'і': "i", 'ї': "yi", 'є': "ye", 'ґ': "g",
}

// Slug turns s into a branch-name-safe word: Cyrillic is transliterated, the
// result is lower-case ASCII letters and digits with every other run replaced
// by one "-", without dashes at the ends and at most 50 characters, cut at a
// dash when there is one to cut at. It returns "" when nothing is left.
func Slug(s string) string {
	var b strings.Builder
	dash := false
	for _, r := range strings.ToLower(s) {
		out := string(r)
		if r >= 0x80 {
			t, ok := translit[r]
			if !ok {
				dash = true // any other non-ASCII character separates words
				continue
			}
			out = t // "" for the hard and soft signs: dropped inside a word
		} else if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9') {
			dash = true
			continue
		}
		if out == "" {
			continue
		}
		if dash && b.Len() > 0 {
			b.WriteByte('-')
		}
		dash = false
		b.WriteString(out)
	}
	slug := b.String()
	if len(slug) > maxSlug {
		cut := slug[:maxSlug]
		if slug[maxSlug] != '-' {
			if i := strings.LastIndexByte(cut, '-'); i > 0 {
				cut = cut[:i]
			}
		}
		slug = cut
	}
	return strings.Trim(slug, "-")
}
