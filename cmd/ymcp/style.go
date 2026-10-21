package main

import "github.com/charmbracelet/lipgloss"

// YMCA red, as in the banner.
const brandRed = lipgloss.Color("#C62A22")

var (
	styleOK    = lipgloss.NewStyle().Foreground(lipgloss.Color("10")).Bold(true)
	styleWarn  = lipgloss.NewStyle().Foreground(lipgloss.Color("11")).Bold(true)
	styleBold  = lipgloss.NewStyle().Bold(true)
	styleDim   = lipgloss.NewStyle().Faint(true)
	styleTitle = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("15")).Background(brandRed).Padding(0, 1)
)
