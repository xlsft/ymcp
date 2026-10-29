//go:build e2e

// Package e2e runs the ymcp binary as an MCP server over stdio against a real
// YouTrack, the way MCP clients do. With YOUTRACK_E2E_ENDPOINT,
// YOUTRACK_E2E_LOGIN and YOUTRACK_E2E_PASSWORD it tests that instance;
// without them it starts a fresh one in Docker for the run (see
// youtrack_test.go), and skips when there is no Docker.
//
//	go test -tags e2e -count=1 -v ./e2e
//
// Everything the tests create carries the marker prefix and the run's ID and
// is deleted when they finish. Runs may overlap: each sweeps only its own
// objects and those of runs long over, and fails only for its own leftovers.
//
// An instance that is down or read-only isn't a failure of ymcp: the run
// waits for it for YOUTRACK_E2E_WAIT (10m by default) and, if it stays
// unavailable or goes away midway, exits with exitUnavailable.
package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// marker prefixes every object the tests create.
const marker = "ymcp-e2e"

var (
	// admin is the server signed in as the E2E account, a YouTrack admin.
	admin *server
	// adminLogin and adminPassword sign in as the E2E account.
	adminLogin, adminPassword string
	// bin and workDir are the built ymcp and the run's temporary directory.
	bin, workDir string
	// endpoint is the YouTrack under test.
	endpoint string
	// runID tells this run's objects apart from other runs': the Unix
	// time in seconds, its last six digits. Every marked name carries it.
	runID = fmt.Sprintf("%06d", time.Now().Unix()%runIDs)
)

const (
	runIDs = 1_000_000
	// staleAfter is when a run's objects count as left behind: far past
	// the job's timeout, so no run still working loses them.
	staleAfter = 2 * time.Hour
)

// runIDIn finds the run ID in a marked name: six digits on their own.
var runIDIn = regexp.MustCompile(`(?:^|[^0-9])([0-9]{6})(?:[^0-9]|$)`)

// stale reports whether a marked object belongs to a run that is long
// over, judged by the run ID in its name; one without an ID is stale.
func stale(name string) bool {
	m := runIDIn.FindStringSubmatch(name)
	if m == nil {
		return true
	}
	then, _ := strconv.Atoi(m[1])
	age := (int(time.Now().Unix()%runIDs) - then + runIDs) % runIDs
	return time.Duration(age)*time.Second > staleAfter
}

// ours reports whether a marked object is this run's.
func ours(name string) bool {
	m := runIDIn.FindStringSubmatch(name)
	return m != nil && m[1] == runID
}

// server is a ymcp process serving one account over stdio.
type server struct {
	name    string
	session *mcp.ClientSession
	logFile string
}

func TestMain(m *testing.M) { os.Exit(run(m)) }

func run(m *testing.M) int {
	endpoint = os.Getenv("YOUTRACK_E2E_ENDPOINT")
	login := os.Getenv("YOUTRACK_E2E_LOGIN")
	password := os.Getenv("YOUTRACK_E2E_PASSWORD")
	if endpoint == "" {
		if exec.Command("docker", "info").Run() != nil {
			// In CI that would pass a release untested.
			if os.Getenv("CI") != "" {
				fmt.Println("e2e: no YOUTRACK_E2E_ENDPOINT and no Docker to start a YouTrack in")
				return 1
			}
			fmt.Println("e2e: no YOUTRACK_E2E_ENDPOINT and no Docker to start a YouTrack in; skipping")
			return 0
		}
		url, stop, err := startYouTrack()
		if err != nil {
			fmt.Println("e2e:", err)
			return 1
		}
		defer stop()
		endpoint, login, password = url, throwawayLogin, throwawayPassword
	}
	if login == "" || password == "" {
		fmt.Println("e2e: YOUTRACK_E2E_ENDPOINT is set without YOUTRACK_E2E_LOGIN and YOUTRACK_E2E_PASSWORD")
		return 1
	}
	adminLogin, adminPassword = login, password

	dir, err := os.MkdirTemp("", "ymcp-e2e-")
	if err != nil {
		panic(err)
	}
	defer os.RemoveAll(dir)
	workDir = dir
	bin = filepath.Join(dir, "ymcp")
	if runtime.GOOS == "windows" {
		bin += ".exe"
	}
	build := exec.Command("go", "build", "-o", bin, "../cmd/ymcp")
	build.Stdout, build.Stderr = os.Stdout, os.Stderr
	if err := build.Run(); err != nil {
		fmt.Println("e2e: build failed:", err)
		return 1
	}

	if admin, err = startServer("admin", login, password); err != nil {
		fmt.Println("e2e:", err)
		return 1
	}
	defer admin.close()

	wait := 10 * time.Minute
	if v := os.Getenv("YOUTRACK_E2E_WAIT"); v != "" {
		if wait, err = time.ParseDuration(v); err != nil {
			fmt.Println("e2e: YOUTRACK_E2E_WAIT:", err)
			return 1
		}
	}
	if err := waitAvailable(wait); err != nil {
		fmt.Printf("e2e: YouTrack stayed unavailable for %s, so nothing was tested: %s\n", wait, err)
		return exitUnavailable
	}

	sweep(stale)
	if project, err = newProject(); err != nil {
		fmt.Println("e2e: creating the run's project:", err)
		if project != nil && project.projectID != "" {
			project.delete()
		}
		if outageSeen() != "" {
			return exitUnavailable
		}
		return 1
	}
	code := m.Run()
	project.delete()
	sweep(func(name string) bool { return ours(name) || stale(name) })
	// YouTrack removes a deleted project's boards and their backlog
	// searches in the background; what a listing missed meanwhile is swept
	// again before it counts as left behind.
	left := leftovers()
	for i := 0; len(left) > 0 && i < 3; i++ {
		time.Sleep(2 * time.Second)
		sweep(ours)
		left = leftovers()
	}
	if len(left) > 0 {
		fmt.Println("e2e: objects left behind:", left)
		code = 1
	}
	if code != 0 {
		admin.dumpLog()
	}
	// Results from while the instance was out say nothing about ymcp, and
	// an outage during the cleanup may have left objects for later runs.
	if text := outageSeen(); text != "" {
		fmt.Printf("e2e: YouTrack became unavailable during the run, so its results don't count: %s\n", shortText(text))
		return exitUnavailable
	}
	return code
}

