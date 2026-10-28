package site

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/xlsft/ymcp/internal/clients"
	"github.com/xlsft/ymcp/internal/pipeline/spec"
	"github.com/xlsft/ymcp/internal/tools"
	"github.com/xlsft/ymcp/internal/youtrack"
)

type data struct {
	Tools      counts   `json:"tools"`
	Groups     []group  `json:"groups"`
	Prompts    []prompt `json:"prompts"`
	Clients    []client `json:"clients"`
	Agents     []choice `json:"agents"`
	Strategies []choice `json:"strategies"`
}

type counts struct {
	Total   int `json:"total"`
	BuiltIn int `json:"builtIn"`
	Extras  int `json:"extras"`
}

type group struct {
	Name  string `json:"name"`
	Title string `json:"title"`
	Tools []tool `json:"tools"`
}

type tool struct {
	Name        string `json:"name"`
	Title       string `json:"title"`
	Summary     string `json:"summary"`
	BuiltIn     bool   `json:"builtIn"`
	ReadOnly    bool   `json:"readOnly"`
	Destructive bool   `json:"destructive"`
}

type prompt struct {
	Name        string   `json:"name"`
	Title       string   `json:"title"`
	Description string   `json:"description"`
	Arguments   []string `json:"arguments"`
}

type client struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

type choice struct {
	Name  string `json:"name"`
	About string `json:"about"`
}

// TestSiteData writes site.json to the directory in YMCP_SITE. Without it,
// it only checks that the data can be built.
func TestSiteData(t *testing.T) {
	d := build(t)
	if d.Tools.Total == 0 || len(d.Clients) == 0 || len(d.Agents) == 0 || len(d.Strategies) == 0 {
		t.Fatalf("empty data: %+v", d)
	}
	dir := os.Getenv("YMCP_SITE")
	if dir == "" {
		return
	}
	b, err := json.MarshalIndent(d, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "site.json"), append(b, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
}

func build(t *testing.T) data {
	t.Helper()
	served, prompts := serve(t)
	var d data
	for _, g := range tools.Groups {
		out := group{Name: g.Name, Title: g.Title}
		for _, name := range g.Tools {
			mt, ok := served[name]
			if !ok {
				t.Fatalf("%s is grouped but not served", name)
			}
			tl := tool{Name: name, Title: mt.Title, Summary: summary(mt.Description), BuiltIn: slices.Contains(tools.BuiltIn, name)}
			if a := mt.Annotations; a != nil {
				tl.ReadOnly = a.ReadOnlyHint
				tl.Destructive = a.DestructiveHint != nil && *a.DestructiveHint
				if tl.Title == "" {
					tl.Title = a.Title
				}
			}
			out.Tools = append(out.Tools, tl)
			d.Tools.Total++
			if tl.BuiltIn {
				d.Tools.BuiltIn++
			}
		}
		d.Groups = append(d.Groups, out)
	}
	d.Tools.Extras = d.Tools.Total - d.Tools.BuiltIn
	for _, p := range prompts {
		out := prompt{Name: p.Name, Title: p.Title, Description: p.Description, Arguments: []string{}}
		for _, a := range p.Arguments {
			out.Arguments = append(out.Arguments, a.Name)
		}
		d.Prompts = append(d.Prompts, out)
	}
	for _, c := range clients.All() {
		d.Clients = append(d.Clients, client{ID: c.ID, Name: c.Name})
	}
	for _, c := range spec.Providers {
		d.Agents = append(d.Agents, choice(c))
	}
	for _, c := range spec.StrategyKinds {
		d.Strategies = append(d.Strategies, choice(c))
	}
	return d
}

// serve lists the tools and prompts a server with every tool offers, as
// a client sees them.
func serve(t *testing.T) (map[string]*mcp.Tool, []*mcp.Prompt) {
	t.Helper()
	yt, err := youtrack.New(youtrack.Options{BaseURL: "https://yt.example.com", Login: "a", Password: "b", Proxy: "none"})
	if err != nil {
		t.Fatal(err)
	}
	server := mcp.NewServer(&mcp.Implementation{Name: "site", Version: "0"}, nil)
	tools.Register(server, yt, nil)
	ct, st := mcp.NewInMemoryTransports()
	ctx := context.Background()
	if _, err := server.Connect(ctx, st, nil); err != nil {
		t.Fatal(err)
	}
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "site"}, nil).Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close()
	tl, err := cs.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	byName := map[string]*mcp.Tool{}
	for _, x := range tl.Tools {
		byName[x.Name] = x
	}
	pl, err := cs.ListPrompts(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	return byName, pl.Prompts
}

// summary is the first sentence of a tool's description.
func summary(desc string) string {
	desc = strings.TrimSpace(strings.SplitN(desc, "\n", 2)[0])
	if i := strings.Index(desc, ". "); i >= 0 {
		desc = desc[:i+1]
	}
	return desc
}
