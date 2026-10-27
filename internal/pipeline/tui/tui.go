// Package tui is the terminal UI of `ymcp pipeline`: profiles, then the
// pipelines of a profile, a pipeline's board, run cards, the inbox of
// questions and the pipeline wizard. The screens live in tui/views and
// tui/wizard; this package stacks them, routes messages and drives the
// one-second refresh.
//
// The TUI reads the state database and talks to runners through it
// (answers, cancels) or through the CLI (`run -d`, `stop`); the run card
// also reads the run's YouTrack issue and posts comments on it as the
// profile's user, which the runner picks up like any comment. Closing the
// TUI leaves the runners running.
package tui

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/xlsft/ymcp/internal/pipeline/spec"
	"github.com/xlsft/ymcp/internal/pipeline/store"
	"github.com/xlsft/ymcp/internal/pipeline/tui/theme"
	"github.com/xlsft/ymcp/internal/pipeline/tui/views"
)

// Options configure the app.
type Options struct {
	// Deps are the screens' services; Store is required, the rest default
	// (see Defaults).
	Deps views.Deps
	// Pipeline, when set, opens that pipeline's board first.
	Pipeline string
	// TickEvery is the refresh interval: 0 means a second, negative turns
	// the refresh off (tests send views.Tick themselves).
	TickEvery time.Duration
	// Wizard makes the pipeline wizard; nil uses tui/wizard.
	Wizard func(ctx context.Context, existing *spec.Pipeline) tea.Model
	// Save writes the pipeline the wizard produced; old is the name of the
	// edited pipeline ("" for a new one). Nil uses SavePipeline.
	Save func(ctx context.Context, st *store.Store, old string, p spec.Pipeline) error
}

// App is the root model: a stack of screens, the top one shown.
type App struct {
	ctx   context.Context
	opts  Options
	d     *views.Deps
	stack []tea.Model

	width, height int

	wizardOpen bool
	wizardOrig string // name of the pipeline being edited
	wizardErr  string // why the wizard's pipeline was not saved
}

type tickMsg time.Time

// New makes the app.
func New(ctx context.Context, opts Options) *App {
	d := opts.Deps.WithDefaults()
	if opts.Wizard == nil {
		opts.Wizard = newWizard
	}
	if opts.Save == nil {
		opts.Save = SavePipeline
	}
	a := &App{ctx: ctx, opts: opts, d: &d}
	a.stack = []tea.Model{views.NewProfiles(a.d)}
	// One profile: go straight to its pipelines; back still leads to the
	// profile list.
	if profile, ok := onlyProfile(a.d); ok {
		a.stack = append(a.stack, views.NewPipelines(a.d, profile))
	}
	if opts.Pipeline != "" {
		a.stack = append(a.stack, views.NewBoard(a.d, opts.Pipeline))
	}
	return a
}

// onlyProfile is the single profile that has or can have pipelines.
func onlyProfile(d *views.Deps) (string, bool) {
	names := map[string]bool{}
	if profiles, err := d.Profiles(); err == nil {
		for _, c := range profiles {
			names[c.Name] = true
		}
	}
	if list, err := d.Pipelines(); err == nil {
		for _, p := range list {
			names[p.Profile] = true
		}
	}
	if len(names) != 1 {
		return "", false
	}
	for name := range names {
		return name, true
	}
	return "", false
}

// Run shows the TUI until the user quits or ctx ends.
func Run(ctx context.Context, opts Options) error {
	if opts.Deps.Store == nil {
		return errors.New("tui: no state database")
	}
	p := tea.NewProgram(New(ctx, opts), tea.WithAltScreen(), tea.WithContext(ctx))
	_, err := p.Run()
	if errors.Is(err, tea.ErrProgramKilled) && ctx.Err() != nil {
		return nil
	}
	return err
}

// Init starts every screen of the initial stack and the refresh.
func (a *App) Init() tea.Cmd {
	cmds := []tea.Cmd{a.tick()}
	for _, m := range a.stack {
		cmds = append(cmds, m.Init())
	}
	return tea.Batch(cmds...)
}

