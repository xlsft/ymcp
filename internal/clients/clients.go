// Package clients registers ymcp as a stdio MCP server in MCP clients
// (Claude, Cursor, VS Code, Zed, Codex, Gemini, Kimi and others).
package clients

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// Server is a ymcp registration: the name it appears under in a client
// and the command that starts it, e.g. youtrack-beget running
// `ymcp --profile youtrack-beget`.
type Server struct {
	Name    string
	Command string
	Args    []string
}

// Env is the machine the clients live on; tests replace it.
type Env struct {
	OS       string // runtime.GOOS
	Home     string
	Getenv   func(string) string
	LookPath func(string) (string, error)
	Run      func(name string, args ...string) error
}

// System returns the Env of this machine.
func System(goos string) (Env, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return Env{}, err
	}
	return Env{
		OS:       goos,
		Home:     home,
		Getenv:   os.Getenv,
		LookPath: exec.LookPath,
		Run: func(name string, args ...string) error {
			out, err := exec.Command(name, args...).CombinedOutput()
			if err != nil {
				return fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, strings.TrimSpace(string(out)))
			}
			return nil
		},
	}, nil
}

func (e Env) exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func (e Env) onPath(name string) bool {
	_, err := e.LookPath(name)
	return err == nil
}

// appData is the per-user application settings directory:
// ~/Library/Application Support, %APPDATA% or $XDG_CONFIG_HOME.
func (e Env) appData() string {
	switch e.OS {
	case "darwin":
		return filepath.Join(e.Home, "Library", "Application Support")
	case "windows":
		if d := e.Getenv("APPDATA"); d != "" {
			return d
		}
		return filepath.Join(e.Home, "AppData", "Roaming")
	default:
		return e.xdgConfig()
	}
}

func (e Env) xdgConfig() string {
	if d := e.Getenv("XDG_CONFIG_HOME"); d != "" {
		return d
	}
	return filepath.Join(e.Home, ".config")
}

// Client is one MCP client ymcp can be registered in.
type Client struct {
	ID   string
	Name string
	// Note is shown after registering, e.g. a restart hint.
	Note string

	// app is the process of a desktop app that keeps its config in memory
	// and writes it back, dropping edits made while it runs.
	app string

	detect    func(Env) bool
	installed func(Env, string) bool
	install   func(Env, Server) error
	uninstall func(Env, string) error
}

func (c Client) Detected(e Env) bool { return c.detect(e) }

// Installed reports whether a server named name is registered.
func (c Client) Installed(e Env, name string) bool { return c.installed(e, name) }

// Install registers s, replacing a registration of the same name.
func (c Client) Install(e Env, s Server) error { return c.install(e, s) }

// Uninstall removes the registration named name.
func (c Client) Uninstall(e Env, name string) error { return c.uninstall(e, name) }

// App is the name of the client's app process when the client overwrites
// its config while running, so it must be closed during edits; else "".
func (c Client) App() string { return c.app }

// pollInterval is how often Quit checks that the app has exited.
var pollInterval = 500 * time.Millisecond

// Running reports whether the client's app is running.
func (c Client) Running(e Env) bool {
	switch {
	case c.app == "":
		return false
	case e.OS == "darwin":
		return e.Run("pgrep", "-x", c.app) == nil
	case e.OS == "windows":
		return e.Run("powershell", "-NoProfile", "-Command", "Get-Process -Name "+c.app+" -ErrorAction Stop") == nil
	}
	return false
}

// Quit asks the client's app to quit and waits until it has.
func (c Client) Quit(e Env) error {
	var err error
	switch e.OS {
	case "darwin":
		err = e.Run("osascript", "-e", `quit app "`+c.app+`"`)
	case "windows":
		err = e.Run("taskkill", "/IM", c.app+".exe")
	default:
		return fmt.Errorf("can't quit %s on %s", c.Name, e.OS)
	}
	if err != nil {
		return err
	}
	for range 40 {
		if !c.Running(e) {
			return nil
		}
		time.Sleep(pollInterval)
	}
	return fmt.Errorf("%s is still running", c.Name)
}

// Open starts the client's app again; only macOS knows how.
func (c Client) Open(e Env) error {
	if e.OS != "darwin" {
		return fmt.Errorf("start %s again yourself", c.Name)
	}
	return e.Run("open", "-a", c.app)
}

// jsonClient is a client configured by a JSON or JSONC file.
func jsonClient(id, name, note string, path func(Env) string, detect func(Env) bool, keyPath []string, entry func(Server) any) Client {
	return Client{
		ID: id, Name: name, Note: note,
		detect:    detect,
		installed: func(e Env, name string) bool { return hasJSONEntry(path(e), keyPath, name) },
		install:   func(e Env, s Server) error { return setJSONEntry(path(e), keyPath, s.Name, entry(s)) },
		uninstall: func(e Env, name string) error { return setJSONEntry(path(e), keyPath, name, nil) },
	}
}

func (s Server) args() []string {
	if s.Args == nil {
		return []string{}
	}
	return s.Args
}

