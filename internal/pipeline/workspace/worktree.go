package workspace

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// Worktree is a git worktree made for one run.
type Worktree struct{ Path, Branch, Base string }

// commonDir returns the absolute git common directory of the repository at
// dir: the .git directory shared by all worktrees.
func commonDir(ctx context.Context, h Host, dir string) (string, error) {
	out, err := run(ctx, h, dir, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil {
		if ctx.Err() != nil {
			return "", err
		}
		// git older than 2.31 has no --path-format.
		out, err = run(ctx, h, dir, "rev-parse", "--git-common-dir")
		if err != nil {
			return "", fmt.Errorf("locating the git directory of %s: %w", dir, err)
		}
		if !isAbs(out) {
			base := dir
			if !isAbs(base) {
				top, terr := run(ctx, h, dir, "rev-parse", "--show-toplevel")
				if terr != nil {
					return "", fmt.Errorf("locating the git directory of %s: %w", dir, terr)
				}
				base = top
			}
			out = joinPath(base, out)
		}
	}
	return out, nil
}

type listedWorktree struct {
	path, branch string
	main         bool
	prunable     bool
}

func listWorktrees(ctx context.Context, h Host, dir string) ([]listedWorktree, error) {
	out, err := run(ctx, h, dir, "worktree", "list", "--porcelain")
	if err != nil {
		return nil, err
	}
	var list []listedWorktree
	var cur *listedWorktree
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimRight(line, "\r")
		switch {
		case strings.HasPrefix(line, "worktree "):
			list = append(list, listedWorktree{path: strings.TrimPrefix(line, "worktree "), main: len(list) == 0})
			cur = &list[len(list)-1]
		case cur == nil:
		case strings.HasPrefix(line, "branch "):
			cur.branch = strings.TrimPrefix(strings.TrimPrefix(line, "branch "), "refs/heads/")
		case line == "prunable" || strings.HasPrefix(line, "prunable "):
			cur.prunable = true
		}
	}
	return list, nil
}

func refExists(ctx context.Context, h Host, dir, ref string) bool {
	_, err := run(ctx, h, dir, "rev-parse", "--verify", "--quiet", ref+"^{commit}")
	return err == nil
}

// Create makes the worktree for one run in the repository at repoDir, on
// branch, and gives it the commit identity id.
//
// It fetches origin first. base is the branch to start from; empty means the
// default branch of origin. root is the directory that holds worktrees; empty
// means <git-common-dir>/ymcp/worktrees. The worktree lives at root/<branch>.
//
// If a worktree for branch already exists it is reused as it is. If the branch
// exists without a worktree (a previous run removed it), it is checked out
// again; if it exists only on origin, it is created from there so earlier work
// continues. Only a brand new branch starts from origin/<base>. A branch that
// is checked out in the main working tree is refused.
//
// extensions.worktreeConfig is switched on for the repository, and the identity
// is written to the worktree's own config, so the user's commits are unaffected.
func Create(ctx context.Context, h Host, repoDir, root, branch, base string, id Identity) (Worktree, error) {
	if strings.TrimSpace(branch) == "" {
		return Worktree{}, errors.New("worktree branch is empty")
	}
	if id.Email == "" {
		return Worktree{}, errors.New("agent identity has no email")
	}
	if _, err := run(ctx, h, repoDir, "check-ref-format", "--branch", branch); err != nil {
		return Worktree{}, fmt.Errorf("invalid branch name %q", branch)
	}
	common, err := commonDir(ctx, h, repoDir)
	if err != nil {
		return Worktree{}, err
	}
	if _, err := run(ctx, h, repoDir, "fetch", "origin"); err != nil {
		return Worktree{}, fmt.Errorf("fetching origin: %w", err)
	}

	base = strings.TrimPrefix(strings.TrimPrefix(strings.TrimSpace(base), "refs/heads/"), "origin/")
	if base == "" {
		if base, err = DefaultBranch(ctx, h, repoDir); err != nil {
			return Worktree{}, err
		}
	}
	wt := Worktree{Branch: branch, Base: base}

	// One-time repository switch; the per-worktree identity depends on it.
	if _, err := run(ctx, h, repoDir, "config", "extensions.worktreeConfig", "true"); err != nil {
		return Worktree{}, fmt.Errorf("enabling extensions.worktreeConfig: %w", err)
	}

	existing, err := listWorktrees(ctx, h, repoDir)
	if err != nil {
		return Worktree{}, fmt.Errorf("listing worktrees: %w", err)
	}
	reuse := false
	for _, w := range existing {
		if w.branch != branch {
			continue
		}
		if w.main {
			return Worktree{}, fmt.Errorf("branch %s is checked out in the main working tree %s; refusing to run an agent there", branch, w.path)
		}
		if w.prunable {
			if _, err := run(ctx, h, repoDir, "worktree", "prune"); err != nil {
				return Worktree{}, fmt.Errorf("pruning stale worktrees: %w", err)
			}
			break
		}
		wt.Path, reuse = w.path, true
		break
	}

	if !reuse {
		if root == "" {
			root = joinPath(common, "ymcp", "worktrees")
		}
		wt.Path = joinPath(root, branch)
		switch {
		case refExists(ctx, h, repoDir, "refs/heads/"+branch):
			_, err = run(ctx, h, repoDir, "worktree", "add", wt.Path, branch)
		case refExists(ctx, h, repoDir, "refs/remotes/origin/"+branch):
			_, err = run(ctx, h, repoDir, "worktree", "add", "-b", branch, wt.Path, "origin/"+branch)
		case refExists(ctx, h, repoDir, "refs/remotes/origin/"+base):
			_, err = run(ctx, h, repoDir, "worktree", "add", "--no-track", "-b", branch, wt.Path, "origin/"+base)
		default:
			return Worktree{}, fmt.Errorf("base branch origin/%s does not exist", base)
		}
		if err != nil {
			return Worktree{}, fmt.Errorf("creating worktree: %w", err)
		}
	}

	for _, kv := range [][2]string{{"user.name", id.Name}, {"user.email", id.Email}} {
		if _, err := run(ctx, h, wt.Path, "config", "--worktree", kv[0], kv[1]); err != nil {
			return Worktree{}, fmt.Errorf("setting the agent %s: %w", kv[0], err)
		}
	}
	return wt, nil
}

