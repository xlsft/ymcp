package agent

import (
	"os"
	"runtime"
	"sort"
	"strings"
)

// scrubbed are variables dropped by name; scrubbedPrefixes by prefix.
var (
	scrubbed = map[string]bool{
		// An enclosing Claude Code session: with these a child claude
		// talks to the parent's proxy and can't sign in.
		"CLAUDECODE":                       true,
		"ANTHROPIC_BASE_URL":               true,
		"API_TIMEOUT_MS":                   true,
		"USE_LOCAL_OAUTH":                  true,
		"USE_STAGING_OAUTH":                true,
		"DISABLE_MICROCOMPACT":             true,
		"MCP_CONNECTION_NONBLOCKING":       true,
		"MCP_SERVER_CONNECTION_BATCH_SIZE": true,
		"AI_AGENT":                         true,
		"BAGGAGE":                          true,
		"OPENCODE":                         true,
		// Git state of whatever ran ymcp; it would point the agent's git
		// at another repository or override the worktree's identity.
		"GIT_DIR":              true,
		"GIT_WORK_TREE":        true,
		"GIT_INDEX_FILE":       true,
		"GIT_COMMON_DIR":       true,
		"GIT_OBJECT_DIRECTORY": true,
		"GIT_PREFIX":           true,
		"GIT_AUTHOR_NAME":      true,
		"GIT_AUTHOR_EMAIL":     true,
		"GIT_AUTHOR_DATE":      true,
		"GIT_COMMITTER_NAME":   true,
		"GIT_COMMITTER_EMAIL":  true,
		"GIT_COMMITTER_DATE":   true,
	}
	scrubbedPrefixes = []string{
		"CLAUDE_",   // CLAUDE_CODE_*, CLAUDE_AGENT_SDK_*, CLAUDE_PID, …
		"CODEX_",    // a Codex session
		"YOUTRACK_", // ymcp's own credentials; agents reach YouTrack through the ymcp MCP server
	}
)

// Scrub returns env (KEY=VALUE entries, like os.Environ) without the
// variables of an enclosing agent session (CLAUDECODE, CLAUDE_*,
// ANTHROPIC_BASE_URL, …), git variables that would redirect the agent's
// repository or identity, and ymcp's YOUTRACK_* credentials.
func Scrub(env []string) []string {
	out := make([]string, 0, len(env))
	for _, kv := range env {
		key, _, _ := strings.Cut(kv, "=")
		if !dropVar(key) {
			out = append(out, kv)
		}
	}
	return out
}

func dropVar(key string) bool {
	if runtime.GOOS == "windows" {
		key = strings.ToUpper(key)
	}
	if scrubbed[key] {
		return true
	}
	for _, p := range scrubbedPrefixes {
		if strings.HasPrefix(key, p) {
			return true
		}
	}
	return false
}

// environ is the scrubbed environment of ymcp with extra set on top;
// extra may bring back a scrubbed variable on purpose.
func environ(extra map[string]string) []string {
	env := Scrub(os.Environ())
	if len(extra) == 0 {
		return env
	}
	fold := func(k string) string {
		if runtime.GOOS == "windows" {
			return strings.ToUpper(k)
		}
		return k
	}
	out := env[:0]
	for _, kv := range env {
		key, _, _ := strings.Cut(kv, "=")
		overridden := false
		for k := range extra {
			if fold(k) == fold(key) {
				overridden = true
				break
			}
		}
		if !overridden {
			out = append(out, kv)
		}
	}
	keys := make([]string, 0, len(extra))
	for k := range extra {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		out = append(out, k+"="+extra[k])
	}
	return out
}
