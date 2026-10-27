package workspace

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
)

// Names of the checks Inspect returns, in order.
const (
	CheckRepo           = "git repository"
	CheckOrigin         = "origin remote"
	CheckDefaultBranch  = "default branch"
	CheckUserEmail      = "user.email"
	CheckPush           = "push access"
	CheckWorktreeConfig = "extensions.worktreeConfig"
)

// Check is the outcome of one directory check.
type Check struct {
	Name   string
	OK     bool
	Detail string
}

// Inspect checks whether dir on host h can host pipeline runs. It always
// returns every check, each with a Detail, even when an earlier one failed;
// checks that depend on a failed one are reported as not checked. The error is
// set only when ctx ends.
//
// The checks are: dir is inside a git repository, origin exists, the default
// branch of origin is known, user.email is set (the agent address is derived
// from it), a push to origin is allowed (a dry run to a throwaway branch, which
// leaves nothing on the remote), and the state of extensions.worktreeConfig.
// That last check is informational and always OK: ymcp turns the flag on itself
// when it creates the first worktree, and the Detail says whether it is on.
func Inspect(ctx context.Context, h Host, dir string) ([]Check, error) {
	var checks []Check
	add := func(name string, ok bool, detail string) {
		checks = append(checks, Check{Name: name, OK: ok, Detail: detail})
	}
	notChecked := func(name, why string) { add(name, false, "not checked: "+why) }

	top, err := run(ctx, h, dir, "rev-parse", "--show-toplevel")
	repoOK := err == nil && top != ""
	if repoOK {
		add(CheckRepo, true, top)
	} else {
		if cerr := ctx.Err(); cerr != nil {
			return checks, cerr
		}
		add(CheckRepo, false, fmt.Sprintf("%s is not usable as a git repository: %s", dir, reason(err)))
	}

	originURL, originOK := "", false
	if repoOK {
		originURL, err = run(ctx, h, dir, "remote", "get-url", "origin")
		if originOK = err == nil && originURL != ""; originOK {
			add(CheckOrigin, true, originURL)
		} else {
			add(CheckOrigin, false, "no remote named origin: "+reason(err))
		}
	} else {
		notChecked(CheckOrigin, "not a git repository")
	}

	switch {
	case !repoOK:
		notChecked(CheckDefaultBranch, "not a git repository")
	case !originOK:
		notChecked(CheckDefaultBranch, "no origin remote")
	default:
		if b, derr := DefaultBranch(ctx, h, dir); derr != nil {
			add(CheckDefaultBranch, false, derr.Error())
		} else {
			add(CheckDefaultBranch, true, b)
		}
	}

	if !repoOK {
		notChecked(CheckUserEmail, "not a git repository")
	} else if email, eerr := UserEmail(ctx, h, dir); eerr == nil {
		add(CheckUserEmail, true, email)
	} else {
		add(CheckUserEmail, false, "user.email is not set; run: git config user.email you@example.com (the agent address is derived from it)")
	}

	switch {
	case !repoOK:
		notChecked(CheckPush, "not a git repository")
	case !originOK:
		notChecked(CheckPush, "no origin remote")
	default:
		ok, detail := checkPush(ctx, h, dir)
		add(CheckPush, ok, detail)
	}

	if !repoOK {
		notChecked(CheckWorktreeConfig, "not a git repository")
	} else if v, _ := run(ctx, h, dir, "config", "--get", "extensions.worktreeConfig"); strings.EqualFold(v, "true") {
		add(CheckWorktreeConfig, true, "enabled")
	} else {
		add(CheckWorktreeConfig, true, "off; ymcp enables it for this repository when it creates the first worktree")
	}

	return checks, ctx.Err()
}

// UserEmail returns the git user.email in effect in dir on host h, the base
// of the agent's commit address (see AgentIdentity). It is an error when the
// address is not set.
func UserEmail(ctx context.Context, h Host, dir string) (string, error) {
	email, err := run(ctx, h, dir, "config", "user.email")
	if err != nil || email == "" {
		if cerr := ctx.Err(); cerr != nil {
			return "", cerr
		}
		return "", fmt.Errorf("git user.email is not set in %s: the agent's commit address is derived from it", dir)
	}
	return email, nil
}

// checkPush runs `git push --dry-run` of HEAD to a throwaway branch name. A
// dry run talks to the remote, so credentials and permissions are exercised,
// but no ref is created there.
func checkPush(ctx context.Context, h Host, dir string) (bool, string) {
	if _, err := run(ctx, h, dir, "rev-parse", "--verify", "--quiet", "HEAD"); err != nil {
		return false, "the repository has no commits yet, so a push cannot be tested"
	}
	ref := "ymcp-check-" + randHex(4)
	if _, err := run(ctx, h, dir, "push", "--dry-run", "origin", "HEAD:refs/heads/"+ref); err != nil {
		return false, "push to origin is not possible: " + reason(err)
	}
	return true, "dry run to origin succeeded"
}

func randHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "0000"
	}
	return hex.EncodeToString(b)
}

// DefaultBranch returns the default branch of origin, without the "origin/"
// prefix. It asks the remote first, then falls back to the local origin/HEAD
// and finally to origin/main or origin/master.
func DefaultBranch(ctx context.Context, h Host, dir string) (string, error) {
	if out, err := run(ctx, h, dir, "ls-remote", "--symref", "origin", "HEAD"); err == nil {
		for _, line := range strings.Split(out, "\n") {
			if rest, ok := strings.CutPrefix(line, "ref: refs/heads/"); ok {
				if name, _, _ := strings.Cut(rest, "\t"); name != "" {
					return name, nil
				}
			}
		}
	} else if cerr := ctx.Err(); cerr != nil {
		return "", cerr
	}
	if out, err := run(ctx, h, dir, "symbolic-ref", "--quiet", "--short", "refs/remotes/origin/HEAD"); err == nil {
		if name := strings.TrimPrefix(out, "origin/"); name != "" {
			return name, nil
		}
	}
	for _, name := range []string{"main", "master"} {
		if _, err := run(ctx, h, dir, "rev-parse", "--verify", "--quiet", "refs/remotes/origin/"+name); err == nil {
			return name, nil
		}
	}
	return "", errors.New("cannot determine the default branch of origin (is the remote empty?)")
}
