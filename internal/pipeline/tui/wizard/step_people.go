package wizard

import (
	"context"
	"slices"
	"strconv"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/xlsft/ymcp/internal/pipeline/spec"
)

var eventAbout = map[string]string{
	"needs_input":    "an agent asked a question or a permission",
	"run_failed":     "a run failed",
	"review_ready":   "work is ready for review",
	"run_done":       "a run finished",
	"runner_stopped": "the runner stopped",
}

// peopleStep lists whose messages the agent reacts to (YouTrack logins
// with completion from the users API, GitHub and GitLab logins as text)
// and narrows the machine's notifications for this pipeline.
type peopleStep struct {
	base
	all        []*item
	advanced   bool
	notifyOpen bool

	yt       *loginsCtl
	gh, gl   *assistField
	debounce *assistField
	durErr   string

	providers []NotifyProvider
	notifyErr error
	loaded    bool
}

func newPeopleStep(sh *shared) *peopleStep {
	s := &peopleStep{base: base{sh: sh}}
	s.yt = &loginsCtl{sh: sh, f: newAssist(sh, "type a name or login; enter adds it", userSuggest(sh))}
	s.gh = newAssist(sh, "e.g. octocat, hubot", nil)
	s.gh.set(strings.Join(sh.p.Feedback.GitHub, ", "))
	s.gl = newAssist(sh, "e.g. jdoe, asmith", nil)
	s.gl.set(strings.Join(sh.p.Feedback.GitLab, ", "))
	s.debounce = newAssist(sh, fmtDuration(spec.DefaultDebounce), staticSuggest([]string{"30s", "60s", "2m", "5m"}))
	if d := sh.p.Feedback.Debounce.Duration; d > 0 && d != spec.DefaultDebounce {
		s.debounce.set(fmtDuration(d))
	}
	s.advanced = s.debounce.value() != ""

	store := func() tea.Cmd { s.store(); return nil }
	s.all = []*item{
		{
			id: "youtrack", label: "YouTrack logins",
			descFn: func() string {
				me := "the profile's account"
				if sh.conn.login != "" {
					me = sh.conn.login
				}
				return "People whose YouTrack comments reach the agent; " + me + " always counts."
			},
			ctl:   s.yt,
			below: s.yt.below,
		},
		{
			id: "github", label: "GitHub logins",
			desc: "Comma-separated reviewers on GitHub; the token's own login counts.",
			ctl:  newText(s.gh, store),
			hidden: func() bool {
				return hosting(sh.p.Strategy.Kind) == "gitlab" && len(sh.p.Feedback.GitHub) == 0
			},
		},
		{
			id: "gitlab", label: "GitLab logins",
			desc: "Comma-separated reviewers on GitLab; the token's own login counts.",
			ctl:  newText(s.gl, store),
			hidden: func() bool {
				return hosting(sh.p.Strategy.Kind) == "github" && len(sh.p.Feedback.GitLab) == 0
			},
		},
		{
			id: "autoCompact", inline: true,
			desc: "When a run ends: hide its comments, leave a summary (/compact).",
			ctl: &toggleCtl{
				label: "Compact finished runs",
				get:   func() bool { return sh.p.AutoCompact },
				set:   func(v bool) tea.Cmd { sh.p.AutoCompact = v; return nil },
			},
		},
		{
			id: "advanced", inline: true,
			ctl: &disclosureCtl{label: "Advanced: feedback delay", open: &s.advanced, force: func() bool { return s.durErr != "" }, note: func() string {
				if s.durErr != "" {
					return markErr + styleErr.Render(" a field here is wrong")
				}
				return ""
			}},
		},
		{
			id: "debounce", label: "Wait for more feedback", hidden: func() bool { return !s.advanced && s.durErr == "" },
			desc:    "After the last comment, so several comments make one turn.",
			ctl:     newText(s.debounce, store),
			problem: func() string { return s.durErr },
		},
		{
			id: "notify", inline: true, heading: "Notifications",
			ctl: &disclosureCtl{label: "Narrow them for this pipeline", open: &s.notifyOpen, note: func() string {
				return styleDim.Render(s.notifySummary())
			}},
			desc: "Set up for the whole machine with ymcp pipeline notify.",
		},
		{
			id: "providers", label: "Providers", hidden: func() bool { return !s.notifyOpen },
			desc: "None ticked: every provider that is on for this machine.",
			ctl: &checklistCtl{
				rows: s.providerRows,
				on: func(v string) bool {
					return sh.p.Notifications != nil && slices.Contains(sh.p.Notifications.Providers, v)
				},
				flip: func(v string) { s.toggle(true, v) },
			},
			below: func(width int) string {
				if s.notifyErr != nil {
					return markWarn + " " + oneLine("can't read the notification settings: "+s.notifyErr.Error(), width-2)
				}
				if len(s.providerRows()) == 0 {
					return styleDim.Render(oneLine("No providers are known here; events still narrow what is sent.", width))
				}
				return ""
			},
		},
		{
			id: "events", label: "Events", hidden: func() bool { return !s.notifyOpen },
			desc: "None ticked: each provider's own events.",
			ctl: &checklistCtl{
				rows: func() []option {
					var out []option
					for _, e := range spec.NotificationEvents {
						out = append(out, option{value: e, title: e, desc: eventAbout[e]})
					}
					return out
				},
				on:   func(v string) bool { return sh.p.Notifications != nil && slices.Contains(sh.p.Notifications.Events, v) },
				flip: func(v string) { s.toggle(false, v) },
			},
		},
	}
	s.notifyOpen = sh.p.Notifications != nil
	return s
}

