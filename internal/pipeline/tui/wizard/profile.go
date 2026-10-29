package wizard

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"slices"
	"strings"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"

	"github.com/xlsft/ymcp/internal/config"
	"github.com/xlsft/ymcp/internal/pipeline/spec"
	"github.com/xlsft/ymcp/internal/tools"
	"github.com/xlsft/ymcp/internal/youtrack"
)

// The profile wizard edits a YouTrack profile of ymcp with the steps,
// fields and keys of the pipeline wizard: its connection, the tools it
// serves and whether destructive ones ask first, and the MCP clients it
// is registered in. Like the pipeline wizard it writes nothing: it ends
// with ProfileDone or Cancelled.

// ProfileDone is sent when the user saves the profile. Old is the key the
// profile had ("" for a new one).
type ProfileDone struct {
	Old     string
	Profile ProfileDraft
}

// ProfileDraft is a profile as the wizard leaves it.
type ProfileDraft struct {
	// Config is the profile, password included; its Name is the key
	// made from the display name.
	Config config.Config
	// Clients are the IDs of the clients to register it in; the others
	// lose it.
	Clients []string
	// Quit lets the saver quit and reopen running clients that would
	// drop the change (see Client.Running).
	Quit bool
}

// Client is an MCP client the profile can be registered in.
type Client struct {
	ID, Name string
	// Note is what to know about it, e.g. that it needs a restart.
	Note string
	// Registered: the profile is registered in it now.
	Registered bool
	// Running: its app is running and writes its config back from
	// memory, dropping ymcp's change unless it quits first.
	Running bool
	// Inside: ymcp runs inside its app, which therefore can't quit.
	Inside bool
}

// ProfileDeps are what the profile wizard asks outside.
type ProfileDeps struct {
	// SignIn signs in with a connection and returns the login.
	SignIn func(ctx context.Context, c config.Config) (string, error)
	// Command is how clients start ymcp for a profile key.
	Command func(key string) string
}

// NewProfile returns the wizard for a new profile (existing nil, or
// without a Name to start from its values) or for editing existing,
// password included. taken are the keys of the other profiles; clients
// are the detected MCP clients.
func NewProfile(ctx context.Context, deps ProfileDeps, existing *config.Config, taken []string, clients []Client) tea.Model {
	sh := &shared{ctx: ctx, delay: debounceDelay, width: 100, height: 30, p: &spec.Pipeline{}}
	ps := &profileState{sh: sh, deps: deps, taken: taken, clients: clients, signDeb: debouncer{id: sh.newID()}}
	if existing != nil {
		ps.cfg = *existing
		ps.orig = existing.Name
	} else {
		ps.cfg = config.Config{DisplayName: config.DefaultProfile, URL: "https://"}
		if slices.Contains(taken, config.DefaultProfile) {
			ps.cfg.DisplayName = ""
		}
	}
	if ps.cfg.DisplayName == "" {
		ps.cfg.DisplayName = ps.cfg.Name
	}
	ps.cfg.Tools = slices.Clone(ps.cfg.Tools)
	ps.enabled, _ = tools.Select(ps.cfg.Tools)
	if ps.enabled == nil {
		ps.enabled = map[string]bool{}
		for _, it := range tools.Catalog() {
			ps.enabled[it.Name] = true
		}
	}
	anyRegistered := slices.ContainsFunc(clients, func(c Client) bool { return c.Registered })
	for _, c := range clients {
		// A new profile goes everywhere; an existing one stays put.
		if c.Registered || (!anyRegistered && ps.orig == "") {
			ps.chosen = append(ps.chosen, c.ID)
		}
	}
	ps.quit = true

	m := &Model{sh: sh, kind: ps}
	m.steps = []step{newConnectionStep(ps), newToolsStep(ps), newClientsStep(ps)}
	m.seen = make([]bool, len(m.steps))
	m.seen[0] = true
	if ps.orig != "" {
		for _, s := range m.steps {
			s.form().tried = true
		}
	}
	m.initial = m.snapshot()
	return &profileModel{Model: m, ps: ps}
}

// profileModel is the profile wizard: the wizard model, signing in as
// the connection changes.
type profileModel struct {
	*Model
	ps *profileState
}

// FocusID is the focused field or button, for tests.
func (m *profileModel) FocusID() string { return m.focusID() }

// Init starts the first step and signs in with a saved connection.
func (m *profileModel) Init() tea.Cmd {
	return tea.Batch(m.Model.Init(), m.ps.signIn())
}

