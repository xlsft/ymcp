package wizard

import (
	"context"
	"slices"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

// feedbackStep lists whose messages the agent reacts to: YouTrack logins
// with completion from the users API, GitHub and GitLab logins as text.
type feedbackStep struct {
	sh       *shared
	yt       *assistField
	gh, gl   *assistField
	debounce *assistField
	focus    int
	durErr   string
}

func newFeedbackStep(sh *shared) *feedbackStep {
	s := &feedbackStep{sh: sh}
	s.yt = newAssist(sh, "YouTrack logins", "type a name or login, enter adds it", userSuggest(sh))
	s.gh = newAssist(sh, "GitHub logins", "comma-separated, e.g. octocat, hubot", nil)
	s.gh.set(strings.Join(sh.p.Feedback.GitHub, ", "))
	s.gl = newAssist(sh, "GitLab logins", "comma-separated", nil)
	s.gl.set(strings.Join(sh.p.Feedback.GitLab, ", "))
	s.debounce = newAssist(sh, "Wait for more feedback", "60s", staticSuggest([]string{"30s", "60s", "2m", "5m"}))
	if d := sh.p.Feedback.Debounce.Duration; d > 0 {
		s.debounce.set(fmtDuration(d))
	}
	return s
}

func fmtDuration(d time.Duration) string {
	switch {
	case d%time.Hour == 0:
		return strings.TrimSuffix(d.String(), "0m0s")
	case d%time.Minute == 0:
		return strings.TrimSuffix(d.String(), "0s")
	}
	return d.String()
}

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

func (s *feedbackStep) fields() []*assistField { return []*assistField{s.yt, s.gh, s.gl, s.debounce} }

func (s *feedbackStep) title() string { return "Feedback" }

func (s *feedbackStep) enter() tea.Cmd {
	s.setFocus(s.focus)
	return nil
}

func (s *feedbackStep) leave() {
	for _, f := range s.fields() {
		f.blur()
	}
}

func (s *feedbackStep) setFocus(i int) {
	if s.focus == 0 && i != 0 && s.yt.value() != "" {
		s.addLogin(s.yt.value())
	}
	s.leave()
	s.focus = i
	s.fields()[i].focus()
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

func (s *feedbackStep) addLogin(login string) {
	login = strings.TrimPrefix(strings.TrimSpace(login), "@")
	if login != "" && !slices.Contains(s.sh.p.Feedback.YouTrack, login) {
		s.sh.p.Feedback.YouTrack = append(s.sh.p.Feedback.YouTrack, login)
	}
	s.yt.set("")
}

func (s *feedbackStep) key(msg tea.KeyMsg) (tea.Cmd, bool) {
	if s.focus == 0 {
		switch msg.String() {
		case "enter", "tab":
			if s.yt.showing() && (msg.String() == "tab" || s.yt.cur >= 0) {
				s.addLogin(s.yt.list[max(s.yt.cur, 0)].Option)
				return nil, true
			}
			if msg.String() == "enter" && s.yt.value() != "" {
				s.addLogin(s.yt.value())
				return nil, true
			}
		case "backspace":
			if s.yt.in.Value() == "" && len(s.sh.p.Feedback.YouTrack) > 0 {
				l := s.sh.p.Feedback.YouTrack
				s.sh.p.Feedback.YouTrack = l[:len(l)-1]
				return nil, true
			}
		}
	}
	f := s.fields()[s.focus]
	cmd, consumed, changed := f.key(msg)
	if changed {
		s.store()
	}
	if consumed {
		return cmd, true
	}
	switch msg.String() {
	case "tab", "down", "enter":
		if s.focus == len(s.fields())-1 {
			return tea.Batch(cmd, next), true
		}
		s.setFocus(s.focus + 1)
		return cmd, true
	case "shift+tab", "up":
		s.setFocus(max(s.focus-1, 0))
		return cmd, true
	}
	return cmd, false
}

func (s *feedbackStep) store() {
	fb := &s.sh.p.Feedback
	fb.GitHub = splitLogins(s.gh.value())
	fb.GitLab = splitLogins(s.gl.value())
	s.durErr = ""
	if v := s.debounce.value(); v == "" {
		fb.Debounce.Duration = 0
	} else if d, err := time.ParseDuration(v); err != nil || d < 0 {
		s.durErr = "use a duration such as 30s, 60s or 2m"
	} else {
		fb.Debounce.Duration = d
	}
}

func (s *feedbackStep) msg(msg tea.Msg) tea.Cmd {
	var cmds []tea.Cmd
	for _, f := range s.fields() {
		cmds = append(cmds, f.msg(msg))
	}
	return tea.Batch(cmds...)
}

func (s *feedbackStep) view(width, _ int) string {
	var b strings.Builder
	b.WriteString(wrap("Comments and reviews from these people reach the agent as feedback. Everyone else is ignored, so strangers can't steer the agent.", width) + "\n\n")
	b.WriteString(s.yt.view(width, s.focus == 0) + "\n")
	var logins []string
	if me := s.sh.conn.login; me != "" {
		logins = append(logins, styleBold.Render(me)+styleDim.Render(" (you, always)"))
	}
	for _, l := range s.sh.p.Feedback.YouTrack {
		logins = append(logins, styleCode.Render(l))
	}
	if len(logins) == 0 {
		b.WriteString(styleDim.Render("    only the profile's own account") + "\n")
	} else {
		b.WriteString("    " + strings.Join(logins, styleDim.Render(" · ")) + "\n")
	}
	if s.yt.err != nil && s.focus == 0 {
		b.WriteString("    " + markWarn + styleDim.Render(" user search is unavailable; type logins yourself") + "\n")
	}
	b.WriteString(s.gh.view(width, s.focus == 1) + "\n")
	b.WriteString(s.gl.view(width, s.focus == 2) + "\n")
	b.WriteString(styleDim.Render("    for the github-* and gitlab-mr strategies; the token's own login always counts") + "\n")
	b.WriteString(s.debounce.view(width, s.focus == 3) + "\n")
	if s.durErr != "" {
		b.WriteString("    " + markErr + " " + s.durErr + "\n")
	} else {
		b.WriteString(styleDim.Render("    after the last comment, so several comments make one turn") + "\n")
	}
	return b.String()
}

func (s *feedbackStep) help() string {
	if s.focus == 0 {
		return keyHelp("enter", "add login", "backspace", "remove last", "↑/↓", "choose", "tab", "next field")
	}
	return keyHelp("tab", "next field", "shift+tab", "back")
}

func (s *feedbackStep) mark() string {
	if s.durErr != "" {
		return markErr
	}
	return markOK
}
