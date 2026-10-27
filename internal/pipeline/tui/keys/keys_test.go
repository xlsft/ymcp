package keys

import (
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

// TestLatin checks every key of the Russian PC layout, both cases, and the
// Ukrainian and Belarusian extras against the US QWERTY key at the same
// place.
func TestLatin(t *testing.T) {
	rows := []struct{ cyr, lat string }{
		// Russian, the letter rows and ё, unshifted then shifted.
		{"йцукенгшщзхъ", "qwertyuiop[]"},
		{"фывапролджэ", "asdfghjkl;'"},
		{"ячсмитьбю", "zxcvbnm,."},
		{"ё", "`"},
		{"ЙЦУКЕНГШЩЗХЪ", "QWERTYUIOP{}"},
		{"ФЫВАПРОЛДЖЭ", "ASDFGHJKL:\""},
		{"ЯЧСМИТЬБЮ", "ZXCVBNM<>"},
		{"Ё", "~"},
		// Shift+3, and the key right of Ю shifted (the "?" key).
		{"№,", "#?"},
		// Ukrainian.
		{"іІїЇєЄґҐ", "sS]}'\"\\|"},
		// Belarusian.
		{"ўЎ", "oO"},
	}
	n := 0
	for _, r := range rows {
		cyr, lat := []rune(r.cyr), []rune(r.lat)
		if len(cyr) != len(lat) {
			t.Fatalf("bad row %q/%q", r.cyr, r.lat)
		}
		for i := range cyr {
			n++
			if got := Latin(cyr[i]); got != lat[i] {
				t.Errorf("Latin(%q) = %q, want %q", cyr[i], got, lat[i])
			}
		}
	}
	// 33 Russian letters in two cases, №, the "?" key, 8 Ukrainian, 2
	// Belarusian.
	if n != 66+2+8+2 {
		t.Errorf("checked %d keys", n)
	}

	// What a Latin layout types, and "?" itself (shift+7 on the Russian
	// layout), stays as it is.
	for _, r := range "abcxyzABCXYZ0123456789?/.;:'\"[]{}<>`~!@#$%^&*()-_=+ \\|" {
		if got := Latin(r); got != r {
			t.Errorf("Latin(%q) = %q, want it unchanged", r, got)
		}
	}
}

func TestOf(t *testing.T) {
	runes := func(s string, alt bool) tea.KeyMsg {
		return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(s), Alt: alt}
	}
	cases := []struct {
		msg  tea.KeyMsg
		want string
	}{
		{runes("т", false), "n"},
		{runes("Т", false), "N"},
		{runes("n", false), "n"},
		{runes("у", false), "e"},
		{runes("к", false), "r"},
		{runes("й", false), "q"},
		{runes("П", false), "G"},
		{runes("х", false), "["},
		{runes(",", false), "?"},
		{runes("?", false), "?"},
		{runes("т", true), "alt+n"},
		{runes("1", true), "alt+1"},
		{runes("ш", true), "alt+i"},
		// Several runes at once are text, not a key; a paste never
		// matches a shortcut.
		{runes("тест", false), "тест"},
		{tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("т"), Paste: true}, "[т]"},
		{tea.KeyMsg{Type: tea.KeyEnter}, "enter"},
		{tea.KeyMsg{Type: tea.KeyCtrlN}, "ctrl+n"},
		{tea.KeyMsg{Type: tea.KeySpace, Runes: []rune{' '}}, " "},
		{tea.KeyMsg{Type: tea.KeyEsc, Alt: true}, "alt+esc"},
	}
	for _, c := range cases {
		if got := Of(c.msg); got != c.want {
			t.Errorf("Of(%q) = %q, want %q", c.msg.String(), got, c.want)
		}
	}
	// Of leaves the message itself alone.
	m := runes("т", false)
	Of(m)
	if string(m.Runes) != "т" {
		t.Errorf("Of changed the message to %q", string(m.Runes))
	}
}
