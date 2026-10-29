package tools

import (
	"context"
	"maps"
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

// TestBuiltIn: the 23 tools of YouTrack's own server are all served.
func TestBuiltIn(t *testing.T) {
	all := listed(t, nil)
	if n := len(slices.Compact(slices.Sorted(slices.Values(BuiltIn)))); n != 23 || len(BuiltIn) != 23 {
		t.Errorf("%d built-in tools, %d distinct, want 23", len(BuiltIn), n)
	}
	for _, name := range BuiltIn {
		if !slices.Contains(all, name) {
			t.Errorf("built-in %s is not served", name)
		}
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

func TestCatalog(t *testing.T) {
	list := Catalog()
	if len(list) != len(listed(t, nil)) {
		t.Fatalf("%d tools in the catalog", len(list))
	}
	for _, it := range list {
		if it.Title == "" || it.Group == "" {
			t.Errorf("%+v", it)
		}
	}
	if !slices.ContainsFunc(list, func(it Info) bool { return it.Name == "delete_issue" && it.Destructive }) {
		t.Error("delete_issue is not destructive")
	}
}

func TestCompact(t *testing.T) {
	for _, items := range [][]string{
		nil,
		{"issues", "comments"},
		{"-api", "-delete_issue"},
		{"projects", "get_issue"},
		{"-issues", "-comments", "-attachments", "-articles", "-time", "-agile", "-projects", "-people", "-tags", "-searches", "get_issue"},
	} {
		enabled, err := Select(items)
		if err != nil {
			t.Fatal(err)
		}
		got := Compact(enabled)
		back, err := Select(got)
		if err != nil || !maps.Equal(back, enabled) {
			t.Errorf("Compact(Select(%v)) = %v, which selects %d tools, want %d", items, got, len(back), len(enabled))
		}
	}
	if got := strings.Join(Compact(mustSelect(t, "-api", "-delete_issue")), ","); got != "-delete_issue,-api" {
		t.Errorf("all but two = %s", got)
	}
	if got := strings.Join(Compact(mustSelect(t, "projects", "get_issue")), ","); got != "get_issue,projects" {
		t.Errorf("a group and a tool = %s", got)
	}
}

func mustSelect(t *testing.T, items ...string) map[string]bool {
	t.Helper()
	enabled, err := Select(items)
	if err != nil {
		t.Fatal(err)
	}
	return enabled
}
