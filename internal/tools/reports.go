package tools

import (
	"context"
	"fmt"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type getTimeReportIn struct {
	Query        string `json:"query,omitempty" jsonschema:"Issue search query limiting the issues, e.g. project: DEMO"`
	Author       string `json:"author,omitempty" jsonschema:"Login of whoever logged the time, or me"`
	From         string `json:"from,omitempty" jsonschema:"First day, yyyy-MM-dd"`
	To           string `json:"to,omitempty" jsonschema:"Last day, yyyy-MM-dd"`
	GroupBy      string `json:"groupBy,omitempty" jsonschema:"Group totals by author, issue, workType, day or project. Default author."`
	IncludeItems bool   `json:"includeItems,omitempty" jsonschema:"Also list the work items (up to itemsLimit). Default false."`
	ItemsLimit   int    `json:"itemsLimit,omitempty" jsonschema:"Max work items to list with includeItems. Default 50, max 200."`
}

// reportItem is a work item with its issue, as /api/workItems returns it.
type reportItem struct {
	apiWorkItem
	Issue struct {
		IDReadable string        `json:"idReadable"`
		Summary    string        `json:"summary"`
		Project    apiProjectRef `json:"project"`
	} `json:"issue"`
}

// maxReportItems caps how many work items one report reads.
const maxReportItems = 10000

func (t *toolset) registerReports(s *mcp.Server) {
	add(s, &mcp.Tool{
		Name: "get_time_report",
		Description: "Summarizes spent time across issues: total and per author, issue, work type, day or project, " +
			"filtered by issue query, author and period. E.g. what did I log this week, or time spent on a project in a month.",
		Annotations: readOnly("Get time report"),
		InputSchema: schemaFor[getTimeReportIn](func(p map[string]*jsonschema.Schema) {
			p["groupBy"].Enum = enum("author", "issue", "workType", "day", "project")
		}),
	}, t.getTimeReport)
}

func (t *toolset) getTimeReport(ctx context.Context, in getTimeReportIn) (any, error) {
	q := url.Values{"fields": {workItemFields + ",issue(idReadable,summary,project(shortName))"}}
	if in.Query != "" {
		q.Set("query", in.Query)
	}
	author := strings.TrimSpace(in.Author)
	if strings.EqualFold(author, "me") {
		me, err := t.currentUser(ctx)
		if err != nil {
			return nil, err
		}
		author = me.Login
	}
	if author != "" {
		q.Set("author", author)
	}
	for key, val := range map[string]string{"startDate": in.From, "endDate": in.To} {
		if val == "" {
			continue
		}
		if _, err := time.Parse("2006-01-02", val); err != nil {
			return nil, fmt.Errorf("cannot parse date %q, use yyyy-MM-dd", val)
		}
		q.Set(key, val)
	}

	var items []reportItem
	const batch = 500
	for len(items) < maxReportItems {
		q.Set("$skip", strconv.Itoa(len(items)))
		q.Set("$top", strconv.Itoa(batch))
		var chunk []reportItem
		if err := t.yt.Get(ctx, "workItems", q, &chunk); err != nil {
			return nil, err
		}
		items = append(items, chunk...)
		if len(chunk) < batch {
			break
		}
	}

	groupBy := in.GroupBy
	if groupBy == "" {
		groupBy = "author"
	}
	type group struct {
		Key      string `json:"key"`
		Minutes  int    `json:"minutes"`
		Duration string `json:"duration"`
		Items    int    `json:"items"`
	}
	groups := map[string]*group{}
	total := 0
	for _, it := range items {
		key := reportKey(it, groupBy)
		g := groups[key]
		if g == nil {
			g = &group{Key: key}
			groups[key] = g
		}
		g.Minutes += it.Duration.Minutes
		g.Items++
		total += it.Duration.Minutes
	}
	list := make([]group, 0, len(groups))
	for _, g := range groups {
		g.Duration = formatMinutes(g.Minutes)
		list = append(list, *g)
	}
	sort.Slice(list, func(i, j int) bool {
		if groupBy == "day" {
			return list[i].Key < list[j].Key
		}
		if list[i].Minutes != list[j].Minutes {
			return list[i].Minutes > list[j].Minutes
		}
		return list[i].Key < list[j].Key
	})

	out := map[string]any{
		"totalMinutes": total,
		"total":        formatMinutes(total),
		"itemsCount":   len(items),
		"groupBy":      groupBy,
		"groups":       list,
	}
	if len(items) >= maxReportItems {
		out["truncated"] = fmt.Sprintf("only the first %d work items were read; narrow the filters", maxReportItems)
	}
	if in.IncludeItems {
		_, limit := page(0, in.ItemsLimit, 50, 200)
		listed := make([]map[string]any, 0, min(limit, len(items)))
		for _, it := range items[:min(limit, len(items))] {
			m := it.public()
			m["issueId"] = it.Issue.IDReadable
			m["issueSummary"] = it.Issue.Summary
			listed = append(listed, m)
		}
		out["items"] = listed
	}
	return out, nil
}

func reportKey(it reportItem, groupBy string) string {
	switch groupBy {
	case "issue":
		return it.Issue.IDReadable + " " + it.Issue.Summary
	case "workType":
		if it.Type != nil {
			return it.Type.Name
		}
		return "(no type)"
	case "day":
		return time.UnixMilli(int64(it.Date)).UTC().Format("2006-01-02")
	case "project":
		return it.Issue.Project.ShortName
	default:
		if it.Author != nil {
			return it.Author.Login
		}
		return "(unknown)"
	}
}

// formatMinutes renders minutes as "3h 15m" (no days or weeks, which depend
// on the instance's working hours).
func formatMinutes(m int) string {
	switch {
	case m == 0:
		return "0m"
	case m < 60:
		return fmt.Sprintf("%dm", m)
	case m%60 == 0:
		return fmt.Sprintf("%dh", m/60)
	default:
		return fmt.Sprintf("%dh %dm", m/60, m%60)
	}
}
