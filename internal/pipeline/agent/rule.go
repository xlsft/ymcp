package agent

import (
	"encoding/json"
	"net/url"
	"slices"
	"strings"
)

// ClaudeRules are the claude permission rules, as --allowedTools takes
// them, that allow exactly the call tc, so approving a denied call doesn't
// grant the whole tool:
//
//   - Bash(<command>) (PowerShell(<command>)) for a shell command, which
//     matches that command only. claude checks the commands of a compound
//     one (a && b, a; b, a | b, one per line) one by one, and a rule for
//     the whole line never matches one that starts with cd, so there is a
//     rule for each of them;
//   - Edit(//<path>) for a call that writes a file (Edit, MultiEdit, Write,
//     NotebookEdit: claude matches file writes by Edit rules) and
//     Read(//<path>) for a read; "//" starts an absolute path;
//   - WebFetch(domain:<host>) for a fetch;
//   - the tool name for any other tool, such as an MCP tool.
//
// They are nil when the call can't be put into rules exactly: a shell call
// without a command or a file call without a path, or one whose command or
// path holds characters claude would read as a pattern (*, ?, [ …) or that
// would break the comma and space separated --allowedTools list apart,
// such as parentheses. Such a call must not be approved by a broader rule.
func ClaudeRules(tc ToolCall) []string {
	if tc.Title != "Bash" && tc.Title != "PowerShell" {
		if r := claudeRule(tc); r != "" {
			return []string{r}
		}
		return nil
	}
	var in map[string]any
	_ = json.Unmarshal(tc.Input, &in)
	cmd, _ := in["command"].(string)
	var out []string
	for _, part := range splitCommands(cmd) {
		r := shellRule(tc.Title, part)
		if r == "" {
			return nil
		}
		if !slices.Contains(out, r) {
			out = append(out, r)
		}
	}
	return out
}

// shellRule is the rule of one shell command, "" if there is none.
func shellRule(tool, cmd string) string {
	cmd = strings.TrimSpace(cmd)
	if cmd == "" || strings.ContainsAny(cmd, "()*\x00") {
		return ""
	}
	return tool + "(" + strings.ReplaceAll(cmd, `\`, `\\`) + ")"
}

// splitCommands splits a shell line into its commands at &&, ||, ;, | and
// line breaks outside quotes. A lone & (a redirection such as 2>&1, or a
// background job) does not split.
func splitCommands(line string) []string {
	var out []string
	var cur strings.Builder
	var quote rune
	escaped := false
	r := []rune(line)
	flush := func() {
		if s := strings.TrimSpace(cur.String()); s != "" {
			out = append(out, s)
		}
		cur.Reset()
	}
	for i := 0; i < len(r); i++ {
		c := r[i]
		switch {
		case escaped:
			escaped = false
		case c == '\\' && quote != '\'':
			escaped = true
		case quote != 0:
			if c == quote {
				quote = 0
			}
		case c == '\'' || c == '"':
			quote = c
		case c == ';' || c == '\n':
			flush()
			continue
		case c == '|' || (c == '&' && i+1 < len(r) && r[i+1] == '&'):
			if i+1 < len(r) && r[i+1] == c {
				i++
			}
			flush()
			continue
		}
		cur.WriteRune(c)
	}
	flush()
	return out
}

// claudeRule is the rule of a call to a tool other than the shell.
func claudeRule(tc ToolCall) string {
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
