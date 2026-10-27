package vcs

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/zalando/go-keyring"
)

// fakeCLI replaces gh and glab: answers maps "name args…" to stdout; a
// missing entry fails like a CLI without a login.
func fakeCLI(t *testing.T, answers map[string]string) *[]string {
	t.Helper()
	var calls []string
	old := runCLI
	runCLI = func(_ context.Context, name string, args ...string) ([]byte, error) {
		line := name + " " + strings.Join(args, " ")
		calls = append(calls, line)
		out, ok := answers[line]
		if !ok {
			return nil, errors.New("exit status 1")
		}
		return []byte(out), nil
	}
	t.Cleanup(func() { runCLI = old })
	return &calls
}

func TestTokenFor(t *testing.T) {
	keyring.MockInit()
	ctx := context.Background()

	calls := fakeCLI(t, map[string]string{
		"gh auth token --hostname github.com":            "gho_abc\n",
		"glab config get token --host gitlab.example.ru": "  glpat-xyz  \n",
		"glab config get token --host gitlab.com":        "",
		"gh auth token --hostname github.example.com":    "no oauth token found for github.example.com\n",
	})
	a, err := TokenFor(ctx, Repo{Host: "github.com", Kind: KindGitHubPR})
	if err != nil || a.Token != "gho_abc" || a.Source != "gh" {
		t.Errorf("github = %+v, %v", a, err)
	}
	a, err = TokenFor(ctx, Repo{Host: "gitlab.example.ru", Kind: KindGitLabMR})
	if err != nil || a.Token != "glpat-xyz" || a.Source != "glab" {
		t.Errorf("gitlab = %+v, %v", a, err)
	}
	if strings.Join(*calls, "|") != "gh auth token --hostname github.com|glab config get token --host gitlab.example.ru" {
		t.Errorf("calls = %v", *calls)
	}

	// Nothing from the CLIs: the keychain.
	if _, err := TokenFor(ctx, Repo{Host: "gitlab.com", Kind: KindGitLabMR}); !errors.Is(err, ErrNoToken) || !strings.Contains(err.Error(), "glab auth login") {
		t.Errorf("no token: %v", err)
	}
	if err := SetToken("gitlab.com", " glpat-stored "); err != nil {
		t.Fatal(err)
	}
	a, err = TokenFor(ctx, Repo{Host: "gitlab.com", Kind: KindGitLabMR})
	if err != nil || a.Token != "glpat-stored" || a.Source != "keyring" {
		t.Errorf("keyring = %+v, %v", a, err)
	}
	if v, _ := keyring.Get("ymcp", "vcs:gitlab.com"); v != "glpat-stored" {
		t.Errorf("keychain entry = %q", v)
	}
	if err := SetToken("gitlab.com", ""); err != nil {
		t.Fatal(err)
	}
	if err := SetToken("gitlab.com", ""); err != nil {
		t.Errorf("removing twice: %v", err)
	}
	if _, err := TokenFor(ctx, Repo{Host: "gitlab.com", Kind: KindGitLabMR}); !errors.Is(err, ErrNoToken) {
		t.Errorf("after removal: %v", err)
	}

	// A helper printing a message instead of a token is not a token.
	if _, err := TokenFor(ctx, Repo{Host: "github.example.com", Kind: KindGitHubPR}); !errors.Is(err, ErrNoToken) {
		t.Errorf("message as token: %v", err)
	}

	// An unknown host asks both CLIs.
	*calls = nil
	_, _ = TokenFor(ctx, Repo{Host: "code.example.com", Kind: KindBranch})
	if len(*calls) != 2 || !strings.HasPrefix((*calls)[0], "gh ") || !strings.HasPrefix((*calls)[1], "glab ") {
		t.Errorf("calls = %v", *calls)
	}
	if _, err := TokenFor(ctx, Repo{}); err == nil {
		t.Error("empty host: no error")
	}
	if err := SetToken("", "x"); err == nil {
		t.Error("SetToken without a host: no error")
	}
}
