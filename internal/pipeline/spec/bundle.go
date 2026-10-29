package spec

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strconv"
)

// The format and version of an export file. A file of a newer version is
// refused rather than read halfway.
const (
	BundleFormat  = "ymcp-pipelines"
	BundleVersion = 1
)

// Bundle is an export file: pipelines and the user presets they were made
// from, so their prompts still resolve on the machine that imports them.
// Built-in presets are not included; every ymcp has them.
type Bundle struct {
	Format    string     `json:"format"`
	Version   int        `json:"version"`
	Pipelines []Pipeline `json:"pipelines"`
	Presets   []Preset   `json:"presets,omitempty"`
}

// Export bundles the named pipelines with the user presets they name. A
// preset that is gone is left out: the pipeline keeps its own copies of
// the texts.
func Export(names ...string) (Bundle, error) {
	b := Bundle{Format: BundleFormat, Version: BundleVersion}
	seen := map[string]bool{}
	for _, name := range names {
		p, err := Load(name)
		if err != nil {
			return Bundle{}, err
		}
		b.Pipelines = append(b.Pipelines, p)
		preset := p.Prompts.Preset
		if seen[preset] || IsBuiltinPreset(preset) {
			continue
		}
		seen[preset] = true
		pr, err := LoadPreset(preset)
		if errors.Is(err, ErrPresetNotFound) {
			continue
		}
		if err != nil {
			return Bundle{}, err
		}
		b.Presets = append(b.Presets, pr)
	}
	return b, nil
}

// WriteBundle writes b to path, atomically, replacing what is there.
func WriteBundle(path string, b Bundle) error { return writeJSON(path, b) }

// ReadBundle reads an export file. Pipelines get their defaults; nothing
// is validated, so half-finished pipelines can be moved too.
func ReadBundle(path string) (Bundle, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Bundle{}, err
	}
	var b Bundle
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&b); err != nil {
		return Bundle{}, fmt.Errorf("%s is not a ymcp pipelines file: %w", path, err)
	}
	switch {
	case b.Format != BundleFormat:
		return Bundle{}, fmt.Errorf("%s is not a ymcp pipelines file", path)
	case b.Version > BundleVersion:
		return Bundle{}, fmt.Errorf("%s was exported by a newer ymcp (version %d); update ymcp to import it", path, b.Version)
	}
	for i, p := range b.Pipelines {
		if err := CheckName(p.Name); err != nil {
			return Bundle{}, fmt.Errorf("%s: %w", path, err)
		}
		b.Pipelines[i] = p.WithDefaults()
	}
	for _, p := range b.Presets {
		if err := p.Validate(); err != nil {
			return Bundle{}, fmt.Errorf("%s: %w", path, err)
		}
	}
	return b, nil
}

// Imported is one pipeline or preset an import saved: its name in the
// file and the name it got, which differs when the first was taken.
type Imported struct {
	From, To string
}

// Renamed reports whether the name was taken and another one was given.
func (i Imported) Renamed() bool { return i.From != i.To }

// ImportResult is what an import saved. A preset identical to one already
// there is reused, not saved again, and is not listed.
type ImportResult struct {
	Pipelines []Imported
	Presets   []Imported
}

// Import saves the pipelines and presets of b. Nothing is overwritten: a
// taken name gets a free one with a number ("backend-2"), and the
// pipelines follow their presets' new names. A non-empty profile moves
// every pipeline to that profile.
func Import(b Bundle, profile string) (ImportResult, error) {
	var res ImportResult
	presetTo := map[string]string{}
	for _, pr := range b.Presets {
		to, saved, err := importPreset(pr)
		if err != nil {
			return res, err
		}
		presetTo[pr.Name] = to
		if saved {
			res.Presets = append(res.Presets, Imported{pr.Name, to})
		}
	}
	for _, p := range b.Pipelines {
		from := p.Name
		to, err := freeName(from, pipelineTaken)
		if err != nil {
			return res, err
		}
		p.Name = to
		if profile != "" {
			p.Profile = profile
		}
		if n, ok := presetTo[p.Prompts.Preset]; ok {
			p.Prompts.Preset = n
		}
		if err := Save(p); err != nil {
			return res, err
		}
		res.Pipelines = append(res.Pipelines, Imported{from, to})
	}
	return res, nil
}

// importPreset saves a preset under a free name and returns that name.
// When a user preset of the same name has the same texts already, it is
// reused: its name is returned and saved is false.
func importPreset(pr Preset) (to string, saved bool, err error) {
	if !IsBuiltinPreset(pr.Name) {
		have, err := LoadPreset(pr.Name)
		switch {
		case err == nil && samePreset(have, pr):
			return pr.Name, false, nil
		case err != nil && !errors.Is(err, ErrPresetNotFound):
			return "", false, err
		}
	}
	if to, err = freeName(pr.Name, PresetExists); err != nil {
		return "", false, err
	}
	pr.Name = to
	return to, true, SavePreset(pr)
}

func samePreset(a, b Preset) bool {
	return a.Rationale == b.Rationale && a.Start == b.Start && a.Feedback == b.Feedback && a.Answer == b.Answer
}

func pipelineTaken(name string) (bool, error) {
	_, err := Load(name)
	if errors.Is(err, ErrNotFound) {
		return false, nil
	}
	// A file that cannot be read is still taken.
	return true, nil
}

// freeName is name, or else the first of name-2, name-3… that is not
// taken, shortened to stay a valid name.
func freeName(name string, taken func(string) (bool, error)) (string, error) {
	for n := 1; ; n++ {
		try := name
		if n > 1 {
			suffix := "-" + strconv.Itoa(n)
			try = name[:min(len(name), 40-len(suffix))] + suffix
		}
		t, err := taken(try)
		if err != nil {
			return "", err
		}
		if !t {
			return try, nil
		}
	}
}
