package tui

import (
	"context"
	"fmt"
	"strings"

	"github.com/xlsft/ymcp/internal/pipeline/tui/wizard"
	"github.com/xlsft/ymcp/internal/pipeline/vcs"
	"github.com/xlsft/ymcp/internal/pipeline/workspace"
)

// strategyDetector suggests a strategy from the repository's origin and
// says where its token would come from.
type strategyDetector struct {
	// host returns the workspace host and a function that closes it.
	host func(name string) (workspace.Host, func(), error)
	// token is vcs.TokenFor; tests replace it.
	token func(ctx context.Context, repo vcs.Repo) (vcs.Auth, error)
}

func newStrategyDetector() strategyDetector {
	return strategyDetector{host: hostNamed, token: vcs.TokenFor}
}

// hostNamed is this machine for "" and "local", the ssh host otherwise.
func hostNamed(name string) (workspace.Host, func(), error) {
	if n := strings.TrimSpace(name); n != "" && n != "local" {
		s := workspace.SSH(n)
		return s, func() { s.Close() }, nil
	}
	return workspace.Local(), func() {}, nil
}

func (d strategyDetector) Detect(ctx context.Context, host, dir string) (wizard.Detection, error) {
	h, closeHost, err := d.host(host)
	if err != nil {
		return wizard.Detection{}, err
	}
	defer closeHost()
	out, stderr, err := h.Run(ctx, dir, []string{"git", "remote", "get-url", "origin"}, nil)
	if err != nil {
		msg := strings.TrimSpace(string(stderr))
		if msg == "" {
			msg = err.Error()
		}
		return wizard.Detection{}, fmt.Errorf("git remote get-url origin: %s", msg)
	}
	repo, err := vcs.ParseRemote(strings.TrimSpace(string(out)))
	if err != nil {
		return wizard.Detection{Notes: []string{err.Error(), "only pushing the branch is possible"}}, nil
	}
	det := wizard.Detection{Kind: string(repo.Kind), Repo: repo.Host + "/" + repo.Path}
	if repo.Kind == vcs.KindBranch {
		det.Notes = append(det.Notes, "not a GitHub or GitLab remote: only pushing the branch is possible")
		return det, nil
	}
	auth, err := d.token(ctx, repo)
	if err != nil {
		det.Auth = err.Error()
		return det, nil
	}
	det.Auth, det.AuthOK = "token from "+auth.Source, true
	return det, nil
}
