package text

import (
	"regexp"
	"strings"
	"testing"
)

var m17 = Mark{Pipeline: "backend", Run: 17, Msg: "3f9a"}

const marker17 = "<!-- ymcp:agent pipeline=backend run=17 msg=3f9a -->"

func TestMarkerLineAndPrefix(t *testing.T) {
	if got := MarkerLine(m17); got != marker17 {
		t.Errorf("MarkerLine = %q", got)
	}
	if got := Prefix("@xlsft", m17); got != "@xlsft's agent #17 (backend)" {
		t.Errorf("Prefix = %q", got)
	}
	if got := Prefix("", m17); got != "Agent #17 (backend)" {
		t.Errorf("Prefix without mention = %q", got)
	}
}

func TestParseMarker(t *testing.T) {
	tests := []struct {
		name, in string
		want     Mark
		ok       bool
	}{
		{"exact", marker17, m17, true},
		{"with body", marker17 + "\n@x's agent #17 (backend)\n\nhello", m17, true},
		{"crlf", marker17 + "\r\n@x's agent #17 (backend)\r\n\r\nhello", m17, true},
		{"leading spaces", "   \t" + marker17, m17, true},
		{"leading newlines", "\n\n  " + marker17 + "\n", m17, true},
		{"not first line", "some quote\n> text\n" + marker17 + "\nrest", m17, true},
		{"middle of line", "foo " + marker17 + " bar", m17, true},
		{"loose spacing", "<!--ymcp:agent   pipeline=backend  run=17   msg=3f9a-->", m17, true},
		{"newlines inside", "<!--\n ymcp:agent pipeline=backend\nrun=17 msg=3f9a\n-->", m17, true},
		{"dashes in name", "<!-- ymcp:agent pipeline=my-pipe_1 run=2 msg=ab12cd34 -->", Mark{"my-pipe_1", 2, "ab12cd34"}, true},
		{"first of two", marker17 + "\n<!-- ymcp:agent pipeline=other run=1 msg=zz -->", m17, true},
		{"cyrillic around", "Привет 🚀\n" + marker17 + "\nТекст", m17, true},
		{"none", "just a comment", Mark{}, false},
		{"empty", "", Mark{}, false},
		{"other comment", "<!-- hello -->", Mark{}, false},
		{"no run", "<!-- ymcp:agent pipeline=a msg=b -->", Mark{}, false},
		{"run not number", "<!-- ymcp:agent pipeline=a run=x msg=b -->", Mark{}, false},
		{"unclosed", "<!-- ymcp:agent pipeline=a run=1 msg=b", Mark{}, false},
		{"run overflow", "<!-- ymcp:agent pipeline=a run=99999999999999999999 msg=b -->", Mark{}, false},
	}
	for _, tc := range tests {
		got, ok := ParseMarker(tc.in)
		if ok != tc.ok || got != tc.want {
			t.Errorf("%s: ParseMarker = %+v, %v; want %+v, %v", tc.name, got, ok, tc.want, tc.ok)
		}
	}
}

func TestDecorate(t *testing.T) {
	got := Decorate("Done!", "@xlsft", m17)
	want := marker17 + "\n@xlsft's agent #17 (backend)\n\nDone!"
	if got != want {
		t.Errorf("Decorate = %q, want %q", got, want)
	}
	if mk, ok := ParseMarker(got); !ok || mk != m17 {
		t.Errorf("ParseMarker(Decorate) = %+v, %v", mk, ok)
	}
}

func TestDecorateIdempotentAndReplaces(t *testing.T) {
	other := Mark{Pipeline: "backend", Run: 17, Msg: "beef"}
	bodies := []string{
		"Done!",
		"",
		Working,
		"Русский текст 🚀\n\n- пункт\n- пункт",
		"line1\r\nline2",
		"  indented first line",
		"mentions agent #3 (x) in the body",
	}
	for _, body := range bodies {
		once := Decorate(body, "@xlsft", m17)
		if twice := Decorate(once, "@xlsft", m17); twice != once {
			t.Errorf("not idempotent for %q:\n%q\n%q", body, once, twice)
		}
		if n := strings.Count(once, "ymcp:agent"); n != 1 {
			t.Errorf("%d markers in %q", n, once)
		}
		// Replacing "Working..." by the answer keeps one header, with the new one.
		re := Decorate(once, "@someone", other)
		if n := strings.Count(re, "ymcp:agent"); n != 1 {
			t.Errorf("%d markers after redecorate: %q", n, re)
		}
		if re != Decorate(body, "@someone", other) {
			t.Errorf("redecorate = %q", re)
		}
		if got := StripHeader(once); got != body {
			t.Errorf("StripHeader(Decorate(%q)) = %q", body, got)
		}
	}
}

