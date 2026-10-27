package text

import (
	"reflect"
	"testing"
)

func TestRenderFeedback(t *testing.T) {
	tests := []struct {
		name  string
		items []FeedbackItem
		want  string
	}{
		{"none", nil, ""},
		{"full", []FeedbackItem{{
			N: 1, Source: "github", Author: "alice", URL: "https://github.com/o/r/pull/3#discussion_r1",
			File: "internal/x.go", Line: 42, Hunk: "@@ -1,2 +1,3 @@\n-old\n+new", Body: "Please rename this.\nAnd test it.",
		}}, "[1] github · alice · https://github.com/o/r/pull/3#discussion_r1\n" +
			"  internal/x.go:42\n" +
			"  @@ -1,2 +1,3 @@\n  -old\n  +new\n" +
			"  Please rename this.\n  And test it."},
		{"comment only", []FeedbackItem{{N: 2, Source: "youtrack", Author: "ilya", URL: "https://y/issue/A-1#focus=Comments-1-2", Body: "Почему так? 🤔"}},
			"[2] youtrack · ilya · https://y/issue/A-1#focus=Comments-1-2\n  Почему так? 🤔"},
		{"file without line", []FeedbackItem{{N: 3, Source: "gitlab", Author: "bob", File: "a.go", Body: "x"}},
			"[3] gitlab · bob\n  a.go\n  x"},
		{"outdated", []FeedbackItem{{N: 4, Source: "github", Author: "c", File: "a.go", Line: 1, Body: "x", Outdated: true}},
			"[4] github · c (outdated)\n  a.go:1\n  x"},
		{"no meta", []FeedbackItem{{N: 5, Body: "bare"}}, "[5]\n  bare"},
		{"two", []FeedbackItem{{N: 1, Source: "s", Author: "a", Body: "one"}, {N: 2, Source: "s", Author: "b", Body: "two"}},
			"[1] s · a\n  one\n\n[2] s · b\n  two"},
		{"crlf and blank lines", []FeedbackItem{{N: 1, Source: "s", Body: "a\r\n\r\nb\r\n"}},
			"[1] s\n  a\n\n  b"},
		{"empty body", []FeedbackItem{{N: 1, Source: "s", Author: "a"}}, "[1] s · a"},
	}
	for _, tc := range tests {
		if got := RenderFeedback(tc.items); got != tc.want {
			t.Errorf("%s:\n got %q\nwant %q", tc.name, got, tc.want)
		}
	}
}

func TestRenderedFeedbackDoesNotLookLikeAnAnswer(t *testing.T) {
	// A comment body that itself starts with "[2]" is indented, so quoting
	// the rendered feedback back does not split.
	out := RenderFeedback([]FeedbackItem{{N: 1, Source: "s", Body: "[2] sneaky"}})
	_, rest := SplitAnswer(out, []int{2})
	if rest == "" {
		t.Error("indented [2] was taken as an answer")
	}
}

func TestSplitAnswer(t *testing.T) {
	tests := []struct {
		name     string
		in       string
		ns       []int
		wantPart map[int]string
		wantRest string
	}{
		{"empty", "", []int{1}, map[int]string{}, ""},
		{"no markers", "All fixed.", []int{1, 2}, map[int]string{}, "All fixed."},
		{"basic", "[1] fixed\n[2] won't do", []int{1, 2},
			map[int]string{1: "fixed", 2: "won't do"}, ""},
		{"multi-line parts", "[1] fixed\nsecond line\n\n[2] other\n  more", []int{1, 2},
			map[int]string{1: "fixed\nsecond line", 2: "other\n  more"}, ""},
		{"preamble and epilogue", "Summary:\n\n[1] a\n[2] b\n\nAnything else?", []int{1, 2},
			map[int]string{1: "a", 2: "b\n\nAnything else?"}, "Summary:"},
		{"bullets", "- [1] one\n* [2] two\n  - [3] three\n• [4] four", []int{1, 2, 3, 4},
			map[int]string{1: "one", 2: "two", 3: "three", 4: "four"}, ""},
		{"separators", "[1]: colon\n[2] - dash\n[3] – en\n[4] — em\n[5] -5 stays", []int{1, 2, 3, 4, 5},
			map[int]string{1: "colon", 2: "dash", 3: "en", 4: "em", 5: "-5 stays"}, ""},
		{"bold", "**[1]** done\n**[2]:** also **bold** text\n- **[3]** x", []int{1, 2, 3},
			map[int]string{1: "done", 2: "also **bold** text", 3: "x"}, ""},
		{"marker alone then text", "[1]\nthe answer\n[2]\nanother", []int{1, 2},
			map[int]string{1: "the answer", 2: "another"}, ""},
		{"unknown number before any part", "[9] stray\n[1] ok", []int{1},
			map[int]string{1: "ok"}, "[9] stray"},
		{"unknown number inside a part", "[1] ok\n[9] part of one\n[2] two", []int{1, 2},
			map[int]string{1: "ok\n[9] part of one", 2: "two"}, ""},
		{"number not at line start", "see [1] above\n[2] real", []int{1, 2},
			map[int]string{2: "real"}, "see [1] above"},
		{"not in ns at all", "[1] a\n[2] b", []int{3},
			map[int]string{}, "[1] a\n[2] b"},
		{"nil ns", "[1] a", nil, map[int]string{}, "[1] a"},
		{"crlf", "[1] a\r\nmore\r\n[2] b\r\n", []int{1, 2},
			map[int]string{1: "a\nmore", 2: "b"}, ""},
		{"repeated number appends", "[1] a\n[2] b\n[1] c", []int{1, 2},
			map[int]string{1: "a\nc", 2: "b"}, ""},
		{"leading zeros", "[01] a", []int{1}, map[int]string{1: "a"}, ""},
		{"huge number", "[99999999999999999999] a\n[1] b", []int{1},
			map[int]string{1: "b"}, "[99999999999999999999] a"},
		{"empty reply dropped", "[1]\n[2] b", []int{1, 2},
			map[int]string{2: "b"}, ""},
		{"russian and emoji", "[1] Исправил ✅\nДобавил тест\n[2] Нет, не буду 🙅", []int{1, 2},
			map[int]string{1: "Исправил ✅\nДобавил тест", 2: "Нет, не буду 🙅"}, ""},
		{"text after bracket without space", "[1]fixed", []int{1}, map[int]string{1: "fixed"}, ""},
		{"link syntax is not a marker", "[1](http://x) text", []int{1}, map[int]string{1: "(http://x) text"}, ""},
	}
	for _, tc := range tests {
		parts, rest := SplitAnswer(tc.in, tc.ns)
		if parts == nil {
			t.Errorf("%s: parts is nil", tc.name)
		}
		if !reflect.DeepEqual(parts, tc.wantPart) {
			t.Errorf("%s: parts = %v, want %v", tc.name, parts, tc.wantPart)
		}
		if rest != tc.wantRest {
			t.Errorf("%s: rest = %q, want %q", tc.name, rest, tc.wantRest)
		}
	}
}
