package vcs

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"github.com/zalando/go-keyring"
)

const keyringService = "ymcp"

// keyringUser is the keychain account of a host's token.
func keyringUser(host string) string { return "vcs:" + host }

// runCLI runs a credential helper and returns its stdout. A variable so
// tests can fake gh and glab.
var runCLI = func(ctx context.Context, name string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	var out bytes.Buffer
	cmd.Stdout = &out // stderr is dropped: it may echo configuration
	err := cmd.Run()
	return out.Bytes(), err
}

// TokenFor finds a token for repo's host: the gh CLI for GitHub hosts
// (`gh auth token --hostname <host>`), the glab CLI for GitLab hosts
// (`glab config get token --host <host>`), both for a host of unknown kind,
// then the OS keychain (service "ymcp", account "vcs:<host>", see SetToken).
// Tokens are never logged or put in errors.
func TokenFor(ctx context.Context, repo Repo) (Auth, error) {
	host := repo.Host
	if host == "" {
		return Auth{}, errors.New("vcs: no host to find a token for")
	}
	fam := familyOfKind(repo.Kind)
	if fam == "" {
		fam = family(host)
	}
	type helper struct {
		source string
		argv   []string
	}
	var helpers []helper
	gh := helper{"gh", []string{"gh", "auth", "token", "--hostname", host}}
	glab := helper{"glab", []string{"glab", "config", "get", "token", "--host", host}}
	switch fam {
	case familyGitHub:
		helpers = []helper{gh}
	case familyGitLab:
		helpers = []helper{glab}
	default:
		helpers = []helper{gh, glab}
	}
	for _, h := range helpers {
		out, err := runCLI(ctx, h.argv[0], h.argv[1:]...)
		if err != nil {
			continue
		}
		if tok := cleanToken(out); tok != "" {
			return Auth{Token: tok, Source: h.source}, nil
		}
	}
	if tok, err := keyring.Get(keyringService, keyringUser(host)); err == nil {
		if tok = strings.TrimSpace(tok); tok != "" {
			return Auth{Token: tok, Source: "keyring"}, nil
		}
	}
	hint := "store one with `ymcp pipeline`"
	switch fam {
	case familyGitHub:
		hint = "run `gh auth login --hostname " + host + "` or " + hint
	case familyGitLab:
		hint = "run `glab auth login --hostname " + host + "` or " + hint
	}
	return Auth{}, fmt.Errorf("%w for %s: %s", ErrNoToken, host, hint)
}

// cleanToken accepts a helper's output only when it is one token-like line.
func cleanToken(out []byte) string {
	tok := strings.TrimSpace(string(out))
	if tok == "" || strings.ContainsAny(tok, " \t\r\n") {
		return ""
	}
	return tok
}

// SetToken stores a token for host in the OS keychain; an empty token
// removes it.
func SetToken(host, token string) error {
	if host == "" {
		return errors.New("vcs: no host to store a token for")
	}
	token = strings.TrimSpace(token)
	if token == "" {
		if err := keyring.Delete(keyringService, keyringUser(host)); err != nil && !errors.Is(err, keyring.ErrNotFound) {
			return fmt.Errorf("vcs: removing the token for %s from the keychain: %w", host, err)
		}
		return nil
	}
	if err := keyring.Set(keyringService, keyringUser(host), token); err != nil {
		return fmt.Errorf("vcs: saving the token for %s in the keychain: %w", host, err)
	}
	return nil
}
