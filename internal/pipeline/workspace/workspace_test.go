package workspace

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// isolate keeps git away from the developer's own configuration.
func isolate(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	home := t.TempDir()
	empty := filepath.Join(home, "gitconfig")
	if err := os.WriteFile(empty, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "xdg"))
	// Worktrees go to the user cache directory; keep it in the temp home.
	t.Setenv("XDG_CACHE_HOME", filepath.Join(home, "cache"))
	t.Setenv("LocalAppData", filepath.Join(home, "appdata"))
	t.Setenv("GIT_CONFIG_GLOBAL", empty)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GIT_CEILING_DIRECTORIES", filepath.Dir(home))
}

// git runs a git command for test setup and fails the test on error.
func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := run(context.Background(), Local(), dir, args...)
	// git on a busy macOS CI machine now and then dies of a segfault
	// before doing anything; run does not repeat what changes the repo.
	for try := 0; try < 2 && crashed(err); try++ {
		out, err = run(context.Background(), Local(), dir, args...)
	}
	if err != nil {
		t.Fatalf("git %v: %v", args, err)
	}
	return out
}

func TestUserEmail(t *testing.T) {
	f := newFixture(t)
	got, err := UserEmail(context.Background(), Local(), f.repo)
	if err != nil || got != "dev@xlsft.ru" {
		t.Fatalf("UserEmail = %q, %v", got, err)
	}
	git(t, f.repo, "config", "--unset", "user.email")
	if got, err := UserEmail(context.Background(), Local(), f.repo); err == nil || !strings.Contains(err.Error(), "user.email is not set") {
		t.Errorf("unset: UserEmail = %q, %v", got, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := UserEmail(ctx, Local(), f.repo); !errors.Is(err, context.Canceled) {
		t.Errorf("canceled: err = %v", err)
	}
}

type fixture struct {
	origin string // bare repository
	repo   string // working clone with one commit on main, pushed
}

// newFixture builds a bare origin and a clone with user.email set.
func newFixture(t *testing.T) fixture {
	t.Helper()
	isolate(t)
	base := t.TempDir()
	f := fixture{origin: filepath.Join(base, "origin.git"), repo: filepath.Join(base, "repo")}
	git(t, base, "init", "--bare", "-b", "main", f.origin)
	git(t, base, "init", "-b", "main", f.repo)
	git(t, f.repo, "config", "user.name", "Dev")
	git(t, f.repo, "config", "user.email", "dev@xlsft.ru")
	git(t, f.repo, "remote", "add", "origin", f.origin)
	if err := os.WriteFile(filepath.Join(f.repo, "README.md"), []byte("hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, f.repo, "add", ".")
	git(t, f.repo, "commit", "-m", "init")
	git(t, f.repo, "push", "-u", "origin", "main")
	git(t, f.repo, "remote", "set-head", "origin", "main")
	return f
}

func checkByName(t *testing.T, checks []Check, name string) Check {
	t.Helper()
	for _, c := range checks {
		if c.Name == name {
			return c
		}
	}
	t.Fatalf("no check %q in %+v", name, checks)
	return Check{}
}

func realPath(t *testing.T, p string) string {
	t.Helper()
	r, err := filepath.EvalSymlinks(p)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func worktreeCount(t *testing.T, repo string) int {
	t.Helper()
	return len(strings.Split(git(t, repo, "worktree", "list"), "\n"))
}

var testIdentity = Identity{Name: "Claude · backend #17", Email: "dev+agent_backend_17@xlsft.ru"}

func TestLocalHost(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	h := Local()
	if h.Name() != "local" {
		t.Errorf("Name = %q", h.Name())
	}
	dir := t.TempDir()
	stdout, _, err := h.Run(context.Background(), dir, []string{"git", "--version"}, nil)
	if err != nil || !strings.HasPrefix(string(stdout), "git version") {
		t.Errorf("git --version: %q, %v", stdout, err)
	}
	if runtime.GOOS != "windows" {
		stdout, _, err = h.Run(context.Background(), dir, []string{"env"}, nil)
		if err != nil || !strings.Contains(string(stdout), "\nGIT_TERMINAL_PROMPT=0\n") || !strings.Contains(string(stdout), "\nLC_ALL=C\n") {
			t.Errorf("env = %q, %v", stdout, err)
		}
		// argv only: shell metacharacters are plain arguments.
		stdout, _, err = h.Run(context.Background(), dir, []string{"echo", "$HOME;", "&&", "id"}, nil)
		if err != nil || string(stdout) != "$HOME; && id\n" {
			t.Errorf("echo = %q, %v", stdout, err)
		}
	}
	_, stderr, err := h.Run(context.Background(), dir, []string{"git", "rev-parse", "--verify", "nope"}, nil)
	if err == nil || len(stderr) == 0 {
		t.Errorf("failure: stderr=%q err=%v", stderr, err)
	}
	if _, _, err := h.Run(context.Background(), dir, nil, nil); err == nil {
		t.Error("empty argv must fail")
	}
	// A missing directory is named as such, not blamed on the program.
	gone := filepath.Join(dir, "gone")
	if _, _, err := h.Run(context.Background(), gone, []string{"git", "--version"}, nil); err == nil || !strings.Contains(err.Error(), gone+" does not exist") {
		t.Errorf("missing dir: err = %v", err)
	}
	if ok, err := h.IsDir(context.Background(), dir); !ok || err != nil {
		t.Errorf("IsDir(dir) = %v, %v", ok, err)
	}
	if ok, err := h.IsDir(context.Background(), gone); ok || err != nil {
		t.Errorf("IsDir(gone) = %v, %v", ok, err)
	}
	file := filepath.Join(dir, "file")
	writeFile(t, file, "x")
	if ok, err := h.IsDir(context.Background(), file); ok || err != nil {
		t.Errorf("IsDir(file) = %v, %v", ok, err)
	}
}

func TestAgentIdentity(t *testing.T) {
	tests := []struct {
		email, pipeline string
		run             int
		agent           string
		wantEmail       string
		wantName        string
	}{
		{"dev@xlsft.ru", "backend", 17, "Claude", "dev+agent_backend_17@xlsft.ru", "Claude · backend #17"},
		{"dev+old@xlsft.ru", "backend", 1, "Kimi", "dev+agent_backend_1@xlsft.ru", "Kimi · backend #1"},
		{"dev+a+b@xlsft.ru", "Front-End API", 2, "", "dev+agent_front_end_api_2@xlsft.ru", "Agent · Front-End API #2"},
		{"  dev@xlsft.ru ", "__x__", 3, "A", "dev+agent_x_3@xlsft.ru", "A · __x__ #3"},
		{"dev@xlsft.ru", "Бэкенд", 4, "A", "dev+agent_pipeline_4@xlsft.ru", "A · Бэкенд #4"},
		{"not-an-email", "p", 5, "A", "", "A · p #5"},
		{"@xlsft.ru", "p", 5, "A", "", "A · p #5"},
		{"+tag@xlsft.ru", "p", 5, "A", "", "A · p #5"},
		{"", "p", 5, "A", "", "A · p #5"},
	}
	for _, tt := range tests {
		got := AgentIdentity(tt.email, tt.pipeline, tt.run, tt.agent)
		if got.Email != tt.wantEmail || got.Name != tt.wantName {
			t.Errorf("AgentIdentity(%q, %q, %d, %q) = %+v, want {%q %q}", tt.email, tt.pipeline, tt.run, tt.agent, got, tt.wantName, tt.wantEmail)
		}
	}
}

func TestInspectOK(t *testing.T) {
	f := newFixture(t)
	checks, err := Inspect(context.Background(), Local(), f.repo)
	if err != nil {
		t.Fatal(err)
	}
	if len(checks) != 6 {
		t.Fatalf("checks = %+v", checks)
	}
	for _, c := range checks {
		if !c.OK || c.Detail == "" {
			t.Errorf("check %+v", c)
		}
	}
	if d := checkByName(t, checks, CheckDefaultBranch).Detail; d != "main" {
		t.Errorf("default branch = %q", d)
	}
	if d := checkByName(t, checks, CheckUserEmail).Detail; d != "dev@xlsft.ru" {
		t.Errorf("email = %q", d)
	}
	if d := checkByName(t, checks, CheckWorktreeConfig).Detail; !strings.HasPrefix(d, "off") {
		t.Errorf("worktreeConfig = %q", d)
	}
	// The dry run must leave nothing on the remote.
	if refs := git(t, f.origin, "for-each-ref", "--format=%(refname)"); refs != "refs/heads/main" {
		t.Errorf("remote refs after Inspect:\n%s", refs)
	}

	git(t, f.repo, "config", "extensions.worktreeConfig", "true")
	checks, _ = Inspect(context.Background(), Local(), f.repo)
	if c := checkByName(t, checks, CheckWorktreeConfig); !c.OK || c.Detail != "enabled" {
		t.Errorf("worktreeConfig = %+v", c)
	}
}

func TestInspectNotARepo(t *testing.T) {
	isolate(t)
	dir := t.TempDir()
	checks, err := Inspect(context.Background(), Local(), dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(checks) != 6 {
		t.Fatalf("checks = %+v", checks)
	}
	for _, c := range checks {
		if c.Name != CheckWorktreeConfig && c.OK {
			t.Errorf("check %+v should fail", c)
		}
		if c.Detail == "" {
			t.Errorf("check %q has no detail", c.Name)
		}
	}
	if d := checks[0].Detail; !strings.Contains(d, "not a git repository") {
		t.Errorf("repo detail = %q", d)
	}
	if d := checkByName(t, checks, CheckPush).Detail; !strings.HasPrefix(d, "not checked") {
		t.Errorf("push detail = %q", d)
	}

	checks, err = Inspect(context.Background(), Local(), filepath.Join(dir, "missing"))
	if err != nil || len(checks) != 6 || checks[0].OK || checks[0].Detail == "" {
		t.Errorf("missing dir: %+v, %v", checks, err)
	}
}

func TestInspectMissingEmailKeepsGoing(t *testing.T) {
	f := newFixture(t)
	git(t, f.repo, "config", "--unset", "user.email")
	checks, err := Inspect(context.Background(), Local(), f.repo)
	if err != nil {
		t.Fatal(err)
	}
	c := checkByName(t, checks, CheckUserEmail)
	if c.OK || !strings.Contains(c.Detail, "user.email is not set") {
		t.Errorf("email check = %+v", c)
	}
	for _, name := range []string{CheckRepo, CheckOrigin, CheckDefaultBranch, CheckPush} {
		if c := checkByName(t, checks, name); !c.OK {
			t.Errorf("check %+v should pass after the email failure", c)
		}
	}
}

func TestInspectBrokenAndMissingOrigin(t *testing.T) {
	f := newFixture(t)
	git(t, f.repo, "remote", "set-url", "origin", filepath.Join(filepath.Dir(f.origin), "gone.git"))
	checks, err := Inspect(context.Background(), Local(), f.repo)
	if err != nil {
		t.Fatal(err)
	}
	if c := checkByName(t, checks, CheckPush); c.OK || !strings.Contains(c.Detail, "push to origin is not possible") {
		t.Errorf("push = %+v", c)
	}
	if c := checkByName(t, checks, CheckOrigin); !c.OK {
		t.Errorf("origin = %+v", c)
	}
	if c := checkByName(t, checks, CheckUserEmail); !c.OK {
		t.Errorf("email = %+v", c)
	}

	git(t, f.repo, "remote", "remove", "origin")
	checks, err = Inspect(context.Background(), Local(), f.repo)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{CheckOrigin, CheckDefaultBranch, CheckPush} {
		if c := checkByName(t, checks, name); c.OK || c.Detail == "" {
			t.Errorf("check %+v", c)
		}
	}
	if c := checkByName(t, checks, CheckRepo); !c.OK {
		t.Errorf("repo = %+v", c)
	}
}

func TestInspectCanceled(t *testing.T) {
	f := newFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := Inspect(ctx, Local(), f.repo); err == nil {
		t.Error("canceled context must return an error")
	}
}

func TestDefaultBranchFallbacks(t *testing.T) {
	f := newFixture(t)
	git(t, f.origin, "symbolic-ref", "HEAD", "refs/heads/gone") // dangling: ls-remote reports no symref
	git(t, f.repo, "remote", "set-head", "origin", "--delete")
	b, err := DefaultBranch(context.Background(), Local(), f.repo)
	if err != nil || b != "main" {
		t.Errorf("DefaultBranch = %q, %v", b, err)
	}
}

func TestCreateAppliesIdentity(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	h := Local()
	wt, err := Create(ctx, h, f.repo, "", "BE-1-fix", "", testIdentity)
	if err != nil {
		t.Fatal(err)
	}
	if wt.Branch != "BE-1-fix" || wt.Base != "main" {
		t.Errorf("worktree = %+v", wt)
	}
	cache, err := os.UserCacheDir()
	if err != nil {
		t.Fatal(err)
	}
	wantPath := filepath.Join(cache, "ymcp", "worktrees", "BE-1-fix")
	if realPath(t, wt.Path) != realPath(t, wantPath) {
		t.Errorf("path = %q, want %q", wt.Path, wantPath)
	}
	if rel, err := filepath.Rel(realPath(t, f.repo), realPath(t, wt.Path)); err != nil || !strings.HasPrefix(rel, "..") {
		t.Errorf("worktree %s is inside the repository %s", wt.Path, f.repo)
	}
	if got := git(t, f.repo, "config", "--get", "extensions.worktreeConfig"); got != "true" {
		t.Errorf("extensions.worktreeConfig = %q", got)
	}
	if b := git(t, wt.Path, "rev-parse", "--abbrev-ref", "HEAD"); b != "BE-1-fix" {
		t.Errorf("branch = %q", b)
	}
	if git(t, wt.Path, "rev-parse", "HEAD") != git(t, f.repo, "rev-parse", "origin/main") {
		t.Error("worktree did not start from origin/main")
	}
	if up, err := run(ctx, h, wt.Path, "rev-parse", "--abbrev-ref", "@{upstream}"); err == nil {
		t.Errorf("new branch must not track the base, upstream = %q", up)
	}

	// The identity must really end up on a commit made inside the worktree.
	writeFile(t, filepath.Join(wt.Path, "a.txt"), "a\n")
	git(t, wt.Path, "add", "a.txt")
	git(t, wt.Path, "commit", "-m", "BE-1 add a")
	want := "dev+agent_backend_17@xlsft.ru|Claude · backend #17|dev+agent_backend_17@xlsft.ru"
	if got := git(t, wt.Path, "log", "-1", "--format=%ae|%an|%ce"); got != want {
		t.Errorf("commit identity = %q, want %q", got, want)
	}
	if got := git(t, wt.Path, "log", "--format=%ae", "-1"); got != "dev+agent_backend_17@xlsft.ru" {
		t.Errorf("author email = %q", got)
	}
	// ...and not on the user's own checkout.
	if got := git(t, f.repo, "config", "user.email"); got != "dev@xlsft.ru" {
		t.Errorf("main checkout email = %q", got)
	}
	git(t, f.repo, "commit", "--allow-empty", "-m", "mine")
	if got := git(t, f.repo, "log", "-1", "--format=%ae"); got != "dev@xlsft.ru" {
		t.Errorf("main checkout commit email = %q", got)
	}

	head, err := Head(ctx, h, wt)
	if err != nil || head != git(t, wt.Path, "rev-parse", "HEAD") {
		t.Errorf("Head = %q, %v", head, err)
	}
}

func TestDefaultRoot(t *testing.T) {
	isolate(t)
	ctx := context.Background()
	cache, err := os.UserCacheDir()
	if err != nil {
		t.Fatal(err)
	}
	got, err := DefaultRoot(ctx, Local(), "backend")
	if err != nil || filepath.Clean(got) != filepath.Join(cache, "ymcp", "worktrees", "backend") {
		t.Errorf("DefaultRoot = %q, %v", got, err)
	}
	if got, err := DefaultRoot(ctx, Local(), ""); err != nil || filepath.Clean(got) != filepath.Join(cache, "ymcp", "worktrees") {
		t.Errorf("DefaultRoot without a pipeline = %q, %v", got, err)
	}
	if _, err := DefaultRoot(ctx, cachelessHost{Local()}, "backend"); err == nil || !strings.Contains(err.Error(), "cache directory") {
		t.Errorf("no cache dir: err = %v", err)
	}
}

// cachelessHost is a host whose cache directory is unknown.
type cachelessHost struct{ Host }

func (cachelessHost) CacheDir(context.Context) (string, error) {
	return "", errors.New("$HOME is not defined")
}

func TestCreateWithRootAndBase(t *testing.T) {
	f := newFixture(t)
	git(t, f.repo, "checkout", "-b", "release")
	git(t, f.repo, "commit", "--allow-empty", "-m", "release work")
	git(t, f.repo, "push", "origin", "release")
	git(t, f.repo, "checkout", "main")

	root := filepath.Join(t.TempDir(), "wts")
	wt, err := Create(context.Background(), Local(), f.repo, root, "feat/BE-2", "origin/release", testIdentity)
	if err != nil {
		t.Fatal(err)
	}
	if wt.Base != "release" || realPath(t, wt.Path) != realPath(t, filepath.Join(root, "feat", "BE-2")) {
		t.Errorf("worktree = %+v", wt)
	}
	if git(t, wt.Path, "rev-parse", "HEAD") != git(t, f.repo, "rev-parse", "origin/release") {
		t.Error("worktree did not start from origin/release")
	}
}

func TestCreateFetchesFirst(t *testing.T) {
	f := newFixture(t)
	// Someone else pushes to origin after our clone was made.
	other := filepath.Join(t.TempDir(), "other")
	git(t, "", "clone", f.origin, other)
	git(t, other, "config", "user.email", "x@y.z")
	git(t, other, "config", "user.name", "X")
	git(t, other, "commit", "--allow-empty", "-m", "newer")
	git(t, other, "push", "origin", "main")
	newest := git(t, other, "rev-parse", "HEAD")

	wt, err := Create(context.Background(), Local(), f.repo, "", "BE-3", "", testIdentity)
	if err != nil {
		t.Fatal(err)
	}
	if git(t, wt.Path, "rev-parse", "HEAD") != newest {
		t.Error("worktree does not start from the freshly fetched origin/main")
	}
}

func TestCreateReusesExisting(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	h := Local()
	wt, err := Create(ctx, h, f.repo, "", "BE-4", "", testIdentity)
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(wt.Path, "wip.txt"), "work in progress\n")
	newID := Identity{Name: "Claude · backend #18", Email: "dev+agent_backend_18@xlsft.ru"}
	again, err := Create(ctx, h, f.repo, "", "BE-4", "", newID)
	if err != nil {
		t.Fatal(err)
	}
	if realPath(t, again.Path) != realPath(t, wt.Path) || again.Branch != "BE-4" || again.Base != "main" {
		t.Errorf("reused = %+v, first = %+v", again, wt)
	}
	if _, err := os.Stat(filepath.Join(again.Path, "wip.txt")); err != nil {
		t.Errorf("work in progress lost: %v", err)
	}
	if got := git(t, again.Path, "config", "user.email"); got != newID.Email {
		t.Errorf("identity after reuse = %q", got)
	}
	if n := worktreeCount(t, f.repo); n != 2 {
		t.Errorf("worktrees = %d, want 2", n)
	}
}

func TestCreateAfterRemoveKeepsBranch(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	h := Local()
	wt, err := Create(ctx, h, f.repo, "", "BE-5", "", testIdentity)
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(wt.Path, "done.txt"), "x\n")
	if ok, err := CommitLeftovers(ctx, h, wt, "BE-5 work"); err != nil || !ok {
		t.Fatalf("CommitLeftovers = %v, %v", ok, err)
	}
	sha := git(t, wt.Path, "rev-parse", "HEAD")
	if err := Remove(ctx, h, f.repo, wt); err != nil {
		t.Fatal(err)
	}
	again, err := Create(ctx, h, f.repo, "", "BE-5", "", testIdentity)
	if err != nil {
		t.Fatal(err)
	}
	if git(t, again.Path, "rev-parse", "HEAD") != sha {
		t.Error("recreated worktree lost the earlier commit")
	}
}

func TestCreateContinuesRemoteBranch(t *testing.T) {
	f := newFixture(t)
	git(t, f.repo, "checkout", "-b", "BE-6")
	git(t, f.repo, "commit", "--allow-empty", "-m", "earlier agent work")
	git(t, f.repo, "push", "origin", "BE-6")
	sha := git(t, f.repo, "rev-parse", "HEAD")
	git(t, f.repo, "checkout", "main")
	git(t, f.repo, "branch", "-D", "BE-6")

	wt, err := Create(context.Background(), Local(), f.repo, "", "BE-6", "", testIdentity)
	if err != nil {
		t.Fatal(err)
	}
	if git(t, wt.Path, "rev-parse", "HEAD") != sha {
		t.Error("worktree should continue the branch that exists on origin")
	}
}

func TestCreateErrors(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	h := Local()
	tests := []struct {
		name         string
		branch, base string
		id           Identity
		want         string
	}{
		{"empty branch", "", "", testIdentity, "branch is empty"},
		{"bad branch", "a..b", "", testIdentity, "invalid branch"},
		{"dash branch", "-x", "", testIdentity, "invalid branch"},
		{"no identity", "BE-7", "", Identity{Name: "x"}, "no email"},
		{"unknown base", "BE-7", "nope", testIdentity, "origin/nope does not exist"},
		{"main checkout branch", "main", "", testIdentity, "main working tree"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Create(ctx, h, f.repo, "", tt.branch, tt.base, tt.id)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("err = %v, want %q", err, tt.want)
			}
		})
	}
	if n := worktreeCount(t, f.repo); n != 1 {
		t.Errorf("failed Create left worktrees behind: %d", n)
	}
	if _, err := Create(ctx, h, t.TempDir(), "", "BE-8", "", testIdentity); err == nil {
		t.Error("Create outside a repository must fail")
	}
}

func TestCommitLeftovers(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	h := Local()
	wt, err := Create(ctx, h, f.repo, "", "BE-9", "", testIdentity)
	if err != nil {
		t.Fatal(err)
	}
	before := git(t, wt.Path, "rev-parse", "HEAD")

	committed, err := CommitLeftovers(ctx, h, wt, "BE-9 leftovers")
	if err != nil || committed {
		t.Fatalf("clean tree: committed=%v err=%v", committed, err)
	}
	if git(t, wt.Path, "rev-parse", "HEAD") != before {
		t.Error("clean tree must not commit")
	}

	writeFile(t, filepath.Join(wt.Path, "README.md"), "changed\n")
	writeFile(t, filepath.Join(wt.Path, "sub", "new.txt"), "new\n")
	committed, err = CommitLeftovers(ctx, h, wt, "BE-9 leftovers")
	if err != nil || !committed {
		t.Fatalf("dirty tree: committed=%v err=%v", committed, err)
	}
	want := "BE-9 leftovers|dev+agent_backend_17@xlsft.ru|Claude · backend #17"
	if got := git(t, wt.Path, "log", "-1", "--format=%s|%ae|%an"); got != want {
		t.Errorf("commit = %q, want %q", got, want)
	}
	if st := git(t, wt.Path, "status", "--porcelain"); st != "" {
		t.Errorf("tree still dirty:\n%s", st)
	}
	if files := git(t, wt.Path, "show", "--name-only", "--format=", "HEAD"); !strings.Contains(files, "sub/new.txt") || !strings.Contains(files, "README.md") {
		t.Errorf("committed files:\n%s", files)
	}

	if _, err := CommitLeftovers(ctx, h, wt, " "); err == nil {
		t.Error("empty message must fail")
	}
}

func TestPush(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	h := Local()
	wt, err := Create(ctx, h, f.repo, "", "BE-10", "", testIdentity)
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(wt.Path, "p.txt"), "p\n")
	if _, err := CommitLeftovers(ctx, h, wt, "BE-10 p"); err != nil {
		t.Fatal(err)
	}
	if err := Push(ctx, h, wt); err != nil {
		t.Fatal(err)
	}
	head, _ := Head(ctx, h, wt)
	if got := git(t, f.origin, "rev-parse", "refs/heads/BE-10"); got != head {
		t.Errorf("origin branch = %s, want %s", got, head)
	}
	if got := git(t, wt.Path, "rev-parse", "--abbrev-ref", "@{upstream}"); got != "origin/BE-10" {
		t.Errorf("upstream = %q", got)
	}
}

func TestAhead(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	h := Local()
	wt, err := Create(ctx, h, f.repo, "", "BE-16", "", testIdentity)
	if err != nil {
		t.Fatal(err)
	}
	if n, err := Ahead(ctx, h, wt); n != 0 || err != nil {
		t.Errorf("new branch: Ahead = %d, %v", n, err)
	}
	git(t, wt.Path, "commit", "--allow-empty", "-m", "BE-16 one")
	git(t, wt.Path, "commit", "--allow-empty", "-m", "BE-16 two")
	if n, err := Ahead(ctx, h, wt); n != 2 || err != nil {
		t.Errorf("two commits: Ahead = %d, %v", n, err)
	}
	if _, err := Ahead(ctx, h, Worktree{Path: wt.Path, Branch: wt.Branch, Base: "nope"}); err == nil {
		t.Error("an unknown base must fail")
	}
	if _, err := Ahead(ctx, h, Worktree{Path: wt.Path}); err == nil {
		t.Error("an empty base must fail")
	}
}

func TestPushRejected(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("hooks need a POSIX shell")
	}
	f := newFixture(t)
	ctx := context.Background()
	h := Local()
	wt, err := Create(ctx, h, f.repo, "", "BE-11", "", testIdentity)
	if err != nil {
		t.Fatal(err)
	}
	hook := "#!/bin/sh\necho 'pushes are closed today' >&2\nexit 1\n"
	if err := os.WriteFile(filepath.Join(f.origin, "hooks", "pre-receive"), []byte(hook), 0o755); err != nil {
		t.Fatal(err)
	}
	git(t, wt.Path, "commit", "--allow-empty", "-m", "BE-11 x")
	err = Push(ctx, h, wt)
	if err == nil {
		t.Fatal("push must be rejected")
	}
	if msg := err.Error(); !strings.Contains(msg, "pushes are closed today") || !strings.Contains(msg, "BE-11") {
		t.Errorf("error = %v", err)
	}
	if NonFastForward(err) {
		t.Error("a push the hook declined is not a non-fast-forward")
	}
	if refs := git(t, f.origin, "for-each-ref", "--format=%(refname)", "refs/heads/BE-11"); refs != "" {
		t.Errorf("rejected push created %q", refs)
	}
}