func stdioEntry(s Server) any { return map[string]any{"command": s.Command, "args": s.args()} }

func typedStdioEntry(s Server) any {
	return map[string]any{"type": "stdio", "command": s.Command, "args": s.args()}
}

// All lists the supported clients.
func All() []Client {
	restart := "restart the app to load the server"
	return []Client{
		claudeCode(),
		claudeDesktop(restart),
		jsonClient("cursor", "Cursor", restart,
			func(e Env) string { return filepath.Join(e.Home, ".cursor", "mcp.json") },
			func(e Env) bool { return e.exists(filepath.Join(e.Home, ".cursor")) },
			[]string{"mcpServers"}, typedStdioEntry),
		jsonClient("vscode", "VS Code (Copilot)", "",
			func(e Env) string { return filepath.Join(e.appData(), "Code", "User", "mcp.json") },
			func(e Env) bool { return e.exists(filepath.Join(e.appData(), "Code", "User")) },
			[]string{"servers"}, typedStdioEntry),
		jsonClient("windsurf", "Windsurf", restart,
			func(e Env) string { return filepath.Join(e.Home, ".codeium", "windsurf", "mcp_config.json") },
			func(e Env) bool { return e.exists(filepath.Join(e.Home, ".codeium", "windsurf")) },
			[]string{"mcpServers"}, stdioEntry),
		jsonClient("devin", "Devin Desktop", restart,
			func(e Env) string { return filepath.Join(devinDir(e), "mcp_config.json") },
			func(e Env) bool { return e.exists(devinDir(e)) },
			[]string{"mcpServers"}, stdioEntry),
		jsonClient("zed", "Zed", "",
			func(e Env) string { return filepath.Join(zedDir(e), "settings.json") },
			func(e Env) bool { return e.exists(zedDir(e)) || e.exists("/Applications/Zed.app") },
			[]string{"context_servers"}, stdioEntry),
		codex(),
		jsonClient("gemini", "Gemini CLI", "",
			func(e Env) string { return filepath.Join(e.Home, ".gemini", "settings.json") },
			func(e Env) bool {
				return e.onPath("gemini") || e.exists(filepath.Join(e.Home, ".gemini", "settings.json"))
			},
			[]string{"mcpServers"}, stdioEntry),
		jsonClient("antigravity", "Antigravity", restart,
			func(e Env) string { return filepath.Join(e.Home, ".gemini", "config", "mcp_config.json") },
			func(e Env) bool { return e.onPath("agy") || e.exists(filepath.Join(e.Home, ".gemini", "config")) },
			[]string{"mcpServers"}, stdioEntry),
		jsonClient("junie", "JetBrains Junie", "",
			func(e Env) string { return filepath.Join(e.Home, ".junie", "mcp", "mcp.json") },
			func(e Env) bool { return e.exists(filepath.Join(e.Home, ".junie")) },
			[]string{"mcpServers"}, stdioEntry),
		jsonClient("cline", "Cline", "",
			func(e Env) string { return clineSettings(e) },
			func(e Env) bool { return e.exists(filepath.Join(e.Home, ".cline")) || e.onPath("cline") },
			[]string{"mcpServers"},
			func(s Server) any {
				return map[string]any{"command": s.Command, "args": s.args(), "disabled": false, "autoApprove": []string{}}
			}),
		jsonClient("lmstudio", "LM Studio", restart,
			func(e Env) string { return filepath.Join(e.Home, ".lmstudio", "mcp.json") },
			func(e Env) bool { return e.exists(filepath.Join(e.Home, ".lmstudio")) },
			[]string{"mcpServers"}, stdioEntry),
		jsonClient("opencode", "opencode", "",
			opencodeConfig,
			func(e Env) bool { return e.onPath("opencode") || e.exists(filepath.Join(e.xdgConfig(), "opencode")) },
			[]string{"mcp"},
			func(s Server) any {
				return map[string]any{"type": "local", "command": append([]string{s.Command}, s.Args...), "enabled": true}
			}),
		jsonClient("kimi", "Kimi Code", "start a new Kimi session to load the server",
			func(e Env) string { return filepath.Join(kimiHome(e), "mcp.json") },
			func(e Env) bool { return e.onPath("kimi") || e.exists(kimiHome(e)) },
			[]string{"mcpServers"}, stdioEntry),
	}
}

// kimiHome is Kimi Code's directory, ~/.kimi-code or $KIMI_CODE_HOME.
func kimiHome(e Env) string {
	if h := e.Getenv("KIMI_CODE_HOME"); h != "" {
		return h
	}
	return filepath.Join(e.Home, ".kimi-code")
}

// claudeDesktop writes its config back from memory, e.g. when a session
// starts, so ymcp's entry only sticks while the app is closed.
func claudeDesktop(note string) Client {
	c := jsonClient("claude-desktop", "Claude Desktop", note,
		func(e Env) string { return filepath.Join(e.appData(), "Claude", "claude_desktop_config.json") },
		func(e Env) bool {
			return e.exists(filepath.Join(e.appData(), "Claude")) || e.exists("/Applications/Claude.app")
		},
		[]string{"mcpServers"}, stdioEntry)
	c.app = "Claude"
	return c
}

