package spec

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/xlsft/ymcp/internal/config"
)

// ErrPresetNotFound is returned, wrapped, when a preset does not exist.
var ErrPresetNotFound = errors.New("preset not found")

// PresetDir is the directory holding the user presets: "presets" next to
// the config file of internal/config, like the pipelines directory.
func PresetDir() (string, error) {
	path, err := config.Path()
	if err != nil {
		return "", err
	}
	return filepath.Join(filepath.Dir(path), "presets"), nil
}

// CheckPresetName reports whether name can be a preset name. The rules are
// those of pipeline names: 1 to 40 lowercase letters, digits, dashes and
// underscores, starting with a letter or digit. The name is also the file
// name of a user preset.
func CheckPresetName(name string) error {
	if name == "" {
		return fmt.Errorf("preset name is empty")
	}
	if !nameRE.MatchString(name) {
		return fmt.Errorf("preset name %q is invalid: use 1 to 40 lowercase letters, digits, \"-\" or \"_\", starting with a letter or digit", name)
	}
	return nil
}

// Validate checks a preset before it is saved: a valid name and the three
// prompt texts. The rationale is optional.
func (p Preset) Validate() error {
	if err := CheckPresetName(p.Name); err != nil {
		return err
	}
	var missing []string
	for _, t := range []struct{ name, text string }{{"start", p.Start}, {"feedback", p.Feedback}, {"answer", p.Answer}} {
		if strings.TrimSpace(t.text) == "" {
			missing = append(missing, t.name)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("preset %q has no %s prompt", p.Name, strings.Join(missing, ", "))
	}
	return nil
}

func presetPath(name string) (string, error) {
	if err := CheckPresetName(name); err != nil {
		return "", err
	}
	dir, err := PresetDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, name+".json"), nil
}

// readOnly is the error for changing a built-in preset.
func readOnly(name string) error {
	return fmt.Errorf("preset %q is built in and read-only; copy it to a preset of your own first", name)
}

// LoadPreset returns the preset called name: the built-in one, or else the
// user preset from its file.
func LoadPreset(name string) (Preset, error) {
	if p, ok := builtinPreset(name); ok {
		return p, nil
	}
	path, err := presetPath(name)
	if err != nil {
		return Preset{}, err
	}
	return readPreset(name, path)
}

// readPreset reads a user preset file. As with pipelines, the file name is
// the identity: a "name" inside the file that differs is ignored.
func readPreset(name, path string) (Preset, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return Preset{}, fmt.Errorf("preset %q: %w", name, ErrPresetNotFound)
	}
	if err != nil {
		return Preset{}, err
	}
	var p Preset
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&p); err != nil {
		return Preset{}, fmt.Errorf("preset file %s: %w", path, err)
	}
	p.Name, p.Builtin = name, false
	return p, nil
}

// PresetExists reports whether name is taken: by a built-in preset or by a
// user preset file, readable or not.
func PresetExists(name string) (bool, error) {
	if IsBuiltinPreset(name) {
		return true, nil
	}
	path, err := presetPath(name)
	if err != nil {
		return false, err
	}
	_, err = os.Stat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	return err == nil, err
}

// AllPresets returns the built-in presets in their documented order, then
// the user presets sorted by name. A user preset file that cannot be read,
// or that takes a built-in name, does not hide the others: they are
// returned along with an error that names each such file.
func AllPresets() ([]Preset, error) {
	list := Presets()
	dir, err := PresetDir()
	if err != nil {
		return list, err
	}
	entries, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return list, nil
	}
	if err != nil {
		return list, err
	}
	var user []Preset
	var errs []error
	for _, e := range entries {
		name, ok := strings.CutSuffix(e.Name(), ".json")
		if !ok || e.IsDir() || CheckPresetName(name) != nil {
			continue
		}
		path := filepath.Join(dir, e.Name())
		if IsBuiltinPreset(name) {
			errs = append(errs, fmt.Errorf("preset file %s is ignored: %q is a built-in preset; rename the file", path, name))
			continue
		}
		p, err := readPreset(name, path)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		user = append(user, p)
	}
	sort.Slice(user, func(i, j int) bool { return user[i].Name < user[j].Name })
	return append(list, user...), errors.Join(errs...)
}

// SavePreset writes a user preset with mode 0600, atomically, creating it
// or replacing the user preset of that name. It refuses built-in names and
// presets that fail Validate.
func SavePreset(p Preset) error {
	if err := p.Validate(); err != nil {
		return err
	}
	if IsBuiltinPreset(p.Name) {
		return fmt.Errorf("preset %q is built in; choose another name", p.Name)
	}
	path, err := presetPath(p.Name)
	if err != nil {
		return err
	}
	p.Builtin = false
	return writeJSON(path, p)
}

// DeletePreset removes a user preset. Pipelines that were made from it keep
// their own copies of its texts.
func DeletePreset(name string) error {
	if IsBuiltinPreset(name) {
		return fmt.Errorf("preset %q is built in and cannot be deleted", name)
	}
	path, err := presetPath(name)
	if err != nil {
		return err
	}
	if err := os.Remove(path); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("preset %q: %w", name, ErrPresetNotFound)
		}
		return err
	}
	return nil
}

// RenamePreset moves a user preset to a new name. It refuses built-in
// presets on either side and will not overwrite another user preset.
func RenamePreset(oldName, newName string) error {
	if IsBuiltinPreset(oldName) {
		return readOnly(oldName)
	}
	from, err := presetPath(oldName)
	if err != nil {
		return err
	}
	p, err := readPreset(oldName, from)
	if err != nil {
		return err
	}
	if oldName == newName {
		return nil
	}
	if err := checkFree(newName); err != nil {
		return err
	}
	p.Name = newName
	if err := SavePreset(p); err != nil {
		return err
	}
	return os.Remove(from)
}

// CopyPreset saves a copy of the preset from, built-in or user, as the new
// user preset to.
func CopyPreset(from, to string) error {
	p, err := LoadPreset(from)
	if err != nil {
		return err
	}
	if err := checkFree(to); err != nil {
		return err
	}
	p.Name = to
	return SavePreset(p)
}

// checkFree fails unless name is a valid preset name nobody has taken.
func checkFree(name string) error {
	taken, err := PresetExists(name)
	if err != nil {
		return err
	}
	if taken {
		if IsBuiltinPreset(name) {
			return fmt.Errorf("preset %q is built in; choose another name", name)
		}
		return fmt.Errorf("preset %q already exists", name)
	}
	return nil
}