func (m *profileModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	if m.ps.signDeb.fired(msg) {
		return m, m.ps.signIn()
	}
	if s, ok := msg.(signInMsg); ok {
		if s.key != m.ps.connKey() {
			return m, nil
		}
		m.ps.sign = signState{key: s.key, login: s.login, err: s.err}
		if m.ps.saving {
			// Save was pressed while signing in.
			m.ps.saving = false
			return m, m.save(false)
		}
		return m, nil
	}
	if _, ok := msg.(tea.KeyMsg); ok {
		m.ps.saving = false
	}
	_, cmd := m.Model.Update(msg)
	return m, cmd
}

// profileState is the draft profile the steps share.
type profileState struct {
	sh      *shared
	deps    ProfileDeps
	cfg     config.Config
	orig    string
	taken   []string
	enabled map[string]bool
	clients []Client
	chosen  []string
	quit    bool

	sign    signState
	signDeb debouncer
	saving  bool // Save waits for signing in
}

// signState is the result of signing in with a connection.
type signState struct {
	key     string // connKey of the connection
	login   string
	err     error
	pending bool
}

type signInMsg struct {
	key   string
	login string
	err   error
}

// connKey identifies the connection the draft has now.
func (ps *profileState) connKey() string {
	return strings.Join([]string{youtrack.NormalizeURL(ps.cfg.URL), ps.cfg.Login, ps.cfg.Password, ps.cfg.Proxy}, "\x00")
}

// connReady reports whether the connection fields are filled in well
// enough to sign in.
func (ps *profileState) connReady() bool {
	return urlProblem(ps.cfg.URL) == "" && strings.TrimSpace(ps.cfg.Login) != "" && ps.cfg.Password != ""
}

// signIn signs in with the current connection, unless that is done or
// under way.
func (ps *profileState) signIn() tea.Cmd {
	key := ps.connKey()
	if !ps.connReady() || ps.deps.SignIn == nil || ps.sign.key == key {
		return nil
	}
	ps.sign = signState{key: key, pending: true}
	c := ps.cfg
	c.URL = youtrack.NormalizeURL(c.URL)
	ctx, f := ps.sh.ctx, ps.deps.SignIn
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(ctx, requestTimeout)
		defer cancel()
		login, err := f(ctx, c)
		return signInMsg{key: key, login: login, err: err}
	}
}

// connChanged signs in again once typing pauses.
func (ps *profileState) connChanged() tea.Cmd {
	if ps.sign.key != ps.connKey() {
		ps.sign = signState{}
	}
	return ps.signDeb.bump(ps.sh.delay)
}

// signedIn reports whether the current connection signed in.
func (ps *profileState) signedIn() bool {
	return ps.sign.key == ps.connKey() && !ps.sign.pending && ps.sign.err == nil && ps.sign.login != ""
}

func (ps *profileState) key() string { return config.Slug(ps.cfg.DisplayName) }

func (ps *profileState) isNew() bool { return ps.orig == "" }

// buttons: Save is on every step, so a change is one key away.
func (ps *profileState) buttons(cur, n int) []string {
	var out []string
	if cur > 0 {
		out = append(out, btnBack)
	}
	if cur < n-1 {
		out = append(out, btnNext)
	}
	return append(out, btnSave)
}

func (ps *profileState) snapshot() string {
	data, _ := json.Marshal(struct {
		Cfg     config.Config
		Enabled map[string]bool
		Chosen  []string
		Quit    bool
	}{ps.cfg, ps.enabled, ps.chosen, ps.quit})
	return string(data)
}

func (ps *profileState) save(bool) (tea.Cmd, string) {
	var texts []string
	for _, p := range ps.problems() {
		texts = append(texts, p.text)
	}
	switch {
	case len(texts) > 0:
		return nil, "needs: " + strings.Join(texts, "; ")
	case ps.sign.pending || ps.sign.key != ps.connKey():
		ps.saving = true
		return ps.signIn(), "signing in first…"
	case ps.sign.err != nil:
		return nil, "can't sign in with this connection: " + ps.sign.err.Error()
	}
	c := ps.cfg
	c.Name = ps.key()
	c.DisplayName = strings.TrimSpace(c.DisplayName)
	c.URL = youtrack.NormalizeURL(c.URL)
	c.Login = strings.TrimSpace(c.Login)
	c.Proxy = strings.TrimSpace(c.Proxy)
	c.Tools = tools.Compact(ps.enabled)
	d := ProfileDraft{Config: c, Clients: slices.Clone(ps.chosen), Quit: ps.quit}
	old := ps.orig
	return func() tea.Msg { return ProfileDone{Old: old, Profile: d} }, ""
}

