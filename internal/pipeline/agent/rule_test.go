package agent

import (
	"encoding/json"
	"testing"
)

func TestClaudeRule(t *testing.T) {
	call := func(title string, in map[string]any) ToolCall {
		b, _ := json.Marshal(in)
		return ToolCall{Title: title, Status: "denied", Input: b}
	}
	for _, tc := range []struct {
		name string
		call ToolCall
		want string
	}{
		{"bash", call("Bash", map[string]any{"command": "git push origin main", "description": "Push"}), "Bash(git push origin main)"},
		{"bash keeps commas and pipes", call("Bash", map[string]any{"command": "go test ./... 2>&1 | grep -v ok, fine"}), "Bash(go test ./... 2>&1 | grep -v ok, fine)"},
		{"bash escapes backslashes", call("Bash", map[string]any{"command": `printf 'a\n'`}), `Bash(printf 'a\\n')`},
		{"bash multi-line", call("Bash", map[string]any{"command": "make\nmake test"}), "Bash(make\nmake test)"},
		{"powershell", call("PowerShell", map[string]any{"command": "Get-ChildItem"}), "PowerShell(Get-ChildItem)"},
		// Never the whole shell.
		{"bash without a command", call("Bash", nil), ""},
		{"bash with an empty command", call("Bash", map[string]any{"command": "  "}), ""},
		{"bash with a wildcard", call("Bash", map[string]any{"command": "rm *"}), ""},
		{"bash prefix syntax", call("Bash", map[string]any{"command": "rm:*"}), ""},
		{"bash with parentheses", call("Bash", map[string]any{"command": "true) Bash ("}), ""},
		{"bash with a subshell", call("Bash", map[string]any{"command": `git commit -m "$(cat msg)"`}), ""},

		{"write", call("Write", map[string]any{"file_path": "/tmp/out.txt", "content": "x"}), "Edit(//tmp/out.txt)"},
		{"edit", call("Edit", map[string]any{"file_path": "/home/me/a b/c.go"}), "Edit(//home/me/a b/c.go)"},
		{"multi edit", call("MultiEdit", map[string]any{"file_path": "/x/y.go"}), "Edit(//x/y.go)"},
		{"notebook", call("NotebookEdit", map[string]any{"notebook_path": "/n/b.ipynb"}), "Edit(//n/b.ipynb)"},
		{"read", call("Read", map[string]any{"file_path": "/etc/hosts"}), "Read(//etc/hosts)"},
		{"windows path", call("Write", map[string]any{"file_path": `C:\Users\me\out.txt`}), "Edit(//c/Users/me/out.txt)"},
		{"relative path", call("Write", map[string]any{"file_path": "out.txt"}), ""},
		{"path with a glob", call("Write", map[string]any{"file_path": "/tmp/*.txt"}), ""},
		{"path with parentheses", call("Edit", map[string]any{"file_path": "/tmp/a (1).txt"}), ""},
		{"path with a trailing space", call("Edit", map[string]any{"file_path": "/tmp/a "}), ""},
		{"write without a path", call("Write", map[string]any{"content": "x"}), ""},

		{"fetch", call("WebFetch", map[string]any{"url": "https://Docs.Example.com/a?b=c", "prompt": "read"}), "WebFetch(domain:docs.example.com)"},
		{"fetch without a url", call("WebFetch", nil), ""},
		{"mcp tool", call("mcp__github__create_issue", map[string]any{"title": "x"}), "mcp__github__create_issue"},
		{"other tool", call("Glob", map[string]any{"pattern": "**/*.go"}), "Glob"},
		{"no title", call("", nil), ""},
		{"odd title", call("Bash Edit", nil), ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := ClaudeRule(tc.call); got != tc.want {
				t.Errorf("ClaudeRule = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestClaudeRuleSurvivesTheAllowedToolsList: claude splits --allowedTools
// at commas and spaces outside parentheses; a rule must come out of that
// whole, so that no part of it becomes a bare tool name.
func TestClaudeRuleSurvivesTheAllowedToolsList(t *testing.T) {
	// split is claude's own splitting of the --allowedTools values.
	split := func(v string) []string {
		var out []string
		cur, in := "", false
		for _, c := range v {
			switch {
			case c == '(':
				in, cur = true, cur+string(c)
			case c == ')':
				in, cur = false, cur+string(c)
			case (c == ',' || c == ' ') && !in:
				if cur != "" {
					out = append(out, cur)
				}
				cur = ""
			default:
				cur += string(c)
			}
		}
		if cur != "" {
			out = append(out, cur)
		}
		return out
	}
	for _, cmd := range []string{"git push origin main", "a, b c", `echo "x y" , z`, "true) Bash (", "x)", "(y"} {
		in, _ := json.Marshal(map[string]string{"command": cmd})
		rule := ClaudeRule(ToolCall{Title: "Bash", Input: in})
		if rule == "" {
			continue
		}
		if got := split("mcp__ymcp," + rule); len(got) != 2 || got[1] != rule {
			t.Errorf("%q splits into %q", rule, got)
		}
	}
}
