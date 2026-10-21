package tools

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"testing"

	"github.com/xlsft/ymcp/internal/youtrack"
)

func TestHintFor(t *testing.T) {
	for _, tc := range []struct {
		err  error
		want string
	}{
		{&youtrack.Error{Status: 401, Description: `The operation requires the "0-0-0-0-0" scope`}, "Hub-wide permissions"},
		{&youtrack.Error{Status: 401}, "ymcp auth"},
		{&youtrack.Error{Status: 429}, "after ymcp's retries"},
		{&youtrack.Error{Status: 400}, "get_issue_fields_schema"},
		{&youtrack.Error{Status: 404, Path: "/api/users/me/drafts/3-1"}, "Check the ID and the path."},
		{&youtrack.Error{Status: 404, Path: "/api/issues/X-1/comments/4-1"}, "get_issue_comments"},
		{fmt.Errorf("youtrack: GET /api/issues: %w", &net.OpError{Op: "dial", Err: errors.New("no route to host")}), "VPN"},
		{fmt.Errorf("wrapped: %w", context.DeadlineExceeded), "didn't answer in time"},
		{errors.New("unknown link type"), ""},
	} {
		if got := hintFor(tc.err); !strings.Contains(got, tc.want) || (tc.want == "" && got != "") {
			t.Errorf("%v: hint %q, want %q", tc.err, got, tc.want)
		}
	}
}