// Remove deletes the worktree and its administrative files; the branch stays.
// Uncommitted changes are discarded, so call CommitLeftovers first. A worktree
// that is already gone is not an error.
func Remove(ctx context.Context, h Host, repoDir string, wt Worktree) error {
	if wt.Path == "" {
		return errors.New("worktree path is empty")
	}
	_, err := run(ctx, h, repoDir, "worktree", "remove", "--force", wt.Path)
	if err == nil {
		return nil
	}
	if ctx.Err() != nil {
		return err
	}
	// Maybe the directory was deleted by hand: prune, and see whether git still knows it.
	_, _ = run(ctx, h, repoDir, "worktree", "prune")
	list, lerr := listWorktrees(ctx, h, repoDir)
	if lerr != nil {
		return fmt.Errorf("removing worktree: %w", err)
	}
	for _, w := range list {
		if !w.main && (w.path == wt.Path || (wt.Branch != "" && w.branch == wt.Branch)) {
			return fmt.Errorf("removing worktree: %w", err)
		}
	}
	return nil
}

// CommitLeftovers commits whatever is left in the worktree (modified, deleted
// and untracked files) with message, as the worktree's agent identity. It
// reports whether it made a commit. Repository hooks run; signing is switched
// off because the agent identity has no key.
func CommitLeftovers(ctx context.Context, h Host, wt Worktree, message string) (bool, error) {
	if strings.TrimSpace(message) == "" {
		return false, errors.New("commit message is empty")
	}
	status, err := run(ctx, h, wt.Path, "status", "--porcelain", "--untracked-files=all")
	if err != nil {
		return false, fmt.Errorf("checking the worktree: %w", err)
	}
	if status == "" {
		return false, nil
	}
	if _, err := run(ctx, h, wt.Path, "add", "--all"); err != nil {
		return false, fmt.Errorf("staging leftovers: %w", err)
	}
	if _, err := run(ctx, h, wt.Path, "-c", "commit.gpgsign=false", "commit", "-m", message); err != nil {
		return false, fmt.Errorf("committing leftovers: %w", err)
	}
	return true, nil
}

// Push publishes the worktree's branch to origin and sets it as upstream.
func Push(ctx context.Context, h Host, wt Worktree) error {
	if wt.Branch == "" {
		return errors.New("worktree branch is empty")
	}
	if _, err := run(ctx, h, wt.Path, "push", "-u", "origin", wt.Branch); err != nil {
		return fmt.Errorf("pushing %s: %w", wt.Branch, err)
	}
	return nil
}

// Head returns the commit the worktree is at.
func Head(ctx context.Context, h Host, wt Worktree) (string, error) {
	sha, err := run(ctx, h, wt.Path, "rev-parse", "HEAD")
	if err != nil {
		return "", fmt.Errorf("reading HEAD: %w", err)
	}
	return sha, nil
}

// RunDir returns <git-common-dir>/ymcp/runs/<run>, creating it. ymcp keeps its
// per-run files (agent MCP config, prompts) there, outside any working tree.
func RunDir(ctx context.Context, h Host, repoDir string, number int) (string, error) {
	common, err := commonDir(ctx, h, repoDir)
	if err != nil {
		return "", err
	}
	dir := joinPath(common, "ymcp", "runs", strconv.Itoa(number))
	if dm, ok := h.(dirMaker); ok {
		if err := dm.MkdirAll(ctx, dir); err != nil {
			return "", fmt.Errorf("creating %s: %w", dir, err)
		}
		return dir, nil
	}
	if _, stderr, err := h.Run(ctx, "", []string{"mkdir", "-p", dir}, nil); err != nil {
		return "", fmt.Errorf("creating %s: %w: %s", dir, err, trim(string(stderr)))
	}
	return dir, nil
}
