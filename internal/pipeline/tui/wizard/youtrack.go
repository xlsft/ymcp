package wizard

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strings"

	"github.com/xlsft/ymcp/internal/pipeline/ytwatch"
	"github.com/xlsft/ymcp/internal/youtrack"
)

// maxSuggestions caps the completions shown under an input.
const maxSuggestions = 8

// NewYouTrack implements YouTrack on a client: the watcher of the runner
// for searches and command checks, plus the assist, field, tag, user and
// saved-search endpoints the wizard needs on top.
func NewYouTrack(yt *youtrack.Client) YouTrack {
	return &ytClient{yt: yt, watch: ytwatch.New(yt)}
}

type ytClient struct {
	yt    *youtrack.Client
	watch *ytwatch.Watcher
}

func (c *ytClient) BaseURL() string                        { return c.yt.BaseURL() }
func (c *ytClient) Me(ctx context.Context) (string, error) { return c.watch.Me(ctx) }
func (c *ytClient) Search(ctx context.Context, q string) ([]ytwatch.Issue, error) {
	return c.watch.Search(ctx, q)
}
func (c *ytClient) Issue(ctx context.Context, key string) (ytwatch.Issue, error) {
	return c.watch.Issue(ctx, key)
}
func (c *ytClient) Validate(ctx context.Context, command, key string) (ytwatch.CommandCheck, error) {
	return c.watch.Validate(ctx, command, key)
}

type apiSuggestion struct {
	Option          string `json:"option"`
	Prefix          string `json:"prefix"`
	Suffix          string `json:"suffix"`
	Description     string `json:"description"`
	CompletionStart int    `json:"completionStart"`
	CompletionEnd   int    `json:"completionEnd"`
	Caret           int    `json:"caret"`
}

const suggestionFields = "suggestions(option,prefix,suffix,description,completionStart,completionEnd,caret)"

func suggestions(in []apiSuggestion) []Suggestion {
	out := make([]Suggestion, 0, min(len(in), maxSuggestions))
	for _, s := range in {
		if len(out) == maxSuggestions {
			break
		}
		out = append(out, Suggestion{Option: s.Option, Prefix: s.Prefix, Suffix: s.Suffix, Description: s.Description,
			CompletionStart: s.CompletionStart, CompletionEnd: s.CompletionEnd, Caret: s.Caret})
	}
	return out
}

func (c *ytClient) SearchAssist(ctx context.Context, query string, caret int) ([]Suggestion, error) {
	var resp struct {
		Suggestions []apiSuggestion `json:"suggestions"`
	}
	body := map[string]any{"query": query, "caret": caret}
	if err := c.yt.Post(ctx, "search/assist", youtrack.Fields(suggestionFields), body, &resp); err != nil {
		return nil, fmt.Errorf("search assist: %w", err)
	}
	return suggestions(resp.Suggestions), nil
}

var dbID = regexp.MustCompile(`^\d+-\d+$`)

func (c *ytClient) CommandAssist(ctx context.Context, command string, caret int, issueKey string) ([]Suggestion, error) {
	if issueKey == "" {
		return nil, errors.New("command assist: an issue is required")
	}
	ref := map[string]string{"idReadable": issueKey}
	if dbID.MatchString(issueKey) {
		ref = map[string]string{"id": issueKey}
	}
	var resp struct {
		Suggestions []apiSuggestion `json:"suggestions"`
	}
	body := map[string]any{"query": command, "caret": caret, "issues": []map[string]string{ref}}
	if err := c.yt.Post(ctx, "commands/assist", youtrack.Fields(suggestionFields), body, &resp); err != nil {
		return nil, fmt.Errorf("command assist: %w", err)
	}
	return suggestions(resp.Suggestions), nil
}

type apiProjectField struct {
	Type  string `json:"$type"`
	Field struct {
		Name string `json:"name"`
	} `json:"field"`
	Bundle *struct {
		Values []struct {
			Name     string `json:"name"`
			Archived bool   `json:"archived"`
		} `json:"values"`
	} `json:"bundle"`
}

const projectFieldFields = "$type,field(name),bundle(values(name,archived))"

// fieldTypes maps the project field classes a stage can use to Field.Type.
var fieldTypes = map[string]string{
	"StateProjectCustomField":   "state",
	"EnumProjectCustomField":    "enum",
	"UserProjectCustomField":    "user",
	"VersionProjectCustomField": "version",
	"BuildProjectCustomField":   "build",
	"OwnedProjectCustomField":   "owned",
}