func (a *App) tick() tea.Cmd {
	every := a.opts.TickEvery
	if every < 0 {
		return nil
	}
	if every == 0 {
		every = time.Second
	}
	return tea.Tick(every, func(t time.Time) tea.Msg { return tickMsg(t) })
}

func (a *App) top() tea.Model { return a.stack[len(a.stack)-1] }

// Top is the screen on top, for tests.
func (a *App) Top() tea.Model { return a.top() }

// Depth is how many screens are stacked, for tests.
func (a *App) Depth() int { return len(a.stack) }

// toTop sends msg to the screen on top.
func (a *App) toTop(msg tea.Msg) tea.Cmd {
	m, cmd := a.top().Update(msg)
	a.stack[len(a.stack)-1] = m
	return cmd
}

// tickUnder refreshes the screen under a modal, which shows around it.
func (a *App) tickUnder(t views.Tick) tea.Cmd {
	if _, ok := a.top().(views.Overlay); !ok || len(a.stack) < 2 {
		return nil
	}
	i := len(a.stack) - 2
	m, cmd := a.stack[i].Update(t)
	a.stack[i] = m
	return cmd
}

// push puts a screen on top and starts it at the current size.
func (a *App) push(m tea.Model) tea.Cmd {
	a.stack = append(a.stack, m)
	var cmds []tea.Cmd
	if a.width > 0 {
		cmds = append(cmds, a.toTop(tea.WindowSizeMsg{Width: a.width, Height: a.height}))
	}
	return tea.Batch(append(cmds, a.top().Init())...)
}

// pop closes the screen on top and refreshes the one under it.
func (a *App) pop() tea.Cmd {
	if len(a.stack) == 1 {
		return nil
	}
	a.stack = a.stack[:len(a.stack)-1]
	return a.toTop(views.Tick{Now: a.d.Now()})
}

// Update routes a message: keys and ticks to the screen on top,
// navigation to the stack, the rest to every screen (each ignores replies
// meant for another).
func (a *App) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		a.width, a.height = msg.Width, msg.Height
		return a, a.broadcast(msg)
	case tickMsg:
		return a, tea.Batch(a.toTop(views.Tick{Now: time.Time(msg)}), a.tickUnder(views.Tick{Now: time.Time(msg)}), a.tick())
	case views.Tick:
		return a, a.toTop(msg)
	case tea.KeyMsg:
		a.wizardErr = ""
		if msg.String() == "ctrl+c" && !a.wizardOpen { // the wizard asks before it drops a draft
			return a, tea.Quit
		}
		return a, a.toTop(msg)
	case views.Notice:
		return a, a.toTop(msg)
	case views.Back:
		return a, a.pop()
	case views.OpenPipelines:
		return a, a.push(views.NewPipelines(a.d, msg.Profile))
	case views.OpenBoard:
		return a, a.push(views.NewBoard(a.d, msg.Pipeline))
	case views.OpenRun:
		return a, a.push(views.NewRun(a.d, msg.Pipeline, msg.Number, msg.Answer))
	case views.OpenIssue:
		return a, a.push(views.NewIssueCard(a.d, msg.Pipeline, msg.Profile, msg.Issue))
	case views.OpenLogs:
		return a, a.push(views.NewLogs(a.d, msg.Pipeline))
	case views.OpenInbox:
		if _, ok := a.top().(*views.Inbox); ok {
			return a, nil
		}
		return a, a.push(views.NewInbox(a.d))
	case views.OpenWizard:
		a.wizardOpen, a.wizardOrig, a.wizardErr = true, "", ""
		if msg.Existing != nil {
			a.wizardOrig = msg.Existing.Name
		}
		return a, a.push(a.opts.Wizard(a.ctx, msg.Existing))
	}
	if a.wizardOpen {
		if res, ok := wizardResult(msg); ok {
			return a, a.wizardDone(res)
		}
	}
	return a, a.broadcast(msg)
}

func (a *App) broadcast(msg tea.Msg) tea.Cmd {
	cmds := make([]tea.Cmd, 0, len(a.stack))
	for i, m := range a.stack {
		var cmd tea.Cmd
		a.stack[i], cmd = m.Update(msg)
		cmds = append(cmds, cmd)
	}
	return tea.Batch(cmds...)
}

