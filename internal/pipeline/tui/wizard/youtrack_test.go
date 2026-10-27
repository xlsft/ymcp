package wizard

import (
	"encoding/json"
	"io"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/xlsft/ymcp/internal/youtrack"
	"github.com/xlsft/ymcp/internal/youtrack/youtracktest"
)

func reply(w http.ResponseWriter, status int, body string) {
	w.Header().Set("Content-Type", "application/json;charset=utf-8")
	w.WriteHeader(status)
	io.WriteString(w, body)
}

func newClient(t *testing.T, mux *http.ServeMux) YouTrack {
	t.Helper()
	srv := youtracktest.New(t, mux)
	yt, err := youtrack.New(youtrack.Options{BaseURL: srv.URL, Login: youtracktest.Login, Password: youtracktest.Password, Proxy: "none"})
	if err != nil {
		t.Fatal(err)
	}
	return NewYouTrack(yt)
}

func TestSearchAssist(t *testing.T) {
	mux := http.NewServeMux()
	var body map[string]any
	mux.HandleFunc("POST /api/search/assist", func(w http.ResponseWriter, r *http.Request) {
		json.NewDecoder(r.Body).Decode(&body)
		if f := r.URL.Query().Get("fields"); !strings.Contains(f, "completionStart") || !strings.Contains(f, "caret") {
			t.Errorf("fields = %q", f)
		}
		// The shape YouTrack answers with (checked live).
		reply(w, 200, `{"caret":16,"suggestions":[
			{"completionStart":13,"completionEnd":16,"matchingStart":0,"matchingEnd":3,"caret":20,"group":null,"suffix":": ","prefix":null,"description":"by State","option":"State","$type":"Suggestion"},
			{"completionStart":13,"completionEnd":16,"caret":19,"suffix":" ","prefix":"#","description":"favourite","option":"Star","$type":"Suggestion"}
		],"query":"project: YPC Sta","$type":"SearchSuggestions"}`)
	})
	yt := newClient(t, mux)
	got, err := yt.SearchAssist(t.Context(), "project: YPC Sta", 16)
	if err != nil {
		t.Fatal(err)
	}
	if body["query"] != "project: YPC Sta" || body["caret"] != float64(16) {
		t.Errorf("body = %v", body)
	}
	if len(got) != 2 {
		t.Fatalf("got %+v", got)
	}
	if text, caret := got[0].Apply("project: YPC Sta"); text != "project: YPC State: " || caret != 20 {
		t.Errorf("applied = %q @%d", text, caret)
	}
	if text, _ := got[1].Apply("project: YPC Sta"); text != "project: YPC #Star " {
		t.Errorf("applied = %q", text)
	}
}

func TestCommandAssist(t *testing.T) {
	mux := http.NewServeMux()
	var bodies []map[string]any
	mux.HandleFunc("POST /api/commands/assist", func(w http.ResponseWriter, r *http.Request) {
		var b map[string]any
		json.NewDecoder(r.Body).Decode(&b)
		bodies = append(bodies, b)
		reply(w, 200, `{"caret":8,"suggestions":[{"completionStart":6,"completionEnd":8,"caret":18,"suffix":" ","prefix":null,"description":"State","option":"In Progress"}],
			"commands":[{"error":true,"description":"State expected: In"}],"query":"State In"}`)
	})
	yt := newClient(t, mux)
	got, err := yt.CommandAssist(t.Context(), "State In", 8, "YPC-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Option != "In Progress" {
		t.Fatalf("got %+v", got)
	}
	if _, err := yt.CommandAssist(t.Context(), "State In", 8, "2-14"); err != nil {
		t.Fatal(err)
	}
	issue := func(i int) map[string]any { return bodies[i]["issues"].([]any)[0].(map[string]any) }
	if issue(0)["idReadable"] != "YPC-1" || issue(1)["id"] != "2-14" {
		t.Errorf("issue refs = %v, %v", issue(0), issue(1))
	}
	if _, err := yt.CommandAssist(t.Context(), "State", 5, ""); err == nil {
		t.Error("no issue: want an error")
	}
}

