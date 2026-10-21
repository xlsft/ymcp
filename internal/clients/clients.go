// Package clients registers ymcp as a stdio MCP server in MCP clients
// (Claude, Cursor, VS Code, Zed, Codex, Gemini and others).
package clients

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
)

// ServerName is the name ymcp is registered under in every client.
const ServerName = "youtrack"

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

	detect    func(Env) bool
	installed func(Env) bool
	install   func(Env, string) error
	uninstall func(Env) error
}

func (c Client) Detected(e Env) bool  { return c.detect(e) }
func (c Client) Installed(e Env) bool { return c.installed(e) }

// Install registers the ymcp binary at bin.
func (c Client) Install(e Env, bin string) error { return c.install(e, bin) }

// Uninstall removes the ymcp registration.
func (c Client) Uninstall(e Env) error { return c.uninstall(e) }

// jsonClient is a client configured by a JSON or JSONC file.
func jsonClient(id, name, note string, path func(Env) string, detect func(Env) bool, keyPath []string, entry func(bin string) any) Client {
	return Client{
		ID: id, Name: name, Note: note,
		detect:    detect,
		installed: func(e Env) bool { return hasJSONEntry(path(e), keyPath, ServerName) },
		install:   func(e Env, bin string) error { return setJSONEntry(path(e), keyPath, ServerName, entry(bin)) },
		uninstall: func(e Env) error { return setJSONEntry(path(e), keyPath, ServerName, nil) },
	}
}

func stdioEntry(bin string) any { return map[string]any{"command": bin, "args": []string{}} }

func typedStdioEntry(bin string) any {
	return map[string]any{"type": "stdio", "command": bin, "args": []string{}}
}

// All lists the supported clients.
func All() []Client {
	restart := "restart the app to load the server"
	return []Client{
		claudeCode(),
		jsonClient("claude-desktop", "Claude Desktop", restart,
			func(e Env) string { return filepath.Join(e.appData(), "Claude", "claude_desktop_config.json") },
			func(e Env) bool {
				return e.exists(filepath.Join(e.appData(), "Claude")) || e.exists("/Applications/Claude.app")
			},
			[]string{"mcpServers"}, stdioEntry),
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
			func(bin string) any {
				return map[string]any{"command": bin, "args": []string{}, "disabled": false, "autoApprove": []string{}}
			}),
		jsonClient("lmstudio", "LM Studio", restart,
			func(e Env) string { return filepath.Join(e.Home, ".lmstudio", "mcp.json") },
			func(e Env) bool { return e.exists(filepath.Join(e.Home, ".lmstudio")) },
			[]string{"mcpServers"}, stdioEntry),
		jsonClient("opencode", "opencode", "",
			opencodeConfig,
			func(e Env) bool { return e.onPath("opencode") || e.exists(filepath.Join(e.xdgConfig(), "opencode")) },
			[]string{"mcp"},
			func(bin string) any {
				return map[string]any{"type": "local", "command": []string{bin}, "enabled": true}
			}),
	}
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
		installed: func(e Env) bool { return hasJSONEntry(path(e), []string{"mcpServers"}, ServerName) },
		install: func(e Env, bin string) error {
			_ = e.Run("claude", "mcp", "remove", "--scope", "user", ServerName)
			return e.Run("claude", "mcp", "add", "--scope", "user", ServerName, "--", bin)
		},
		uninstall: func(e Env) error { return e.Run("claude", "mcp", "remove", "--scope", "user", ServerName) },
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
		installed: func(e Env) bool {
			data, err := os.ReadFile(path(e))
			return err == nil && tomlBlock.Match(data)
		},
		install: func(e Env, bin string) error {
			if e.onPath("codex") {
				_ = e.Run("codex", "mcp", "remove", ServerName)
				return e.Run("codex", "mcp", "add", ServerName, "--", bin)
			}
			return setTOMLBlock(path(e), fmt.Sprintf("[mcp_servers.%s]\ncommand = %s\nargs = []\n", ServerName, tomlString(bin)))
		},
		uninstall: func(e Env) error {
			if e.onPath("codex") {
				return e.Run("codex", "mcp", "remove", ServerName)
			}
			return setTOMLBlock(path(e), "")
		},
	}
}

var tomlBlock = regexp.MustCompile(`(?m)^\[mcp_servers\.` + ServerName + `\]\s*$`)

// setTOMLBlock replaces the [mcp_servers.youtrack] table (up to the next
// table header) with block, or appends it; an empty block removes it.
func setTOMLBlock(path, block string) error {
	data, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	text := string(data)
	if loc := tomlBlock.FindStringIndex(text); loc != nil {
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
