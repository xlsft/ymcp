package tools

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/xlsft/ymcp/internal/youtrack"
)

// Group is a set of related tools that can be turned on or off together,
// so a client only spends context on the tools it needs.
type Group struct {
	Name, Title string
	Tools       []string
}

// Groups lists every tool by group; each tool is in exactly one.
var Groups = []Group{
	{"issues", "Issues: search, read, create, edit, link, commands, history", []string{
		"search_issues", "count_issues", "get_issue", "get_issue_fields_schema", "create_issue", "create_draft_issue",
		"update_issue", "edit_issue_description", "change_issue_assignee", "delete_issue", "apply_command", "get_issue_activities",
		"link_issues", "unlink_issues", "get_issue_vcs_changes"}},
	{"comments", "Comments on issues", []string{
		"get_issue_comments", "add_issue_comment", "update_issue_comment", "restore_issue_comment", "delete_issue_comment"}},
	{"attachments", "Files on issues and articles", []string{
		"get_issue_attachments", "get_attachment", "add_attachment", "delete_attachment"}},
	{"articles", "Knowledge base articles", []string{
		"search_articles", "get_article", "create_article", "update_article", "edit_article_content", "delete_article"}},
	{"time", "Time tracking and reports", []string{
		"log_work", "get_issue_work_items", "delete_work_item", "get_time_report"}},
	{"agile", "Agile boards and sprints", []string{
		"find_agile_boards", "get_agile_board", "get_sprint", "create_sprint", "update_sprint",
		"add_issue_to_sprint", "remove_issue_from_sprint", "delete_sprint"}},
	{"projects", "Projects", []string{"find_projects", "get_project"}},
	{"people", "Users and groups", []string{"get_current_user", "find_user", "find_user_groups", "get_user_group_members"}},
	{"tags", "Tags", []string{"manage_issue_tags", "find_tags", "create_tag", "update_tag", "delete_tag"}},
	{"searches", "Saved searches", []string{
		"get_saved_issue_searches", "create_saved_search", "update_saved_search", "delete_saved_search"}},
	{"api", "Raw REST calls (call_api)", []string{"call_api"}},
}

// BuiltIn lists the tools of YouTrack's own MCP server, which ymcp serves
// under the same names, arguments and limits; the others are its extras.
var BuiltIn = []string{
	"search_issues", "get_issue", "create_issue", "create_draft_issue", "update_issue", "change_issue_assignee",
	"get_issue_fields_schema", "add_issue_comment", "get_issue_comments", "manage_issue_tags", "link_issues", "log_work",
	"get_saved_issue_searches", "search_articles", "get_article", "create_article", "update_article",
	"find_projects", "get_project", "find_user", "get_current_user", "find_user_groups", "get_user_group_members",
}

// GroupNames lists the group names in order.
func GroupNames() []string {
	names := make([]string, len(Groups))
	for i, g := range Groups {
		names[i] = g.Name
	}
	return names
}

// Select resolves a tool selection into the set of enabled tools; nil
// means all. Items are group names or tool names; "all" adds everything,
// and a leading "-" removes a group or tool. A selection of removals only
// starts from all tools, e.g. "-api,-delete_issue".
func Select(items []string) (map[string]bool, error) {
	var clean []string
	for _, it := range items {
		if it = strings.TrimSpace(it); it != "" {
			clean = append(clean, it)
		}
	}
	if len(clean) == 0 {
		return nil, nil
	}
	enabled := map[string]bool{}
	if !slices.ContainsFunc(clean, func(s string) bool { return !strings.HasPrefix(s, "-") }) {
		clean = append([]string{"all"}, clean...)
	}
	for _, it := range clean {
		name, remove := strings.CutPrefix(it, "-")
		tools, err := resolve(name)
		if err != nil {
			return nil, err
		}
		for _, tool := range tools {
			if remove {
				delete(enabled, tool)
			} else {
				enabled[tool] = true
			}
		}
	}
	if len(enabled) == 0 {
		return nil, fmt.Errorf("tool selection %q leaves no tools", strings.Join(items, ","))
	}
	return enabled, nil
}

func resolve(name string) ([]string, error) {
	if name == "all" {
		var all []string
		for _, g := range Groups {
			all = append(all, g.Tools...)
		}
		return all, nil
	}
	for _, g := range Groups {
		if g.Name == name {
			return g.Tools, nil
		}
		if slices.Contains(g.Tools, name) {
			return []string{name}, nil
		}
	}
	return nil, fmt.Errorf("unknown tool or group %q; groups: %s", name, strings.Join(GroupNames(), ", "))
}

// Info describes a tool for a picker.
type Info struct {
	Name, Title, Group string
	// Destructive tools ask for confirmation when it is on.
	Destructive bool
}

var catalog = sync.OnceValue(func() []Info {
	yt, err := youtrack.New(youtrack.Options{BaseURL: "https://youtrack.invalid", Login: "-", Password: "-", Proxy: "none"})
	if err != nil {
		panic(err)
	}
	s := mcp.NewServer(&mcp.Implementation{Name: "catalog"}, nil)
	Register(s, yt, nil)
	ctx := context.Background()
	ct, st := mcp.NewInMemoryTransports()
	ss, err := s.Connect(ctx, st, nil)
	if err != nil {
		panic(err)
	}
	defer ss.Close()
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "catalog"}, nil).Connect(ctx, ct, nil)
	if err != nil {
		panic(err)
	}
	defer cs.Close()
	titles := map[string]string{}
	for tool, err := range cs.Tools(ctx, nil) {
		if err != nil {
			panic(err)
		}
		if tool.Annotations != nil {
			titles[tool.Name] = tool.Annotations.Title
		}
	}
	var out []Info
	for _, g := range Groups {
		for _, name := range g.Tools {
			out = append(out, Info{Name: name, Title: titles[name], Group: g.Name, Destructive: slices.Contains(Destructive, name)})
		}
	}
	return out
})

// Catalog lists every tool, group by group, with its title.
func Catalog() []Info { return slices.Clone(catalog()) }

// Compact turns a set of enabled tools into a profile's tool selection,
// the shorter of the groups and tools to serve or those to leave out
// ("-api,-delete_issue"); nil when every tool is enabled, and for a nil
// set, which means all too. Select reads it back to the same set; an
// empty set has no selection, so pickers must keep one tool on.
func Compact(enabled map[string]bool) []string {
	var on, off []string
	for _, g := range Groups {
		var in, out []string
		for _, tool := range g.Tools {
			if enabled[tool] {
				in = append(in, tool)
			} else {
				out = append(out, "-"+tool)
			}
		}
		switch {
		case len(out) == 0:
			on = append(on, g.Name)
		case len(in) == 0:
			off = append(off, "-"+g.Name)
		default:
			on = append(on, in...)
			off = append(off, out...)
		}
	}
	switch {
	case len(off) == 0:
		return nil
	case len(off) < len(on):
		return off
	}
	return on
}
