package clients

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type fakeEnv struct {
	Env
	ran []string
}

func newEnv(t *testing.T, onPath ...string) *fakeEnv {
	t.Helper()
	f := &fakeEnv{}
	f.Env = Env{
		OS:     "darwin",
		Home:   t.TempDir(),
		Getenv: func(string) string { return "" },
		LookPath: func(name string) (string, error) {
			for _, p := range onPath {
				if p == name {
					return "/usr/local/bin/" + name, nil
				}
			}
			return "", errors.New("not found")
		},
		Run: func(name string, args ...string) error {
			f.ran = append(f.ran, name+" "+strings.Join(args, " "))
			return nil
		},
	}
	return f
}

// srv is the default profile's registration.
var srv = Server{Name: "youtrack", Command: "/opt/ymcp", Args: []string{"--profile", "youtrack"}}

func byID(t *testing.T, id string) Client {
	t.Helper()
	for _, c := range All() {
		if c.ID == id {
			return c
		}
	}
	t.Fatalf("no client %s", id)
	return Client{}
}

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func read(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestZedKeepsCommentsAndKeys(t *testing.T) {
	e := newEnv(t)
	path := filepath.Join(e.Home, ".config", "zed", "settings.json")
	write(t, path, `// Zed settings
{
  "theme": "One Dark", // keep me
  "context_servers": {
    "other": {"command": "x"},
  },
}
`)
	zed := byID(t, "zed")
	if !zed.Detected(e.Env) || zed.Installed(e.Env, "youtrack") {
		t.Fatal("zed should be detected and not installed")
	}
	if err := zed.Install(e.Env, srv); err != nil {
		t.Fatal(err)
	}
	got := read(t, path)
	for _, want := range []string{"// Zed settings", "// keep me", `"theme": "One Dark"`, `"other"`, `"youtrack"`, `"/opt/ymcp"`} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in\n%s", want, got)
		}
	}
	if info, _ := os.Stat(path); info.Mode().Perm() != 0o600 {
		t.Errorf("mode changed to %v", info.Mode().Perm())
	}
	if !zed.Installed(e.Env, "youtrack") || read(t, path+".ymcp.bak") == "" {
		t.Error("not installed or no backup")
	}
	if err := zed.Uninstall(e.Env, "youtrack"); err != nil {
		t.Fatal(err)
	}
	if got := read(t, path); strings.Contains(got, "youtrack") || !strings.Contains(got, `"other"`) {
		t.Errorf("after uninstall:\n%s", got)
	}
}

func TestProfilesAreSeparateServers(t *testing.T) {
	e := newEnv(t)
	os.MkdirAll(filepath.Join(e.Home, ".cursor"), 0o755)
	beget := Server{Name: "youtrack-beget", Command: "/opt/ymcp", Args: []string{"--profile", "youtrack-beget"}}
	c := byID(t, "cursor")
	for _, s := range []Server{srv, beget} {
		if err := c.Install(e.Env, s); err != nil {
			t.Fatal(err)
		}
	}
	if !c.Installed(e.Env, "youtrack") || !c.Installed(e.Env, "youtrack-beget") || c.Installed(e.Env, "other") {
		t.Fatal("both profiles should be registered, and nothing else")
	}
	if err := c.Uninstall(e.Env, "youtrack"); err != nil {
		t.Fatal(err)
	}
	got := read(t, filepath.Join(e.Home, ".cursor", "mcp.json"))
	if strings.Contains(got, `"youtrack":`) || !strings.Contains(got, `"youtrack-beget"`) {
		t.Errorf("after removing youtrack:\n%s", got)
	}

	// Codex tables of several profiles don't clobber each other.
	codex := byID(t, "codex")
	os.MkdirAll(filepath.Join(e.Home, ".codex"), 0o755)
	codex.Install(e.Env, srv)
	codex.Install(e.Env, beget)
	codex.Install(e.Env, srv)
	toml := read(t, filepath.Join(e.Home, ".codex", "config.toml"))
	if strings.Count(toml, "[mcp_servers.youtrack]") != 1 || strings.Count(toml, "[mcp_servers.youtrack-beget]") != 1 {
		t.Errorf("config.toml:\n%s", toml)
	}
}

