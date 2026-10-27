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

// ErrNotFound is returned, wrapped, when a pipeline does not exist.
var ErrNotFound = errors.New("pipeline not found")

// Dir is the directory holding the pipeline files: "pipelines" next to the
// config file of internal/config, so YMCP_CONFIG moves both.
func Dir() (string, error) {
	path, err := config.Path()
	if err != nil {
		return "", err
	}
	return filepath.Join(filepath.Dir(path), "pipelines"), nil
}

func filePath(name string) (string, error) {
	if err := CheckName(name); err != nil {
		return "", err
	}
	dir, err := Dir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, name+".json"), nil
}

// Load reads the pipeline called name, with defaults applied. The file name
// is the pipeline's identity: a "name" inside the file that differs is
// ignored. It does not run Validate, so half-finished pipelines can be
// opened for editing.
func Load(name string) (Pipeline, error) {
	path, err := filePath(name)
	if err != nil {
		return Pipeline{}, err
	}
	return read(name, path)
}

func read(name, path string) (Pipeline, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return Pipeline{}, fmt.Errorf("pipeline %q: %w", name, ErrNotFound)
	}
	if err != nil {
		return Pipeline{}, err
	}
	var p Pipeline
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&p); err != nil {
		return Pipeline{}, fmt.Errorf("pipeline file %s: %w", path, err)
	}
	p.Name = name
	return p.WithDefaults(), nil
}

// Save writes p to its file with mode 0600, atomically, with defaults
// filled in. Only the name is checked; call Validate for the rest.
func Save(p Pipeline) error {
	path, err := filePath(p.Name)
	if err != nil {
		return err
	}
	return write(path, p.WithDefaults())
}

func write(path string, p Pipeline) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".pipeline-*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name()) // a no-op once renamed
	if err := tmp.Chmod(0o600); err != nil && !errors.Is(err, errors.ErrUnsupported) {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(append(data, '\n')); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// List returns every stored pipeline sorted by name. A file that cannot be
// read does not hide the others: they are returned along with an error that
// names each broken file.
func List() ([]Pipeline, error) {
	dir, err := Dir()
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var list []Pipeline
	var errs []error
	for _, e := range entries {
		name, ok := strings.CutSuffix(e.Name(), ".json")
		if !ok || e.IsDir() || CheckName(name) != nil {
			continue
		}
		p, err := read(name, filepath.Join(dir, e.Name()))
		if err != nil {
			errs = append(errs, err)
			continue
		}
		list = append(list, p)
	}
	sort.Slice(list, func(i, j int) bool { return list[i].Name < list[j].Name })
	return list, errors.Join(errs...)
}

// Delete removes the pipeline file. The run history in the state database
// is not touched.
func Delete(name string) error {
	path, err := filePath(name)
	if err != nil {
		return err
	}
	if err := os.Remove(path); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("pipeline %q: %w", name, ErrNotFound)
		}
		return err
	}
	return nil
}

// Rename moves a pipeline to a new name. It refuses to overwrite another
// pipeline.
func Rename(oldName, newName string) error {
	from, err := filePath(oldName)
	if err != nil {
		return err
	}
	to, err := filePath(newName)
	if err != nil {
		return err
	}
	p, err := read(oldName, from)
	if err != nil {
		return err
	}
	if oldName == newName {
		return nil
	}
	if _, err := os.Stat(to); err == nil {
		return fmt.Errorf("pipeline %q already exists", newName)
	}
	p.Name = newName
	if err := write(to, p); err != nil {
		return err
	}
	return os.Remove(from)
}
