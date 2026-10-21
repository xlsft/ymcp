package tools

import (
	"context"
	"fmt"
	"net/url"
	"sort"
	"strconv"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Agile board and sprint tools are not part of the built-in YouTrack MCP server.

type apiSprint struct {
	ID                    string `json:"id"`
	Name                  string `json:"name"`
	Goal                  string `json:"goal,omitempty"`
	Start                 millis `json:"start"`
	Finish                millis `json:"finish"`
	Archived              bool   `json:"archived"`
	IsDefault             bool   `json:"isDefault,omitempty"`
	UnresolvedIssuesCount *int   `json:"unresolvedIssuesCount,omitempty"`
}

const sprintFields = "id,name,goal,start,finish,archived,isDefault"

type apiAgile struct {
	ID              string          `json:"id"`
	Name            string          `json:"name"`
	Owner           *userRef        `json:"owner"`
	Projects        []apiProjectRef `json:"projects"`
	CurrentSprint   *apiSprint      `json:"currentSprint"`
	SprintsSettings struct {
		DisableSprints bool `json:"disableSprints"`
		IsExplicit     bool `json:"isExplicit"`
	} `json:"sprintsSettings"`
	ColumnSettings struct {
		Field *struct {
			Name string `json:"name"`
		} `json:"field"`
		Columns []struct {
			Presentation string `json:"presentation"`
			IsResolved   bool   `json:"isResolved"`
		} `json:"columns"`
	} `json:"columnSettings"`
	EstimationField *struct {
		Name string `json:"name"`
	} `json:"estimationField"`
	Sprints []apiSprint `json:"sprints"`
}

const agileFields = "id,name,owner(login,fullName),projects(shortName,name)," +
	"currentSprint(" + sprintFields + "),sprintsSettings(disableSprints,isExplicit)," +
	"columnSettings(field(name),columns(presentation,isResolved)),estimationField(name)," +
	"sprints(" + sprintFields + ")"

func (a apiAgile) projectKeys() []string {
	keys := make([]string, 0, len(a.Projects))
	for _, p := range a.Projects {
		keys = append(keys, p.ShortName)
	}
	return keys
}

func (a apiAgile) columnField() string {
	if a.ColumnSettings.Field == nil {
		return ""
	}
	return a.ColumnSettings.Field.Name
}

type findAgileBoardsIn struct {
	NameContains string `json:"nameContains" jsonschema:"Case-insensitive substring of the board name or project key; empty string lists all boards"`
	Offset       int    `json:"offset,omitempty" jsonschema:"Number of boards to skip. Default 0."`
	Limit        int    `json:"limit,omitempty" jsonschema:"Max boards to return. Default 20, max 20."`
}

type getAgileBoardIn struct {
	Board           string `json:"board" jsonschema:"Agile board name or ID, e.g. 195-13"`
	IncludeArchived bool   `json:"includeArchived,omitempty" jsonschema:"Also list archived sprints. Default false."`
	SprintsLimit    int    `json:"sprintsLimit,omitempty" jsonschema:"Max sprints to list, newest first. Default 20."`
}

type getSprintIn struct {
	Board                string   `json:"board" jsonschema:"Agile board name or ID, e.g. 195-13"`
	Sprint               string   `json:"sprint,omitempty" jsonschema:"Sprint name or ID, or current. Default: current."`
	CustomFieldsToReturn []string `json:"customFieldsToReturn,omitempty" jsonschema:"Custom fields to include per issue. Default: the board's column field, Assignee and the estimation field. Pass [] for none."`
	Offset               int      `json:"offset,omitempty" jsonschema:"Number of issues to skip. Default 0."`
	Limit                int      `json:"limit,omitempty" jsonschema:"Max issues to return. Default 20, max 50."`
}

func (t *toolset) registerAgiles(s *mcp.Server) {
	add(s, &mcp.Tool{
		Name:        "find_agile_boards",
		Description: "Finds agile boards whose name or project key contains a substring (case-insensitive). Returns ID, name, projects, whether sprints are used and the current sprint.",
		Annotations: readOnly("Find agile boards"),
	}, t.findAgileBoards)

	add(s, &mcp.Tool{
		Name: "get_agile_board",
		Description: "Returns an agile board: projects, owner, columns and the field they map to, estimation field, " +
			"sprint settings, the current sprint and recent sprints.",
		Annotations: readOnly("Get agile board"),
	}, t.getAgileBoard)

	add(s, &mcp.Tool{
		Name: "get_sprint",
		Description: "Returns a sprint of an agile board (the current one by default): name, goal, dates, archived flag " +
			"and its issues with board-relevant custom fields. Paginated over issues.",
		Annotations: readOnly("Get sprint"),
	}, t.getSprint)
}

func (t *toolset) listAgiles(ctx context.Context, fields string) ([]apiAgile, error) {
	var boards []apiAgile
	err := t.yt.Get(ctx, "agiles", url.Values{"fields": {fields}, "$top": {"-1"}}, &boards)
	return boards, err
}

// resolveBoard finds a board by ID or name and loads it in full.
func (t *toolset) resolveBoard(ctx context.Context, ref string) (apiAgile, error) {
	ref = strings.TrimSpace(ref)
	id := ""
	if dbID.MatchString(ref) {
		id = ref
	} else {
		boards, err := t.listAgiles(ctx, "id,name,projects(shortName)")
		if err != nil {
			return apiAgile{}, err
		}
		var matches []apiAgile
		for _, b := range boards {
			if strings.EqualFold(b.Name, ref) {
				matches = append(matches, b)
			}
		}
		switch len(matches) {
		case 0:
			names := make([]string, 0, len(boards))
			for _, b := range boards {
				names = append(names, b.Name)
			}
			return apiAgile{}, fmt.Errorf("agile board %q not found; boards: %s", ref, strings.Join(names, ", "))
		case 1:
			id = matches[0].ID
		default:
			ids := make([]string, 0, len(matches))
			for _, b := range matches {
				ids = append(ids, fmt.Sprintf("%s (%s)", b.ID, strings.Join(b.projectKeys(), ",")))
			}
			return apiAgile{}, fmt.Errorf("several boards are named %q, pass an ID: %s", ref, strings.Join(ids, ", "))
		}
	}
	var b apiAgile
	if err := t.yt.Get(ctx, "agiles/"+url.PathEscape(id), url.Values{"fields": {agileFields}}, &b); err != nil {
		return apiAgile{}, err
	}
	return b, nil
}

// resolveSprint finds a sprint of the board by ID or name; "" and "current" mean the current sprint.
func resolveSprint(b apiAgile, ref string) (apiSprint, error) {
	ref = strings.TrimSpace(ref)
	if ref == "" || strings.EqualFold(ref, "current") {
		if b.CurrentSprint == nil {
			return apiSprint{}, fmt.Errorf("board %s has no current sprint", b.Name)
		}
		return *b.CurrentSprint, nil
	}
	for _, s := range b.Sprints {
		if s.ID == ref || strings.EqualFold(s.Name, ref) {
			return s, nil
		}
	}
	return apiSprint{}, fmt.Errorf("sprint %q not found on board %s; use get_agile_board to list sprints", ref, b.Name)
}

func (t *toolset) boardURL(b apiAgile, sprintID string) string {
	u := t.yt.BaseURL() + "/agiles/" + b.ID
	if sprintID != "" {
		u += "/" + sprintID
	}
	return u
}

func (t *toolset) findAgileBoards(ctx context.Context, in findAgileBoardsIn) (any, error) {
	offset, limit := page(in.Offset, in.Limit, 20, 20)
	boards, err := t.listAgiles(ctx, "id,name,projects(shortName),currentSprint(id,name),sprintsSettings(disableSprints)")
	if err != nil {
		return nil, err
	}
	type board struct {
		ID            string   `json:"id"`
		Name          string   `json:"name"`
		Projects      []string `json:"projects"`
		Sprints       bool     `json:"sprintsEnabled"`
		CurrentSprint string   `json:"currentSprint,omitempty"`
	}
	needle := strings.ToLower(in.NameContains)
	matched := []board{}
	for _, b := range boards {
		hit := strings.Contains(strings.ToLower(b.Name), needle)
		for _, k := range b.projectKeys() {
			hit = hit || strings.Contains(strings.ToLower(k), needle)
		}
		if !hit {
			continue
		}
		out := board{ID: b.ID, Name: b.Name, Projects: b.projectKeys(), Sprints: !b.SprintsSettings.DisableSprints}
		if b.CurrentSprint != nil && out.Sprints {
			out.CurrentSprint = b.CurrentSprint.Name
		}
		matched = append(matched, out)
	}
	total := len(matched)
	return map[string]any{"boards": matched[min(offset, total):min(offset+limit, total)], "offset": offset, "total": total}, nil
}

func (t *toolset) getAgileBoard(ctx context.Context, in getAgileBoardIn) (any, error) {
	b, err := t.resolveBoard(ctx, in.Board)
	if err != nil {
		return nil, err
	}
	limit := in.SprintsLimit
	if limit <= 0 {
		limit = 20
	}
	sprints := make([]apiSprint, 0, len(b.Sprints))
	archived := 0
	for _, s := range b.Sprints {
		if s.Archived {
			archived++
			if !in.IncludeArchived {
				continue
			}
		}
		sprints = append(sprints, s)
	}
	sort.SliceStable(sprints, func(i, j int) bool { return sprints[i].Start > sprints[j].Start })
	sprints = sprints[:min(limit, len(sprints))]

	columns := make([]string, 0, len(b.ColumnSettings.Columns))
	for _, c := range b.ColumnSettings.Columns {
		columns = append(columns, c.Presentation)
	}
	out := map[string]any{
		"id":              b.ID,
		"name":            b.Name,
		"url":             t.boardURL(b, ""),
		"owner":           b.Owner,
		"projects":        b.projectKeys(),
		"columnField":     b.columnField(),
		"columns":         columns,
		"sprintsEnabled":  !b.SprintsSettings.DisableSprints,
		"explicitSprints": b.SprintsSettings.IsExplicit,
		"currentSprint":   b.CurrentSprint,
		"sprints":         sprints,
		"sprintsTotal":    len(b.Sprints),
		"sprintsArchived": archived,
	}
	if b.EstimationField != nil {
		out["estimationField"] = b.EstimationField.Name
	}
	return out, nil
}

func (t *toolset) getSprint(ctx context.Context, in getSprintIn) (any, error) {
	offset, limit := page(in.Offset, in.Limit, 20, 50)
	b, err := t.resolveBoard(ctx, in.Board)
	if err != nil {
		return nil, err
	}
	ref, err := resolveSprint(b, in.Sprint)
	if err != nil {
		return nil, err
	}
	base := "agiles/" + url.PathEscape(b.ID) + "/sprints/" + url.PathEscape(ref.ID)
	var sprint apiSprint
	if err := t.yt.Get(ctx, base, url.Values{"fields": {sprintFields + ",unresolvedIssuesCount"}}, &sprint); err != nil {
		return nil, err
	}

	fieldNames := in.CustomFieldsToReturn
	if fieldNames == nil {
		for _, name := range []string{b.columnField(), "Assignee"} {
			if name != "" {
				fieldNames = append(fieldNames, name)
			}
		}
		if b.EstimationField != nil {
			fieldNames = append(fieldNames, b.EstimationField.Name)
		}
	}
	q := url.Values{
		"fields": {"idReadable,summary,resolved,customFields(" + customFieldValueFields + ")"},
		"$skip":  {strconv.Itoa(offset)},
		"$top":   {strconv.Itoa(limit + 1)},
	}
	var issues []apiIssue
	if err := t.yt.Get(ctx, base+"/issues", q, &issues); err != nil {
		return nil, err
	}
	hasMore := len(issues) > limit
	if hasMore {
		issues = issues[:limit]
	}
	type sprintIssue struct {
		ID           string         `json:"id"`
		Summary      string         `json:"summary"`
		Resolved     millis         `json:"resolved"`
		CustomFields map[string]any `json:"customFields,omitempty"`
	}
	out := make([]sprintIssue, 0, len(issues))
	for _, is := range issues {
		si := sprintIssue{ID: is.IDReadable, Summary: is.Summary, Resolved: is.Resolved}
		if len(fieldNames) > 0 {
			all := flattenFields(is.CustomFields)
			si.CustomFields = map[string]any{}
			for _, name := range fieldNames {
				if v, ok := all[name]; ok {
					si.CustomFields[name] = v
				}
			}
		}
		out = append(out, si)
	}
	return map[string]any{
		"board":   map[string]any{"id": b.ID, "name": b.Name},
		"sprint":  sprint,
		"url":     t.boardURL(b, sprint.ID),
		"issues":  out,
		"offset":  offset,
		"hasMore": hasMore,
	}, nil
}
