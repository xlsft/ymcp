package engine

import "testing"

func TestWithTailQuotesTheOutput(t *testing.T) {
	var tl tail
	tl.add("Confirmed: cargo is not approved here.\n\nI stopped.")
	got := withTail("The agent produced no output for 2m.", &tl)
	want := "The agent produced no output for 2m.\n\nLast output:\n\n> Confirmed: cargo is not approved here.\n>\n> I stopped."
	if got != want {
		t.Errorf("withTail = %q, want %q", got, want)
	}
	if got := codeBlock("exit status 1"); got != "```text\nexit status 1\n```" {
		t.Errorf("codeBlock = %q", got)
	}
}
