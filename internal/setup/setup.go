// Package setup is the terminal UI of `ymcp setup`: the YouTrack profiles
// in the look of `ymcp pipeline`, each opening the profile wizard of
// tui/wizard (its connection, allowed tools, confirmations and MCP
// clients).
//
// The UI writes nothing itself: saving and removing go through Deps,
// which cmd/ymcp backs with the config store and the client registrations.
package setup

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/xlsft/ymcp/internal/config"
	"github.com/xlsft/ymcp/internal/pipeline/tui/keys"
	"github.com/xlsft/ymcp/internal/pipeline/tui/theme"
	"github.com/xlsft/ymcp/internal/pipeline/tui/wizard"
	"github.com/xlsft/ymcp/internal/tools"
)

// Profile is a stored profile and where it is registered.
type Profile struct {
	Config config.Config // without the password
	// Clients name the MCP clients it is registered in.
	Clients []string
}

// Deps are what the UI reads and changes. Every field is required.
type Deps struct {
	// Profiles lists the stored profiles by key.
	Profiles func() ([]Profile, error)
	// Load reads a profile with its password.
	Load func(key string) (config.Config, error)
	// Clients lists the detected MCP clients as they stand for a
	// profile key ("" for a new profile).
	Clients func(key string) []wizard.Client
	// Save stores a profile from the wizard, moving it from old, and
	// registers it in the chosen clients; lines say what happened.
	Save func(ctx context.Context, old string, p wizard.ProfileDraft) (lines []string, err error)
	// Remove deletes a profile and its client registrations.
	Remove func(ctx context.Context, key string) (lines []string, err error)
	// Wizard is what the profile wizard asks.
	Wizard wizard.ProfileDeps
}

// Options configure the UI.
type Options struct {
	Deps Deps
	// Open is the key of a profile to edit at once; a key no profile has
	// starts a new one with that name.
	Open string
}

// Run shows the UI until the user quits or ctx ends.
func Run(ctx context.Context, opts Options) error {
	p := tea.NewProgram(New(ctx, opts), tea.WithAltScreen(), tea.WithContext(ctx))
	_, err := p.Run()
	if errors.Is(err, tea.ErrProgramKilled) && ctx.Err() != nil {
		return nil
	}
	return err
}

// App is the profiles screen with the profile wizard over it when open.
type App struct {
	ctx  context.Context
	d    Deps
	open string

	width, height int

	loaded   bool
	err      error
	profiles []Profile
	sel      int

	wiz     tea.Model
	wizErr  string // why the wizard's profile was not saved
	busy    string // what runs in the background
	lines   []string
	linesOK bool
	ask     string // a yes/no question
	onYes   func() tea.Cmd
}

// New makes the UI.
func New(ctx context.Context, opts Options) *App {
	return &App{ctx: ctx, d: opts.Deps, open: opts.Open, width: 80, height: 24}
}

type loadedMsg struct {
	profiles []Profile
	err      error
}

type doneMsg struct {
	key   string // the profile to select
	lines []string
	err   error
	saved bool
}

func (a *App) load() tea.Msg {
	list, err := a.d.Profiles()
	return loadedMsg{profiles: list, err: err}
}

// Init loads the profiles.
func (a *App) Init() tea.Cmd { return a.load }

// Wizard is the open profile wizard, or nil; for tests.
func (a *App) Wizard() tea.Model { return a.wiz }

// Selected is the key of the profile under the cursor, or "".
func (a *App) Selected() string {
	if a.sel < len(a.profiles) {
		return a.profiles[a.sel].Config.Name
	}
	return ""
}

func (a *App) keys() []string {
	out := make([]string, 0, len(a.profiles))
	for _, p := range a.profiles {
		out = append(out, p.Config.Name)
	}
	return out
}

// edit opens the wizard on a profile; "" makes a new one, and so does a
// key no profile has, named after it.
func (a *App) edit(key string) tea.Cmd {
	var existing *config.Config
	taken := a.keys()
	if slices.Contains(taken, key) {
		c, err := a.d.Load(key)
		if err != nil {
			a.say([]string{err.Error()}, false)
			return nil
		}
		existing = &c
		taken = slices.DeleteFunc(taken, func(k string) bool { return k == key })
	} else if key != "" {
		existing = &config.Config{DisplayName: key, URL: "https://"}
		key = ""
	}
	a.wiz = wizard.NewProfile(a.ctx, a.d.Wizard, existing, taken, a.d.Clients(key))
	a.wizErr = ""
	m, cmd := a.wiz.Update(tea.WindowSizeMsg{Width: a.width, Height: a.height})
	a.wiz = m
	return tea.Batch(cmd, a.wiz.Init())
}

