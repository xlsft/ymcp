package tui

import (
	"errors"

	"github.com/ncruces/zenity"

	"github.com/xlsft/ymcp/internal/pipeline/tui/views"
)

// SystemFiles picks files in the system's own dialog: the open and save
// panels on macOS and Windows, zenity or kdialog on Linux.
type SystemFiles struct{}

// pipelineFiles is the filter of the dialogs: export files are JSON.
var pipelineFiles = zenity.FileFilters{{Name: "ymcp pipelines", Patterns: []string{"*.json"}, CaseFold: true}}

// Save asks where to save a file, suggesting name.
func (SystemFiles) Save(title, name string) (string, error) {
	return picked(zenity.SelectFileSave(zenity.Title(title), zenity.Filename(name), zenity.ConfirmOverwrite(), pipelineFiles))
}

// Open asks for a file to read.
func (SystemFiles) Open(title string) (string, error) {
	return picked(zenity.SelectFile(zenity.Title(title), pipelineFiles))
}

// picked turns a cancelled dialog into an empty path.
func picked(path string, err error) (string, error) {
	if errors.Is(err, zenity.ErrCanceled) {
		return "", nil
	}
	return path, err
}

// systemFiles is the system's dialog, or nil in an SSH session: the
// dialog would open on the far machine's screen, if it has one, so the
// TUI asks for a path instead.
func systemFiles(getenv func(string) string) views.Files {
	if getenv("SSH_CONNECTION") != "" || getenv("SSH_TTY") != "" {
		return nil
	}
	return SystemFiles{}
}