const adminFields = `[
	{"$type":"EnumProjectCustomField","field":{"name":"Priority"},"bundle":{"values":[{"name":"Major","archived":false},{"name":"Old","archived":true}]}},
	{"$type":"StateProjectCustomField","field":{"name":"State"},"bundle":{"values":[{"name":"Open"},{"name":"In Progress"}]}},
	{"$type":"UserProjectCustomField","field":{"name":"Assignee"},"bundle":{"values":[{"name":"Team"}]}},
	{"$type":"VersionProjectCustomField","field":{"name":"Fix versions"},"bundle":{"values":[]}},
	{"$type":"SimpleProjectCustomField","field":{"name":"Estimate"}}
]`

func TestFields(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/admin/projects/YPC/customFields", func(w http.ResponseWriter, r *http.Request) {
		reply(w, 200, adminFields)
	})
	got, err := newClient(t, mux).Fields(t.Context(), "YPC")
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, f := range got {
		names = append(names, f.Name+":"+f.Type+":"+strings.Join(f.Values, "/"))
	}
	want := []string{"State:state:Open/In Progress", "Priority:enum:Major", "Assignee:user:me"}
	if !slices.Equal(names, want) {
		t.Fatalf("got %v, want %v", names, want)
	}
}

// Without admin rights the settings endpoint is empty or forbidden, and the
// fields come from recent issues.
func TestFieldsFromIssues(t *testing.T) {
	for _, admin := range []func(w http.ResponseWriter){
		func(w http.ResponseWriter) { reply(w, 200, `[]`) },
		func(w http.ResponseWriter) { reply(w, 403, `{"error":"Forbidden"}`) },
	} {
		mux := http.NewServeMux()
		mux.HandleFunc("GET /api/admin/projects/YPC/customFields", func(w http.ResponseWriter, r *http.Request) { admin(w) })
		mux.HandleFunc("GET /api/issues", func(w http.ResponseWriter, r *http.Request) {
			if q := r.URL.Query().Get("query"); q != "project: {YPC} sort by: updated desc" {
				t.Errorf("query = %q", q)
			}
			reply(w, 200, `[
				{"customFields":[{"projectCustomField":{"$type":"StateProjectCustomField","field":{"name":"State"},"bundle":{"values":[{"name":"Open"}]}}}]},
				{"customFields":[{"projectCustomField":{"$type":"StateProjectCustomField","field":{"name":"State"},"bundle":{"values":[{"name":"Open"}]}}},
				                 {"projectCustomField":{"$type":"EnumProjectCustomField","field":{"name":"Type"},"bundle":{"values":[{"name":"Bug"}]}}}]}
			]`)
		})
		got, err := newClient(t, mux).Fields(t.Context(), "YPC")
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 2 || got[0].Name != "State" || got[1].Name != "Type" {
			t.Fatalf("got %+v", got)
		}
	}
}

func TestUsersTagsSaved(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/users", func(w http.ResponseWriter, r *http.Request) {
		if q := r.URL.Query(); q.Get("query") != "iv" || q.Get("fields") != "login,fullName" {
			t.Errorf("query = %v", q)
		}
		reply(w, 200, `[{"login":"ivan.petrov","fullName":"Ivan Petrov","$type":"User"}]`)
	})
	mux.HandleFunc("GET /api/tags", func(w http.ResponseWriter, r *http.Request) {
		reply(w, 200, `[{"name":"needs-input"},{"name":"AI"},{"name":"needs-input"}]`)
	})
	mux.HandleFunc("GET /api/savedQueries", func(w http.ResponseWriter, r *http.Request) {
		reply(w, 200, `[{"name":"Assigned to me","query":"for: me","$type":"SavedQuery"}]`)
	})
	yt := newClient(t, mux)
	users, err := yt.Users(t.Context(), "iv")
	if err != nil || len(users) != 1 || users[0] != (User{Login: "ivan.petrov", FullName: "Ivan Petrov"}) {
		t.Fatalf("users = %v, %v", users, err)
	}
	tags, err := yt.Tags(t.Context())
	if err != nil || !slices.Equal(tags, []string{"AI", "needs-input"}) {
		t.Fatalf("tags = %v, %v", tags, err)
	}
	saved, err := yt.SavedSearches(t.Context())
	if err != nil || len(saved) != 1 || saved[0].Query != "for: me" {
		t.Fatalf("saved = %v, %v", saved, err)
	}
}

func TestUsersForbidden(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/users", func(w http.ResponseWriter, r *http.Request) {
		reply(w, 403, `{"error":"Forbidden","error_description":"You do not have permissions"}`)
	})
	if _, err := newClient(t, mux).Users(t.Context(), "iv"); err == nil {
		t.Fatal("want an error")
	}
}