// startServer runs `ymcp -v serve` signed in as login and connects to it.
// Destructive tools run at once, so the suite can clean up after itself;
// env adds or overrides variables.
func startServer(name, login, password string, env ...string) (*server, error) {
	s := &server{name: name, logFile: filepath.Join(workDir, name+".log")}
	cmd := exec.Command(bin, "-v", "serve")
	cmd.Env = append(cleanEnv(),
		"YOUTRACK_URL="+endpoint,
		"YOUTRACK_LOGIN="+login,
		"YOUTRACK_PASSWORD="+password,
		"YMCP_CONFIG="+filepath.Join(workDir, name+".json"),
		"YMCP_LOG_FILE="+s.logFile,
		"YMCP_CONFIRM=off",
	)
	cmd.Env = append(cmd.Env, env...)
	cmd.Stderr = os.Stderr // the server's own log, visible in test output
	client := mcp.NewClient(&mcp.Implementation{Name: "ymcp-e2e-" + name, Version: runID}, nil)
	var err error
	if s.session, err = client.Connect(context.Background(), &mcp.CommandTransport{Command: cmd}, nil); err != nil {
		s.dumpLog()
		return nil, fmt.Errorf("connect as %s: %w", name, err)
	}
	return s, nil
}

func (s *server) close() { s.session.Close() }

// cleanEnv is the environment without YOUTRACK_*/YMCP_* settings of the machine.
func cleanEnv() []string {
	var env []string
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "YOUTRACK_") && !strings.HasPrefix(kv, "YMCP_") {
			env = append(env, kv)
		}
	}
	return env
}

func (s *server) dumpLog() {
	data, err := os.ReadFile(s.logFile)
	if err != nil {
		return
	}
	fmt.Printf("---- ymcp server log (%s) ----\n", s.name)
	fmt.Println(scrub(string(data)))
}

// result is a tool call outcome.
type result struct {
	text    string
	isError bool
}

// call invokes a tool as the admin; see server.call.
func call(t testing.TB, tool string, args map[string]any) result {
	t.Helper()
	return admin.call(t, tool, args)
}

// must calls a tool as the admin; see server.must.
func must(t testing.TB, tool string, args map[string]any, out any) string {
	t.Helper()
	return admin.must(t, tool, args, out)
}

// mustFail calls a tool as the admin; see server.mustFail.
func mustFail(t testing.TB, tool string, args map[string]any, want string) {
	t.Helper()
	admin.mustFail(t, tool, args, want)
}

// call invokes a tool and returns its text, failing the test on protocol errors.
func (s *server) call(t testing.TB, tool string, args map[string]any) result {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	res, err := s.session.CallTool(ctx, &mcp.CallToolParams{Name: tool, Arguments: args})
	if err != nil {
		t.Fatalf("%s: %v", tool, err)
	}
	var b strings.Builder
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			b.WriteString(tc.Text)
		}
	}
	if res.IsError {
		noteOutage(b.String())
	}
	return result{b.String(), res.IsError}
}

// must calls a tool, fails the test if it returned an error and decodes
// the JSON result into out (when out is not nil).
func (s *server) must(t testing.TB, tool string, args map[string]any, out any) string {
	t.Helper()
	r := s.call(t, tool, args)
	if r.isError {
		t.Fatalf("%s", scrub(fmt.Sprintf("%s(%v) failed: %s", tool, args, r.text)))
	}
	if out != nil {
		if err := json.Unmarshal([]byte(r.text), out); err != nil {
			t.Fatalf("%s: decode %q: %v", tool, r.text, err)
		}
	}
	return r.text
}

