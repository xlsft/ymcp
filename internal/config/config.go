// Package config stores the YouTrack connection profiles of ymcp.
//
// Each profile is one YouTrack instance: a display name, URL, login and
// proxy in a JSON file in the user config directory, and the password in
// the OS keychain (macOS Keychain, Secret Service, Windows Credential
// Manager), falling back to the file, with mode 0600, where no keychain is
// available. A profile's key, derived from its display name, is also the
// name ymcp is registered under in MCP clients. YOUTRACK_* environment
// variables override whatever is stored.
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/zalando/go-keyring"
)

const keyringService = "ymcp"

// DefaultProfile is the key of the profile used when none is chosen.
const DefaultProfile = "youtrack"

// Config is a YouTrack connection profile.
type Config struct {
	Name        string `json:"-"` // the profile key, e.g. youtrack-beget
	DisplayName string `json:"displayName,omitempty"`
	URL         string `json:"url"`
	Login       string `json:"login"`
	Proxy       string `json:"proxy,omitempty"`
	// Tools are the tool groups or tools this profile serves, e.g.
	// ["issues", "articles", "-delete_issue"]; empty means all.
	Tools    []string `json:"tools,omitempty"`
	Password string   `json:"password,omitempty"` // only when the keychain is unavailable
}

// Complete reports whether the config is enough to log in.
func (c Config) Complete() bool {
	return c.URL != "" && c.Login != "" && c.Password != ""
}

// Title is the display name, or the key when there is none.
func (c Config) Title() string {
	if c.DisplayName != "" {
		return c.DisplayName
	}
	return c.Name
}

// Slug turns a display name into a profile key: "YouTrack (xlsft)" becomes
// "youtrack-xlsft". Letters and digits are kept, lowercased; everything
// else becomes a single dash.
func Slug(display string) string {
	var b strings.Builder
	dash := false
	for _, r := range strings.ToLower(display) {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' {
			b.WriteRune(r)
			dash = false
		} else if !dash && b.Len() > 0 {
			b.WriteByte('-')
			dash = true
		}
	}
	key := strings.TrimRight(b.String(), "-")
	if key == "" {
		return DefaultProfile
	}
	return key
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

// file is the config file. Before profiles it held a single connection at
// the top level, which is read as the default profile.
type file struct {
	Profiles map[string]Config `json:"profiles"`

	URL      string `json:"url,omitempty"`
	Login    string `json:"login,omitempty"`
	Proxy    string `json:"proxy,omitempty"`
	Password string `json:"password,omitempty"`
}

func readProfiles() (map[string]Config, error) {
	path, err := Path()
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return map[string]Config{}, nil
	}
	if err != nil {
		return nil, err
	}
	var f file
	if err := json.Unmarshal(data, &f); err != nil {
		return nil, fmt.Errorf("config %s: %w", path, err)
	}
	profiles := f.Profiles
	if profiles == nil {
		profiles = map[string]Config{}
	}
	if len(profiles) == 0 && f.URL != "" {
		profiles[DefaultProfile] = Config{URL: f.URL, Login: f.Login, Proxy: f.Proxy, Password: f.Password}
	}
	for key, c := range profiles {
		c.Name = key
		profiles[key] = c
	}
	return profiles, nil
}

// writeProfiles replaces the profiles in the config file, keeping its other
// sections (see Section) and dropping the legacy top-level connection.
func writeProfiles(profiles map[string]Config) error {
	data, err := json.Marshal(profiles)
	if err != nil {
		return err
	}
	return updateRaw(func(raw map[string]json.RawMessage) {
		for _, legacy := range []string{"url", "login", "proxy", "password"} {
			delete(raw, legacy)
		}
		raw["profiles"] = data
	})
}

// Profiles lists the stored profiles by key, without passwords.
func Profiles() ([]Config, error) {
	profiles, err := readProfiles()
	if err != nil {
		return nil, err
	}
	list := make([]Config, 0, len(profiles))
	for _, c := range profiles {
		c.Password = ""
		list = append(list, c)
	}
	sort.Slice(list, func(i, j int) bool { return list[i].Name < list[j].Name })
	return list, nil
}

// Resolve picks the profile key to use: name if given, else YMCP_PROFILE,
// else the default profile, else the only one stored.
func Resolve(name string) (string, error) {
	if name != "" {
		return name, nil
	}
	if env := os.Getenv("YMCP_PROFILE"); env != "" {
		return env, nil
	}
	profiles, err := readProfiles()
	if err != nil {
		return "", err
	}
	if _, ok := profiles[DefaultProfile]; ok || len(profiles) == 0 {
		return DefaultProfile, nil
	}
	if len(profiles) == 1 {
		for key := range profiles {
			return key, nil
		}
	}
	keys := make([]string, 0, len(profiles))
	for key := range profiles {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return "", fmt.Errorf("several profiles are configured, pick one with --profile: %s", strings.Join(keys, ", "))
}

// Load returns the profile name resolves to, with its password from the
// keychain and environment overrides applied. A profile that isn't stored
// yields an empty config, so environment variables alone can work.
func Load(name string) (Config, error) {
	key, err := Resolve(name)
	if err != nil {
		return Config{}, err
	}
	profiles, err := readProfiles()
	if err != nil {
		return Config{}, err
	}
	c := profiles[key]
	c.Name = key
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

// Save stores c under c.Name (a slug of the display name when empty),
// replacing the profile previously stored under oldName, if any, so a
// renamed profile moves. It reports where the password went: "keychain",
// or "file" when no keychain is available.
func Save(c Config, oldName string) (passwordStore string, err error) {
	if c.Name == "" {
		c.Name = Slug(c.DisplayName)
	}
	profiles, err := readProfiles()
	if err != nil {
		return "", err
	}
	previous, hadPrevious := profiles[c.Name]
	if oldName != "" && oldName != c.Name {
		previous, hadPrevious = profiles[oldName]
		delete(profiles, oldName)
	}

	stored := c
	passwordStore = "keychain"
	if err := keyring.Set(keyringService, account(c), c.Password); err == nil {
		stored.Password = ""
	} else {
		passwordStore = "file"
	}
	profiles[c.Name] = stored
	if err := writeProfiles(profiles); err != nil {
		return "", err
	}
	// Forget the password of the login this profile had before, unless
	// another profile still signs in with it.
	if hadPrevious && previous.URL != "" && account(previous) != account(c) {
		forgetUnused(profiles, previous)
	}
	return passwordStore, nil
}

// Delete removes a profile and, if no other profile uses it, its password.
func Delete(name string) error {
	profiles, err := readProfiles()
	if err != nil {
		return err
	}
	c, ok := profiles[name]
	if !ok {
		return fmt.Errorf("no profile %q", name)
	}
	delete(profiles, name)
	if err := writeProfiles(profiles); err != nil {
		return err
	}
	forgetUnused(profiles, c)
	return nil
}

func forgetUnused(profiles map[string]Config, c Config) {
	for _, other := range profiles {
		if account(other) == account(c) {
			return
		}
	}
	_ = keyring.Delete(keyringService, account(c))
}

// account is the keychain entry name, login@host.
func account(c Config) string {
	host := c.URL
	if u, err := url.Parse(c.URL); err == nil && u.Host != "" {
		host = u.Host
	}
	return c.Login + "@" + host
}
