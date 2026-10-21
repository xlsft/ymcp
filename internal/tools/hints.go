package tools

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"

	"github.com/xlsft/ymcp/internal/install"
	"github.com/xlsft/ymcp/internal/youtrack"
)

// withHint adds a line to an error saying what likely went wrong and what
// to try next, so the model can recover instead of guessing. Errors the
// tools write themselves already say so and pass unchanged.
func withHint(err error) error {
	if hint := hintFor(err); hint != "" {
		return fmt.Errorf("%w\nHint: %s", err, hint)
	}
	return err
}

func hintFor(err error) string {
	var yt *youtrack.Error
	if errors.As(err, &yt) {
		return statusHint(yt)
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "YouTrack didn't answer in time. Try again; for searches, a narrower query is faster."
	}
	var netErr net.Error
	if errors.As(err, &netErr) {
		return "YouTrack can't be reached from the machine running ymcp. The user should check the VPN, network or " +
			"proxy (YOUTRACK_PROXY); retrying won't help until then."
	}
	return ""
}

func statusHint(e *youtrack.Error) string {
	switch {
	case e.Status == http.StatusBadRequest:
		return "YouTrack rejected the arguments. Check names and values against the tool's schema; for custom " +
			"fields, get_issue_fields_schema lists the allowed values."
	case e.Status == http.StatusUnauthorized && strings.Contains(e.Description+e.Body, "scope"):
		return "This endpoint needs Hub-wide permissions that ymcp's YouTrack token doesn't carry; for Hub data, " +
			"call_api with api=hub may work."
	case e.Status == http.StatusUnauthorized:
		return "YouTrack refused the sign-in even after logging in again: the password may have changed or the " +
			"account may be banned. The user can check with `" + install.Command("check") + "` and fix it with `" +
			install.Command("auth") + "`."
	case e.Status == http.StatusForbidden:
		return "This account lacks the YouTrack permission for this action. Ask a YouTrack admin to grant it, or " +
			"leave the action to someone who has it; retrying won't help."
	case e.Status == http.StatusNotFound:
		return "It doesn't exist, or this account can't see it. " + findHint(e.Path)
	case e.Status == http.StatusMethodNotAllowed:
		return "YouTrack doesn't support this method on this path; check the endpoint in the YouTrack REST docs."
	case e.Status == http.StatusConflict:
		return "It changed or already exists. Read it again and retry with the current state."
	case e.Status == http.StatusTooManyRequests || e.Status == http.StatusServiceUnavailable ||
		e.Status == http.StatusBadGateway || e.Status == http.StatusGatewayTimeout:
		return "YouTrack is overloaded or restarting and still was after ymcp's retries. Wait a minute and try again."
	case e.Status >= 500:
		return "YouTrack failed on its side. The request may be invalid in a way it didn't report: check the " +
			"arguments, then try once more."
	}
	return ""
}

// findHint names the tool that finds valid IDs for a path.
func findHint(path string) string {
	for _, h := range []struct{ part, tool string }{
		{"/drafts", ""},
		{"/articles", "search_articles"},
		{"/agiles", "find_agile_boards"},
		{"/sprints", "get_agile_board"},
		{"/tags", "find_tags"},
		{"/savedQueries", "get_saved_issue_searches"},
		{"/comments", "get_issue_comments"},
		{"/attachments", "get_issue_attachments"},
		{"/workItems", "get_issue_work_items"},
		{"/issues", "search_issues"},
		{"/projects", "find_projects"},
		{"/users", "find_user"},
		{"/groups", "find_user_groups"},
	} {
		if strings.Contains(path, h.part) && h.tool != "" {
			return "Check the ID; " + h.tool + " finds the right one."
		}
		if strings.Contains(path, h.part) {
			break
		}
	}
	return "Check the ID and the path."
}