func (a *App) say(lines []string, ok bool) { a.lines, a.linesOK = lines, ok }

// Update routes keys to the wizard while it is open, and handles the
// screen's own.
func (a *App) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		a.width, a.height = msg.Width, msg.Height
	case loadedMsg:
		first := !a.loaded
		a.loaded, a.err, a.profiles = true, msg.err, msg.profiles
		a.sel = min(a.sel, max(len(a.profiles)-1, 0))
		if first && a.wiz == nil && (a.open != "" || len(a.profiles) == 0) {
			if i := slices.Index(a.keys(), a.open); i >= 0 {
				a.sel = i
			}
			return a, a.edit(a.open)
		}
		return a, nil
	case doneMsg:
		a.busy = ""
		if msg.err != nil && msg.saved {
			a.wizErr = "Not saved: " + msg.err.Error()
			return a, nil
		}
		if msg.saved {
			a.wiz = nil
		}
		lines := msg.lines
		if msg.err != nil {
			lines = append(lines, msg.err.Error())
		}
		a.say(lines, msg.err == nil)
		return a, tea.Sequence(a.load, func() tea.Msg { return selectMsg(msg.key) })
	case selectMsg:
		if i := slices.Index(a.keys(), string(msg)); i >= 0 {
			a.sel = i
		}
		return a, nil
	case wizard.ProfileDone:
		a.busy = "Saving " + msg.Profile.Config.Title() + "…"
		a.wizErr = ""
		return a, a.save(msg)
	case wizard.Cancelled:
		a.wiz = nil
		return a, nil
	}
	if a.wiz != nil {
		if k, ok := msg.(tea.KeyMsg); ok {
			a.wizErr = ""
			if a.busy != "" && k.String() != "ctrl+c" {
				return a, nil // saving: the draft stays as it is
			}
		}
		var cmd tea.Cmd
		a.wiz, cmd = a.wiz.Update(msg)
		return a, cmd
	}
	if k, ok := msg.(tea.KeyMsg); ok {
		return a, a.key(k)
	}
	return a, nil
}

type selectMsg string

func (a *App) save(done wizard.ProfileDone) tea.Cmd {
	ctx, save := a.ctx, a.d.Save
	return func() tea.Msg {
		lines, err := save(ctx, done.Old, done.Profile)
		return doneMsg{key: done.Profile.Config.Name, lines: lines, err: err, saved: true}
	}
}

func (a *App) key(msg tea.KeyMsg) tea.Cmd {
	k := keys.Of(msg)
	if k == "ctrl+c" {
		return tea.Quit
	}
	if a.onYes != nil {
		yes := a.onYes
		a.ask, a.onYes = "", nil
		if k == "y" || k == "Y" {
			return yes()
		}
		a.say([]string{"Nothing removed."}, true)
		return nil
	}
	if a.busy != "" {
		return nil
	}
	switch k {
	case "q", "esc":
		return tea.Quit
	case "up", "k":
		a.sel = max(a.sel-1, 0)
	case "down", "j":
		a.sel = min(a.sel+1, max(len(a.profiles)-1, 0))
	case "home", "g":
		a.sel = 0
	case "end", "G":
		a.sel = max(len(a.profiles)-1, 0)
	case "enter", "right", "l", "e":
		if key := a.Selected(); key != "" {
			return a.edit(key)
		}
	case "n", "a":
		return a.edit("")
	case "d", "delete", "backspace":
		key := a.Selected()
		if key == "" {
			return nil
		}
		a.ask = "Remove profile " + key + " and its client registrations?"
		a.onYes = func() tea.Cmd {
			a.busy = "Removing " + key + "…"
			ctx, remove := a.ctx, a.d.Remove
			return func() tea.Msg {
				lines, err := remove(ctx, key)
				return doneMsg{lines: lines, err: err}
			}
		}
	}
	return nil
}

// View shows the wizard when open, else the profiles.
func (a *App) View() string {
	if a.wiz != nil {
		v := a.wiz.View()
		top := ""
		switch {
		case a.busy != "":
			top = theme.Dim.Render("… ") + a.busy
		case a.wizErr != "":
			top = theme.Err.Render("✗ ") + a.wizErr
		default:
			return v
		}
		// The draft stays on screen; the news replaces its first line.
		if i := strings.IndexByte(v, '\n'); i >= 0 {
			v = v[i+1:]
		}
		return cut(top, a.width) + "\n" + v
	}
	return a.list()
}

