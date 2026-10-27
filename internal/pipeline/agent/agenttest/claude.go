package agenttest

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"time"
)

// runClaude imitates one `claude -p … --output-format stream-json` run.
func runClaude(s Script, args []string) int {
	var prompt, sessionID, resume, mcpConfig string
	for i := 0; i < len(args); i++ {
		next := func() string {
			if i+1 < len(args) {
				i++
				return args[i]
			}
			return ""
		}
		switch args[i] {
		case "-p", "--print":
			prompt = next()
		case "--session-id":
			sessionID = next()
		case "--resume":
			resume = next()
		case "--mcp-config":
			mcpConfig = next()
		}
	}
	dir, _ := os.Getwd()
	c := Call{Method: "claude", Args: args, Dir: dir, StdinEOF: stdinAtEOF(), Env: os.Environ()}
	if mcpConfig != "" {
		b, _ := os.ReadFile(mcpConfig)
		c.MCPConfig = string(b)
	}
	record(s.State, c)

	out := bufio.NewWriter(os.Stdout)
	defer out.Flush()
	emit := func(v any) {
		b, _ := json.Marshal(v)
		out.Write(append(b, '\n'))
		out.Flush()
	}
	if s.Banner != "" {
		fmt.Fprintln(out, s.Banner)
	}

	id := sessionID
	var sess *session
	if resume != "" {
		id = resume
		var ok bool
		if sess, ok = loadSession(s.State, id); !ok {
			fmt.Fprintf(os.Stderr, "No conversation found with session ID: %s\n", id)
			printTranscript(out, "no_conversation.jsonl", id)
			return 1
		}
	} else {
		if id == "" {
			fmt.Fprintln(os.Stderr, "agenttest: neither --session-id nor --resume")
			return 2
		}
		if _, exists := loadSession(s.State, id); exists {
			fmt.Fprintf(os.Stderr, "Error: Session ID %s is already in use.\n", id)
			return 1
		}
		sess = &session{ID: id, Cwd: dir}
	}
	steps := turn(s, len(sess.Prompts))
	sess.Prompts = append(sess.Prompts, prompt)
	sess.save(s.State)

	transcript := ""
	for _, st := range steps {
		if st.Transcript != "" {
			transcript = st.Transcript
		}
	}
	if transcript == "" {
		emit(map[string]any{"type": "system", "subtype": "init", "cwd": dir, "session_id": id, "tools": []string{"Bash", "Read", "Write"}, "model": "fake"})
	}
	var text []string
	var denied []map[string]any
	for n, st := range steps {
		msg := func(role string, content ...map[string]any) {
			emit(map[string]any{"type": role, "session_id": id, "message": map[string]any{"role": role, "content": content}})
		}
		switch {
		case st.Exit != 0:
			out.Flush()
			os.Exit(st.Exit)
		case st.Transcript != "":
			printTranscript(out, st.Transcript, id)
		case st.Text != "":
			text = append(text, st.Text)
			msg("assistant", map[string]any{"type": "text", "text": st.Text})
		case st.Thought != "":
			msg("assistant", map[string]any{"type": "thinking", "thinking": st.Thought, "signature": ""})
		case st.Tool != nil:
			tid := toolID(st.Tool, n)
			msg("assistant", map[string]any{"type": "tool_use", "id": tid, "name": st.Tool.Title, "input": orEmpty(st.Tool.Input)})
			msg("user", map[string]any{"type": "tool_result", "tool_use_id": tid, "content": "done"})
		case st.Echo:
			t := sess.echo(prompt)
			text = append(text, t)
			msg("assistant", map[string]any{"type": "text", "text": t})
		case st.Env != "":
			t := envText(st.Env)
			text = append(text, t)
			msg("assistant", map[string]any{"type": "text", "text": t})
		case st.Sleep > 0:
			time.Sleep(st.Sleep)
		case st.Spawn:
			out.Flush()
			spawn(s.State)
		case st.Garbage != "":
			fmt.Fprintln(out, st.Garbage)
			out.Flush()
		case st.Stderr != "":
			fmt.Fprintln(os.Stderr, st.Stderr)
		case len(st.Denied) > 0:
			for i, d := range st.Denied {
				tid := toolID(&d, n*100+i)
				msg("assistant", map[string]any{"type": "tool_use", "id": tid, "name": d.Title, "input": orEmpty(d.Input)})
				msg("user", map[string]any{"type": "tool_result", "tool_use_id": tid, "is_error": true,
					"content": fmt.Sprintf("Claude requested permissions to use %s, but you haven't granted it yet.", d.Title)})
				denied = append(denied, map[string]any{"tool_name": d.Title, "tool_use_id": tid, "tool_input": orEmpty(d.Input)})
			}
		case st.Error != "":
			// As Claude Code reports a failed sign-in or an unknown model:
			// subtype success with is_error set, then exit 1.
			msg("assistant", map[string]any{"type": "text", "text": st.Error})
			emit(map[string]any{"type": "result", "subtype": "success", "is_error": true, "result": st.Error,
				"stop_reason": "stop_sequence", "session_id": id, "permission_denials": []any{}, "total_cost_usd": 0})
			return 1
		}
	}
	if transcript == "" {
		if denied == nil {
			denied = []map[string]any{}
		}
		emit(map[string]any{"type": "result", "subtype": "success", "is_error": false, "result": strings.Join(text, "\n\n"),
			"stop_reason": "end_turn", "session_id": id, "permission_denials": denied, "total_cost_usd": 0.0123,
			"usage": map[string]any{"input_tokens": 10, "output_tokens": 20}})
	}
	return 0
}

// printTranscript prints an embedded transcript with our session ID.
func printTranscript(w io.Writer, name, id string) {
	b, err := transcripts.ReadFile("transcripts/" + name)
	if err != nil {
		fmt.Fprintln(os.Stderr, "agenttest:", err)
		return
	}
	io.WriteString(w, strings.ReplaceAll(string(b), "SESSION_ID", id))
}

// stdinAtEOF reports whether stdin is empty and closed, as /dev/null is.
func stdinAtEOF() bool {
	done := make(chan bool, 1)
	go func() {
		n, err := os.Stdin.Read(make([]byte, 1))
		done <- n == 0 && err == io.EOF
	}()
	select {
	case eof := <-done:
		return eof
	case <-time.After(time.Second):
		return false
	}
}

func toolID(t *Tool, n int) string {
	if t.ID != "" {
		return t.ID
	}
	return fmt.Sprintf("toolu_%02d", n)
}

func orEmpty(m map[string]any) map[string]any {
	if m == nil {
		return map[string]any{}
	}
	return m
}
