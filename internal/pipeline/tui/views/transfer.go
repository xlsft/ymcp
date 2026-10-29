package views

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/xlsft/ymcp/internal/pipeline/spec"
)

// fileChosen is what the system's file dialog returned for an export or
// an import; use carries on with the path.
type fileChosen struct {
	to    int64
	title string
	def   string
	path  string
	err   error
	use   func(path string) tea.Cmd
}

// imported is the outcome of an import.
type imported struct {
	to  int64
	res spec.ImportResult
}

// exportFile asks where to export the named pipelines, with their
// presets, and writes them there; what names them on the status line.
func (m *Pipelines) exportFile(names []string, what, file string) tea.Cmd {
	return m.pickFile("Export "+what, file, true, func(path string) tea.Cmd {
		return func() tea.Msg {
			err := m.d.ExportPipelines(path, names)
			return m.report("Exported "+what+" to "+homeShort(path, m.home)+".", err)
		}
	})
}

// importFile asks for an export file and saves its pipelines in this
// profile.
func (m *Pipelines) importFile() tea.Cmd {
	return m.pickFile("Import pipelines", "", false, func(path string) tea.Cmd {
		return func() tea.Msg {
			res, err := m.d.ImportPipelines(path, m.profile)
			if err != nil {
				return m.report("", err)
			}
			return imported{to: m.id, res: res}
		}
	})
}

// pickFile asks for a file in the system's dialog, or for its path where
// there is none, and runs use with it. def is the suggested file name.
func (m *Pipelines) pickFile(title, def string, save bool, use func(string) tea.Cmd) tea.Cmd {
	if m.d.Files == nil {
		return m.askPath(title, def, use)
	}
	return func() tea.Msg {
		var path string
		var err error
		if save {
			path, err = m.d.Files.Save(title, def)
		} else {
			path, err = m.d.Files.Open(title)
		}
		return fileChosen{to: m.id, title: title, def: def, path: path, err: err, use: use}
	}
}

// chosen carries on with a file the dialog returned; a dialog that could
// not be shown falls back to typing the path.
func (m *Pipelines) chosen(msg fileChosen) tea.Cmd {
	switch {
	case msg.err != nil:
		m.fail(fmt.Errorf("the file dialog can't be shown (%v); type the path instead", msg.err))
		return m.askPath(msg.title, msg.def, msg.use)
	case msg.path == "":
		return nil
	}
	return msg.use(msg.path)
}

// askPath asks for a file path in a box, starting in the home directory.
func (m *Pipelines) askPath(title, def string, use func(string) tea.Cmd) tea.Cmd {
	start := "~" + string(filepath.Separator) + def
	return m.prompt.startPath(title, start, func(path string) (tea.Cmd, error) {
		path = expandHome(path, m.home)
		if path == "" || strings.HasSuffix(path, string(filepath.Separator)) {
			return nil, errors.New("type the path of a file")
		}
		return use(path), nil
	})
}

// expandHome replaces a leading ~ with the home directory.
func expandHome(path, home string) string {
	if home != "" && (path == "~" || strings.HasPrefix(path, "~/") || strings.HasPrefix(path, "~"+string(filepath.Separator))) {
		return filepath.Join(home, path[1:])
	}
	return path
}

// importSummary says what an import saved: "Imported pipelines a and
// b → b-2, preset mine.", a renamed one with the name it got.
func importSummary(res spec.ImportResult) string {
	if len(res.Pipelines) == 0 {
		return "The file has no pipelines."
	}
	list := func(one string, items []spec.Imported) string {
		names := make([]string, len(items))
		for i, it := range items {
			names[i] = it.To
			if it.Renamed() {
				names[i] = it.From + " → " + it.To
			}
		}
		if len(items) > 1 {
			one += "s"
		}
		if len(names) > 1 {
			return one + " " + strings.Join(names[:len(names)-1], ", ") + " and " + names[len(names)-1]
		}
		return one + " " + names[0]
	}
	out := "Imported " + list("pipeline", res.Pipelines)
	if len(res.Presets) > 0 {
		out += ", " + list("preset", res.Presets)
	}
	return out + "."
}

// exportName is the file name an export suggests: ymcp-<name>.json.
func exportName(name string) string { return "ymcp-" + name + ".json" }

// exportPipelines writes the named pipelines and their presets to path.
func exportPipelines(path string, names []string) error {
	b, err := spec.Export(names...)
	if err != nil {
		return err
	}
	return spec.WriteBundle(path, b)
}

// importPipelines saves the pipelines and presets of the file at path,
// the pipelines in profile.
func importPipelines(path, profile string) (spec.ImportResult, error) {
	b, err := spec.ReadBundle(path)
	if err != nil {
		return spec.ImportResult{}, err
	}
	return spec.Import(b, profile)
}