func devinDir(e Env) string {
	if e.OS == "windows" {
		return filepath.Join(e.appData(), "devin")
	}
	return filepath.Join(e.xdgConfig(), "devin")
}

func zedDir(e Env) string {
	switch e.OS {
	case "windows":
		return filepath.Join(e.appData(), "Zed")
	case "darwin":
		return filepath.Join(e.Home, ".config", "zed")
	default:
		return filepath.Join(e.xdgConfig(), "zed")
	}
}

func clineSettings(e Env) string {
	if p := e.Getenv("CLINE_MCP_SETTINGS_PATH"); p != "" {
		return p
	}
	return filepath.Join(e.Home, ".cline", "data", "settings", "cline_mcp_settings.json")
}

// opencodeConfig prefers the file that already has an "mcp" section.
func opencodeConfig(e Env) string {
	dir := filepath.Join(e.xdgConfig(), "opencode")
	if p := e.Getenv("OPENCODE_CONFIG"); p != "" {
		return p
	}
	jsonc := filepath.Join(dir, "opencode.jsonc")
	if v, ok, err := readJSONC(jsonc); err == nil && ok && v.Find("/mcp") != nil {
		return jsonc
	}
	return filepath.Join(dir, "opencode.json")
}

// claudeCode goes through the claude CLI, which owns ~/.claude.json.
func claudeCode() Client {
	path := func(e Env) string { return filepath.Join(e.Home, ".claude.json") }
	return Client{
		ID: "claude-code", Name: "Claude Code",
		detect:    func(e Env) bool { return e.onPath("claude") },
		installed: func(e Env, name string) bool { return hasJSONEntry(path(e), []string{"mcpServers"}, name) },
		install: func(e Env, s Server) error {
			_ = e.Run("claude", "mcp", "remove", "--scope", "user", s.Name)
			return e.Run("claude", append([]string{"mcp", "add", "--scope", "user", s.Name, "--", s.Command}, s.Args...)...)
		},
		uninstall: func(e Env, name string) error { return e.Run("claude", "mcp", "remove", "--scope", "user", name) },
	}
}

// codex uses the codex CLI when present and edits config.toml otherwise.
func codex() Client {
	home := func(e Env) string {
		if h := e.Getenv("CODEX_HOME"); h != "" {
			return h
		}
		return filepath.Join(e.Home, ".codex")
	}
	path := func(e Env) string { return filepath.Join(home(e), "config.toml") }
	return Client{
		ID: "codex", Name: "Codex",
		detect: func(e Env) bool { return e.onPath("codex") || e.exists(home(e)) },
		installed: func(e Env, name string) bool {
			data, err := os.ReadFile(path(e))
			return err == nil && tomlTable(name).Match(data)
		},
		install: func(e Env, s Server) error {
			if e.onPath("codex") {
				_ = e.Run("codex", "mcp", "remove", s.Name)
				return e.Run("codex", append([]string{"mcp", "add", s.Name, "--", s.Command}, s.Args...)...)
			}
			args := make([]string, len(s.Args))
			for i, a := range s.Args {
				args[i] = tomlString(a)
			}
			block := fmt.Sprintf("[mcp_servers.%s]\ncommand = %s\nargs = [%s]\n", s.Name, tomlString(s.Command), strings.Join(args, ", "))
			return setTOMLBlock(path(e), s.Name, block)
		},
		uninstall: func(e Env, name string) error {
			if e.onPath("codex") {
				return e.Run("codex", "mcp", "remove", name)
			}
			return setTOMLBlock(path(e), name, "")
		},
	}
}

func tomlTable(name string) *regexp.Regexp {
	return regexp.MustCompile(`(?m)^\[mcp_servers\.` + regexp.QuoteMeta(name) + `\]\s*$`)
}

// setTOMLBlock replaces the [mcp_servers.<name>] table (up to the next
// table header) with block, or appends it; an empty block removes it.
func setTOMLBlock(path, name, block string) error {
	data, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	text := string(data)
	if loc := tomlTable(name).FindStringIndex(text); loc != nil {
		end := len(text)
		if next := regexp.MustCompile(`(?m)^\[`).FindStringIndex(text[loc[1]:]); next != nil {
			end = loc[1] + next[0]
		}
		text = text[:loc[0]] + block + text[end:]
	} else if block != "" {
		if text != "" && !strings.HasSuffix(text, "\n") {
			text += "\n"
		}
		if text != "" {
			text += "\n"
		}
		text += block
	}
	return writeFile(path, []byte(text), len(data) > 0)
}

// tomlString quotes s as a TOML literal string when possible (no escapes
// needed for Windows paths), else as a basic string.
func tomlString(s string) string {
	if !strings.Contains(s, "'") {
		return "'" + s + "'"
	}
	return fmt.Sprintf("%q", s)
}
