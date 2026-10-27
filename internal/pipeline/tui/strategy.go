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
	host func(name string) (workspace.Host, error)
	// token is vcs.TokenFor; tests replace it.
	token func(ctx context.Context, repo vcs.Repo) (vcs.Auth, error)
}

func newStrategyDetector() strategyDetector {
	return strategyDetector{host: localHost, token: vcs.TokenFor}
}

func localHost(name string) (workspace.Host, error) {
	if n := strings.TrimSpace(name); n != "" && n != "local" {
		return nil, fmt.Errorf("workspace host %q: ssh hosts are not supported yet", n)
	}
	return workspace.Local(), nil
}

func (d strategyDetector) Detect(ctx context.Context, host, dir string) (wizard.Detection, error) {
	h, err := d.host(host)
	if err != nil {
		return wizard.Detection{}, err
	}
	out, stderr, err := h.Run(ctx, dir, []string{"git", "remote", "get-url", "origin"}, nil)
	if err != nil {
		return wizard.Detection{}, fmt.Errorf("git remote get-url origin: %s", strings.TrimSpace(string(stderr)))
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
