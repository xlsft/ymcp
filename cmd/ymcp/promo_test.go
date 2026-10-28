package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/huh"
	"github.com/charmbracelet/lipgloss"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/xlsft/ymcp/internal/clients"
	"github.com/xlsft/ymcp/internal/config"
)

const promoW, promoH = 120, 34

// promoTerm is a terminal: lines printed so far and a form drawn under them.
type promoTerm struct {
	t       *testing.T
	out     string
	printed []string
	list    []string
}

func (p *promoTerm) println(s string) { p.printed = append(p.printed, strings.Split(s, "\n")...) }

func (p *promoTerm) shoot(sec float64, form string) {
	lines := append([]string{}, p.printed...)
	if form != "" {
		lines = append(lines, strings.Split(strings.TrimRight(form, "\n"), "\n")...)
	}
	if len(lines) > promoH {
		lines = lines[len(lines)-promoH:]
	}
	name := fmt.Sprintf("setup-%04d.ans", len(p.list))
	if err := os.WriteFile(filepath.Join(p.out, name), []byte(strings.Join(lines, "\n")), 0o644); err != nil {
		p.t.Fatal(err)
	}
	p.list = append(p.list, fmt.Sprintf("%.4f %s", sec, name))
}

// promoForm runs a huh form synchronously, like the views' driver.
type promoForm struct {
	m tea.Model
}

func newPromoForm(f *huh.Form) *promoForm {
	p := &promoForm{m: f}
	p.send(tea.WindowSizeMsg{Width: promoW, Height: promoH})
	p.run(f.Init(), 0)
	return p
}

func (p *promoForm) run(cmd tea.Cmd, depth int) {
	if cmd == nil || depth > 30 {
		return
	}
	ch := make(chan tea.Msg, 1)
	go func() { ch <- cmd() }()
	var msg tea.Msg
	select {
	case msg = <-ch:
	case <-time.After(60 * time.Millisecond):
		return
	}
	switch msg := msg.(type) {
	case nil, tea.QuitMsg:
	case tea.BatchMsg:
		for _, c := range msg {
			p.run(c, depth+1)
		}
	default:
		var next tea.Cmd
		p.m, next = p.m.Update(msg)
		p.run(next, depth+1)
	}
}

func (p *promoForm) send(msg tea.Msg) {
	var cmd tea.Cmd
	p.m, cmd = p.m.Update(msg)
	p.run(cmd, 0)
}

func (p *promoForm) key(k tea.KeyType) { p.send(tea.KeyMsg{Type: k}) }
func (p *promoForm) char(r rune)      { p.send(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}}) }
func (p *promoForm) view() string     { return p.m.View() }

// TestPromo writes the ymcp setup frames of the promo video to
// $YMCP_PROMO/setup, with setup.txt listing "seconds file": the command
// typed, the real credentials form filled in, the sign-in lines, the real
// client list ticked and the registration lines. Times start at 0.
func TestPromo(t *testing.T) {
	dir := os.Getenv("YMCP_PROMO")
	if dir == "" {
		t.Skip("YMCP_PROMO not set")
	}
	prev := lipgloss.ColorProfile()
	lipgloss.SetColorProfile(0)
	lipgloss.SetHasDarkBackground(true)
	t.Cleanup(func() { lipgloss.SetColorProfile(prev) })
	out := filepath.Join(dir, "setup")
	if err := os.MkdirAll(out, 0o755); err != nil {
		t.Fatal(err)
	}
	term := &promoTerm{t: t, out: out}
	prompt := styleDim.Render("~/code/backend") + " " + styleOK.Render("❯") + " "

	// The command, typed.
	cmd := "ymcp setup"
	for i := 0; i <= len(cmd); i++ {
		term.printed = []string{prompt + cmd[:i]}
		term.shoot(float64(i)*0.022, "")
	}
	term.println("")
	term.println(styleTitle.Render("ymcp") + " " + styleBold.Render("YouTrack MCP setup"))
	term.println("")

	// The credentials form.
	cfg := config.Config{Name: "youtrack"}
	form := newPromoForm(credentialsForm(&cfg))
	sec := 0.27
	term.shoot(sec, form.view())
	typeIn := func(s string, each float64) {
		for _, r := range s {
			form.char(r)
			sec += each
			term.shoot(sec, form.view())
		}
	}
	typeIn("youtrack.example.com", 0.019)
	form.key(tea.KeyEnter)
	sec += 0.03
	typeIn("jane.doe", 0.02)
	form.key(tea.KeyEnter)
	sec += 0.03
	typeIn("correct-horse", 0.012)
	form.key(tea.KeyEnter)
	sec += 0.05
	term.shoot(sec, form.view())

	// Signed in.
	sec += 0.06
	term.shoot(sec, styleDim.Render("⣾  Logging in to https://youtrack.example.com…"))
	term.println(styleOK.Render("✓") + " Logged in to https://youtrack.example.com as " + styleBold.Render("jane.doe") + " (Jane Doe)")
	term.println(styleOK.Render("✓") + " Password saved to the system keychain, settings to ~/.config/ymcp/config.json")
	term.println("")
	sec += 0.12

	// The client list: Claude Code already has it; the rest get ticked.
	var opts []huh.Option[string]
	var names []string
	for _, c := range clients.All() {
		label := c.Name
		if c.ID == "claude-code" {
			label += styleDim.Render("  · added")
		}
		opts = append(opts, huh.NewOption(label, c.ID).Selected(c.ID == "claude-code"))
		names = append(names, c.Name)
	}
	var chosen []string
	list := newPromoForm(huh.NewForm(huh.NewGroup(huh.NewMultiSelect[string]().
		Title("Register " + cfg.Title() + " in").
		Description("space to toggle · enter to confirm · unchecked clients get it removed").
		Options(opts...).
		Value(&chosen))).WithTheme(huh.ThemeBase16()))
	term.shoot(sec, list.view())
	for i := range names {
		if i > 0 {
			list.key(tea.KeyDown)
		}
		if names[i] != "Claude Code" {
			list.send(tea.KeyMsg{Type: tea.KeySpace, Runes: []rune{' '}})
		}
		sec += 0.045
		term.shoot(sec, list.view())
	}
	list.key(tea.KeyEnter)

	// Registered.
	term.println("")
	for _, n := range names {
		term.println(styleOK.Render("✓") + " " + n)
		sec += 0.012
		term.shoot(sec, "")
	}
	term.println(styleDim.Render("\nServer \"youtrack\": /opt/homebrew/bin/ymcp --profile youtrack (stdio)"))
	sec += 0.02
	term.shoot(sec, "")
	if err := os.WriteFile(filepath.Join(out, "setup.txt"), []byte(strings.Join(term.list, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Logf("setup: %d frames, %.2f s", len(term.list), sec)
}
