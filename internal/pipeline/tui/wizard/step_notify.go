package wizard

import (
	"slices"
	"strings"

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

type notifyRow struct {
	kind string // provider | event
	name string
}

// notifyStep optionally narrows the machine's notification settings for
// this pipeline: which providers and which events.
type notifyStep struct {
	sh        *shared
	providers []NotifyProvider
	err       error
	loaded    bool
	cur       int
}

func newNotifyStep(sh *shared) *notifyStep { return &notifyStep{sh: sh} }

func (s *notifyStep) title() string { return "Notifications" }

func (s *notifyStep) enter() tea.Cmd {
	if !s.loaded {
		s.loaded = true
		if s.sh.deps.Notify != nil {
			s.providers, s.err = s.sh.deps.Notify.Providers()
		}
	}
	return nil
}

func (s *notifyStep) leave() {}

func (s *notifyStep) rows() []notifyRow {
	var out []notifyRow
	names := map[string]bool{}
	for _, p := range s.providers {
		out = append(out, notifyRow{"provider", p.Name})
		names[p.Name] = true
	}
	if n := s.sh.p.Notifications; n != nil {
		for _, p := range n.Providers {
			if !names[p] {
				out = append(out, notifyRow{"provider", p})
				names[p] = true
			}
		}
	}
	for _, e := range spec.NotificationEvents {
		out = append(out, notifyRow{"event", e})
	}
	return out
}

func (s *notifyStep) list(kind string) *[]string {
	if s.sh.p.Notifications == nil {
		s.sh.p.Notifications = &spec.Notifications{}
	}
	if kind == "provider" {
		return &s.sh.p.Notifications.Providers
	}
	return &s.sh.p.Notifications.Events
}

func (s *notifyStep) selected(r notifyRow) bool {
	n := s.sh.p.Notifications
	if n == nil {
		return false
	}
	if r.kind == "provider" {
		return slices.Contains(n.Providers, r.name)
	}
	return slices.Contains(n.Events, r.name)
}

func (s *notifyStep) toggle(r notifyRow) {
	l := s.list(r.kind)
	if i := slices.Index(*l, r.name); i >= 0 {
		*l = slices.Delete(*l, i, i+1)
	} else {
		*l = append(*l, r.name)
	}
	if n := s.sh.p.Notifications; len(n.Providers) == 0 && len(n.Events) == 0 {
		s.sh.p.Notifications = nil
	}
}

func (s *notifyStep) key(msg tea.KeyMsg) (tea.Cmd, bool) {
	rows := s.rows()
	switch msg.String() {
	case "up", "k", "shift+tab":
		s.cur = max(s.cur-1, 0)
		return nil, true
	case "down", "j", "tab":
		s.cur = min(s.cur+1, len(rows)-1)
		return nil, true
	case " ", "x":
		if s.cur < len(rows) {
			s.toggle(rows[s.cur])
		}
		return nil, true
	case "c":
		s.sh.p.Notifications = nil
		return nil, true
	case "enter":
		return next, true
	}
	return nil, false
}

func (s *notifyStep) msg(tea.Msg) tea.Cmd { return nil }

func (s *notifyStep) view(width, _ int) string {
	var b strings.Builder
	b.WriteString(wrap("Optional. Desktop notifications are set up for the whole machine (ymcp pipeline notify). Here this pipeline can use fewer providers or only some events. Nothing ticked: the machine's settings.", width) + "\n\n")
	if s.err != nil {
		b.WriteString(markWarn + " " + oneLine("can't read the notification settings: "+s.err.Error(), width-2) + "\n\n")
	}
	rows := s.rows()
	lastKind := ""
	for i, r := range rows {
		if r.kind != lastKind {
			if lastKind != "" {
				b.WriteString("\n")
			}
			if r.kind == "provider" {
				b.WriteString(styleLabel.Render("Providers") + styleDim.Render("  none ticked: every provider enabled on this machine") + "\n")
			} else {
				b.WriteString(styleLabel.Render("Events") + styleDim.Render("  none ticked: each provider's own events") + "\n")
			}
			lastKind = r.kind
		}
		box := "[ ]"
		if s.selected(r) {
			box = "[x]"
		}
		about := eventAbout[r.name]
		if r.kind == "provider" {
			about = "not on this machine"
			for _, p := range s.providers {
				if p.Name == r.name {
					about = "off on this machine"
					if p.Enabled {
						about = "on here · " + strings.Join(p.Events, ", ")
					}
				}
			}
		}
		line := box + " " + padRight(r.name, 15) + " " + styleDim.Render(oneLine(about, width-24))
		if i == s.cur {
			line = styleFocus.Render("›") + line
		} else {
			line = " " + line
		}
		b.WriteString(line + "\n")
	}
	if len(s.providers) == 0 && s.err == nil {
		b.WriteString("\n" + styleDim.Render(wrap("No notification providers are known here; the events still narrow whatever the machine sends.", width)) + "\n")
	}
	return b.String()
}

func (s *notifyStep) help() string {
	return keyHelp("↑/↓", "move", "space", "tick", "c", "clear", "enter", "continue")
}

func (s *notifyStep) mark() string {
	if s.sh.p.Notifications == nil {
		return markOK
	}
	for _, p := range s.sh.p.Notifications.Providers {
		known := false
		for _, q := range s.providers {
			if q.Name == p {
				known = true
			}
		}
		if !known && s.loaded {
			return markWarn
		}
	}
	return markOK
}