func TestForcePushWithLease(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	h := Local()
	wt, err := Create(ctx, h, f.repo, "", "BE-12", "", testIdentity)
	if err != nil {
		t.Fatal(err)
	}
	git(t, wt.Path, "commit", "--allow-empty", "-m", "BE-12 one")
	if err := Push(ctx, h, wt); err != nil {
		t.Fatal(err)
	}
	pushed, _ := Head(ctx, h, wt)

	// The agent rewrites the history: a plain push is refused.
	git(t, wt.Path, "commit", "--amend", "--allow-empty", "-m", "BE-12 one, amended")
	err = Push(ctx, h, wt)
	if err == nil || !NonFastForward(err) {
		t.Fatalf("Push = %v, want a non-fast-forward rejection", err)
	}
	if err := ForcePush(ctx, h, wt, pushed); err != nil {
		t.Fatal(err)
	}
	amended, _ := Head(ctx, h, wt)
	if got := git(t, f.origin, "rev-parse", "refs/heads/BE-12"); got != amended {
		t.Errorf("origin = %s, want %s", got, amended)
	}

	// Someone else pushed meanwhile: the lease no longer holds.
	other := filepath.Join(t.TempDir(), "other")
	git(t, filepath.Dir(other), "clone", "-b", "BE-12", f.origin, other)
	git(t, other, "-c", "user.name=O", "-c", "user.email=o@x", "commit", "--allow-empty", "-m", "theirs")
	git(t, other, "push", "origin", "BE-12")
	theirs := git(t, other, "rev-parse", "HEAD")
	git(t, wt.Path, "commit", "--amend", "--allow-empty", "-m", "BE-12 one, amended again")
	if err := Push(ctx, h, wt); !NonFastForward(err) {
		t.Fatalf("Push = %v, want a rejection", err)
	}
	if err := ForcePush(ctx, h, wt, amended); err == nil {
		t.Fatal("the lease did not protect the other commit")
	}
	if got := git(t, f.origin, "rev-parse", "refs/heads/BE-12"); got != theirs {
		t.Errorf("origin = %s, want theirs %s", got, theirs)
	}
	if err := ForcePush(ctx, h, wt, ""); err == nil {
		t.Error("an empty lease must fail")
	}
	if NonFastForward(errors.New("[rejected] (non-fast-forward)")) {
		t.Error("only a git error is a rejection")
	}
}

