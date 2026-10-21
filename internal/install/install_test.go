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