func (a *App) list() string {
	w, h := max(a.width, 20), max(a.height, 6)
	header := []string{theme.Title.Render("ymcp setup") + " " + theme.Bold.Render("YouTrack profiles"), ""}
	var body []string
	starts := make([]int, len(a.profiles))
	switch {
	case !a.loaded:
		body = append(body, theme.Dim.Render("  Loading…"))
	case len(a.profiles) == 0:
		body = append(body, "  No YouTrack profiles yet.", "", "  Press "+theme.Bold.Render("n")+" to add one: just your login and password, nothing to set up in YouTrack.")
	}
	total := len(tools.Catalog())
	for i, p := range a.profiles {
		starts[i] = len(body)
		c, sel := p.Config, i == a.sel
		name := theme.Bold.Render(c.Title())
		marker := "  "
		if sel {
			name, marker = theme.Accent.Render(c.Title()), theme.Accent.Render("› ")
		}
		left := marker + name
		if c.DisplayName != "" && c.DisplayName != c.Name {
			left += theme.Dim.Render(" (" + c.Name + ")")
		}
		right := strings.TrimPrefix(strings.TrimPrefix(c.URL, "https://"), "http://")
		body = append(body, spread(left, theme.Dim.Render(right), w))

		toolsNote := fmt.Sprintf("all %d tools", total)
		if enabled, err := tools.Select(c.Tools); err != nil {
			toolsNote = theme.Err.Render(err.Error())
		} else if enabled != nil {
			toolsNote = fmt.Sprintf("%d of %d tools", len(enabled), total)
		}
		confirm := theme.OK.Render("asks before destructive operations")
		if !c.Confirms() {
			confirm = theme.Warn.Render("runs destructive operations at once")
		}
		body = append(body, "      "+cut(c.Login+theme.Dim.Render(" · ")+toolsNote+theme.Dim.Render(" · ")+confirm, w-6))
		where := theme.Warn.Render("not registered in any client")
		if len(p.Clients) > 0 {
			where = theme.Dim.Render("registered in " + strings.Join(p.Clients, ", "))
		}
		body = append(body, "      "+cut(where, w-6), "")
	}
	if a.err != nil {
		body = append(body, theme.Err.Render("  ! ")+a.err.Error())
	}

	var foot []string
	switch {
	case a.ask != "":
		foot = append(foot, theme.Warn.Render(a.ask)+"  "+help("y", "yes", "n", "no"))
	case a.busy != "":
		foot = append(foot, theme.Dim.Render("… ")+a.busy)
	default:
		for _, l := range a.lines {
			if !a.linesOK {
				l = theme.Err.Render("! ") + l
			}
			foot = append(foot, "  "+l)
		}
	}
	foot = append(foot, help("↑↓", "move", "enter", "edit", "n", "new profile", "d", "remove", "q", "quit"))

	room := max(h-len(header)-len(foot), 1)
	top := 0
	if a.sel < len(starts) {
		end := len(body)
		if a.sel+1 < len(starts) {
			end = starts[a.sel+1]
		}
		if end > room {
			top = min(end-room, starts[a.sel])
		}
	}
	body = body[min(top, len(body)):]
	if len(body) > room {
		body = body[:room]
	}
	for len(body) < room {
		body = append(body, "")
	}
	lines := append(append(header, body...), foot...)
	for i, l := range lines {
		lines[i] = cut(l, w)
	}
	return strings.Join(lines, "\n")
}

// spread puts right at the right edge of a w-wide line after left.
func spread(left, right string, w int) string {
	gap := w - lipgloss.Width(left) - lipgloss.Width(right)
	if gap < 2 {
		return left
	}
	return left + strings.Repeat(" ", gap) + right
}

// cut shortens a line to w cells.
func cut(s string, w int) string {
	if lipgloss.Width(s) <= w {
		return s
	}
	return lipgloss.NewStyle().MaxWidth(w).Render(s)
}

// help renders "key action" pairs.
func help(pairs ...string) string {
	var parts []string
	for i := 0; i+1 < len(pairs); i += 2 {
		parts = append(parts, theme.Key.Render(pairs[i])+" "+theme.Dim.Render(pairs[i+1]))
	}
	return strings.Join(parts, "   ")
}
