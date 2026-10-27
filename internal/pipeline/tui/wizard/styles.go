package wizard

import (
	"strings"

	"github.com/charmbracelet/lipgloss"
)

// The wizard keeps its own small palette (the brand red of cmd/ymcp) so
// it does not depend on the TUI shell's theme.
var (
	brandRed    = lipgloss.Color("#C62A22")
	accentRed   = lipgloss.Color("#E5534B")
	borderGray  = lipgloss.Color("8")
	errorRed    = lipgloss.Color("9")
	buttonGray  = lipgloss.Color("238")
	buttonWhite = lipgloss.Color("15")

	styleTitle   = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("15")).Background(brandRed).Padding(0, 1)
	styleOK      = lipgloss.NewStyle().Foreground(lipgloss.Color("10"))
	styleWarn    = lipgloss.NewStyle().Foreground(lipgloss.Color("11"))
	styleErr     = lipgloss.NewStyle().Foreground(errorRed)
	styleDim     = lipgloss.NewStyle().Faint(true)
	styleBold    = lipgloss.NewStyle().Bold(true)
	styleLabel   = lipgloss.NewStyle().Bold(true)
	styleFocus   = lipgloss.NewStyle().Bold(true).Foreground(accentRed)
	styleSel     = lipgloss.NewStyle().Foreground(lipgloss.Color("15")).Background(brandRed)
	styleKey     = lipgloss.NewStyle().Foreground(lipgloss.Color("14"))
	styleCode    = lipgloss.NewStyle().Foreground(lipgloss.Color("14"))
	styleBorder  = lipgloss.NewStyle().Foreground(borderGray)
	styleHolder  = lipgloss.NewStyle().Faint(true).Italic(true)
	styleSection = lipgloss.NewStyle().Bold(true).Underline(true)

	styleButton         = lipgloss.NewStyle().Foreground(buttonWhite).Background(buttonGray).Padding(0, 1)
	styleButtonFocus    = lipgloss.NewStyle().Bold(true).Foreground(buttonWhite).Background(brandRed).Padding(0, 1)
	styleButtonDisabled = lipgloss.NewStyle().Faint(true).Padding(0, 1)
)

// Status marks.
var (
	markOK      = styleOK.Render("✓")
	markWarn    = styleWarn.Render("!")
	markErr     = styleErr.Render("✗")
	markBad     = styleErr.Render("!")
	markPending = styleDim.Render("…")
	markOff     = styleDim.Render("○")
)

// fit cuts s to at most h lines of at most w cells each.
func fit(s string, w, h int) string {
	lines := strings.Split(s, "\n")
	if h > 0 && len(lines) > h {
		lines = lines[:h]
	}
	cut := lipgloss.NewStyle().MaxWidth(w)
	for i, l := range lines {
		if lipgloss.Width(l) > w {
			lines[i] = cut.Render(l)
		}
	}
	return strings.Join(lines, "\n")
}

// wrap word-wraps plain text to w cells.
func wrap(s string, w int) string {
	if w < 10 {
		w = 10
	}
	out := lipgloss.NewStyle().Width(w).Render(s)
	// lipgloss pads every line to the width; the padding only gets in
	// the way of styles and width checks.
	lines := strings.Split(out, "\n")
	for i, l := range lines {
		lines[i] = strings.TrimRight(l, " ")
	}
	return strings.Join(lines, "\n")
}

// keyHelp renders "key action" pairs.
func keyHelp(pairs ...string) string {
	var parts []string
	for i := 0; i+1 < len(pairs); i += 2 {
		parts = append(parts, styleKey.Render(pairs[i])+" "+styleDim.Render(pairs[i+1]))
	}
	return strings.Join(parts, "   ")
}

// oneLine collapses whitespace and cuts s to n runes.
func oneLine(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > n {
		return string(r[:max(n-1, 0)]) + "…"
	}
	return s
}

// lenVisible is the width of s on screen.
func lenVisible(s string) int { return lipgloss.Width(s) }

// cut shortens s to n runes, keeping its spacing.
func cut(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:max(n-1, 0)]) + "…"
	}
	return s
}

func padRight(s string, n int) string {
	if w := lenVisible(s); w < n {
		return s + strings.Repeat(" ", n-w)
	}
	return s
}

// dimLines renders every line of s faint (lipgloss would pad them).
func dimLines(s string) string {
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		lines[i] = styleDim.Render(l)
	}
	return strings.Join(lines, "\n")
}

// errLine is an inline error under a field.
func errLine(s string, width int) string {
	lines := strings.Split(wrap(s, width-2), "\n")
	for i, l := range lines {
		prefix := "  "
		if i == 0 {
			prefix = markErr + " "
		}
		lines[i] = prefix + styleErr.Render(l)
	}
	return strings.Join(lines, "\n")
}
