// Package install tells how ymcp was installed, so messages can name a
// command that works there.
package install

import (
	"os"
	"strings"
)

// Docker reports whether ymcp runs in its Docker image, which sets
// YMCP_INSTALL=docker.
func Docker() bool { return os.Getenv("YMCP_INSTALL") == "docker" }

// Manager is the package manager that installed the binary at exe, judged
// by where it lives: "brew", "scoop", "winget", or "" for the installer
// script, go install or a copy.
func Manager(exe string) string {
	p := strings.ToLower(strings.ReplaceAll(exe, `\`, "/"))
	switch {
	case strings.Contains(p, "/caskroom/") || strings.Contains(p, "/cellar/") || strings.Contains(p, "/homebrew/"):
		return "brew"
	case strings.Contains(p, "/scoop/apps/") || strings.Contains(p, "/scoop/shims/"):
		return "scoop"
	case strings.Contains(p, "/winget/packages/") || strings.Contains(p, "/winget/links/"):
		return "winget"
	}
	return ""
}

// UpdateCommand is how to update ymcp at exe: through its package manager,
// docker pull in the image, or ymcp update.
func UpdateCommand(exe string) string {
	switch {
	case Docker():
		return "docker pull xlsoftware/ymcp:latest"
	case Manager(exe) == "brew":
		return "brew upgrade ymcp"
	case Manager(exe) == "scoop":
		return "scoop update ymcp"
	case Manager(exe) == "winget":
		return "winget upgrade xlsft.ymcp"
	}
	return "ymcp update"
}

// Command is how the user runs ymcp with args: "ymcp auth", or in the
// image "docker run -it --rm -v ymcp:/config xlsoftware/ymcp auth".
func Command(args string) string {
	if Docker() {
		return "docker run -it --rm -v ymcp:/config xlsoftware/ymcp " + args
	}
	return "ymcp " + args
}

// AddProfile is the command that adds or fixes the profile key's
// connection: setup natively, auth in the image, where no clients live.
func AddProfile(key string) string {
	switch {
	case Docker() && key != "":
		return Command("--profile " + key + " auth")
	case Docker():
		return Command("auth")
	case key != "":
		return Command("--profile " + key + " setup")
	}
	return Command("setup")
}
