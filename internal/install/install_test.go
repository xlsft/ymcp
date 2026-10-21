package install

import "testing"

func TestCommands(t *testing.T) {
	t.Setenv("YMCP_INSTALL", "")
	if got := AddProfile("beget"); got != "ymcp --profile beget setup" {
		t.Errorf("native = %q", got)
	}
	if got := Command("auth"); got != "ymcp auth" {
		t.Errorf("native auth = %q", got)
	}
	t.Setenv("YMCP_INSTALL", "docker")
	if got := AddProfile("beget"); got != "docker run -it --rm -v ymcp:/config xlsoftware/ymcp --profile beget auth" {
		t.Errorf("docker = %q", got)
	}
	if got := AddProfile(""); got != "docker run -it --rm -v ymcp:/config xlsoftware/ymcp auth" {
		t.Errorf("docker, no key = %q", got)
	}
}

func TestPackageManagers(t *testing.T) {
	t.Setenv("YMCP_INSTALL", "")
	for exe, want := range map[string]string{
		"/opt/homebrew/Caskroom/ymcp/0.8.0/ymcp":                                   "brew upgrade ymcp",
		"/home/linuxbrew/.linuxbrew/Caskroom/ymcp/0.8.0/ymcp":                      "brew upgrade ymcp",
		`C:\Users\a\scoop\apps\ymcp\current\ymcp.exe`:                              "scoop update ymcp",
		`C:\Users\a\AppData\Local\Microsoft\WinGet\Packages\xlsft.ymcp_x\ymcp.exe`: "winget upgrade xlsft.ymcp",
		"/Users/a/.local/bin/ymcp":                                                 "ymcp update",
	} {
		if got := UpdateCommand(exe); got != want {
			t.Errorf("%s: %q, want %q", exe, got, want)
		}
	}
	t.Setenv("YMCP_INSTALL", "docker")
	if got := UpdateCommand("/usr/local/bin/ymcp"); got != "docker pull xlsoftware/ymcp:latest" {
		t.Errorf("docker: %q", got)
	}
}
