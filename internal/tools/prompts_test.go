package tools

import (
	"context"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/xlsft/ymcp/internal/youtrack"
)

func promptSession(t *testing.T, enabled map[string]bool) *mcp.ClientSession {
	t.Helper()
	yt, err := youtrack.New(youtrack.Options{BaseURL: "https://yt.example.com", Login: "a", Password: "b", Proxy: "none"})
	if err != nil {
		t.Fatal(err)
	}
	server := mcp.NewServer(&mcp.Implementation{Name: "test", Version: "0"}, nil)
	Register(server, yt, enabled)
	ct, st := mcp.NewInMemoryTransports()
	ctx := context.Background()
	if _, err := server.Connect(ctx, st, nil); err != nil {
		t.Fatal(err)
	}
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "c"}, nil).Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cs.Close() })
	return cs
}

func promptNames(t *testing.T, cs *mcp.ClientSession) string {
	t.Helper()
	res, err := cs.ListPrompts(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, p := range res.Prompts {
		names = append(names, p.Name)
	}
	sort.Strings(names)
	return strings.Join(names, ",")
}

func TestPromptsFollowTheServedTools(t *testing.T) {
	if got := promptNames(t, promptSession(t, nil)); got != "issue_brief,sprint_report,standup,triage" {
		t.Errorf("all tools: %s", got)
	}
	issuesOnly, _ := Select([]string{"issues"})
	cs := promptSession(t, issuesOnly)
	if got := promptNames(t, cs); got != "issue_brief,triage" {
		t.Errorf("issues only: %s", got)
	}

	ctx := context.Background()
	res, err := cs.GetPrompt(ctx, &mcp.GetPromptParams{Name: "issue_brief", Arguments: map[string]string{"issue": "DEMO-7"}})
	if err != nil {
		t.Fatal(err)
	}
	text := res.Messages[0].Content.(*mcp.TextContent).Text
	if !strings.Contains(text, "get_issue for DEMO-7") || !strings.Contains(text, "aren't available here, so skip the steps that need them: get_issue_comments") {
		t.Errorf("issue_brief:\n%s", text)
	}
	if _, err := cs.GetPrompt(ctx, &mcp.GetPromptParams{Name: "triage"}); err == nil || !strings.Contains(err.Error(), "needs project") {
		t.Errorf("triage without a project: %v", err)
	}
}

func TestStandupLooksBackOverWeekends(t *testing.T) {
	monday := time.Date(2026, 10, 19, 9, 0, 0, 0, time.Local)
	ts := &toolset{now: func() time.Time { return monday }}
	standup := prompts()[0]
	text, err := standup.text(ts, map[string]string{})
	if err != nil || !strings.Contains(text, "updated: 2026-10-16 .. Today") || !strings.Contains(text, "Today is 2026-10-19") {
		t.Errorf("Monday standup:\n%s\n%v", text, err)
	}
	if text, _ := standup.text(ts, map[string]string{"days": "3"}); !strings.Contains(text, "look back to 2026-10-14") {
		t.Errorf("three days back:\n%s", text)
	}
	if _, err := standup.text(ts, map[string]string{"days": "x"}); err == nil {
		t.Error("days=x accepted")
	}
}
