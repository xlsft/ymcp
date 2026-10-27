package text

import (
	"strings"
	"testing"
)

func TestSlug(t *testing.T) {
	long := strings.Repeat("word-", 20)
	tests := []struct{ in, want string }{
		{"Fix login bug", "fix-login-bug"},
		{"  Hello,   World!!  ", "hello-world"},
		{"", ""},
		{"!!!", ""},
		{"---a---b---", "a-b"},
		{"Исправить ошибку входа", "ispravit-oshibku-vkhoda"},
		{"Ёлка и щётка", "yolka-i-shchyotka"},
		{"ОБЪЕКТ и подъезд", "obekt-i-podezd"},
		{"Сделать API v2 быстрее", "sdelat-api-v2-bystree"},
		{"Їжак, ґанок, є", "yizhak-ganok-ye"},
		{"🚀 Launch 🚀 day", "launch-day"},
		{"emoji🔥only", "emoji-only"},
		{"Fix: crash (#123)", "fix-crash-123"},
		{"snake_case_name", "snake-case-name"},
		{"日本語 title", "title"},
		{long, "word-word-word-word-word-word-word-word-word-word"},
		{strings.Repeat("a", 80), strings.Repeat("a", 50)},
		{strings.Repeat("a", 50) + "-bbb", strings.Repeat("a", 50)},
		{strings.Repeat("a", 49) + " bbb", strings.Repeat("a", 49)},
		{strings.Repeat("ab ", 30), strings.Repeat("ab-", 16) + "ab"},
	}
	for _, tc := range tests {
		got := Slug(tc.in)
		if got != tc.want {
			t.Errorf("Slug(%q) = %q, want %q", tc.in, got, tc.want)
		}
		if len(got) > 50 || strings.HasPrefix(got, "-") || strings.HasSuffix(got, "-") {
			t.Errorf("Slug(%q) = %q breaks the invariants", tc.in, got)
		}
	}
}

func TestRender(t *testing.T) {
	v := Vars{
		"issue.id":                  "YPC-7",
		"issue.summary":             "Исправить ошибку входа 🚀",
		"issue.description":         "абвгдеёжз",
		"issue.fields.Priority":     "Major",
		"issue.fields.Story points": "5",
		"empty":                     "",
		"blank":                     "  \n ",
		"quote":                     `say "hi"`,
	}
	tests := []struct{ name, tmpl, want string }{
		{"plain", "no tags here", "no tags here"},
		{"empty template", "", ""},
		{"var", "Issue {{issue.id}}!", "Issue YPC-7!"},
		{"spaces", "{{   issue.id   }}", "YPC-7"},
		{"tabs and newlines", "{{\tissue.id\n}}", "YPC-7"},
		{"two", "{{issue.id}}/{{ issue.fields.Priority }}", "YPC-7/Major"},
		{"missing", "[{{nope}}]", "[none]"},
		{"empty", "[{{empty}}]", "[none]"},
		{"blank", "[{{blank}}]", "[none]"},
		{"missing slug", "{{nope | slug}}", "none"},
		{"missing trim", "{{nope | trim 3}}", "none"},
		{"slug", "{{issue.id}}-{{issue.summary | slug}}", "YPC-7-ispravit-oshibku-vkhoda"},
		{"slug then trim", "{{quote | slug | trim 3}}", "say… [trimmed]"},
		{"trim cut", "{{issue.description | trim 4}}", "абвг… [trimmed]"},
		{"trim exact", "{{issue.description | trim 9}}", "абвгдеёжз"},
		{"trim longer", "{{issue.description | trim 100}}", "абвгдеёжз"},
		{"trim emoji", "{{issue.summary | trim 22}}", "Исправить ошибку входа… [trimmed]"},
		{"trim emoji keeps", "{{issue.summary | trim 24}}", "Исправить ошибку входа 🚀"},
		{"trim zero", "{{issue.id | trim 0}}", "… [trimmed]"},
		{"default used", `{{ empty | default "n/a" }}`, "n/a"},
		{"default missing", `{{ nope | default "n/a" }}`, "n/a"},
		{"default empty string", `[{{ nope | default "" }}]`, "[]"},
		{"default unused", `{{ issue.id | default "n/a" }}`, "YPC-7"},
		{"default single quotes", `{{ nope | default 'x y' }}`, "x y"},
		{"default escapes", `{{ nope | default "a\"b\\c\nd" }}`, "a\"b\\c\nd"},
		{"chain", `{{ issue.summary | slug | trim 7 }}`, "ispravi… [trimmed]"},
		{"default then slug", `{{ nope | default "Hello World" | slug }}`, "hello-world"},
		{"slug then default", `{{ quote | slug | default "x" }}`, "say-hi"},
		{"slug to empty then default", `{{ "!!!" | slug | default "x" }}`, "x"},
		{"quoted name", `{{ "issue.fields.Story points" }}`, "5"},
		{"quoted name with filter", `{{ 'issue.fields.Story points' | default "0" }}`, "5"},
		{"no pipe spacing", `{{issue.id|slug}}`, "ypc-7"},
		{"lone braces", "a { b } c }} {", "a { b } c }} {"},
		{"value is not re-rendered", "{{ quote }}", `say "hi"`},
		{"adjacent", "{{issue.id}}{{issue.id}}", "YPC-7YPC-7"},
		{"multiline template", "a\n{{ issue.id }}\nb", "a\nYPC-7\nb"},
	}
	for _, tc := range tests {
		got, err := Render(tc.tmpl, v)
		if err != nil {
			t.Errorf("%s: Render(%q) error: %v", tc.name, tc.tmpl, err)
			continue
		}
		if got != tc.want {
			t.Errorf("%s: Render(%q) = %q, want %q", tc.name, tc.tmpl, got, tc.want)
		}
	}
}

