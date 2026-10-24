package tools

import (
	"strings"
	"testing"
)

func TestApplyEdits(t *testing.T) {
	text := "# Setup\n\nRun make.\n\n## Notes\n\nTODO\nTODO\n"
	for _, tc := range []struct {
		name  string
		edits []textEdit
		want  string
		n     int
		err   string
	}{
		{name: "single", edits: []textEdit{{OldText: "Run make.", NewText: "Run make build."}},
			want: "# Setup\n\nRun make build.\n\n## Notes\n\nTODO\nTODO\n", n: 1},
		{name: "chained", edits: []textEdit{{OldText: "make", NewText: "task"}, {OldText: "task.", NewText: "task run."}},
			want: "# Setup\n\nRun task run.\n\n## Notes\n\nTODO\nTODO\n", n: 2},
		{name: "delete", edits: []textEdit{{OldText: "\n## Notes\n\nTODO\nTODO\n"}}, want: "# Setup\n\nRun make.\n", n: 1},
		{name: "replace all", edits: []textEdit{{OldText: "TODO", NewText: "done", ReplaceAll: true}},
			want: "# Setup\n\nRun make.\n\n## Notes\n\ndone\ndone\n", n: 2},
		{name: "ambiguous", edits: []textEdit{{OldText: "TODO", NewText: "done"}}, err: "matches 2 times"},
		{name: "missing", edits: []textEdit{{OldText: "Run make.", NewText: "x"}, {OldText: "nope", NewText: "y"}}, err: "edit 2: oldText not found"},
		{name: "empty old", edits: []textEdit{{NewText: "x"}}, err: "oldText is empty"},
		{name: "none", err: "no edits"},
		{name: "regexp groups", edits: []textEdit{{OldText: `Run (\w+)\.`, NewText: "Run `$1`.", Regexp: true}},
			want: "# Setup\n\nRun `make`.\n\n## Notes\n\nTODO\nTODO\n", n: 1},
		{name: "regexp multiline all", edits: []textEdit{{OldText: `(?m)^TODO$`, NewText: "- [ ] todo", Regexp: true, ReplaceAll: true}},
			want: "# Setup\n\nRun make.\n\n## Notes\n\n- [ ] todo\n- [ ] todo\n", n: 2},
		{name: "regexp section", edits: []textEdit{{OldText: `(?s)## Notes\n.*`, NewText: "## Notes\n\nnone\n", Regexp: true}},
			want: "# Setup\n\nRun make.\n\n## Notes\n\nnone\n", n: 1},
		{name: "regexp ambiguous", edits: []textEdit{{OldText: `(?i)todo`, NewText: "x", Regexp: true}}, err: "matches 2 times"},
		{name: "regexp no match", edits: []textEdit{{OldText: `^Run`, NewText: "x", Regexp: true}}, err: "pattern matches nothing"},
		{name: "regexp invalid", edits: []textEdit{{OldText: `(`, NewText: "x", Regexp: true}}, err: "missing closing )"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, n, err := applyEdits(text, tc.edits)
			if tc.err != "" {
				if err == nil || !strings.Contains(err.Error(), tc.err) {
					t.Fatalf("err = %v, want %q", err, tc.err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got != tc.want || n != tc.n {
				t.Errorf("got %q (%d), want %q (%d)", got, n, tc.want, tc.n)
			}
		})
	}
}

func TestEditArticleContentPostsEditedText(t *testing.T) {
	api := newFakeAPI(t)
	api.json("GET /api/articles/HELP-A-1", `{"content":"# Title\n\nold line\n\nrest"}`)
	api.json("POST /api/articles/HELP-A-1", `{"idReadable":"HELP-A-1"}`)
	cs := connect(t, api)

	var out struct{ Replacements int }
	if isErr, text := call(t, cs, "edit_article_content", map[string]any{
		"articleId": "HELP-A-1", "edits": []map[string]any{{"oldText": "old line", "newText": "new line"}},
	}, &out); isErr {
		t.Fatal(text)
	}
	bodies := api.bodies("/api/articles/HELP-A-1")
	if out.Replacements != 1 || len(bodies) != 1 || bodies[0]["content"] != "# Title\n\nnew line\n\nrest" {
		t.Errorf("replacements = %d, bodies = %v", out.Replacements, bodies)
	}
}

func TestEditIssueDescriptionFailsWithoutWriting(t *testing.T) {
	api := newFakeAPI(t)
	api.json("GET /api/users/me/drafts/3-22", `{"description":"a\na"}`)
	api.json("POST /api/users/me/drafts/3-22", `{"id":"3-22"}`)
	cs := connect(t, api)

	isErr, text := call(t, cs, "edit_issue_description", map[string]any{
		"issueId": "3-22", "edits": []map[string]any{{"oldText": "a", "newText": "b"}},
	}, nil)
	if !isErr || !strings.Contains(text, "matches 2 times") {
		t.Errorf("isErr = %v, text = %s", isErr, text)
	}
	if bodies := api.bodies("/api/users/me/drafts/3-22"); len(bodies) != 0 {
		t.Errorf("posted %v", bodies)
	}

	if isErr, text := call(t, cs, "edit_issue_description", map[string]any{
		"issueId": "3-22", "edits": []map[string]any{{"oldText": "a", "newText": "b", "replaceAll": true}},
	}, nil); isErr {
		t.Fatal(text)
	}
	if bodies := api.bodies("/api/users/me/drafts/3-22"); len(bodies) != 1 || bodies[0]["description"] != "b\nb" {
		t.Errorf("posted %v", bodies)
	}
}
