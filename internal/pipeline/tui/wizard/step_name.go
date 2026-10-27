package wizard

import (
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/huh"
	"github.com/charmbracelet/lipgloss"

	"github.com/xlsft/ymcp/internal/config"
	"github.com/xlsft/ymcp/internal/pipeline/spec"
)

// nameStep asks for the pipeline name and the YouTrack profile, and signs
// in to the profile as soon as it is picked.
type nameStep struct {
	sh       *shared
	form     *huh.Form
	name     string
	profile  string
	profiles []config.Config
	loadErr  error
	loaded   bool
	deb      debouncer
}

func newNameStep(sh *shared) *nameStep {
	return &nameStep{sh: sh, deb: debouncer{id: sh.newID()}}
}

func (s *nameStep) title() string { return "Name & profile" }

func formTheme() *huh.Theme {
	t := huh.ThemeBase()
	t.Focused.Title = t.Focused.Title.Foreground(lipgloss.Color("#E5534B")).Bold(true)
	t.Focused.SelectedOption = t.Focused.SelectedOption.Foreground(lipgloss.Color("15")).Background(brandRed)
	t.Focused.SelectSelector = t.Focused.SelectSelector.Foreground(lipgloss.Color("#E5534B"))
	t.Focused.Base = t.Focused.Base.BorderForeground(brandRed)
	return t
}

// checkName is the name validation of the form.
func (s *nameStep) checkName(name string) error {
	if err := spec.CheckName(name); err != nil {
		return err
	}
	if name == s.sh.orig {
		return nil
	}
	exists, err := s.sh.deps.Local.PipelineExists(name)
	if err != nil {
		return err
	}
	if exists {
		return fmt.Errorf("a pipeline named %q already exists", name)
	}
	return nil
}

func (s *nameStep) build() tea.Cmd {
	if !s.loaded {
		s.profiles, s.loadErr = s.sh.deps.Local.Profiles()
		s.loaded = true
	}
	s.name, s.profile = s.sh.p.Name, s.sh.p.Profile
	if s.profile == "" && len(s.profiles) > 0 {
		s.profile = s.profiles[0].Name
		for _, c := range s.profiles {
			if c.Name == config.DefaultProfile {
				s.profile = c.Name
			}
		}
	}
	fields := []huh.Field{
		huh.NewInput().
			Title("Pipeline name").
			Description("lowercase letters, digits, - and _; shown in the agent's messages").
			Placeholder("backend").
			Value(&s.name).
			Validate(s.checkName),
	}
	if len(s.profiles) > 0 {
		opts := make([]huh.Option[string], 0, len(s.profiles))
		for _, c := range s.profiles {
			label := c.Title()
			if c.URL != "" {
				label += "  " + c.URL
			}
			if c.Login != "" {
				label += " (" + c.Login + ")"
			}
			opts = append(opts, huh.NewOption(label, c.Name))
		}
		fields = append(fields, huh.NewSelect[string]().
			Title("YouTrack profile").
			Description("the account ymcp works as").
			Options(opts...).
			Value(&s.profile).
			Height(min(len(opts), 8)+3))
	}
	s.form = huh.NewForm(huh.NewGroup(fields...)).
		WithShowHelp(false).
		WithTheme(formTheme()).
		WithWidth(max(s.sh.width-sidebarWidth-4, 40))
	s.form.SubmitCmd = next
	s.form.CancelCmd = nil
	return tea.Batch(s.form.Init(), s.sync())
}

// sync copies the form values into the draft and signs in to a newly
// picked profile after a pause.
func (s *nameStep) sync() tea.Cmd {
	s.sh.p.Name = strings.TrimSpace(s.name)
	if s.profile == s.sh.p.Profile {
		if s.sh.conn.profile != s.profile {
			return s.sh.connect(s.profile)
		}
		return nil
	}
	s.sh.p.Profile = s.profile
	return s.deb.bump(s.sh.delay)
}

func (s *nameStep) enter() tea.Cmd { return s.build() }
func (s *nameStep) leave()         {}

func (s *nameStep) key(msg tea.KeyMsg) (tea.Cmd, bool) {
	switch msg.String() {
	case "esc":
		return nil, false
	case "ctrl+r":
		s.sh.conn = connection{}
		return s.sh.connect(s.sh.p.Profile), true
	}
	if s.form == nil {
		return nil, false
	}
	f, cmd := s.form.Update(msg)
	if ff, ok := f.(*huh.Form); ok {
		s.form = ff
	}
	return tea.Batch(cmd, s.sync()), true
}

func (s *nameStep) msg(msg tea.Msg) tea.Cmd {
	if s.deb.fired(msg) {
		return s.sh.connect(s.sh.p.Profile)
	}
	if _, ok := msg.(tickMsg); ok {
		return nil
	}
	switch msg.(type) {
	case suggestMsg, connMsg:
		return nil
	}
	if s.form == nil {
		return nil
	}
	f, cmd := s.form.Update(msg)
	if ff, ok := f.(*huh.Form); ok {
		s.form = ff
	}
	return cmd
}

func (s *nameStep) view(width, _ int) string {
	var b strings.Builder
	if s.form != nil {
		s.form.WithWidth(width)
		b.WriteString(s.form.View())
	}
	b.WriteString("\n")
	switch {
	case s.loadErr != nil:
		b.WriteString(markErr + " can't read the profiles: " + s.loadErr.Error() + "\n")
	case len(s.profiles) == 0:
		b.WriteString(markErr + " No YouTrack profiles yet. Run " + styleCode.Render("ymcp setup") + " to add one, then come back.\n")
	}
	c := s.sh.conn
	switch {
	case c.profile == "":
	case c.pending:
		b.WriteString(markPending + " signing in to " + c.profile + "…\n")
	case c.yt != nil:
		b.WriteString(markOK + " signed in to " + c.yt.BaseURL() + " as " + styleBold.Render(c.login) + "\n")
	case c.err != nil:
		b.WriteString(markErr + " " + wrap("can't sign in with "+c.profile+": "+c.err.Error(), width-2) + "\n")
		b.WriteString(styleDim.Render("  fix the profile with `ymcp setup`, then press ctrl+r to retry") + "\n")
	}
	if err := spec.CheckName(s.sh.p.Name); s.sh.p.Name != "" && err != nil {
		b.WriteString(markErr + " " + wrap(err.Error(), width-2) + "\n")
	}
	return b.String()
}

func (s *nameStep) help() string {
	return keyHelp("tab", "next field", "↑/↓", "pick profile", "enter", "continue", "ctrl+r", "retry sign-in")
}

func (s *nameStep) mark() string {
	switch {
	case s.sh.conn.err != nil || (s.sh.p.Name != "" && spec.CheckName(s.sh.p.Name) != nil):
		return markErr
	case s.sh.p.Name == "":
		return markOff
	case s.sh.conn.pending:
		return markPending
	case s.sh.conn.yt != nil:
		return markOK
	}
	return markOff
}