func TestClaudeDesktopCreatesConfig(t *testing.T) {
	e := newEnv(t)
	os.MkdirAll(filepath.Join(e.Home, "Library", "Application Support", "Claude"), 0o755)
	c := byID(t, "claude-desktop")
	if !c.Detected(e.Env) {
		t.Fatal("not detected")
	}
	if err := c.Install(e.Env, srv); err != nil {
		t.Fatal(err)
	}
	got := read(t, filepath.Join(e.Home, "Library", "Application Support", "Claude", "claude_desktop_config.json"))
	if !strings.Contains(got, `"mcpServers"`) || !strings.Contains(got, `"command": "/opt/ymcp"`) {
		t.Errorf("config:\n%s", got)
	}
	if !c.Installed(e.Env, "youtrack") {
		t.Error("not installed")
	}
}

func TestVSCodeUsesServersKey(t *testing.T) {
	e := newEnv(t)
	e.OS = "linux"
	path := filepath.Join(e.Home, ".config", "Code", "User", "mcp.json")
	write(t, path, `{"inputs": [], "servers": {}}`)
	c := byID(t, "vscode")
	if err := c.Install(e.Env, srv); err != nil {
		t.Fatal(err)
	}
	if got := read(t, path); !strings.Contains(got, `"inputs"`) || !strings.Contains(got, `"youtrack": {`) || !strings.Contains(got, `"type": "stdio"`) {
		t.Errorf("config:\n%s", got)
	}
}

func TestCodexTOMLFallback(t *testing.T) {
	e := newEnv(t)
	path := filepath.Join(e.Home, ".codex", "config.toml")
	write(t, path, "model = \"gpt\"\n\n[mcp_servers.youtrack]\ncommand = \"/old\"\nargs = []\n\n[mcp_servers.other]\ncommand = \"x\"\n")
	c := byID(t, "codex")
	if !c.Detected(e.Env) || !c.Installed(e.Env, "youtrack") {
		t.Fatal("codex should be detected and installed")
	}
	if err := c.Install(e.Env, srv); err != nil {
		t.Fatal(err)
	}
	got := read(t, path)
	if strings.Contains(got, "/old") || !strings.Contains(got, "command = '/opt/ymcp'\nargs = ['--profile', 'youtrack']") || !strings.Contains(got, "[mcp_servers.other]") || !strings.HasPrefix(got, "model") {
		t.Errorf("config.toml:\n%s", got)
	}
	if err := c.Uninstall(e.Env, "youtrack"); err != nil {
		t.Fatal(err)
	}
	if got := read(t, path); strings.Contains(got, "youtrack") || !strings.Contains(got, "[mcp_servers.other]") {
		t.Errorf("after uninstall:\n%s", got)
	}
}

func TestCLIClients(t *testing.T) {
	e := newEnv(t, "claude", "codex")
	for _, id := range []string{"claude-code", "codex"} {
		if err := byID(t, id).Install(e.Env, srv); err != nil {
			t.Fatal(err)
		}
	}
	want := []string{
		"claude mcp remove --scope user youtrack",
		"claude mcp add --scope user youtrack -- /opt/ymcp --profile youtrack",
		"codex mcp remove youtrack",
		"codex mcp add youtrack -- /opt/ymcp --profile youtrack",
	}
	if strings.Join(e.ran, "\n") != strings.Join(want, "\n") {
		t.Errorf("ran:\n%s", strings.Join(e.ran, "\n"))
	}
}

func TestOpencodeCommandIsArray(t *testing.T) {
	e := newEnv(t, "opencode")
	c := byID(t, "opencode")
	if err := c.Install(e.Env, srv); err != nil {
		t.Fatal(err)
	}
	got := read(t, filepath.Join(e.Home, ".config", "opencode", "opencode.json"))
	if !strings.Contains(got, `"/opt/ymcp",`) || !strings.Contains(got, `"--profile",`) || !strings.Contains(got, `"type": "local"`) {
		t.Errorf("opencode.json:\n%s", got)
	}
}

