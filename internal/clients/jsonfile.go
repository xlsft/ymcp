package clients

import (
	"bytes"
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
	original := string(v.Pack())
	keys := append(append([]string{}, keyPath...), name)
	var isNew []bool
	for i := range keys {
		isNew = append(isNew, v.Find(pointer(keys[:i+1]...)) == nil)
	}
	if err := v.Patch(patch); err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	// Plain JSON (no comments or trailing commas) is re-indented with two
	// spaces, keeping key order. JSONC keeps its hand-written formatting
	// and comments; only what the patch added, which it puts on one line,
	// is laid out like its siblings.
	if entry != nil && !isPlainJSON(original) {
		if err := layout(&v, keys, isNew, entry); err != nil {
			return fmt.Errorf("%s: %w", path, err)
		}
	}
	out := v.Pack()
	if isPlainJSON(original) {
		var buf bytes.Buffer
		if err := json.Indent(&buf, out, "", "  "); err == nil {
			out = append(buf.Bytes(), '\n')
		}
	}
	return writeFile(path, out, existed)
}

// layout indents the members at keys that the patch added (isNew), and
// the entry's value at the end of keys, like the members around them.
func layout(root *hujson.Value, keys []string, isNew []bool, entry any) error {
	unit := "  "
	if obj, ok := root.Value.(*hujson.Object); ok {
		for _, m := range obj.Members {
			if ind, ok := indentOf(m.Name.BeforeExtra); ok && ind != "" {
				unit = ind
				break
			}
		}
	}
	cur, indent := root, ""
	for depth, key := range keys {
		obj, ok := cur.Value.(*hujson.Object)
		if !ok {
			return nil
		}
		idx, child := -1, indent+unit
		for i, m := range obj.Members {
			if lit, ok := m.Name.Value.(hujson.Literal); ok && lit.String() == key {
				idx = i
			} else if ind, ok := indentOf(m.Name.BeforeExtra); ok {
				child = ind
			}
		}
		if idx < 0 {
			return nil
		}
		m := &obj.Members[idx]
		if isNew[depth] {
			m.Name.BeforeExtra = hujson.Extra("\n" + child)
			m.Value.BeforeExtra = hujson.Extra(" ")
			if idx == len(obj.Members)-1 && !strings.Contains(string(obj.AfterExtra), "\n") {
				obj.AfterExtra = hujson.Extra("\n" + indent)
			}
		}
		if depth == len(keys)-1 {
			data, err := json.MarshalIndent(entry, child, unit)
			if err != nil {
				return err
			}
			val, err := hujson.Parse(data)
			if err != nil {
				return err
			}
			m.Value.Value = val.Value
		}
		cur, indent = &m.Value, child
	}
	return nil
}

// indentOf is the indentation on the last line of extra, if it ends a line.
func indentOf(extra hujson.Extra) (string, bool) {
	s := string(extra)
	i := strings.LastIndex(s, "\n")
	if i < 0 || strings.TrimLeft(s[i+1:], " \t") != "" {
		return "", false
	}
	return s[i+1:], true
}

func isPlainJSON(s string) bool {
	return json.Valid([]byte(s))
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
