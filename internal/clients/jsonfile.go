package clients

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/tailscale/hujson"
)

// readJSONC parses a JSON or JSONC file; a missing or empty file is an empty object.
func readJSONC(path string) (hujson.Value, bool, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) || (err == nil && strings.TrimSpace(string(data)) == "") {
		v, _ := hujson.Parse([]byte("{}"))
		return v, false, nil
	}
	if err != nil {
		return hujson.Value{}, false, err
	}
	v, err := hujson.Parse(data)
	if err != nil {
		return hujson.Value{}, false, fmt.Errorf("%s: %w", path, err)
	}
	return v, true, nil
}

func pointer(keys ...string) string {
	var b strings.Builder
	for _, k := range keys {
		k = strings.ReplaceAll(strings.ReplaceAll(k, "~", "~0"), "/", "~1")
		b.WriteString("/" + k)
	}
	return b.String()
}

// hasJSONEntry reports whether keyPath/name exists in the file.
func hasJSONEntry(path string, keyPath []string, name string) bool {
	v, existed, err := readJSONC(path)
	if err != nil || !existed {
		return false
	}
	return v.Find(pointer(append(append([]string{}, keyPath...), name)...)) != nil
}

// setJSONEntry sets keyPath/name to entry, or removes it when entry is nil,
// keeping every other key, comment and the original formatting. The old
// file is kept next to it as <file>.ymcp.bak.
func setJSONEntry(path string, keyPath []string, name string, entry any) error {
	v, existed, err := readJSONC(path)
	if err != nil {
		return err
	}
	target := pointer(append(append([]string{}, keyPath...), name)...)
	var ops []map[string]any
	if entry == nil {
		if v.Find(target) == nil {
			return nil
		}
		ops = append(ops, map[string]any{"op": "remove", "path": target})
	} else {
		for i := range keyPath {
			p := pointer(keyPath[:i+1]...)
			if v.Find(p) == nil {
				ops = append(ops, map[string]any{"op": "add", "path": p, "value": map[string]any{}})
			}
		}
		ops = append(ops, map[string]any{"op": "add", "path": target, "value": entry})
	}
	patch, err := json.Marshal(ops)
	if err != nil {
		return err
	}
	if err := v.Patch(patch); err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	if !existed {
		v.Format()
	}
	return writeFile(path, v.Pack(), existed)
}

// writeFile replaces path atomically, keeping its mode and a backup.
func writeFile(path string, data []byte, backup bool) error {
	mode := os.FileMode(0o644)
	if info, err := os.Stat(path); err == nil {
		mode = info.Mode().Perm()
		if backup {
			if old, err := os.ReadFile(path); err == nil {
				_ = os.WriteFile(path+".ymcp.bak", old, mode)
			}
		}
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".ymcp.tmp"
	if err := os.WriteFile(tmp, data, mode); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