// problems are what keeps the profile from being saved, in step order.
func (ps *profileState) problems() []problem {
	var out []problem
	if p := ps.nameProblem(); p != "" {
		out = append(out, problem{"name", p})
	}
	if p := urlProblem(ps.cfg.URL); p != "" {
		out = append(out, problem{"url", p})
	}
	if strings.TrimSpace(ps.cfg.Login) == "" {
		out = append(out, problem{"login", "the login"})
	}
	if ps.cfg.Password == "" {
		out = append(out, problem{"password", "the password"})
	}
	if ps.count() == 0 {
		out = append(out, problem{"tools", "at least one tool"})
	}
	return out
}

func (ps *profileState) nameProblem() string {
	if strings.TrimSpace(ps.cfg.DisplayName) == "" {
		return "a name"
	}
	if key := ps.key(); key != ps.orig && slices.Contains(ps.taken, key) {
		return "another name: a profile " + key + " already exists"
	}
	return ""
}

func urlProblem(s string) string {
	u, err := url.Parse(youtrack.NormalizeURL(strings.TrimSpace(s)))
	if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") {
		return "a full URL, e.g. https://youtrack.example.com"
	}
	return ""
}

// count is how many tools are on.
func (ps *profileState) count() int {
	n := 0
	for _, on := range ps.enabled {
		if on {
			n++
		}
	}
	return n
}

func (ps *profileState) header() string {
	title := "New profile"
	if ps.orig != "" {
		title = "Edit profile " + ps.orig
	}
	head := styleTitle.Render(title)
	if name := strings.TrimSpace(ps.cfg.DisplayName); name != "" && ps.key() != ps.orig {
		head += " " + styleDim.Render(name+" · "+ps.key())
	}
	switch s := ps.sign; {
	case s.key != ps.connKey():
	case s.pending:
		head += "  " + markPending + styleDim.Render(" signing in")
	case s.err != nil:
		head += "  " + markErr + styleDim.Render(" not signed in")
	default:
		head += "  " + markOK + styleDim.Render(" "+s.login+" @ "+youtrack.NormalizeURL(ps.cfg.URL))
	}
	return head
}

// stepProblems keeps the problems of the fields in ids.
func (ps *profileState) stepProblems(ids ...string) []problem {
	var out []problem
	for _, p := range ps.problems() {
		if slices.Contains(ids, p.id) {
			out = append(out, p)
		}
	}
	return out
}

// mark is a step's status from its problems.
func mark(probs []problem) string {
	if len(probs) > 0 {
		return markBad
	}
	return markOK
}

// connectionStep asks for the profile's name and its YouTrack sign-in,
// and signs in as soon as everything is filled in.
type connectionStep struct {
	base
	ps  *profileState
	all []*item
}

func newConnectionStep(ps *profileState) *connectionStep {
	s := &connectionStep{base: base{sh: ps.sh}, ps: ps}
	sh := ps.sh
	text := func(placeholder, value string, set func(string)) *textCtl {
		f := newAssist(sh, placeholder, nil)
		f.set(value)
		return newText(f, func() tea.Cmd {
			set(f.in.Value())
			return ps.connChanged()
		})
	}
	required := func(id string) func() string {
		return func() string {
			for _, p := range ps.stepProblems(id) {
				if s.fs.tried || (id == "name" && strings.TrimSpace(ps.cfg.DisplayName) != "") {
					return "Needs " + p.text + "."
				}
			}
			return ""
		}
	}
	password := text("", ps.cfg.Password, func(v string) { ps.cfg.Password = v })
	password.f.in.EchoMode = textinput.EchoPassword
	password.f.in.EchoCharacter = '•'
	pass := &item{
		id: "password", label: "Password", inRow: true,
		desc:    "Kept in the system keychain.",
		ctl:     password,
		problem: required("password"),
	}
	s.all = []*item{
		{
			id: "name", label: "Profile name",
			descFn: func() string {
				if strings.TrimSpace(ps.cfg.DisplayName) == "" {
					return "MCP clients list this YouTrack under a key made from it."
				}
				return "MCP clients list this YouTrack as " + ps.key() + "."
			},
			ctl:     text("e.g. YouTrack (work)", ps.cfg.DisplayName, func(v string) { ps.cfg.DisplayName = v }),
			problem: required("name"),
		},
		{
			id: "url", label: "YouTrack URL", heading: "Connection",
			headNote: "just your login and password: nothing to set up in YouTrack",
			desc:     "The address you open YouTrack at.",
			ctl:      text("https://youtrack.example.com", ps.cfg.URL, func(v string) { ps.cfg.URL = v }),
			problem: func() string {
				if p := urlProblem(ps.cfg.URL); p != "" && (s.fs.tried || len(strings.TrimSpace(ps.cfg.URL)) > len("https://")) {
					return "Needs " + p + "."
				}
				return ""
			},
		},
		{
			id: "login", label: "Login",
			desc:    "Your login or email.",
			ctl:     text("e.g. jdoe", ps.cfg.Login, func(v string) { ps.cfg.Login = v }),
			problem: required("login"),
			right:   pass,
		},
		pass,
		{
			id: "signin", below: s.signInView,
			hidden: func() bool { return !ps.connReady() },
		},
		{
			id: "proxy", label: "Proxy",
			desc: `Empty: HTTP(S)_PROXY, or direct if it can't reach YouTrack · "none": always direct · or a proxy URL.`,
			ctl:  text("", ps.cfg.Proxy, func(v string) { ps.cfg.Proxy = v }),
		},
	}
	return s
}

