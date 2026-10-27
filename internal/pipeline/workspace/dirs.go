package workspace

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
)

// Dir is a subdirectory found by Dirs.
type Dir struct {
	Name string
	// Git is set when it holds a .git: the top of a repository.
	Git bool
}

// maxDirs bounds what Dirs returns.
const maxDirs = 1000

// dirsScript prints a line per subdirectory of the current one, "g name"
// for a repository and "d name" for the rest, hidden ones included.
const dirsScript = `for e in * .[!.]* ..?*; do
  [ -d "$e" ] || continue
  if [ -e "$e/.git" ]; then printf 'g %s\n' "$e"; else printf 'd %s\n' "$e"; fi
done
true`

// Dirs lists the subdirectories of dir on h, sorted by name. On an ssh
// host dir is read as the shell there would read it: "", ~ and ~/… are
// in the home directory and a relative path starts there. A dir that is
// missing or can't be read has no subdirectories.
func Dirs(ctx context.Context, h Host, dir string) ([]Dir, error) {
	var out []Dir
	if _, ok := h.(localHost); ok {
		entries, err := os.ReadDir(dir)
		if err != nil {
			return nil, nil
		}
		for _, e := range entries {
			path := filepath.Join(dir, e.Name())
			if !e.IsDir() {
				// A link to a directory counts as one.
				if st, err := os.Stat(path); e.Type()&os.ModeSymlink == 0 || err != nil || !st.IsDir() {
					continue
				}
			}
			_, err := os.Stat(filepath.Join(path, ".git"))
			out = append(out, Dir{Name: e.Name(), Git: err == nil})
		}
	} else {
		if dir == "" {
			dir = "~"
		}
		stdout, stderr, err := h.Run(ctx, dir, []string{"sh", "-c", dirsScript}, nil)
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			// The cd failed: exit status 1 or 2 from the shell there;
			// 255 is ssh's own.
			var ee *exec.ExitError
			if errors.As(err, &ee) && ee.ExitCode() != 255 {
				return nil, nil
			}
			return nil, sshError(h.Name(), err, stderr)
		}
		for line := range strings.SplitSeq(string(stdout), "\n") {
			kind, name, ok := strings.Cut(line, " ")
			if !ok || name == "" {
				continue
			}
			out = append(out, Dir{Name: name, Git: kind == "g"})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out[:min(len(out), maxDirs)], nil
}