func (c *ytClient) Fields(ctx context.Context, project string) ([]Field, error) {
	if project == "" {
		return nil, errors.New("no project to read fields from")
	}
	var fields []apiProjectField
	q := url.Values{"fields": {projectFieldFields}, "$top": {"-1"}}
	err := c.yt.Get(ctx, "admin/projects/"+url.PathEscape(project)+"/customFields", q, &fields)
	var ye *youtrack.Error
	if err != nil && !(errors.As(err, &ye) && (ye.Status == http.StatusForbidden || ye.Status == http.StatusNotFound)) {
		return nil, fmt.Errorf("fields of %s: %w", project, err)
	}
	if len(fields) == 0 {
		// Without admin rights the settings endpoint answers an empty
		// list; the schema of recent issues tells the same.
		var issues []struct {
			CustomFields []struct {
				ProjectCustomField *apiProjectField `json:"projectCustomField"`
			} `json:"customFields"`
		}
		q = url.Values{
			"query":  {"project: {" + project + "} sort by: updated desc"},
			"fields": {"customFields(projectCustomField(" + projectFieldFields + "))"},
			"$top":   {"20"},
		}
		if err := c.yt.Get(ctx, "issues", q, &issues); err != nil {
			return nil, fmt.Errorf("fields of %s: %w", project, err)
		}
		seen := map[string]bool{}
		for _, is := range issues {
			for _, cf := range is.CustomFields {
				if f := cf.ProjectCustomField; f != nil && f.Field.Name != "" && !seen[f.Field.Name] {
					seen[f.Field.Name] = true
					fields = append(fields, *f)
				}
			}
		}
	}
	var out []Field
	for _, f := range fields {
		typ, ok := fieldTypes[f.Type]
		if !ok {
			continue
		}
		fd := Field{Name: f.Field.Name, Type: typ}
		if typ == "user" {
			// User bundles hold groups and teams; "me" is what stages use.
			fd.Values = []string{"me"}
		} else if f.Bundle != nil {
			for _, v := range f.Bundle.Values {
				if !v.Archived {
					fd.Values = append(fd.Values, v.Name)
				}
			}
		}
		if len(fd.Values) > 0 {
			out = append(out, fd)
		}
	}
	// State first: it is what stages usually move.
	slices.SortStableFunc(out, func(a, b Field) int {
		ra, rb := a.Type != "state", b.Type != "state"
		switch {
		case ra == rb:
			return 0
		case !ra:
			return -1
		}
		return 1
	})
	return out, nil
}

func (c *ytClient) Tags(ctx context.Context) ([]string, error) {
	var tags []struct {
		Name string `json:"name"`
	}
	if err := c.yt.Get(ctx, "tags", url.Values{"fields": {"name"}, "$top": {"500"}}, &tags); err != nil {
		return nil, fmt.Errorf("tags: %w", err)
	}
	out := make([]string, 0, len(tags))
	for _, t := range tags {
		out = append(out, t.Name)
	}
	slices.SortFunc(out, func(a, b string) int { return strings.Compare(strings.ToLower(a), strings.ToLower(b)) })
	return slices.Compact(out), nil
}

func (c *ytClient) Users(ctx context.Context, query string) ([]User, error) {
	var users []struct {
		Login    string `json:"login"`
		FullName string `json:"fullName"`
	}
	q := url.Values{"fields": {"login,fullName"}, "query": {query}, "$top": {"10"}}
	if err := c.yt.Get(ctx, "users", q, &users); err != nil {
		return nil, fmt.Errorf("users: %w", err)
	}
	out := make([]User, 0, len(users))
	for _, u := range users {
		out = append(out, User(u))
	}
	return out, nil
}

func (c *ytClient) SavedSearches(ctx context.Context) ([]SavedSearch, error) {
	var saved []struct {
		Name  string `json:"name"`
		Query string `json:"query"`
	}
	if err := c.yt.Get(ctx, "savedQueries", url.Values{"fields": {"name,query"}, "$top": {"100"}}, &saved); err != nil {
		return nil, fmt.Errorf("saved searches: %w", err)
	}
	out := make([]SavedSearch, 0, len(saved))
	for _, s := range saved {
		out = append(out, SavedSearch(s))
	}
	return out, nil
}
