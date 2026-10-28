// Package termimg shows pictures in the pipeline TUI where the terminal can.
//
// Pictures go out with the kitty graphics protocol and Unicode placeholders:
// the image is sent once with an escape sequence, and the screen holds plain
// placeholder characters coloured with the image's id. To bubbletea, which
// redraws the screen line by line, those are ordinary text, so pictures
// scroll and redraw with the rest of the card. kitty and Ghostty support
// it. Elsewhere the TUI shows a link instead (see Detect).
package termimg

import "strings"

// Mode is how the terminal shows a picture.
type Mode int

const (
	// Link: the terminal can't show pictures here; the TUI links to them.
	Link Mode = iota
	// Kitty: the kitty graphics protocol with Unicode placeholders.
	Kitty
)

// Detect tells what the terminal can do from the environment, read through
// getenv. YMCP_IMAGES=kitty or YMCP_IMAGES=off overrides it. Otherwise kitty
// and Ghostty get pictures, unless the TUI runs inside tmux or screen, which
// don't pass the protocol on; every other terminal gets links.
func Detect(getenv func(string) string) Mode {
	switch strings.ToLower(strings.TrimSpace(getenv("YMCP_IMAGES"))) {
	case "kitty":
		return Kitty
	case "off", "link", "links", "none", "0", "false":
		return Link
	}
	if getenv("TMUX") != "" || getenv("STY") != "" || getenv("TERM") == "dumb" {
		return Link
	}
	term, program := getenv("TERM"), strings.ToLower(getenv("TERM_PROGRAM"))
	switch {
	case getenv("KITTY_WINDOW_ID") != "", term == "xterm-kitty":
		return Kitty
	case program == "ghostty", term == "xterm-ghostty", getenv("GHOSTTY_RESOURCES_DIR") != "":
		return Kitty
	}
	return Link
}
