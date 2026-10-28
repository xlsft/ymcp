package ytwatch

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/xlsft/ymcp/internal/youtrack"
)

// NewTagError is a search by a tag nobody has created yet. YouTrack can't
// parse such a query, yet the answer is known: nothing matches it until the
// first `tag` command creates the tag.
type NewTagError struct {
	Tags []string
}

func (e *NewTagError) Error() string {
	what := "tag " + e.Tags[0]
	if len(e.Tags) > 1 {
		what = "tags " + strings.Join(e.Tags, ", ")
	}
	return what + " doesn't exist yet; nothing matches until YouTrack creates it"
}

// newTags names the tags of query that don't exist, when err is YouTrack
// refusing the query. It is nil when the query names none, every one of
// them exists, or the tags can't be read.
func (w *Watcher) newTags(ctx context.Context, query string, err error) []string {
	var ye *youtrack.Error
	if !errors.As(err, &ye) || ye.Status != http.StatusBadRequest {
		return nil
	}
	names := queryTags(query)
	if len(names) == 0 {
		return nil
	}
	existing, terr := w.tags(ctx)
	if terr != nil {
		return nil
	}
	var out []string
	for _, n := range names {
		if !existing[strings.ToLower(n)] {
			out = append(out, n)
		}
	}
	return out
}

// tags are the lowercased names of every tag the account can see.
func (w *Watcher) tags(ctx context.Context) (map[string]bool, error) {
	out := map[string]bool{}
	for skip, page := 0, 0; page < maxPages; page++ {
		var batch []struct {
			Name string `json:"name"`
		}
		q := url.Values{"fields": {"name"}, "$skip": {strconv.Itoa(skip)}, "$top": {strconv.Itoa(w.pageSize)}}
		if err := w.yt.Get(ctx, "tags", q, &batch); err != nil {
			return nil, fmt.Errorf("tags: %w", err)
		}
		for _, t := range batch {
			out[strings.ToLower(t.Name)] = true
		}
		if len(batch) < w.pageSize {
			break
		}
		skip += len(batch)
	}
	return out, nil
}

// queryTags are the tags a query filters by: tag: x, tag: {two words},
// tag: -x and tag: x, y. Each name is listed once.
func queryTags(query string) []string {
	var out []string
	seen := map[string]bool{}
	add := func(name string) {
		name = strings.TrimSpace(name)
		if name != "" && !seen[strings.ToLower(name)] {
			seen[strings.ToLower(name)] = true
			out = append(out, name)
		}
	}
	skipSpaces := func(i int) int {
		for i < len(query) && query[i] == ' ' {
			i++
		}
		return i
	}
	for i := 0; i+len("tag:") <= len(query); i++ {
		if !strings.EqualFold(query[i:i+len("tag:")], "tag:") || (i > 0 && !strings.ContainsRune(" (", rune(query[i-1]))) {
			continue
		}
		i += len("tag:")
		for {
			i = skipSpaces(i)
			if i < len(query) && query[i] == '-' {
				i++
			}
			if i < len(query) && query[i] == '{' {
				end := strings.IndexByte(query[i:], '}')
				if end < 0 {
					return out
				}
				add(query[i+1 : i+end])
				i += end + 1
			} else {
				end := i
				for end < len(query) && !strings.ContainsRune(" ,()", rune(query[end])) {
					end++
				}
				add(query[i:end])
				i = end
			}
			if j := skipSpaces(i); j < len(query) && query[j] == ',' {
				i = j + 1
				continue
			}
			break
		}
	}
	return out
}