// mustFail calls a tool that is expected to return an error containing want.
func (s *server) mustFail(t testing.TB, tool string, args map[string]any, want string) {
	t.Helper()
	r := s.call(t, tool, args)
	if !r.isError {
		t.Fatalf("%s", scrub(fmt.Sprintf("%s(%v) succeeded, want an error with %q: %s", tool, args, want, r.text)))
	}
	if !strings.Contains(r.text, want) {
		t.Fatalf("%s error %q does not contain %q", tool, r.text, want)
	}
}

// api is a call_api request that must succeed; pass "hub" for the Hub API.
func api(t testing.TB, method, path string, query map[string]any, body any, out any, hub ...string) {
	t.Helper()
	args := map[string]any{"method": method, "path": path}
	if len(hub) > 0 {
		args["api"] = hub[0]
	}
	if query != nil {
		args["query"] = query
	}
	if body != nil {
		args["body"] = body
	}
	must(t, "call_api", args, out)
}

// marked lists objects named with the marker, by API path: projects (not
// yet deleted), boards, tags, saved searches and Hub users (as hub:users).
// Deleted projects linger as "<name> (pending deletion)" while YouTrack
// removes them in the background.
func marked() map[string][]string {
	t := &sweepT{}
	found := map[string][]string{}
	var hub struct {
		Users []struct{ ID, Login string }
	}
	r := call(t, "call_api", map[string]any{"api": "hub", "method": "GET", "path": "users", "query": map[string]any{"fields": "id,login", "$top": 1000}})
	if r.isError {
		fmt.Printf("e2e: listing Hub users: %s\n", r.text)
	}
	json.Unmarshal([]byte(r.text), &hub)
	for _, u := range hub.Users {
		if strings.HasPrefix(u.Login, marker) && u.Login != memberLogin {
			found["hub:users"] = append(found["hub:users"], u.ID+" "+u.Login)
		}
	}
	for _, path := range []string{"admin/projects", "agiles", "tags", "savedQueries"} {
		var items []struct{ ID, Name string }
		r := listing(t, path)
		if r.isError {
			fmt.Printf("e2e: listing %s: %s\n", path, r.text)
			continue
		}
		json.Unmarshal([]byte(r.text), &items)
		for _, it := range items {
			if strings.HasPrefix(it.Name, marker) && !strings.HasSuffix(it.Name, "(pending deletion)") {
				found[path] = append(found[path], it.ID+" "+it.Name)
			}
		}
	}
	return found
}

// listing reads the id and name of everything at an API path. YouTrack
// fails a listing with "… was removed" while it removes objects of a
// deleted project in the background, so a failed one is tried again.
func listing(t *sweepT, path string) result {
	for try := 1; ; try++ {
		r := call(t, "call_api", map[string]any{"method": "GET", "path": path, "query": map[string]any{"fields": "id,name", "$top": -1}})
		if !r.isError || try == 3 {
			return r
		}
		time.Sleep(time.Second)
	}
}

// sweep deletes the marked objects whose names match: this run's, or
// those of runs long over (see stale), never those of a run still going.
func sweep(match func(name string) bool) {
	t := &sweepT{}
	for path, items := range marked() {
		for _, it := range items {
			id, name, _ := strings.Cut(it, " ")
			if !match(name) {
				continue
			}
			args := map[string]any{"method": "DELETE", "path": path + "/" + id}
			if p, ok := strings.CutPrefix(path, "hub:"); ok {
				args["api"], args["path"] = "hub", p+"/"+id
			}
			r := call(t, "call_api", args)
			fmt.Printf("e2e: sweep %s %q: error=%v\n", path, name, r.isError)
		}
	}
}

// unbanMember lifts a ban YouTrack put on the member account when the
// license had no seat left for it.
func unbanMember(t *testing.T) {
	t.Helper()
	var users []struct {
		ID, Login string
		Banned    bool
	}
	api(t, "GET", "users", map[string]any{"fields": "id,login,banned", "query": memberLogin}, nil, &users)
	for _, u := range users {
		if u.Login == memberLogin && u.Banned {
			r := call(t, "call_api", map[string]any{"method": "POST", "path": "users/" + u.ID, "query": map[string]any{"fields": "banned"}, "body": map[string]any{"banned": false}})
			t.Logf("unbanning %s in YouTrack: error=%v %s", memberLogin, r.isError, shortText(r.text))
		}
	}
}

// scrub hides the passwords the suite knows from text bound for the log;
// CI logs of the public repository are public.
func scrub(s string) string {
	for _, pw := range []string{adminPassword, memberPassword()} {
		if pw != "" {
			s = strings.ReplaceAll(s, pw, "***")
		}
	}
	return s
}

// leftovers is what sweeping couldn't remove of this run's objects.
func leftovers() map[string][]string {
	left := map[string][]string{}
	for path, items := range marked() {
		for _, it := range items {
			if _, name, _ := strings.Cut(it, " "); ours(name) {
				left[path] = append(left[path], it)
			}
		}
	}
	return left
}

// sweepT lets sweep use the test helpers outside a test.
type sweepT struct{ testing.TB }

func (sweepT) Helper()                   {}
func (sweepT) Fatalf(f string, a ...any) { panic(fmt.Sprintf(f, a...)) }
