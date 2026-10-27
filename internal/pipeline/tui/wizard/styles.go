package wizard

import (
	"strings"

	"github.com/charmbracelet/lipgloss"
)

// The wizard keeps its own small palette (the brand red of cmd/ymcp) so
// it does not depend on the TUI shell's theme.
var (
	brandRed = lipgloss.Color("#C62A22")

	styleTitle  = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("15")).Background(brandRed).Padding(0, 1)
	styleOK     = lipgloss.NewStyle().Foreground(lipgloss.Color("10"))
	styleWarn   = lipgloss.NewStyle().Foreground(lipgloss.Color("11"))
	styleErr    = lipgloss.NewStyle().Foreground(lipgloss.Color("9"))
	styleDim    = lipgloss.NewStyle().Faint(true)
	styleBold   = lipgloss.NewStyle().Bold(true)
	styleLabel  = lipgloss.NewStyle().Bold(true)
	styleFocus  = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("#E5534B"))
	styleSel    = lipgloss.NewStyle().Foreground(lipgloss.Color("15")).Background(brandRed)
	styleKey    = lipgloss.NewStyle().Foreground(lipgloss.Color("14"))
	styleCode   = lipgloss.NewStyle().Foreground(lipgloss.Color("14"))
	styleBorder = lipgloss.NewStyle().Foreground(lipgloss.Color("8"))
)

// Status marks.
var (
	markOK      = styleOK.Render("✓")
	markWarn    = styleWarn.Render("!")
	markErr     = styleErr.Render("✗")
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
	return lipgloss.NewStyle().Width(w).Render(s)
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
