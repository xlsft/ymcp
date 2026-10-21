//go:build e2e

package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// TestHTTP serves two accounts, the admin and the member, from one
// `ymcp serve --transport http` behind a token.
func TestHTTP(t *testing.T) {
	dir := httpConfig(t)
	token := "e2e-token-" + runID
	port := freePort(t)
	logPath := filepath.Join(dir, "http.log")
	cmd := exec.Command(bin, "serve", "--transport", "http", "--addr", "127.0.0.1:"+port, "--token", token)
	cmd.Env = append(cleanEnv(), "YMCP_CONFIG="+filepath.Join(dir, "config.json"), "YMCP_LOG_FILE="+logPath, "YMCP_UPDATE_CHECK=off")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cmd.Process.Kill(); cmd.Wait() })
	logs := func() string { data, _ := os.ReadFile(logPath); return string(data) }
	checkHTTP(t, "http://127.0.0.1:"+port, token, logs)
}

// TestDockerImage builds the image from the Dockerfile and checks it the
// same way, with the config mounted as /config.
func TestDockerImage(t *testing.T) {
	if _, err := exec.LookPath("docker"); err != nil || exec.Command("docker", "info").Run() != nil {
		t.Skip("no Docker here")
	}
	image := "ymcp-e2e:" + runID
	build := exec.Command("docker", "build", "-q", "--build-arg", "VERSION=e2e", "-t", image, "..")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("docker build: %v\n%s", err, out)
	}
	t.Cleanup(func() { exec.Command("docker", "rmi", "-f", image).Run() })

	dir := httpConfig(t)
	// The image runs as nonroot (65532), so the mount must be readable.
	os.Chmod(dir, 0o755)
	os.Chmod(filepath.Join(dir, "config.json"), 0o644)
	token := "e2e-token-" + runID
	port := freePort(t)
	name := "ymcp-e2e-" + runID
	run := exec.Command("docker", "run", "-d", "--name", name, "-p", "127.0.0.1:"+port+":8090",
		"-v", dir+":/config", "-e", "YMCP_HTTP_TOKEN="+token, "-e", "YMCP_UPDATE_CHECK=off", image)
	if out, err := run.CombinedOutput(); err != nil {
		t.Fatalf("docker run: %v\n%s", err, out)
	}
	t.Cleanup(func() { exec.Command("docker", "rm", "-f", name).Run() })
	logs := func() string {
		out, _ := exec.Command("docker", "logs", name).CombinedOutput()
		return string(out)
	}
	checkHTTP(t, "http://127.0.0.1:"+port, token, logs)
	if !strings.Contains(logs(), "version=e2e") {
		t.Errorf("container log lacks the version:\n%s", scrub(logs()))
	}
}

// httpConfig writes a config with the admin and member profiles, passwords
// in the file as without a keychain, and returns its directory.
func httpConfig(t *testing.T) string {
	t.Helper()
	ensureMember(t)
	unbanMember(t)
	dir := t.TempDir()
	cfg := map[string]any{"profiles": map[string]any{
		"e2e-admin":  map[string]any{"displayName": "E2E admin", "url": endpoint, "login": os.Getenv("YOUTRACK_E2E_LOGIN"), "password": os.Getenv("YOUTRACK_E2E_PASSWORD")},
		"e2e-member": map[string]any{"displayName": "E2E member", "url": endpoint, "login": memberLogin, "password": memberPassword(), "tools": []string{"people", "projects"}},
	}}
	data, _ := json.Marshal(cfg)
	if err := os.WriteFile(filepath.Join(dir, "config.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

func freePort(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return fmt.Sprint(ln.Addr().(*net.TCPAddr).Port)
}

// bearer adds the token to every request.
type bearer struct{ token string }

func (b bearer) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+b.token)
	return http.DefaultTransport.RoundTrip(r)
}

func checkHTTP(t *testing.T, base, token string, logs func() string) {
	t.Helper()
	defer func() {
		if t.Failed() {
			fmt.Println("---- HTTP server log ----\n" + scrub(logs()))
		}
	}()
	authed := &http.Client{Transport: bearer{token}, Timeout: 30 * time.Second}
	deadline := time.Now().Add(30 * time.Second)
	for {
		resp, err := authed.Get(base + "/nope")
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode != http.StatusNotFound {
				t.Errorf("unknown profile: %d", resp.StatusCode)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("server didn't come up: %v", err)
		}
		time.Sleep(300 * time.Millisecond)
	}
	resp, err := http.Post(base+"/e2e-admin", "application/json", strings.NewReader("{}"))
	if err != nil || resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("without a token: %v %v", resp, err)
	}
	resp.Body.Close()

	for profile, want := range map[string]struct {
		login string
		tools int
	}{
		"e2e-admin":  {os.Getenv("YOUTRACK_E2E_LOGIN"), 56},
		"e2e-member": {memberLogin, 6},
	} {
		ctx := context.Background()
		cs, err := mcp.NewClient(&mcp.Implementation{Name: "ymcp-e2e-http", Version: runID}, nil).Connect(ctx,
			&mcp.StreamableClientTransport{Endpoint: base + "/" + profile, HTTPClient: authed}, nil)
		if err != nil {
			t.Fatalf("%s: connect: %v", profile, err)
		}
		tools, err := cs.ListTools(ctx, nil)
		if err != nil || len(tools.Tools) != want.tools {
			t.Errorf("%s: %d tools, want %d (%v)", profile, len(tools.Tools), want.tools, err)
		}
		res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "get_current_user", Arguments: map[string]any{}})
		cs.Close()
		if err != nil || res.IsError {
			t.Fatalf("%s: get_current_user: %v %+v", profile, err, res)
		}
		var me struct{ Login string }
		json.Unmarshal([]byte(res.Content[0].(*mcp.TextContent).Text), &me)
		if !strings.EqualFold(me.Login, want.login) {
			t.Errorf("%s signed in as %q, want %q", profile, me.Login, want.login)
		}
	}
	for _, line := range []string{"profile=e2e-admin", "profile=e2e-member", `tools="6 of 56: people, projects"`, `auth="bearer token"`} {
		if !strings.Contains(logs(), line) {
			t.Errorf("log lacks %s", line)
		}
	}
}