func (s *connectionStep) title() string { return "Connection" }
func (s *connectionStep) intro() string {
	return "Name the profile and sign in to its YouTrack."
}
func (s *connectionStep) items() []*item      { return s.all }
func (s *connectionStep) enter() tea.Cmd      { return nil }
func (s *connectionStep) leave()              {}
func (s *connectionStep) msg(tea.Msg) tea.Cmd { return nil }
func (s *connectionStep) blockers() []problem {
	return s.ps.stepProblems("name", "url", "login", "password")
}

func (s *connectionStep) mark() string {
	switch sg := s.ps.sign; {
	case len(s.blockers()) > 0:
		return markBad
	case sg.key == s.ps.connKey() && sg.err != nil:
		return markErr
	case !s.ps.signedIn():
		return markPending
	}
	return markOK
}

// extraHelp offers to sign in again after a failure.
func (s *connectionStep) extraHelp(string) string {
	if s.ps.sign.err != nil {
		return keyHelp("ctrl+r", "sign in again")
	}
	return ""
}

func (s *connectionStep) shortcut(msg tea.KeyMsg, _ string) (tea.Cmd, bool) {
	if msg.String() != "ctrl+r" {
		return nil, false
	}
	s.ps.sign = signState{}
	return s.ps.signIn(), true
}

func (s *connectionStep) signInView(width int) string {
	ps := s.ps
	switch sg := ps.sign; {
	case !ps.connReady():
		return ""
	case sg.key != ps.connKey() || sg.pending:
		return markPending + styleDim.Render(" signing in to "+youtrack.NormalizeURL(ps.cfg.URL)+"…")
	case sg.err != nil:
		return errLine("can't sign in: "+sg.err.Error(), width) + "\n" +
			styleDim.Render("  check the URL, login and password, then press ctrl+r to try again")
	}
	return markOK + " signed in to " + youtrack.NormalizeURL(ps.cfg.URL) + " as " + styleBold.Render(ps.sign.login)
}

// toolsStep picks the tools the profile serves and whether destructive
// ones ask for confirmation.
type toolsStep struct {
	base
	ps  *profileState
	all []*item
}

func newToolsStep(ps *profileState) *toolsStep {
	s := &toolsStep{base: base{sh: ps.sh}, ps: ps}
	total := len(tools.Catalog())
	s.all = []*item{
		{
			id: "tools", label: "Allowed tools",
			labelNote: func() string {
				if n := ps.count(); n < total {
					return fmt.Sprintf("%d of %d", n, total)
				}
				return fmt.Sprintf("all %d", total)
			},
			desc: "The tools MCP clients get. Fewer tools leave the model more room.",
			ctl:  newToolTree(ps.sh, ps.enabled),
			problem: func() string {
				if ps.count() == 0 {
					return "Needs at least one tool."
				}
				return ""
			},
		},
		{
			id: "confirm", inline: true,
			desc: "A destructive call first returns a secret instead of changing anything; the model asks you and calls again with it. Off: it runs at once.",
			ctl: &toggleCtl{
				label: "Ask before destructive operations",
				get:   func() bool { return ps.cfg.Confirms() },
				set: func(on bool) tea.Cmd {
					ps.cfg.Confirm = nil
					if !on {
						ps.cfg.Confirm = &on
					}
					return nil
				},
			},
			below: func(width int) string {
				var names []string
				for _, it := range tools.Catalog() {
					if it.Destructive && ps.enabled[it.Name] {
						names = append(names, it.Name)
					}
				}
				if len(names) == 0 {
					return "    " + styleDim.Render("None of the allowed tools is destructive.")
				}
				return "    " + styleDim.Render(oneLine("Destructive: "+strings.Join(names, ", ")+".", width-4))
			},
		},
	}
	return s
}

