//go:build e2e

// Package e2e runs the ymcp binary as an MCP server over stdio against a real
// YouTrack, the way MCP clients do. It needs YOUTRACK_E2E_ENDPOINT,
// YOUTRACK_E2E_LOGIN and YOUTRACK_E2E_PASSWORD; without them it skips.
//
//	go test -tags e2e -count=1 -v ./e2e
//
// Everything the tests create carries the marker prefix and is deleted when
// they finish; leftovers from crashed runs are swept at start and end.
package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// marker prefixes every object the tests create.
const marker = "ymcp-e2e"

var (
	session *mcp.ClientSession
	logFile string
	// runID tells this run's objects apart from other runs'.
	runID = fmt.Sprintf("%d", time.Now().Unix()%1_000_000)
)

func TestMain(m *testing.M) {
	endpoint := os.Getenv("YOUTRACK_E2E_ENDPOINT")
	login := os.Getenv("YOUTRACK_E2E_LOGIN")
	password := os.Getenv("YOUTRACK_E2E_PASSWORD")
	if endpoint == "" || login == "" || password == "" {
		fmt.Println("e2e: YOUTRACK_E2E_ENDPOINT, YOUTRACK_E2E_LOGIN and YOUTRACK_E2E_PASSWORD are not set; skipping")
		os.Exit(0)
	}

	dir, err := os.MkdirTemp("", "ymcp-e2e-")
	if err != nil {
		panic(err)
	}
	defer os.RemoveAll(dir)
	bin := filepath.Join(dir, "ymcp")
	if runtime.GOOS == "windows" {
		bin += ".exe"
	}
	build := exec.Command("go", "build", "-o", bin, "../cmd/ymcp")
	build.Stdout, build.Stderr = os.Stdout, os.Stderr
	if err := build.Run(); err != nil {
		fmt.Println("e2e: build failed:", err)
		os.Exit(1)
	}

	logFile = filepath.Join(dir, "ymcp.log")
	cmd := exec.Command(bin, "-v", "serve")
	cmd.Env = append(cleanEnv(),
		"YOUTRACK_URL="+endpoint,
		"YOUTRACK_LOGIN="+login,
		"YOUTRACK_PASSWORD="+password,
		"YMCP_CONFIG="+filepath.Join(dir, "config.json"),
		"YMCP_LOG_FILE="+logFile,
	)
	cmd.Stderr = os.Stderr // the server's own log, visible in test output
	ctx := context.Background()
	client := mcp.NewClient(&mcp.Implementation{Name: "ymcp-e2e", Version: runID}, nil)
	session, err = client.Connect(ctx, &mcp.CommandTransport{Command: cmd}, nil)
	if err != nil {
		fmt.Println("e2e: connect:", err)
		dumpLog()
		os.Exit(1)
	}

	sweep()
	code := m.Run()
	sweep()
	if left := leftovers(); len(left) > 0 {
		fmt.Println("e2e: objects left behind:", left)
		code = 1
	}
	session.Close()
	if code != 0 {
		dumpLog()
	}
	os.Exit(code)
}

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

func dumpLog() {
	data, err := os.ReadFile(logFile)
	if err != nil {
		return
	}
	fmt.Println("---- ymcp server log ----")
	fmt.Println(string(data))
}

// result is a tool call outcome.
type result struct {
	text    string
	isError bool
}

// call invokes a tool and returns its text, failing the test on protocol errors.
func call(t testing.TB, tool string, args map[string]any) result {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	res, err := session.CallTool(ctx, &mcp.CallToolParams{Name: tool, Arguments: args})
	if err != nil {
		t.Fatalf("%s: %v", tool, err)
	}
	var b strings.Builder
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			b.WriteString(tc.Text)
		}
	}
	return result{b.String(), res.IsError}
}

// must calls a tool, fails the test if it returned an error and decodes
// the JSON result into out (when out is not nil).
func must(t testing.TB, tool string, args map[string]any, out any) string {
	t.Helper()
	r := call(t, tool, args)
	if r.isError {
		t.Fatalf("%s(%v) failed: %s", tool, args, r.text)
	}
	if out != nil {
		if err := json.Unmarshal([]byte(r.text), out); err != nil {
			t.Fatalf("%s: decode %q: %v", tool, r.text, err)
		}
	}
	return r.text
}

// mustFail calls a tool that is expected to return an error containing want.
func mustFail(t testing.TB, tool string, args map[string]any, want string) {
	t.Helper()
	r := call(t, tool, args)
	if !r.isError {
		t.Fatalf("%s(%v) succeeded, want an error with %q: %s", tool, args, want, r.text)
	}
	if !strings.Contains(r.text, want) {
		t.Fatalf("%s error %q does not contain %q", tool, r.text, want)
	}
}

// api is a call_api request that must succeed.
func api(t testing.TB, method, path string, query map[string]any, body any, out any) {
	t.Helper()
	args := map[string]any{"method": method, "path": path}
	if query != nil {
		args["query"] = query
	}
	if body != nil {
		args["body"] = body
	}
	must(t, "call_api", args, out)
}

// marked lists objects named with the marker, by API path: projects (not
// yet deleted), boards, tags and saved searches. Deleted projects linger as
// "<name> (pending deletion)" while YouTrack removes them in the background.
func marked() map[string][]string {
	t := &sweepT{}
	found := map[string][]string{}
	for _, path := range []string{"admin/projects", "agiles", "tags", "savedQueries"} {
		var items []struct{ ID, Name string }
		r := call(t, "call_api", map[string]any{"method": "GET", "path": path, "query": map[string]any{"fields": "id,name", "$top": -1}})
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

// sweep deletes marked objects left by any run, this one included.
func sweep() {
	t := &sweepT{}
	for path, items := range marked() {
		for _, it := range items {
			id, name, _ := strings.Cut(it, " ")
			r := call(t, "call_api", map[string]any{"method": "DELETE", "path": path + "/" + id})
			fmt.Printf("e2e: sweep %s %q: error=%v\n", path, name, r.isError)
		}
	}
}

// leftovers is what sweeping couldn't remove.
func leftovers() map[string][]string { return marked() }

// sweepT lets sweep use the test helpers outside a test.
type sweepT struct{ testing.TB }

func (sweepT) Helper()                   {}
func (sweepT) Fatalf(f string, a ...any) { panic(fmt.Sprintf(f, a...)) }
