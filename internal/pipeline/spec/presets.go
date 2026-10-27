package spec

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"slices"
	"sync"
)

// Preset is a named set of the three prompts: built into ymcp, or saved by
// the user (see SavePreset). A pipeline copies its texts, so changing or
// deleting a preset later never alters a pipeline.
type Preset struct {
	Name      string `json:"name"`
	Rationale string `json:"rationale"`
	Start     string `json:"start"`
	Feedback  string `json:"feedback"`
	Answer    string `json:"answer"`
	// Builtin is set on the presets embedded in ymcp, which are read-only.
	Builtin bool `json:"-"`
}

//go:embed presets.json
var presetsJSON []byte

var (
	presetsOnce sync.Once
	presets     []Preset
)

func loadPresets() []Preset {
	presetsOnce.Do(func() {
		if err := json.Unmarshal(presetsJSON, &presets); err != nil {
			panic(fmt.Sprintf("spec: embedded presets.json is broken: %v", err))
		}
		for i := range presets {
			presets[i].Builtin = true
		}
	})
	return presets
}

// Presets returns the built-in presets in their documented order: fix (the
// default), bugfix-test-first, feature, refactor, docs, investigate.
func Presets() []Preset { return slices.Clone(loadPresets()) }

// builtinPreset finds a built-in preset by name.
func builtinPreset(name string) (Preset, bool) {
	for _, p := range loadPresets() {
		if p.Name == name {
			return p, true
		}
	}
	return Preset{}, false
}

// IsBuiltinPreset reports whether name is a built-in preset.
func IsBuiltinPreset(name string) bool {
	_, ok := builtinPreset(name)
	return ok
}

// LookupPreset finds a preset by name: a built-in one, or else a user
// preset. A user preset whose file cannot be read is not found; LoadPreset
// tells why.
func LookupPreset(name string) (Preset, bool) {
	p, err := LoadPreset(name)
	return p, err == nil
}

func presetNames() []string {
	var names []string
	for _, p := range loadPresets() {
		names = append(names, p.Name)
	}
	return names
}
