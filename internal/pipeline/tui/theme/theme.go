// Package theme holds the colours and styles of the pipeline TUI. The
// palette is the one of cmd/ymcp (the banner's YMCA red, bright green and
// yellow for good and bad news), copied so the TUI does not depend on the
// command package.
package theme

import "github.com/charmbracelet/lipgloss"

// Palette.
const (
	// Brand is YMCA red, as in the banner.
	Brand = lipgloss.Color("#C62A22")
	// Green marks things that are fine: a running runner, a done run.
	Green = lipgloss.Color("10")
	// Yellow marks things that wait for a person.
	Yellow = lipgloss.Color("11")
	// Red marks failures.
	Red = lipgloss.Color("9")
	// White is the text on the brand colour.
	White = lipgloss.Color("15")
	// Black is the text on yellow badges.
	Black = lipgloss.Color("0")
)

// Styles.
var (
	OK    = lipgloss.NewStyle().Foreground(Green).Bold(true)
	Warn  = lipgloss.NewStyle().Foreground(Yellow).Bold(true)
	Err   = lipgloss.NewStyle().Foreground(Red).Bold(true)
	Bold  = lipgloss.NewStyle().Bold(true)
	Dim   = lipgloss.NewStyle().Faint(true)
	Title = lipgloss.NewStyle().Bold(true).Foreground(White).Background(Brand).Padding(0, 1)
	// Accent is the brand colour as text: the selection marker, column
	// rules.
	Accent = lipgloss.NewStyle().Foreground(Brand).Bold(true)
	// Badge is the needs-input flag.
	Badge = lipgloss.NewStyle().Foreground(Black).Background(Yellow).Bold(true).Padding(0, 1)
	// Key is a key name in the help line.
	Key = lipgloss.NewStyle().Bold(true)
	// Header is a column or table header.
	Header = lipgloss.NewStyle().Bold(true).Faint(true)
)