func TestRenderPipeInDefault(t *testing.T) {
	got, err := Render(`{{ nope | default "a|b" | slug }}`, nil)
	if err != nil || got != "a-b" {
		t.Errorf("got %q, %v", got, err)
	}
}

func TestRenderNilVars(t *testing.T) {
	got, err := Render("{{ a }}", nil)
	if err != nil || got != "none" {
		t.Errorf("got %q, %v", got, err)
	}
}

func TestRenderErrors(t *testing.T) {
	tests := []struct{ name, tmpl, want string }{
		{"unknown filter", "{{ a | upper }}", `line 1, column 8: unknown filter "upper"`},
		{"unknown filter later", "x\n{{ a | slug | nope }}", `line 2, column 15: unknown filter "nope"`},
		{"unclosed tag", "abc {{ a", "line 1, column 5: tag is not closed"},
		{"unclosed on line 3", "a\nb\n  {{ a", "line 3, column 3: tag is not closed"},
		{"empty tag", "{{}}", "line 1, column 3: empty tag"},
		{"blank tag", "x {{   }}", "empty tag"},
		{"missing filter name", "{{ a | }}", "missing filter name"},
		{"missing filter name 2", "{{ a | | slug }}", "missing filter name"},
		{"leading pipe", "{{ | slug }}", "missing variable name"},
		{"trim without number", "{{ a | trim }}", "trim needs a number"},
		{"trim not number", "{{ a | trim x }}", `got "x"`},
		{"trim negative", "{{ a | trim -3 }}", `got "-3"`},
		{"default unquoted", "{{ a | default x }}", "default needs a quoted value"},
		{"default without arg", "{{ a | default }}", "default needs a quoted value"},
		{"unclosed string", `{{ a | default "x }}`, "string is not closed"},
		{"missing pipe", "{{ a slug }}", `unexpected "slug", expected |`},
		{"slug with arg", "{{ a | slug 3 }}", `unexpected "3"`},
		{"two names", "{{ a b }}", `unexpected "b"`},
		{"quote in braces", `{{ nope | default "a}}b" }}`, "string is not closed"},
		{"unicode column", "Привет {{ a | zz }}", "line 1, column 15"},
	}
	for _, tc := range tests {
		_, err := Render(tc.tmpl, Vars{"a": "x"})
		if err == nil {
			t.Errorf("%s: Render(%q) succeeded", tc.name, tc.tmpl)
			continue
		}
		if !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: error %q does not contain %q", tc.name, err, tc.want)
		}
		if !strings.HasPrefix(err.Error(), "template: line ") {
			t.Errorf("%s: error %q has no position", tc.name, err)
		}
	}
}