func TestCheckedOut(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	h := Local()
	if p, err := CheckedOut(ctx, h, f.repo, "BE-13"); err != nil || p != "" {
		t.Errorf("before = %q, %v", p, err)
	}
	wt, err := Create(ctx, h, f.repo, filepath.Join(t.TempDir(), "other-pipeline"), "BE-13", "", testIdentity)
	if err != nil {
		t.Fatal(err)
	}
	if p, err := CheckedOut(ctx, h, f.repo, "BE-13"); err != nil || p != wt.Path {
		t.Errorf("CheckedOut = %q, %v; want %q", p, err, wt.Path)
	}
	if p, _ := CheckedOut(ctx, h, f.repo, "main"); realPath(t, p) != realPath(t, f.repo) {
		t.Errorf("main is checked out at %q", p)
	}
	if err := Remove(ctx, h, f.repo, wt); err != nil {
		t.Fatal(err)
	}
	if p, err := CheckedOut(ctx, h, f.repo, "BE-13"); err != nil || p != "" {
		t.Errorf("after removal = %q, %v", p, err)
	}
	if _, err := CheckedOut(ctx, h, t.TempDir(), "x"); err == nil {
		t.Error("not a repository: no error")
	}
}

func TestRemove(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	h := Local()
	wt, err := Create(ctx, h, f.repo, "", "BE-12", "", testIdentity)
	if err != nil {
		t.Fatal(err)
	}
	// Dirty and untracked files do not stop Remove; they are discarded.
	writeFile(t, filepath.Join(wt.Path, "junk.txt"), "j\n")
	if err := Remove(ctx, h, f.repo, wt); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(wt.Path); !os.IsNotExist(err) {
		t.Errorf("worktree directory still there: %v", err)
	}
	if got := git(t, f.repo, "branch", "--list", "BE-12"); !strings.Contains(got, "BE-12") {
		t.Errorf("branch must stay, got %q", got)
	}
	if strings.Contains(git(t, f.repo, "worktree", "list"), "BE-12") {
		t.Error("worktree still registered")
	}
	// Twice is fine, and so is a directory deleted by hand.
	if err := Remove(ctx, h, f.repo, wt); err != nil {
		t.Errorf("second Remove: %v", err)
	}
	wt2, err := Create(ctx, h, f.repo, "", "BE-13", "", testIdentity)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(wt2.Path); err != nil {
		t.Fatal(err)
	}
	if err := Remove(ctx, h, f.repo, wt2); err != nil {
		t.Errorf("Remove after manual delete: %v", err)
	}
	if err := Remove(ctx, h, f.repo, Worktree{}); err == nil {
		t.Error("empty worktree must fail")
	}
}

