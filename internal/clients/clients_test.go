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
	if !zed.Detected(e.Env) || zed.Installed(e.Env) {
		t.Fatal("zed should be detected and not installed")
	}
	if err := zed.Install(e.Env, "/opt/ymcp"); err != nil {
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
	if !zed.Installed(e.Env) || read(t, path+".ymcp.bak") == "" {
		t.Error("not installed or no backup")
	}
	if err := zed.Uninstall(e.Env); err != nil {
		t.Fatal(err)
	}
	if got := read(t, path); strings.Contains(got, "youtrack") || !strings.Contains(got, `"other"`) {
		t.Errorf("after uninstall:\n%s", got)
	}
}

func TestClaudeDesktopCreatesConfig(t *testing.T) {
	e := newEnv(t)
	os.MkdirAll(filepath.Join(e.Home, "Library", "Application Support", "Claude"), 0o755)
	c := byID(t, "claude-desktop")
	if !c.Detected(e.Env) {
		t.Fatal("not detected")
	}
	if err := c.Install(e.Env, "/opt/ymcp"); err != nil {
		t.Fatal(err)
	}
	got := read(t, filepath.Join(e.Home, "Library", "Application Support", "Claude", "claude_desktop_config.json"))
	if !strings.Contains(got, `"mcpServers"`) || !strings.Contains(got, `"command": "/opt/ymcp"`) {
		t.Errorf("config:\n%s", got)
	}
	if !c.Installed(e.Env) {
		t.Error("not installed")
	}
}

func TestVSCodeUsesServersKey(t *testing.T) {
	e := newEnv(t)
	e.OS = "linux"
	path := filepath.Join(e.Home, ".config", "Code", "User", "mcp.json")
	write(t, path, `{"inputs": [], "servers": {}}`)
	c := byID(t, "vscode")
	if err := c.Install(e.Env, "/opt/ymcp"); err != nil {
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
	if !c.Detected(e.Env) || !c.Installed(e.Env) {
		t.Fatal("codex should be detected and installed")
	}
	if err := c.Install(e.Env, "/opt/ymcp"); err != nil {
		t.Fatal(err)
	}
	got := read(t, path)
	if strings.Contains(got, "/old") || !strings.Contains(got, "command = '/opt/ymcp'") || !strings.Contains(got, "[mcp_servers.other]") || !strings.HasPrefix(got, "model") {
		t.Errorf("config.toml:\n%s", got)
	}
	if err := c.Uninstall(e.Env); err != nil {
		t.Fatal(err)
	}
	if got := read(t, path); strings.Contains(got, "youtrack") || !strings.Contains(got, "[mcp_servers.other]") {
		t.Errorf("after uninstall:\n%s", got)
	}
}

func TestCLIClients(t *testing.T) {
	e := newEnv(t, "claude", "codex")
	for _, id := range []string{"claude-code", "codex"} {
		if err := byID(t, id).Install(e.Env, "/opt/ymcp"); err != nil {
			t.Fatal(err)
		}
	}
	want := []string{
		"claude mcp remove --scope user youtrack",
		"claude mcp add --scope user youtrack -- /opt/ymcp",
		"codex mcp remove youtrack",
		"codex mcp add youtrack -- /opt/ymcp",
	}
	if strings.Join(e.ran, "\n") != strings.Join(want, "\n") {
		t.Errorf("ran:\n%s", strings.Join(e.ran, "\n"))
	}
}

func TestOpencodeCommandIsArray(t *testing.T) {
	e := newEnv(t, "opencode")
	c := byID(t, "opencode")
	if err := c.Install(e.Env, "/opt/ymcp"); err != nil {
		t.Fatal(err)
	}
	got := read(t, filepath.Join(e.Home, ".config", "opencode", "opencode.json"))
	if !strings.Contains(got, `"command": [`) || !strings.Contains(got, `"type": "local"`) {
		t.Errorf("opencode.json:\n%s", got)
	}
}

func TestPathsPerOS(t *testing.T) {
	for _, c := range []struct {
		goos, client, want string
		env                map[string]string
	}{
		{"linux", "claude-desktop", ".config/Claude/claude_desktop_config.json", nil},
		{"linux", "zed", "xdg/zed/settings.json", map[string]string{"XDG_CONFIG_HOME": "XDG"}},
		{"windows", "claude-desktop", "Roaming/Claude/claude_desktop_config.json", map[string]string{"APPDATA": "Roaming"}},
		{"windows", "zed", "Roaming/Zed/settings.json", map[string]string{"APPDATA": "Roaming"}},
		{"windows", "devin", "Roaming/devin/mcp_config.json", map[string]string{"APPDATA": "Roaming"}},
		{"darwin", "cline", "custom/cline.json", map[string]string{"CLINE_MCP_SETTINGS_PATH": "CUSTOM"}},
	} {
		e := newEnv(t)
		e.OS = c.goos
		root := e.Home
		e.Getenv = func(k string) string {
			if v, ok := c.env[k]; ok {
				return filepath.Join(root, strings.ToLower(v))
			}
			return ""
		}
		if c.client == "cline" {
			e.Getenv = func(k string) string {
				if k == "CLINE_MCP_SETTINGS_PATH" {
					return filepath.Join(root, "custom", "cline.json")
				}
				return ""
			}
		}
		if err := byID(t, c.client).Install(e.Env, "/opt/ymcp"); err != nil {
			t.Fatalf("%s/%s: %v", c.goos, c.client, err)
		}
		if _, err := os.Stat(filepath.Join(root, filepath.FromSlash(strings.ToLower(c.want)))); err != nil {
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
	if len(All()) != 14 {
		t.Errorf("clients = %d, want 14", len(All()))
	}
}
