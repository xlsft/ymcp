package engine

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"github.com/xlsft/ymcp/internal/pipeline/workspace"
)

// Names of the checks of a remote host.
const (
	checkSSHName     = "ssh"
	checkSSHGit      = "ssh: git"
	checkSSHForward  = "ssh: MCP forward"
	sshProbeTimeout  = 30 * time.Second
	sshProbeResponse = "ymcp ssh check"
)

// checkSSH checks a remote host: the connection, git there, and the
// forward that takes the agent's MCP requests back to this machine. It
// reports whether the host is reachable at all.
func checkSSH(ctx context.Context, host workspace.Host, add func(string, bool, string)) bool {
	out, stderr, err := host.Run(ctx, "", []string{"sh", "-c", "uname -sm 2>/dev/null || echo unknown system"}, nil)
	if err != nil {
		if ctx.Err() != nil {
			return false
		}
		add(checkSSHName, false, commandError(err, stderr))
		return false
	}
	add(checkSSHName, true, "connected to "+host.Name()+" ("+strings.TrimSpace(string(out))+")")

	out, stderr, err = host.Run(ctx, "", []string{"git", "--version"}, nil)
	if err != nil {
		if ctx.Err() != nil {
			return true
		}
		add(checkSSHGit, false, "git does not run on "+host.Name()+": "+commandError(err, stderr))
	} else {
		add(checkSSHGit, true, strings.TrimSpace(string(out)))
	}

	if rh, ok := host.(remoteHost); ok {
		ok, detail := probeForward(ctx, rh)
		add(checkSSHForward, ok, detail)
	}
	return true
}

func commandError(err error, stderr []byte) string {
	if s := strings.TrimSpace(string(stderr)); s != "" && !strings.Contains(err.Error(), s) {
		return err.Error() + ": " + s
	}
	return err.Error()
}

// probeForward forwards a throwaway HTTP listener to the host and has git
// there send a request to it. The request arriving proves that an agent on
// the host can reach ymcp's MCP server the same way.
func probeForward(ctx context.Context, h remoteHost) (bool, string) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return false, "listening for the test request: " + err.Error()
	}
	path := "/ymcp-check-" + randomHex(8)
	var hit atomic.Bool
	srv := &http.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if strings.HasPrefix(r.URL.Path, path) {
				hit.Store(true)
			}
			http.Error(w, sshProbeResponse, http.StatusNotFound)
		}),
		ReadHeaderTimeout: 10 * time.Second,
	}
	go srv.Serve(l)
	defer srv.Close()

	ctx, cancel := context.WithTimeout(ctx, sshProbeTimeout)
	defer cancel()
	fwd, err := h.Forward(ctx, l.Addr().(*net.TCPAddr).Port)
	if err != nil {
		return false, err.Error()
	}
	defer fwd.Close()
	port, err := fwd.Port(ctx)
	if err != nil {
		return false, err.Error()
	}
	url := fmt.Sprintf("http://127.0.0.1:%d%s", port, path)
	// git is on every host ymcp works with; any HTTP client would do.
	_, stderr, gerr := h.Run(ctx, "", []string{"env", "no_proxy=127.0.0.1", "NO_PROXY=127.0.0.1", "git", "ls-remote", url}, nil)
	if hit.Load() {
		return true, fmt.Sprintf("an agent on %s reaches ymcp's MCP server through 127.0.0.1:%d there", h.Name(), port)
	}
	detail := fmt.Sprintf("a test request from %s to 127.0.0.1:%d did not arrive", h.Name(), port)
	if gerr != nil {
		detail += ": " + commandError(gerr, stderr)
	}
	return false, detail
}

func randomHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
