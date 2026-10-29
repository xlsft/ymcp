// Package endpoint is the model API behind the anthropic and openai agent
// providers: Claude Code is pointed at ymcp's gateway, which passes its
// requests on to an Anthropic-compatible API, or translates them for an
// OpenAI-compatible (Chat Completions) one. The API key stays with ymcp:
// the agent only gets the run's own token for the gateway.
//
// Presets know the URLs and the usual key variable of a few services;
// keys are kept in the OS keychain or read from the environment.
package endpoint

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"

	"github.com/zalando/go-keyring"
)

// Protocols, which are also the names of the agent providers.
const (
	Anthropic = "anthropic" // the Anthropic Messages API
	OpenAI    = "openai"    // the OpenAI Chat Completions API
)

// Preset is a known service with its API base URLs.
type Preset struct {
	Name, Title string
	// Anthropic and OpenAI are the base URLs of the two APIs; empty when
	// the service has no such API. Claude Code appends /v1/messages to the
	// first, the gateway /chat/completions to the second.
	Anthropic, OpenAI string
	// KeyEnv is the usual variable with the API key; empty for a service
	// that needs no key.
	KeyEnv string
	// Model is the default model; empty when the user has to pick one.
	Model string
	About string
}

// Presets are the known services, in the order the wizard offers them.
var Presets = []Preset{
	{
		Name: "openrouter", Title: "OpenRouter",
		Anthropic: "https://openrouter.ai/api", OpenAI: "https://openrouter.ai/api/v1",
		KeyEnv: "OPENROUTER_API_KEY",
		About:  "hundreds of models behind one key, some of them free",
	},
	{
		Name: "kimi-coding", Title: "Kimi for Coding",
		Anthropic: "https://api.kimi.com/coding", OpenAI: "https://api.kimi.com/coding/v1",
		KeyEnv: "KIMI_API_KEY", Model: "kimi-for-coding",
		About: "the Kimi Code plan of Moonshot AI",
	},
	{
		Name: "zai", Title: "Z.ai",
		Anthropic: "https://api.z.ai/api/anthropic", OpenAI: "https://api.z.ai/api/coding/paas/v4",
		KeyEnv: "ZAI_API_KEY",
		About:  "GLM models of the GLM Coding Plan",
	},
	{
		Name: "deepseek", Title: "DeepSeek",
		Anthropic: "https://api.deepseek.com/anthropic", OpenAI: "https://api.deepseek.com/v1",
		KeyEnv: "DEEPSEEK_API_KEY", Model: "deepseek-chat",
		About: "DeepSeek's own API",
	},
	{
		Name: "minimax", Title: "MiniMax",
		Anthropic: "https://api.minimax.io/anthropic", OpenAI: "https://api.minimax.io/v1",
		KeyEnv: "MINIMAX_API_KEY",
		About:  "MiniMax's own API",
	},
	{
		Name: "ollama", Title: "Ollama",
		Anthropic: "http://localhost:11434", OpenAI: "http://localhost:11434/v1",
		About: "local models on the machine ymcp runs on, no key",
	},
}

// Find returns the preset of a name.
func Find(name string) (Preset, bool) {
	for _, p := range Presets {
		if p.Name == name {
			return p, true
		}
	}
	return Preset{}, false
}

// URL returns the preset's base URL for a protocol, empty when it has
// none.
func (p Preset) URL(protocol string) string {
	switch protocol {
	case Anthropic:
		return p.Anthropic
	case OpenAI:
		return p.OpenAI
	}
	return ""
}

// Upstream is the API the gateway talks to.
type Upstream struct {
	Protocol string // Anthropic or OpenAI
	URL      string // base URL, without a trailing slash
	Key      string // API key; empty sends none
}

// CheckURL fails for a base URL the gateway can't use.
func CheckURL(raw string) error {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return err
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("%q is not an http or https URL", raw)
	}
	if u.Host == "" {
		return fmt.Errorf("%q has no host", raw)
	}
	if u.RawQuery != "" || u.Fragment != "" {
		return fmt.Errorf("%q must not have a query or a fragment", raw)
	}
	return nil
}

const keyringService = "ymcp"

// account is the keychain entry of the key of an API: llm:<host>, where
// host may carry a port.
func account(baseURL string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(baseURL))
	if err != nil || u.Host == "" {
		return "", fmt.Errorf("endpoint: no host in %q", baseURL)
	}
	return "llm:" + strings.ToLower(u.Host), nil
}

// ErrNoKey means no API key was found for an endpoint.
var ErrNoKey = errors.New("no API key")

// Key finds the API key of the API at baseURL: the variable keyEnv when
// it is set and not empty, then the OS keychain (service "ymcp", account
// "llm:<host>", see SetKey). source says where it came from: the
// variable's name or "keychain". A missing key is ErrNoKey.
func Key(baseURL, keyEnv string) (key, source string, err error) {
	if keyEnv != "" {
		if v := strings.TrimSpace(os.Getenv(keyEnv)); v != "" {
			return v, keyEnv, nil
		}
	}
	acc, err := account(baseURL)
	if err != nil {
		return "", "", err
	}
	if v, err := keyring.Get(keyringService, acc); err == nil {
		if v = strings.TrimSpace(v); v != "" {
			return v, "keychain", nil
		}
	}
	where := "the keychain"
	if keyEnv != "" {
		where = keyEnv + " or the keychain"
	}
	return "", "", fmt.Errorf("%w for %s in %s", ErrNoKey, acc[len("llm:"):], where)
}

// SetKey keeps the API key of the API at baseURL in the OS keychain; an
// empty key removes it. Every API on the same host shares the key.
func SetKey(baseURL, key string) error {
	acc, err := account(baseURL)
	if err != nil {
		return err
	}
	if key = strings.TrimSpace(key); key == "" {
		if err := keyring.Delete(keyringService, acc); err != nil && !errors.Is(err, keyring.ErrNotFound) {
			return fmt.Errorf("endpoint: removing the key from the keychain: %w", err)
		}
		return nil
	}
	if err := keyring.Set(keyringService, acc, key); err != nil {
		return fmt.Errorf("endpoint: keeping the key in the keychain: %w", err)
	}
	return nil
}
