package text

import (
	"strings"
	"testing"
)

// FuzzRender: any template renders or fails without panicking, and one
// with no tag comes back as it is.
func FuzzRender(f *testing.F) {
	for _, s := range []string{"", "plain", "{{issue.id}}-{{issue.summary | slug}}", "{{a | default \"x\" | trim}}", "{{", "}}{{x", "{{ | }}"} {
		f.Add(s)
	}
	v := Vars{"issue.id": "ABC-1", "issue.summary": "Fix the login page", "a": ""}
	f.Fuzz(func(t *testing.T, tmpl string) {
		out, err := Render(tmpl, v)
		if err == nil && !strings.Contains(tmpl, "{{") && out != tmpl {
			t.Errorf("Render(%q) = %q, want it unchanged", tmpl, out)
		}
	})
}

// FuzzMarker: the marker of a decorated text is read back whatever the
// body, and no text makes ParseMarker panic.
func FuzzMarker(f *testing.F) {
	for _, s := range []string{"", "hello", "<!-- ymcp:agent pipeline=p run=1 msg=a -->", "**bold**\n\n- item", "<!-- ymcp:agent"} {
		f.Add(s)
	}
	mark := Mark{Pipeline: "backend", Run: 7, Msg: "a1b2c3d4"}
	f.Fuzz(func(t *testing.T, body string) {
		ParseMarker(body)
		if got, ok := ParseMarker(Decorate(body, "@dev", mark)); !ok || got != mark {
			t.Errorf("ParseMarker(Decorate(%q)) = %+v, %v", body, got, ok)
		}
	})
}
