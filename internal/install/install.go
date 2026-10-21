// Package install tells how ymcp was installed, so messages can name a
// command that works there.
package install

import "os"

// Docker reports whether ymcp runs in its Docker image, which sets
// YMCP_INSTALL=docker.
func Docker() bool { return os.Getenv("YMCP_INSTALL") == "docker" }

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
