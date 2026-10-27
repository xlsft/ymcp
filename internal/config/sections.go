package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// Section returns the raw JSON of a top-level section of the config file
// other than the profiles, such as "notifications", compacted; nil when
// it is absent.
func Section(name string) (json.RawMessage, error) {
	raw, err := readRaw()
	if err != nil || raw[name] == nil {
		return nil, err
	}
	var b bytes.Buffer
	if err := json.Compact(&b, raw[name]); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}

// SetSection stores value as the top-level section name of the config
// file, keeping everything else; nil or JSON null removes the section.
func SetSection(name string, value json.RawMessage) error {
	if name == "" || name == "profiles" {
		return fmt.Errorf("config section %q can't be set directly", name)
	}
	if len(value) > 0 && !json.Valid(value) {
		return fmt.Errorf("config section %q: invalid JSON", name)
	}
	return updateRaw(func(raw map[string]json.RawMessage) {
		if len(value) == 0 || bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
			delete(raw, name)
			return
		}
		raw[name] = value
	})
}

// readRaw reads the config file as its top-level keys; a missing file is
// empty.
func readRaw() (map[string]json.RawMessage, error) {
	path, err := Path()
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return map[string]json.RawMessage{}, nil
	}
	if err != nil {
		return nil, err
	}
	raw := map[string]json.RawMessage{}
	if len(bytes.TrimSpace(data)) == 0 {
		return raw, nil
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("config %s: %w", path, err)
	}
	if raw == nil {
		raw = map[string]json.RawMessage{}
	}
	return raw, nil
}

// updateRaw applies change to the top-level keys of the config file and
// writes it back atomically with mode 0600.
func updateRaw(change func(raw map[string]json.RawMessage)) error {
	path, err := Path()
	if err != nil {
		return err
	}
	raw, err := readRaw()
	if err != nil {
		return err
	}
	change(raw)
	if _, ok := raw["profiles"]; !ok {
		raw["profiles"] = json.RawMessage("{}")
	}
	data, err := json.MarshalIndent(raw, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(data, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