func (s *peopleStep) title() string { return "People & alerts" }
func (s *peopleStep) intro() string {
	return "Who may give the agent feedback, and this pipeline's notifications."
}
func (s *peopleStep) items() []*item { return s.all }

func (s *peopleStep) enter() tea.Cmd {
	if !s.loaded {
		s.loaded = true
		if s.sh.deps.Notify != nil {
			s.providers, s.notifyErr = s.sh.deps.Notify.Providers()
		}
	}
	return nil
}

func (s *peopleStep) leave() {}

// userSuggest completes YouTrack logins from the users API.
func userSuggest(sh *shared) suggestFunc {
	return func(text string, _ int) func(context.Context) ([]Suggestion, error) {
		yt := sh.conn.yt
		q := strings.TrimSpace(text)
		if yt == nil || q == "" {
			return nil
		}
		return func(ctx context.Context) ([]Suggestion, error) {
			users, err := yt.Users(ctx, q)
			if err != nil {
				return nil, err
			}
			out := make([]Suggestion, 0, len(users))
			for _, u := range users {
				out = append(out, Suggestion{Option: u.Login, Description: u.FullName, CompletionEnd: len([]rune(text))})
			}
			return out, nil
		}
	}
}

// splitLogins reads a comma or space separated list.
func splitLogins(s string) []string {
	var out []string
	for _, f := range strings.FieldsFunc(s, func(r rune) bool { return r == ',' || r == ' ' || r == ';' }) {
		f = strings.TrimPrefix(strings.TrimSpace(f), "@")
		if f != "" && !slices.Contains(out, f) {
			out = append(out, f)
		}
	}
	return out
}

func (s *peopleStep) store() {
	fb := &s.sh.p.Feedback
	fb.GitHub = splitLogins(s.gh.value())
	fb.GitLab = splitLogins(s.gl.value())
	s.durErr = ""
	if v := s.debounce.value(); v == "" {
		fb.Debounce.Duration = spec.DefaultDebounce
	} else if d, err := time.ParseDuration(v); err != nil || d < 0 {
		s.durErr = "Use a duration such as 30s, 60s or 2m."
	} else {
		fb.Debounce.Duration = d
	}
}

// notifySummary says what this pipeline's notifications are.
func (s *peopleStep) notifySummary() string {
	n := s.sh.p.Notifications
	if n == nil {
		return "now: the machine's settings"
	}
	parts := []string{"providers " + orDash(strings.Join(n.Providers, ", ")), "events " + orDash(strings.Join(n.Events, ", "))}
	return "now: " + strings.Join(parts, " · ")
}

func (s *peopleStep) providerRows() []option {
	var out []option
	names := map[string]bool{}
	for _, p := range s.providers {
		about := "off on this machine"
		if p.Enabled {
			about = "on here · " + strings.Join(p.Events, ", ")
		}
		out = append(out, option{value: p.Name, title: p.Name, desc: about})
		names[p.Name] = true
	}
	if n := s.sh.p.Notifications; n != nil {
		for _, p := range n.Providers {
			if !names[p] {
				out = append(out, option{value: p, title: p, desc: "not on this machine"})
				names[p] = true
			}
		}
	}
	return out
}

