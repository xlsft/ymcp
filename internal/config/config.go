// Package config stores the YouTrack connection settings of ymcp.
//
// The URL, login and proxy live in a JSON file in the user config
// directory; the password goes to the OS keychain (macOS Keychain, Secret
// Service, Windows Credential Manager) and only falls back to the file,
// with mode 0600, where no keychain is available. YOUTRACK_* environment
// variables override whatever is stored.
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"

	"github.com/zalando/go-keyring"
)

const keyringService = "ymcp"

// Config is a YouTrack connection.
type Config struct {
	URL      string `json:"url"`
	Login    string `json:"login"`
	Proxy    string `json:"proxy,omitempty"`
	Password string `json:"password,omitempty"` // only when the keychain is unavailable
}

// Complete reports whether the config is enough to log in.
func (c Config) Complete() bool {
	return c.URL != "" && c.Login != "" && c.Password != ""
}

// Path is the config file location; YMCP_CONFIG overrides it.
func Path() (string, error) {
	if p := os.Getenv("YMCP_CONFIG"); p != "" {
		return p, nil
	}
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "ymcp", "config.json"), nil
}

// Load reads the stored config, fills the password from the keychain and
// applies environment overrides. A missing file is not an error.
func Load() (Config, error) {
	var c Config
	path, err := Path()
	if err != nil {
		return c, err
	}
	data, err := os.ReadFile(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
	case err != nil:
		return c, err
	default:
		if err := json.Unmarshal(data, &c); err != nil {
			return c, fmt.Errorf("config %s: %w", path, err)
		}
	}
	if c.Password == "" && c.URL != "" && c.Login != "" {
		if pw, err := keyring.Get(keyringService, account(c)); err == nil {
			c.Password = pw
		}
	}
	for env, field := range map[string]*string{
		"YOUTRACK_URL":      &c.URL,
		"YOUTRACK_LOGIN":    &c.Login,
		"YOUTRACK_PASSWORD": &c.Password,
		"YOUTRACK_PROXY":    &c.Proxy,
	} {
		if v, ok := os.LookupEnv(env); ok && v != "" {
			*field = v
		}
	}
	return c, nil
}

// Save stores the config. It reports where the password went: "keychain"
// or "file" when no keychain is available.
func Save(c Config) (passwordStore string, err error) {
	path, err := Path()
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return "", err
	}

	// Forget the password stored for a previous login, if it changed.
	if old, err := loadFile(path); err == nil && old.URL != "" && old.Login != "" && account(old) != account(c) {
		_ = keyring.Delete(keyringService, account(old))
	}

	stored := c
	passwordStore = "keychain"
	if err := keyring.Set(keyringService, account(c), c.Password); err == nil {
		stored.Password = ""
	} else {
		passwordStore = "file"
	}
	data, err := json.MarshalIndent(stored, "", "  ")
	if err != nil {
		return "", err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(data, '\n'), 0o600); err != nil {
		return "", err
	}
	if err := os.Rename(tmp, path); err != nil {
		return "", err
	}
	return passwordStore, nil
}

func loadFile(path string) (Config, error) {
	var c Config
	data, err := os.ReadFile(path)
	if err != nil {
		return c, err
	}
	return c, json.Unmarshal(data, &c)
}

// account is the keychain entry name, login@host.
func account(c Config) string {
	host := c.URL
	if u, err := url.Parse(c.URL); err == nil && u.Host != "" {
		host = u.Host
	}
	return c.Login + "@" + host
}