// wizardOutcome is how the wizard ended.
type wizardOutcome struct {
	saved    bool
	pipeline spec.Pipeline
	runNow   bool
}

// wizardDone saves what the wizard produced, closes it and shows the
// pipeline's board, starting its runner when asked.
func (a *App) wizardDone(res wizardOutcome) tea.Cmd {
	if !res.saved {
		a.wizardOpen = false
		return a.pop()
	}
	p, old := res.pipeline, a.wizardOrig
	if err := a.opts.Save(a.ctx, a.d.Store, old, p); err != nil {
		a.wizardErr = "Not saved: " + err.Error()
		return nil // the wizard stays open with the draft
	}
	a.wizardOpen = false
	a.stack = a.stack[:len(a.stack)-1]
	var cmds []tea.Cmd
	if b, ok := a.top().(*views.Board); !ok || b.Pipeline() != p.Name {
		if ok && b.Pipeline() == old { // renamed: the old board is gone
			a.stack = a.stack[:len(a.stack)-1]
		}
		cmds = append(cmds, a.push(views.NewBoard(a.d, p.Name)))
	} else {
		cmds = append(cmds, a.toTop(views.Tick{Now: a.d.Now()}))
	}
	note := "Saved pipeline " + p.Name + "."
	if old != "" && a.live(p.Name) && !res.runNow {
		note += " Its runner keeps the old settings until it restarts (s, then r)."
	}
	if !res.runNow {
		return tea.Batch(append(cmds, a.toTop(views.Notice{Text: note}))...)
	}
	return tea.Batch(append(cmds, a.toTop(views.Notice{Text: note + " Starting its runner…"}), a.start(p.Name, note))...)
}

// live reports whether a pipeline's runner is running.
func (a *App) live(name string) bool {
	_, _, beat, ok, err := a.d.Store.Runner(a.ctx, name)
	return err == nil && ok && a.d.Now().Sub(beat) <= a.d.RunnerStale
}

// start runs a pipeline's runner in the background after the wizard.
func (a *App) start(name, note string) tea.Cmd {
	if a.d.Daemon == nil {
		return nil
	}
	if a.live(name) {
		return func() tea.Msg {
			return views.Notice{Text: note + " Its runner is already running; restart it (s, then r) to apply the changes."}
		}
	}
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(a.ctx, 90*time.Second)
		defer cancel()
		out, err := a.d.Daemon.Start(ctx, name)
		if err != nil {
			return views.Notice{Text: fmt.Sprintf("%s Could not start the runner: %s", note, lastLine(out, err.Error())), Err: true}
		}
		return views.Notice{Text: note + " " + lastLine(out, "Runner started.")}
	}
}

// View shows the screen on top, a modal over the screen under it.
func (a *App) View() string {
	var v string
	if o, ok := a.top().(views.Overlay); ok && len(a.stack) > 1 {
		v = o.Overlay(a.stack[len(a.stack)-2].View())
	} else {
		v = a.top().View()
	}
	if a.wizardErr == "" {
		return v
	}
	// The draft stays on screen; the reason replaces its first line.
	if i := strings.IndexByte(v, '\n'); i >= 0 {
		v = v[i+1:]
	}
	return theme.Err.Render("✗ ") + a.wizardErr + "\n" + v
}

// SavePipeline writes a pipeline from the wizard. An edited pipeline that
// got a new name is renamed first, which the CLI's rules allow only while
// it has no runner and no active runs (runs are filed under the name).
func SavePipeline(ctx context.Context, st *store.Store, old string, p spec.Pipeline) error {
	if old != "" && old != p.Name {
		if _, _, beat, ok, err := st.Runner(ctx, old); err != nil {
			return err
		} else if ok && time.Since(beat) <= time.Minute {
			return fmt.Errorf("pipeline %s is running; stop it before renaming", old)
		}
		active, err := st.Runs(ctx, old, true)
		if err != nil {
			return err
		}
		if len(active) > 0 {
			return fmt.Errorf("pipeline %s has %d active run(s), which stay filed under the old name; finish or cancel them before renaming", old, len(active))
		}
		if err := spec.Rename(old, p.Name); err != nil {
			return err
		}
	}
	return spec.Save(p)
}
