package notify

import (
	"bytes"
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/xlsft/ymcp/internal/config"
)

// configSection is the key of the settings in config.json.
const configSection = "notifications"

// Settings are the machine-wide notification settings: each provider's
// section by provider name.
type Settings struct {
	Providers map[string]json.RawMessage `json:"providers,omitempty"`
}

// Common are the settings every provider understands. Providers embed it in
// their own settings type so their sections decode in one go.
type Common struct {
	Enabled *bool  `json:"enabled,omitempty"` // nil: see DefaultEnabler
	Events  []Kind `json:"events,omitempty"`  // empty: DefaultEvents
}

// Load reads the settings from config.json; none stored is empty Settings.
func Load() (Settings, error) {
	raw, err := config.Section(configSection)
	if err != nil || raw == nil {
		return Settings{}, err
	}
	var s Settings
	if err := json.Unmarshal(raw, &s); err != nil {
		return Settings{}, fmt.Errorf("notification settings: %w", err)
	}
	return s, nil
}

// Save writes the settings to config.json, keeping its other sections.
// Empty settings remove the section.
func Save(s Settings) error {
	if len(s.Providers) == 0 {
		return config.SetSection(configSection, nil)
	}
	data, err := json.Marshal(s)
	if err != nil {
		return err
	}
	return config.SetSection(configSection, data)
}

// ProviderState is how a registered provider is set up.
type ProviderState struct {
	Name     string
	Enabled  bool
	Events   []Kind
	Settings json.RawMessage // the stored section, nil when there is none
}

// State lists every registered provider with its effective common
// settings, sorted by name.
func (s Settings) State() ([]ProviderState, error) {
	var out []ProviderState
	for _, name := range Providers() {
		st, err := s.state(name)
		if err != nil {
			return nil, err
		}
		out = append(out, st)
	}
	return out, nil
}

func (s Settings) state(name string) (ProviderState, error) {
	f, ok := factory(name)
	if !ok {
		return ProviderState{}, fmt.Errorf("unknown notification provider %q (known: %s)", name, strings.Join(Providers(), ", "))
	}
	raw := s.Providers[name]
	var c Common
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &c); err != nil {
			return ProviderState{}, fmt.Errorf("notification provider %s: %w", name, err)
		}
	}
	st := ProviderState{Name: name, Settings: raw}
	if c.Enabled != nil {
		st.Enabled = *c.Enabled
	} else if d, ok := f().(DefaultEnabler); ok {
		st.Enabled = d.EnabledByDefault()
	}
	st.Events = DefaultEvents
	if len(c.Events) > 0 {
		for _, k := range c.Events {
			if _, err := ParseKind(string(k)); err != nil {
				return ProviderState{}, fmt.Errorf("notification provider %s: %w", name, err)
			}
		}
		st.Events = c.Events
	}
	st.Events = slices.Clone(st.Events)
	return st, nil
}

// Set changes one setting given as "provider.field" and a value, as typed
// on the command line: toast.enabled=false, toast.events=needs_input,run_failed,
// toast.sound=false. An empty value removes the field, which brings back
// its default. The result must be accepted by the provider's Configure.
func (s *Settings) Set(key, value string) error {
	name, field, ok := strings.Cut(key, ".")
	if !ok || name == "" || field == "" {
		return fmt.Errorf("setting %q: use provider.field, e.g. toast.enabled", key)
	}
	f, known := factory(name)
	if !known {
		return fmt.Errorf("unknown notification provider %q (known: %s)", name, strings.Join(Providers(), ", "))
	}
	section := map[string]json.RawMessage{}
	if raw := s.Providers[name]; len(raw) > 0 {
		if err := json.Unmarshal(raw, &section); err != nil {
			return fmt.Errorf("notification provider %s: %w", name, err)
		}
	}
	value = strings.TrimSpace(value)
	if value == "" {
		delete(section, field)
	} else {
		v, err := settingValue(field, value)
		if err != nil {
			return fmt.Errorf("%s: %w", key, err)
		}
		section[field] = v
	}
	var raw json.RawMessage
	if len(section) > 0 {
		data, err := json.Marshal(section)
		if err != nil {
			return err
		}
		raw = data
	}
	if err := f().Configure(raw); err != nil {
		return fmt.Errorf("%s: %w", key, err)
	}
	if s.Providers == nil {
		s.Providers = map[string]json.RawMessage{}
	}
	if raw == nil {
		delete(s.Providers, name)
	} else {
		s.Providers[name] = raw
	}
	return nil
}

// settingValue turns a command-line value into JSON: events is a comma
// list of kinds, enabled a boolean; anything else is taken as JSON when it
// parses (true, 3, "x", […]) and as a string otherwise.
func settingValue(field, value string) (json.RawMessage, error) {
	switch field {
	case "enabled":
		b, err := strconv.ParseBool(value)
		if err != nil {
			return nil, fmt.Errorf("%q is not true or false", value)
		}
		return json.Marshal(b)
	case "events":
		var kinds []Kind
		for _, part := range strings.Split(value, ",") {
			if strings.TrimSpace(part) == "" {
				continue
			}
			k, err := ParseKind(part)
			if err != nil {
				return nil, err
			}
			if !slices.Contains(kinds, k) {
				kinds = append(kinds, k)
			}
		}
		if len(kinds) == 0 {
			return nil, fmt.Errorf("no events given")
		}
		return json.Marshal(kinds)
	}
	if json.Valid([]byte(value)) {
		var b bytes.Buffer
		if err := json.Compact(&b, []byte(value)); err == nil {
			return b.Bytes(), nil
		}
	}
	return json.Marshal(value)
}

// DecodeSettings decodes a provider's section into v, rejecting unknown
// fields so typos are caught. Providers use it in Configure; v should embed
// Common. A nil section leaves v as it is.
func DecodeSettings(raw json.RawMessage, v any) error {
	if len(bytes.TrimSpace(raw)) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return nil
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return err
	}
	return nil
}
