package tools

import (
	"context"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/xlsft/ymcp/internal/youtrack"
)

func listed(t *testing.T, enabled map[string]bool) []string {
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
	defer cs.Close()
	res, err := cs.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, tool := range res.Tools {
		names = append(names, tool.Name)
	}
	sort.Strings(names)
	return names
}

func TestGroupsCoverEveryToolOnce(t *testing.T) {
	var grouped []string
	for _, g := range Groups {
		grouped = append(grouped, g.Tools...)
	}
	sort.Strings(grouped)
	if dup := slices.Compact(slices.Clone(grouped)); len(dup) != len(grouped) {
		t.Errorf("a tool is in two groups: %v", grouped)
	}
	if got := listed(t, nil); !slices.Equal(got, grouped) {
		t.Errorf("registered tools and groups differ:\nregistered %v\ngrouped    %v", got, grouped)
	}
}

func TestSelect(t *testing.T) {
	if got, err := Select(nil); got != nil || err != nil {
		t.Errorf("empty = %v, %v", got, err)
	}
	got, err := Select([]string{"projects", " get_issue ", "-find_projects"})
	if err != nil || len(got) != 2 || !got["get_issue"] || !got["get_project"] {
		t.Errorf("projects,get_issue,-find_projects = %v, %v", got, err)
	}
	if names := listed(t, got); strings.Join(names, ",") != "get_issue,get_project" {
		t.Errorf("listed = %v", names)
	}
	got, _ = Select([]string{"-api", "-delete_issue"})
	if got["call_api"] || got["delete_issue"] || !got["search_issues"] || len(got) != 56 {
		t.Errorf("removals only: %d tools", len(got))
	}
	if _, err := Select([]string{"issues", "nope"}); err == nil || !strings.Contains(err.Error(), "groups: issues, comments") {
		t.Errorf("unknown = %v", err)
	}
	if _, err := Select([]string{"api", "-call_api"}); err == nil {
		t.Error("an empty selection was accepted")
	}
}