func TestPathsPerOS(t *testing.T) {
	for _, c := range []struct {
		goos, client, want string
		env                map[string]string // values are relative to the fake home
	}{
		{"linux", "claude-desktop", ".config/Claude/claude_desktop_config.json", nil},
		{"linux", "zed", "xdg/zed/settings.json", map[string]string{"XDG_CONFIG_HOME": "xdg"}},
		{"windows", "claude-desktop", "Roaming/Claude/claude_desktop_config.json", map[string]string{"APPDATA": "Roaming"}},
		{"windows", "zed", "Roaming/Zed/settings.json", map[string]string{"APPDATA": "Roaming"}},
		{"windows", "devin", "Roaming/devin/mcp_config.json", map[string]string{"APPDATA": "Roaming"}},
		{"darwin", "cline", "custom/cline.json", map[string]string{"CLINE_MCP_SETTINGS_PATH": "custom/cline.json"}},
		{"linux", "kimi", ".kimi-code/mcp.json", nil},
		{"windows", "kimi", "kimi-home/mcp.json", map[string]string{"KIMI_CODE_HOME": "kimi-home"}},
	} {
		e := newEnv(t)
		e.OS = c.goos
		root := e.Home
		e.Getenv = func(k string) string {
			if v, ok := c.env[k]; ok {
				return filepath.Join(root, filepath.FromSlash(v))
			}
			return ""
		}
		if err := byID(t, c.client).Install(e.Env, srv); err != nil {
			t.Fatalf("%s/%s: %v", c.goos, c.client, err)
		}
		// Exact case matters: Linux file systems are case-sensitive.
		if _, err := os.Stat(filepath.Join(root, filepath.FromSlash(c.want))); err != nil {
			t.Errorf("%s/%s: expected %s: %v", c.goos, c.client, c.want, err)
		}
	}
}

func TestSystemEnv(t *testing.T) {
	e, err := System("darwin")
	if err != nil || e.Home == "" || e.Getenv == nil || e.LookPath == nil || e.Run == nil {
		t.Fatalf("System = %+v, %v", e, err)
	}
	if err := e.Run("false"); err == nil {
		t.Error("failing command reported success")
	}
	if len(All()) != 15 {
		t.Errorf("clients = %d, want 15", len(All()))
	}
}

func TestClaudeDesktopQuitsAndReopens(t *testing.T) {
	pollInterval = 0
	e := newEnv(t)
	running := true
	e.Run = func(name string, args ...string) error {
		e.ran = append(e.ran, name+" "+strings.Join(args, " "))
		switch name {
		case "pgrep":
			if running {
				return nil
			}
			return errors.New("exit status 1")
		case "osascript":
			running = false
		}
		return nil
	}
	c := byID(t, "claude-desktop")
	if c.App() != "Claude" || !c.Running(e.Env) {
		t.Fatal("Claude Desktop should be running")
	}
	if err := c.Quit(e.Env); err != nil || c.Running(e.Env) {
		t.Fatalf("Quit = %v, running = %v", err, c.Running(e.Env))
	}
	if err := c.Open(e.Env); err != nil {
		t.Fatal(err)
	}
	want := []string{"pgrep -x Claude", "osascript -e quit app \"Claude\"", "pgrep -x Claude", "pgrep -x Claude", "open -a Claude"}
	if strings.Join(e.ran, "\n") != strings.Join(want, "\n") {
		t.Errorf("ran:\n%s", strings.Join(e.ran, "\n"))
	}
	if byID(t, "cursor").App() != "" || byID(t, "cursor").Running(e.Env) {
		t.Error("Cursor has no app to close")
	}
}

func TestKimiKeepsOtherServers(t *testing.T) {
	e := newEnv(t)
	path := filepath.Join(e.Home, ".kimi-code", "mcp.json")
	write(t, path, `{"mcpServers": {"nuxt": {"url": "https://nuxt.com/mcp"}}}`)
	c := byID(t, "kimi")
	if !c.Detected(e.Env) || c.Installed(e.Env, "youtrack") {
		t.Fatal("Kimi should be detected and not installed")
	}
	if err := c.Install(e.Env, srv); err != nil {
		t.Fatal(err)
	}
	got := read(t, path)
	for _, want := range []string{`"nuxt"`, `"youtrack": {`, `"command": "/opt/ymcp"`, `"--profile"`} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in\n%s", want, got)
		}
	}
}
