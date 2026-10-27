package views

import (
	"context"
	"fmt"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"

	"github.com/xlsft/ymcp/internal/pipeline/tui/keys"
	"github.com/xlsft/ymcp/internal/pipeline/tui/theme"
)

// logsEvery is how often the logs screen rereads the log.
const logsEvery = 2 * time.Second

// Logs shows the tail of a pipeline's runner log and follows it while
// the view sits at the bottom.
type Logs struct {
	base
	name    string
	loaded  bool
	loading bool
	at      time.Time
	text    string
	err     error
	top     int
	follow  bool
}

type logsLoaded struct {
	to   int64
	text string
	err  error
}

// NewLogs makes the runner log screen of a pipeline.
func NewLogs(d *Deps, pipeline string) *Logs {
	return &Logs{base: newBase(d), name: pipeline, follow: true}
}

// Pipeline is the pipeline whose log the screen shows.
func (m *Logs) Pipeline() string { return m.name }

// Init reads the log.
func (m *Logs) Init() tea.Cmd { return m.reload() }

func (m *Logs) reload() tea.Cmd {
	if m.d.RunnerLog == nil || m.loading {
		return nil
	}
	m.loading, m.at = true, m.d.Now()
	read, id, name := m.d.RunnerLog, m.id, m.name
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), loadTimeout)
		defer cancel()
		out, err := read(ctx, name)
		if err != nil {
			return logsLoaded{to: id, err: daemonErr("read the log of "+name, out, err)}
		}
		return logsLoaded{to: id, text: out}
	}
}

// Update handles keys, ticks and reloads.
func (m *Logs) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	if done, cmd := m.common(msg); done {
		return m, cmd
	}
	switch msg := msg.(type) {
	case logsLoaded:
		if msg.to != m.id {
			return m, nil
		}
		m.loaded, m.loading = true, false
		m.err = msg.err
		if msg.err == nil {
			m.text = msg.text
		}
	case Tick:
		if msg.Now.Sub(m.at) >= logsEvery {
			return m, m.reload()
		}
	case tea.KeyMsg:
		return m, m.key(keys.Of(msg))
	}
	return m, nil
}

func (m *Logs) key(k string) tea.Cmd {
	page := max(m.rows()-1, 1)
	switch k {
	case "q":
		return tea.Quit
	case "esc", "left", "h":
		return navigate(Back{})
	case "?":
		m.fullHelp = !m.fullHelp
	case "up", "k":
		m.scroll(-1)
	case "down", "j":
		m.scroll(1)
	case "pgup", "ctrl+u", "b":
		m.scroll(-page)
	case "pgdown", "ctrl+d", " ":
		m.scroll(page)
	case "home", "g":
		m.top, m.follow = 0, false
	case "end", "G":
		m.follow = true
	case "r":
		return m.reload()
	}
	return nil
}

// lines are the log wrapped to the width, escape sequences removed.
func (m *Logs) lines() []string {
	text := strings.ReplaceAll(ansi.Strip(m.text), "\r", "")
	text = strings.TrimRight(text, "\n")
	if text == "" {
		return nil
	}
	return wrap(text, max(m.width, 1))
}

// rows is how many log lines fit.
func (m *Logs) rows() int {
	return max(m.height-2-1-len(m.helpLines(m.help(), max(m.width, 1))), 1)
}

func (m *Logs) scroll(d int) {
	n, rows := len(m.lines()), m.rows()
	last := max(n-rows, 0)
	if m.follow {
		m.top = last
	}
	m.top = min(max(m.top+d, 0), last)
	m.follow = m.top == last
}

func (m *Logs) help() []binding {
	return []binding{{"↑↓", "scroll"}, {"g/G", "top/end"}, {"r", "reload"}, {"esc", "back"}, {"q", "quit"}}
}

// View renders the log.
func (m *Logs) View() string {
	w := max(m.width, 1)
	lines := m.lines()
	rows := m.rows()
	last := max(len(lines)-rows, 0)
	if m.follow {
		m.top = last
	}
	m.top = min(m.top, last)
	where := theme.Dim.Render("following")
	if !m.follow {
		where = theme.Dim.Render(fmt.Sprintf("lines %d–%d of %d", m.top+1, min(m.top+rows, len(lines)), len(lines)))
	}
	header := []string{spread(title(m.name)+" "+theme.Dim.Render("runner log"), where, w), ""}
	var body []string
	switch {
	case m.d.RunnerLog == nil:
		body = []string{"  The runner log can't be read here; run " + theme.Bold.Render("ymcp pipeline "+m.name+" logs") + "."}
	case !m.loaded:
		body = []string{theme.Dim.Render("  Loading…")}
	case m.err != nil:
		for _, l := range wrap(oneLine(m.err.Error()), max(w-2, 1)) {
			body = append(body, "  "+theme.Dim.Render(l))
		}
	case len(lines) == 0:
		body = []string{theme.Dim.Render("  The log is empty.")}
	default:
		body = lines[m.top:min(m.top+rows, len(lines))]
	}
	return m.frame(header, body, m.help())
}
