package ytwatch

import (
	"context"
	"errors"
	"net/http"
	"reflect"
	"testing"
)

func TestQueryTags(t *testing.T) {
	for _, c := range []struct {
		query string
		want  []string
	}{
		{"project: ABC tag: ai #Unresolved", []string{"ai"}},
		{"tag: {Needs attention}", []string{"Needs attention"}},
		{"(tag: -done) and Tag: x, {y z} , w", []string{"done", "x", "y z", "w"}},
		{"tag: ai or tag: AI", []string{"ai"}},
		{"hashtag: x #ai State: Open", nil},
		{"tag: {unclosed", nil},
	} {
		if got := queryTags(c.query); !reflect.DeepEqual(got, c.want) {
			t.Errorf("queryTags(%q) = %q, want %q", c.query, got, c.want)
		}
	}
}

// TestSearchNewTag: YouTrack refuses a query by a tag that doesn't exist;
// Search says so, Members matches nothing, and any other refusal stays an
// error.
func TestSearchNewTag(t *testing.T) {
	api := newFakeAPI()
	api.json("GET /api/issues", http.StatusBadRequest, `{"error":"invalid_query","error_description":"Can't parse search query, please check and update query syntax"}`)
	api.json("GET /api/tags", 200, `[{"name":"Star"},{"name":"AI"}]`)
	w, _ := newWatcher(t, api)
	ctx := context.Background()

	_, err := w.Search(ctx, "project: ABC tag: {Needs attention} tag: ai")
	var nt *NewTagError
	if !errors.As(err, &nt) || !reflect.DeepEqual(nt.Tags, []string{"Needs attention"}) {
		t.Fatalf("Search = %v", err)
	}
	if err.Error() != "tag Needs attention doesn't exist yet; nothing matches until YouTrack creates it" {
		t.Errorf("message = %q", err)
	}

	got, err := w.Members(ctx, "tag: needs-input", []string{"3-1", "ABC-2"})
	if err != nil || !reflect.DeepEqual(got, map[string]bool{"3-1": false, "ABC-2": false}) {
		t.Errorf("Members = %v, %v", got, err)
	}

	for _, q := range []string{"State: Bogus", "tag: ai"} {
		if _, err := w.Search(ctx, q); err == nil || errors.As(err, &nt) {
			t.Errorf("Search(%q) = %v, want the refusal", q, err)
		}
	}
}
