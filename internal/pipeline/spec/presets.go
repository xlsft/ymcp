package spec

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"slices"
	"sync"
)

// Preset is a built-in set of the three prompts. A pipeline copies its
// texts, so changing a preset later never alters a pipeline.
type Preset struct {
	Name      string `json:"name"`
	Rationale string `json:"rationale"`
	Start     string `json:"start"`
	Feedback  string `json:"feedback"`
	Answer    string `json:"answer"`
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
	})
	return presets
}

// Presets returns the built-in presets in their documented order: fix (the
// default), bugfix-test-first, feature, refactor, docs, investigate.
func Presets() []Preset { return slices.Clone(loadPresets()) }

// LookupPreset finds a built-in preset by name.
func LookupPreset(name string) (Preset, bool) {
	for _, p := range loadPresets() {
		if p.Name == name {
			return p, true
		}
	}
	return Preset{}, false
}

func presetNames() []string {
	var names []string
	for _, p := range loadPresets() {
		names = append(names, p.Name)
	}
	return names
}
