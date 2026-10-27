package engine

import (
	"errors"
	"testing"
)

func TestMaybeNewTag(t *testing.T) {
	parse := errors.New("youtrack: GET /api/issues: 400 Bad Request: invalid_query: Can't parse search query")
	for _, tc := range []struct {
		err   error
		query string
		want  bool
	}{
		{parse, "project: YPC tag: ymcp-live2 #Unresolved", true},
		{parse, "project: YPC #new-tag", true},
		{parse, "project: YPC State: Bogus", false},
		{errors.New("youtrack: 500 Internal Server Error"), "tag: x", false},
	} {
		if got := maybeNewTag(tc.err, tc.query); got != tc.want {
			t.Errorf("maybeNewTag(%q, %q) = %v, want %v", tc.err, tc.query, got, tc.want)
		}
	}
}
