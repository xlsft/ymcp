// Package keys makes the TUI's single-letter shortcuts work whatever the
// keyboard layout. bubbletea v1 reports the character a key typed, not
// the physical key, so with a Russian layout the N key arrives as "т" and
// "n new" does nothing. Of translates such a character back to the Latin
// key at the same place on a US QWERTY keyboard, so shortcut switches can
// keep matching Latin keys.
//
// Use Of only where a key is matched as a shortcut. Text typed into an
// input (a reply, a form field, a filter) must reach it untranslated.
package keys

import tea "github.com/charmbracelet/bubbletea"

// latin maps characters of the Cyrillic PC layouts (Russian ЙЦУКЕН,
// Ukrainian, Belarusian) to the QWERTY character on the same key, shift
// included.
var latin = map[rune]rune{
	// Russian, unshifted.
	'й': 'q', 'ц': 'w', 'у': 'e', 'к': 'r', 'е': 't', 'н': 'y', 'г': 'u', 'ш': 'i', 'щ': 'o', 'з': 'p', 'х': '[', 'ъ': ']',
	'ф': 'a', 'ы': 's', 'в': 'd', 'а': 'f', 'п': 'g', 'р': 'h', 'о': 'j', 'л': 'k', 'д': 'l', 'ж': ';', 'э': '\'',
	'я': 'z', 'ч': 'x', 'с': 'c', 'м': 'v', 'и': 'b', 'т': 'n', 'ь': 'm', 'б': ',', 'ю': '.',
	'ё': '`',
	// Russian, shifted.
	'Й': 'Q', 'Ц': 'W', 'У': 'E', 'К': 'R', 'Е': 'T', 'Н': 'Y', 'Г': 'U', 'Ш': 'I', 'Щ': 'O', 'З': 'P', 'Х': '{', 'Ъ': '}',
	'Ф': 'A', 'Ы': 'S', 'В': 'D', 'А': 'F', 'П': 'G', 'Р': 'H', 'О': 'J', 'Л': 'K', 'Д': 'L', 'Ж': ':', 'Э': '"',
	'Я': 'Z', 'Ч': 'X', 'С': 'C', 'М': 'V', 'И': 'B', 'Т': 'N', 'Ь': 'M', 'Б': '<', 'Ю': '>',
	'Ё': '~',
	'№': '#', // shift+3
	// Ukrainian: і on S, ї on ], є on ', ґ on \ (the Enhanced layout).
	'і': 's', 'І': 'S', 'ї': ']', 'Ї': '}', 'є': '\'', 'Є': '"', 'ґ': '\\', 'Ґ': '|',
	// Belarusian: і on S (above), ў on O.
	'ў': 'o', 'Ў': 'O',

	// The Russian layout also moves some ASCII punctuation: the key right
	// of Ю types "." and, shifted, ","; shift+7 types "?". Those
	// characters exist on the Latin layout too, so they can't be told
	// apart in general. Only "," is translated: it is what the "?" key
	// types there, and no screen binds "," itself. "?" typed with shift+7
	// already matches as is.
	',': '?',
}

// Latin returns the QWERTY character on the key that typed r with a
// Cyrillic layout, or r when it has no such mapping.
func Latin(r rune) rune {
	if l, ok := latin[r]; ok {
		return l
	}
	return r
}

// Of is msg.String() with a typed character, alt+ ones too, translated to
// the Latin key at the same position: "т" gives "n", "alt+т" gives "alt+n",
// "Т" gives "N". Named keys, ctrl combinations and pastes are unchanged.
func Of(msg tea.KeyMsg) string {
	if msg.Type != tea.KeyRunes || msg.Paste || len(msg.Runes) != 1 {
		return msg.String()
	}
	r := Latin(msg.Runes[0])
	if r == msg.Runes[0] {
		return msg.String()
	}
	k := msg
	k.Runes = []rune{r}
	return k.String()
}
