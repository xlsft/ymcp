package tui

import (
	"context"
	"errors"
	"io"
	"testing"

	"github.com/xlsft/ymcp/internal/pipeline/vcs"
	"github.com/xlsft/ymcp/internal/pipeline/workspace"
)

// originHost answers `git remote get-url origin` with a fixed URL.
type originHost struct{ url string }

func (h originHost) Name() string { return "local" }

func (h originHost) Run(_ context.Context, _ string, argv []string, _ io.Reader) ([]byte, []byte, error) {
	if h.url == "" {
		return nil, []byte("error: No such remote 'origin'"), errors.New("exit status 2")
	}
	return []byte(h.url + "\n"), nil, nil
}

func (h originHost) CacheDir(context.Context) (string, error) { return "", nil }

func (h originHost) IsDir(context.Context, string) (bool, error) { return true, nil }

func TestStrategyDetector(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		url      string
		tokenErr error
		kind     string
		repo     string
		authOK   bool
		auth     string
		wantErr  bool
	}{
		{url: "git@github.com:xlsft/ymcp.git", kind: "github-pr", repo: "github.com/xlsft/ymcp", authOK: true, auth: "token from gh"},
		{url: "https://gitlab.com/team/sub/app.git", tokenErr: errors.New("no token for gitlab.com"), kind: "gitlab-mr", repo: "gitlab.com/team/sub/app", auth: "no token for gitlab.com"},
		{url: "ssh://git@example.org/team/app.git", kind: "branch", repo: "example.org/team/app"},
		{url: "", wantErr: true},
	} {
		d := strategyDetector{
			host: func(string) (workspace.Host, func(), error) { return originHost{tc.url}, func() {}, nil },
			token: func(context.Context, vcs.Repo) (vcs.Auth, error) {
				if tc.tokenErr != nil {
					return vcs.Auth{}, tc.tokenErr
				}
				return vcs.Auth{Token: "t", Source: "gh"}, nil
			},
		}
		got, err := d.Detect(ctx, "", "/repo")
		if tc.wantErr {
			if err == nil {
				t.Errorf("%q: no error", tc.url)
			}
			continue
		}
		if err != nil || got.Kind != tc.kind || got.Repo != tc.repo || got.AuthOK != tc.authOK || got.Auth != tc.auth {
			t.Errorf("%q: Detect = %+v, %v", tc.url, got, err)
		}
	}
}

func TestHostNamed(t *testing.T) {
	for _, name := range []string{"", "local", " local "} {
		h, closeHost, err := hostNamed(name)
		if err != nil || h.Name() != "local" {
			t.Errorf("hostNamed(%q) = %v, %v", name, h, err)
		}
		closeHost()
	}
	h, closeHost, err := hostNamed("devbox")
	defer closeHost()
	if s, ok := h.(*workspace.SSHHost); err != nil || !ok || s.Name() != "devbox" {
		t.Errorf("hostNamed(devbox) = %#v, %v", h, err)
	}
}