func TestExists(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	h := Local()
	wt, err := Create(ctx, h, f.repo, "", "BE-15", "", testIdentity)
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := Exists(ctx, h, wt); !ok || err != nil {
		t.Errorf("Exists = %v, %v", ok, err)
	}
	if ok, _ := Exists(ctx, h, Worktree{}); ok {
		t.Error("an empty worktree exists")
	}
	if err := os.RemoveAll(wt.Path); err != nil {
		t.Fatal(err)
	}
	if ok, err := Exists(ctx, h, wt); ok || err != nil {
		t.Errorf("deleted: Exists = %v, %v", ok, err)
	}
	// Create makes it again on the same branch.
	again, err := Create(ctx, h, f.repo, "", "BE-15", "", testIdentity)
	if err != nil {
		t.Fatal(err)
	}
	if ok, _ := Exists(ctx, h, again); !ok {
		t.Error("the recreated worktree does not exist")
	}
	cctx, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := Exists(cctx, h, again); !errors.Is(err, context.Canceled) {
		t.Errorf("canceled: err = %v", err)
	}
}

func TestRunDir(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	dir, err := RunDir(ctx, Local(), f.repo, 17)
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(f.repo, ".git", "ymcp", "runs", "17")
	if realPath(t, dir) != realPath(t, want) {
		t.Errorf("RunDir = %q, want %q", dir, want)
	}
	if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
		t.Errorf("run dir not created: %v", err)
	}
	if again, err := RunDir(ctx, Local(), f.repo, 17); err != nil || again != dir {
		t.Errorf("second RunDir = %q, %v", again, err)
	}
	// From inside a linked worktree the directory is still the shared one.
	wt, err := Create(ctx, Local(), f.repo, "", "BE-14", "", testIdentity)
	if err != nil {
		t.Fatal(err)
	}
	if inWT, err := RunDir(ctx, Local(), wt.Path, 17); err != nil || realPath(t, inWT) != realPath(t, dir) {
		t.Errorf("RunDir from worktree = %q, %v", inWT, err)
	}
}
