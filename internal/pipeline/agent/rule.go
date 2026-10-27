package agent

import (
	"encoding/json"
	"net/url"
	"strings"
)

// ClaudeRule is the claude permission rule, as --allowedTools takes it,
// that allows exactly the call tc, so approving a denied call doesn't grant
// the whole tool:
//
//   - Bash(<command>) (PowerShell(<command>)) for a shell command, which
//     matches that command only;
//   - Edit(//<path>) for a call that writes a file (Edit, MultiEdit, Write,
//     NotebookEdit: claude matches file writes by Edit rules) and
//     Read(//<path>) for a read; "//" starts an absolute path;
//   - WebFetch(domain:<host>) for a fetch;
//   - the tool name for any other tool, such as an MCP tool.
//
// It is "" when the call can't be put into a rule exactly: a shell call
// without a command or a file call without a path, or one whose command or
// path holds characters claude would read as a pattern (*, ?, [ …) or that
// would break the comma and space separated --allowedTools list apart,
// such as parentheses. Such a call must not be approved by a broader rule.
func ClaudeRule(tc ToolCall) string {
	var in map[string]any
	_ = json.Unmarshal(tc.Input, &in)
	str := func(keys ...string) string {
		for _, k := range keys {
			if s, ok := in[k].(string); ok && strings.TrimSpace(s) != "" {
				return s
			}
		}
		return ""
	}
	switch tc.Title {
	case "":
		return ""
	case "Bash", "PowerShell":
		cmd := strings.TrimSpace(str("command"))
		if cmd == "" || strings.ContainsAny(cmd, "()*\x00") {
			return ""
		}
		return tc.Title + "(" + strings.ReplaceAll(cmd, `\`, `\\`) + ")"
	case "Edit", "MultiEdit", "Write", "NotebookEdit", "Read":
		path := rulePath(str("file_path", "notebook_path", "path"))
		if path == "" {
			return ""
		}
		tool := "Edit"
		if tc.Title == "Read" {
			tool = "Read"
		}
		return tool + "(/" + path + ")"
	case "WebFetch":
		u, err := url.Parse(str("url"))
		if err != nil {
			return ""
		}
		host := strings.ToLower(u.Hostname())
		if host == "" || strings.Trim(host, "abcdefghijklmnopqrstuvwxyz0123456789.-") != "" {
			return ""
		}
		return "WebFetch(domain:" + host + ")"
	}
	if strings.ContainsAny(tc.Title, "() ,*\t\n") {
		return ""
	}
	return tc.Title
}

// rulePath is an absolute path as claude's path rules spell it, without
// the leading "/" that a rule adds to mark it absolute: POSIX form, with a
// Windows drive as its first element (C:\x becomes /c/x). It is "" for a
// relative path and for one with characters the gitignore-style rule
// syntax treats as special or that --allowedTools would split at.
func rulePath(p string) string {
	if len(p) >= 3 && p[1] == ':' && (p[2] == '\\' || p[2] == '/') {
		d := p[0] | 0x20
		if d < 'a' || d > 'z' {
			return ""
		}
		p = "/" + string(d) + "/" + strings.ReplaceAll(p[3:], `\`, "/")
	}
	if !strings.HasPrefix(p, "/") || strings.HasPrefix(p, "//") || strings.ContainsAny(p, "*?[]{}()!#\\,\x00\n\r\t") ||
		strings.TrimRight(p, " ") != p {
		return ""
	}
	return p
}