func TestDecorateEditFlow(t *testing.T) {
	thinking := Decorate(Working, "@xlsft", m17)
	// The answer is written over the placeholder: the agent sends a body, the
	// engine decorates it again.
	answer := Decorate("All done.", "@xlsft", m17)
	if strings.Contains(answer, Working) {
		t.Error("placeholder survived")
	}
	// Decorating the whole text read back from YouTrack, with CRLF endings.
	crlf := strings.ReplaceAll(thinking, "\n", "\r\n")
	got := Decorate(crlf, "@xlsft", m17)
	if got != thinking {
		t.Errorf("CRLF header not replaced cleanly: %q", got)
	}
}

func TestStripHeader(t *testing.T) {
	tests := []struct{ name, in, want string }{
		{"no header", "hello", "hello"},
		{"full", marker17 + "\n@xlsft's agent #17 (backend)\n\nhello", "hello"},
		{"crlf", marker17 + "\r\n@xlsft's agent #17 (backend)\r\n\r\nhello\r\nworld", "hello\r\nworld"},
		{"leading whitespace", "\n  " + marker17 + "\n@xlsft's agent #17 (backend)\n\nhello", "hello"},
		{"marker only", marker17 + "\nhello", "hello"},
		{"marker only, no body", marker17, ""},
		{"no blank line", marker17 + "\n@xlsft's agent #17 (backend)\nhello", "hello"},
		{"many blanks", marker17 + "\n@xlsft's agent #17 (backend)\n\n\n\nhello", "hello"},
		{"dotted login", marker17 + "\n@ilya.torshin's agent #17 (backend)\n\nhello", "hello"},
		{"curly apostrophe", marker17 + "\n@xlsft’s agent #17 (backend)\n\nhello", "hello"},
		{"no mention", marker17 + "\nAgent #17 (backend)\n\nhello", "hello"},
		{"keeps body that looks like a header", marker17 + "\n@x's agent #17 (backend)\n\n@y's agent #2 (z)\nhi", "@y's agent #2 (z)\nhi"},
		{"keeps inner blank lines", marker17 + "\n@x's agent #17 (backend)\n\na\n\nb", "a\n\nb"},
		{"marker in the middle stays", "intro\n" + marker17 + "\nmore", "intro\n" + marker17 + "\nmore"},
		{"prefix without marker stays", "@x's agent #17 (backend)\n\nhello", "@x's agent #17 (backend)\n\nhello"},
		{"empty", "", ""},
	}
	for _, tc := range tests {
		if got := StripHeader(tc.in); got != tc.want {
			t.Errorf("%s: StripHeader = %q, want %q", tc.name, got, tc.want)
		}
	}
}

func TestNewMsgID(t *testing.T) {
	re := regexp.MustCompile(`^[0-9a-f]{8}$`)
	seen := map[string]bool{}
	for i := 0; i < 200; i++ {
		id := NewMsgID()
		if !re.MatchString(id) {
			t.Fatalf("NewMsgID = %q", id)
		}
		seen[id] = true
	}
	if len(seen) < 199 {
		t.Errorf("only %d distinct ids out of 200", len(seen))
	}
	// An id round-trips through the marker.
	m := Mark{Pipeline: "p", Run: 1, Msg: NewMsgID()}
	if got, ok := ParseMarker(MarkerLine(m)); !ok || got != m {
		t.Errorf("round trip = %+v, %v", got, ok)
	}
}

func TestDecorateCodeHost(t *testing.T) {
	m := Mark{Pipeline: "stress", Run: 8, Msg: "ab12"}
	got := DecorateCodeHost("Done.", "@xlsft", m)
	want := "<!-- ymcp:agent pipeline=stress run=8 msg=ab12 -->\n@xlsft's agent #\u20608 (stress)\n\nDone."
	if got != want {
		t.Errorf("DecorateCodeHost = %q, want %q", got, want)
	}
	if strings.Contains(got, "#8") {
		t.Error("the run number can still be read as a reference")
	}
	// Re-decorating replaces the header, also the plain one and vice versa.
	if again := DecorateCodeHost(got, "@xlsft", m); again != want {
		t.Errorf("redecorated = %q", again)
	}
	if plain := Decorate(got, "@xlsft", m); StripHeader(plain) != "Done." || strings.Contains(plain, "\u2060") {
		t.Errorf("plain over code-host header = %q", plain)
	}
	if StripHeader(got) != "Done." {
		t.Errorf("StripHeader = %q", StripHeader(got))
	}
}