func (s *peopleStep) toggle(provider bool, v string) {
	if s.sh.p.Notifications == nil {
		s.sh.p.Notifications = &spec.Notifications{}
	}
	l := &s.sh.p.Notifications.Events
	if provider {
		l = &s.sh.p.Notifications.Providers
	}
	if i := slices.Index(*l, v); i >= 0 {
		*l = slices.Delete(*l, i, i+1)
	} else {
		*l = append(*l, v)
	}
	if n := s.sh.p.Notifications; len(n.Providers) == 0 && len(n.Events) == 0 {
		s.sh.p.Notifications = nil
	}
}

func (s *peopleStep) msg(msg tea.Msg) tea.Cmd {
	return tea.Batch(s.yt.f.msg(msg), s.gh.msg(msg), s.gl.msg(msg), s.debounce.msg(msg))
}

func (s *peopleStep) blockers() []problem {
	if s.durErr != "" {
		return []problem{{"debounce", "a valid feedback delay"}}
	}
	return nil
}

func (s *peopleStep) mark() string {
	if s.durErr != "" {
		return markBad
	}
	if n := s.sh.p.Notifications; n != nil && s.loaded {
		for _, p := range n.Providers {
			if !slices.ContainsFunc(s.providers, func(q NotifyProvider) bool { return q.Name == p }) {
				return markWarn
			}
		}
	}
	return markOK
}

// loginsCtl edits the YouTrack logins: the chosen ones as chips in the
// box, an input with completions from the users API to add more.
type loginsCtl struct {
	sh *shared
	f  *assistField
}

func (c *loginsCtl) focus() { c.f.focus() }

// blur adds what was typed, so a login isn't lost by tabbing away.
func (c *loginsCtl) blur() {
	if c.f.in.Focused() && c.f.value() != "" {
		c.add(c.f.value())
	}
	c.f.blur()
}

func (c *loginsCtl) add(login string) {
	login = strings.TrimPrefix(strings.TrimSpace(login), "@")
	if login != "" && !slices.Contains(c.sh.p.Feedback.YouTrack, login) {
		c.sh.p.Feedback.YouTrack = append(c.sh.p.Feedback.YouTrack, login)
	}
	c.f.set("")
}

func (c *loginsCtl) help() string {
	if c.f.showing() {
		return keyHelp("↓", "choose", "enter", "add", "esc", "hide")
	}
	return keyHelp("enter", "add login", "backspace", "remove last")
}

func (c *loginsCtl) key(msg tea.KeyMsg) (tea.Cmd, bool) {
	switch msg.String() {
	case "enter":
		if c.f.showing() && c.f.cur >= 0 {
			c.add(c.f.list[c.f.cur].Option)
			return nil, true
		}
		if c.f.value() != "" {
			c.add(c.f.value())
			return nil, true
		}
		return nil, false
	case "backspace":
		if c.f.in.Value() == "" && len(c.sh.p.Feedback.YouTrack) > 0 {
			l := c.sh.p.Feedback.YouTrack
			c.sh.p.Feedback.YouTrack = l[:len(l)-1]
			return nil, true
		}
	}
	cmd, consumed, _ := c.f.key(msg)
	return cmd, consumed
}

func (c *loginsCtl) view(width int, focused bool) string {
	var chips []string
	for _, l := range c.sh.p.Feedback.YouTrack {
		chips = append(chips, styleSel.Render(" "+l+" "))
	}
	c.f.in.Width = max(boxWidth(width)-5, 5)
	content := c.f.in.View()
	if len(chips) > 0 {
		content = oneLineChips(chips, boxWidth(width)-4) + "\n" + content
	}
	out := inputBox(content, width, focused, false)
	if cl := c.f.completions(width, focused); cl != "" {
		out += "\n" + cl
	}
	return out
}

// below says when the user search doesn't work.
func (c *loginsCtl) below(int) string {
	if c.f.err != nil && c.f.in.Focused() {
		return markWarn + styleDim.Render(" user search is unavailable; type logins yourself")
	}
	return ""
}

// oneLineChips joins chips up to w cells, saying how many didn't fit.
func oneLineChips(chips []string, w int) string {
	out, used := "", 0
	for i, c := range chips {
		cw := lenVisible(c) + 1
		if used+cw > w-8 && i < len(chips)-1 {
			return out + styleDim.Render("+"+strconv.Itoa(len(chips)-i)+" more")
		}
		out += c + " "
		used += cw
	}
	return strings.TrimRight(out, " ")
}