func (s *toolsStep) title() string { return "Tools" }
func (s *toolsStep) intro() string {
	return "Choose the tools this profile serves, and whether destructive ones ask first."
}
func (s *toolsStep) items() []*item      { return s.all }
func (s *toolsStep) enter() tea.Cmd      { return nil }
func (s *toolsStep) leave()              {}
func (s *toolsStep) msg(tea.Msg) tea.Cmd { return nil }
func (s *toolsStep) blockers() []problem { return s.ps.stepProblems("tools") }
func (s *toolsStep) mark() string        { return mark(s.blockers()) }

// clientsStep picks the MCP clients the profile is registered in.
type clientsStep struct {
	base
	ps  *profileState
	all []*item
}

func newClientsStep(ps *profileState) *clientsStep {
	s := &clientsStep{base: base{sh: ps.sh}, ps: ps}
	s.all = []*item{
		{
			id: "clients", label: "Register in",
			desc: "Unticked clients get the profile removed.",
			ctl: &checklistCtl{
				rows: s.rows,
				on:   func(id string) bool { return slices.Contains(ps.chosen, id) },
				flip: func(id string) {
					if i := slices.Index(ps.chosen, id); i >= 0 {
						ps.chosen = slices.Delete(ps.chosen, i, i+1)
					} else {
						ps.chosen = append(ps.chosen, id)
					}
				},
			},
			hidden: func() bool { return len(ps.clients) == 0 },
		},
		{
			id: "quit", inline: true,
			descFn: func() string {
				return strings.Join(s.running(), ", ") + " write their config back from memory, which drops ymcp's change. " +
					"Their windows close and reopen; Claude Code sessions in them close too."
			},
			ctl: &toggleCtl{
				label: "Quit and reopen running apps while saving",
				get:   func() bool { return ps.quit },
				set:   func(on bool) tea.Cmd { ps.quit = on; return nil },
			},
			hidden: func() bool { return len(s.running()) == 0 },
		},
		{
			id: "inside",
			below: func(width int) string {
				return markWarn + " " + styleDim.Render(oneLine(strings.Join(s.inside(), ", ")+" runs this setup and drops the change while it runs: quit it and save from another terminal.", width-2))
			},
			hidden: func() bool { return len(s.inside()) == 0 },
		},
		{
			id: "none",
			below: func(width int) string {
				return markWarn + " " + styleDim.Render(oneLine("No MCP clients found on this machine. Register the command below as a stdio server by hand.", width-2))
			},
			hidden: func() bool { return len(ps.clients) > 0 },
		},
		{
			id:     "command",
			hidden: func() bool { return ps.deps.Command == nil },
			below: func(width int) string {
				return styleDim.Render("Server ") + styleBold.Render(ps.key()) + styleDim.Render(": ") +
					styleCode.Render(oneLine(ps.deps.Command(ps.key()), width-12)) + styleDim.Render(" (stdio)")
			},
		},
	}
	return s
}

// touched reports whether saving edits a client's config.
func (s *clientsStep) touched(c Client) bool {
	return c.Registered || slices.Contains(s.ps.chosen, c.ID)
}

// inside names the clients ymcp runs in, which would drop the change.
func (s *clientsStep) inside() []string {
	var out []string
	for _, c := range s.ps.clients {
		if c.Inside && s.touched(c) {
			out = append(out, c.Name)
		}
	}
	return out
}

// running names the clients that would drop the change and can quit.
func (s *clientsStep) running() []string {
	var out []string
	for _, c := range s.ps.clients {
		if c.Running && !c.Inside && s.touched(c) {
			out = append(out, c.Name)
		}
	}
	return out
}

func (s *clientsStep) rows() []option {
	var out []option
	for _, c := range s.ps.clients {
		var notes []string
		if c.Registered {
			notes = append(notes, "registered")
		}
		if c.Note != "" {
			notes = append(notes, c.Note)
		}
		out = append(out, option{value: c.ID, title: c.Name, desc: strings.Join(notes, " · ")})
	}
	return out
}

func (s *clientsStep) title() string { return "Clients" }
func (s *clientsStep) intro() string {
	return "Choose the MCP clients that get this YouTrack."
}
func (s *clientsStep) items() []*item      { return s.all }
func (s *clientsStep) enter() tea.Cmd      { return nil }
func (s *clientsStep) leave()              {}
func (s *clientsStep) msg(tea.Msg) tea.Cmd { return nil }
func (s *clientsStep) blockers() []problem { return nil }
func (s *clientsStep) mark() string {
	if len(s.ps.clients) == 0 {
		return markWarn
	}
	return markOK
}
