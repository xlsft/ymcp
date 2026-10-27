package ytwatch

import (
	"context"
	"errors"
	"fmt"
	"html"
	"net/http"
	"net/url"
	"regexp"
	"strings"

	"github.com/xlsft/ymcp/internal/youtrack"
)

// CommandError is a command YouTrack refused with 400: an invalid value,
// an unknown user or field, a missing issue. It is permanent; retrying the
// same command does not help.
type CommandError struct {
	Command string
	Issue   string
	// Description is YouTrack's explanation, e.g. "State expected: Bogus".
	Description string
	Err         *youtrack.Error
}

func (e *CommandError) Error() string {
	return fmt.Sprintf("command %q on %s failed: %s", e.Command, e.Issue, e.Description)
}

func (e *CommandError) Unwrap() error { return e.Err }

// Apply runs a command on one issue, as typed in YouTrack's command
// dialog. Multi-part commands are atomic. A 400 comes back as
// *CommandError; other failures (network, 401, 5xx) as they are.
func (w *Watcher) Apply(ctx context.Context, command, issueKey string) error {
	if strings.TrimSpace(command) == "" {
		return errors.New("command is empty")
	}
	body := map[string]any{"query": command, "issues": []map[string]string{issueRef(issueKey)}}
	err := w.yt.Post(ctx, "commands", nil, body, nil)
	if err == nil {
		return nil
	}
	var ye *youtrack.Error
	if errors.As(err, &ye) && ye.Status == http.StatusBadRequest {
		desc := ye.Description
		if desc == "" {
			desc = ye.Code
		}
		if desc == "" {
			desc = strings.TrimSpace(ye.Body)
		}
		return &CommandError{Command: command, Issue: issueKey, Description: desc, Err: ye}
	}
	return fmt.Errorf("apply %q to %s: %w", command, issueKey, err)
}

// CommandState is the verdict of Validate.
type CommandState string

const (
	// Valid commands parse completely and would apply.
	Valid CommandState = "valid"
	// Incomplete commands are a prefix of something valid, e.g. "State" or
	// "State In"; YouTrack offers ways to finish them.
	Incomplete CommandState = "incomplete"
	// Invalid commands name an unknown command, field or value.
	Invalid CommandState = "invalid"
)

// CommandCheck is what YouTrack's command parser says about a command.
type CommandCheck struct {
	State CommandState
	// Text is what the command would do when valid ("Set State In
	// Progress"), otherwise the parser's errors ("State expected: Bogus").
	// Parts are joined with "; ".
	Text string
	// Suggestions are completions YouTrack offers, at most 10.
	Suggestions []string
}

var markup = regexp.MustCompile(`<[^>]*>`)

// maxSuggestions caps CommandCheck.Suggestions.
const maxSuggestions = 10

// Validate parses a command against an issue without applying it
// (POST commands/assist). Field values only resolve against an issue, so
// issueKey is required.
func (w *Watcher) Validate(ctx context.Context, command, issueKey string) (CommandCheck, error) {
	if issueKey == "" {
		return CommandCheck{}, errors.New("validate command: an issue is required")
	}
	var assist struct {
		Commands []struct {
			Description string `json:"description"`
			Error       bool   `json:"error"`
		} `json:"commands"`
		Suggestions []struct {
			Option string `json:"option"`
		} `json:"suggestions"`
	}
	body := map[string]any{
		"query":  command,
		"caret":  len([]rune(command)),
		"issues": []map[string]string{issueRef(issueKey)},
	}
	q := url.Values{"fields": {"commands(description,error),suggestions(option)"}}
	if err := w.yt.Post(ctx, "commands/assist", q, body, &assist); err != nil {
		return CommandCheck{}, fmt.Errorf("validate %q on %s: %w", command, issueKey, err)
	}

	var check CommandCheck
	for _, s := range assist.Suggestions {
		if len(check.Suggestions) == maxSuggestions {
			break
		}
		check.Suggestions = append(check.Suggestions, s.Option)
	}
	var ok, failed []string
	for _, c := range assist.Commands {
		// Descriptions carry HTML markup for the UI.
		d := strings.TrimSpace(html.UnescapeString(markup.ReplaceAllString(c.Description, "")))
		if c.Error {
			failed = append(failed, d)
		} else {
			ok = append(ok, d)
		}
	}
	switch {
	case len(assist.Commands) == 0:
		check.State, check.Text = Incomplete, "empty command"
	case len(failed) == 0:
		check.State, check.Text = Valid, strings.Join(ok, "; ")
	case len(check.Suggestions) > 0:
		// "State" or "State In": the parser still sees ways to finish it.
		check.State, check.Text = Incomplete, strings.Join(failed, "; ")
	default:
		check.State, check.Text = Invalid, strings.Join(failed, "; ")
	}
	return check, nil
}
