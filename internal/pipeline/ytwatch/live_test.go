package ytwatch

import (
	"context"
	"errors"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/xlsft/ymcp/internal/config"
	"github.com/xlsft/ymcp/internal/youtrack"
)

// liveHost is the only instance the live test talks to.
const liveHost = "xlsft.youtrack.cloud"

// TestLive runs read-only checks against project YPC on the xlsft
// instance with a stored ymcp profile (YMCP_PIPELINE_PROFILE, default
// youtrack-mcp-xlsft). It only applies a command YouTrack rejects, so
// nothing changes. Enable with YMCP_PIPELINE_LIVE=1.
func TestLive(t *testing.T) {
	if os.Getenv("YMCP_PIPELINE_LIVE") != "1" {
		t.Skip("set YMCP_PIPELINE_LIVE=1 to run against " + liveHost)
	}
	profile := os.Getenv("YMCP_PIPELINE_PROFILE")
	if profile == "" {
		profile = "youtrack-mcp-xlsft"
	}
	cfg, err := config.Load(profile)
	if err != nil {
		t.Fatal(err)
	}
	if u, err := url.Parse(youtrack.NormalizeURL(cfg.URL)); err != nil || u.Host != liveHost {
		t.Fatalf("profile %s points at %q; the live test only runs against %s", profile, cfg.URL, liveHost)
	}
	yt, err := youtrack.New(youtrack.Options{BaseURL: cfg.URL, Login: cfg.Login, Password: cfg.Password, Proxy: cfg.Proxy})
	if err != nil {
		t.Fatal(err)
	}
	w := New(yt)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	login, err := w.Me(ctx)
	if err != nil || login == "" {
		t.Fatalf("Me = %q, %v", login, err)
	}

	now, err := w.ServerTime(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if skew := time.Since(now); skew > 5*time.Minute || skew < -5*time.Minute {
		t.Errorf("server time %v is %v off the local clock", now, skew)
	}

	issues, err := w.Search(ctx, "project: YPC")
	if err != nil || len(issues) == 0 {
		t.Fatalf("Search = %d issues, %v", len(issues), err)
	}
	var ids []string
	for _, is := range issues {
		if !strings.HasPrefix(is.Key, "YPC-") || is.ID == "" || is.Updated.IsZero() {
			t.Errorf("issue %+v", is)
		}
		ids = append(ids, is.ID)
	}

	full, err := w.Issue(ctx, issues[0].ID)
	if err != nil || full.Key != issues[0].Key || full.Fields == nil {
		t.Errorf("Issue = %+v, %v", full, err)
	}
	if _, ok := full.Fields["State"]; !ok {
		t.Errorf("Issue fields have no State: %v", full.Fields)
	}

	if ok, err := w.Exists(ctx, issues[0].Key); !ok || err != nil {
		t.Errorf("Exists(%s) = %v, %v", issues[0].Key, ok, err)
	}
	if ok, err := w.Exists(ctx, "YPC-999999"); ok || err != nil {
		t.Errorf("Exists(YPC-999999) = %v, %v", ok, err)
	}

	members, err := w.Members(ctx, "project: YPC", append(ids, "YPC-999999"))
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range ids {
		if !members[id] {
			t.Errorf("Members: %s missing", id)
		}
	}
	if members["YPC-999999"] {
		t.Error("Members: YPC-999999 present")
	}

	key := issues[0].Key
	for _, tc := range []struct {
		command string
		state   CommandState
	}{
		{"State In Progress", Valid},
		{"State", Incomplete},
		{"State Bogus", Invalid},
	} {
		check, err := w.Validate(ctx, tc.command, key)
		if err != nil || check.State != tc.state {
			t.Errorf("Validate(%q) = %+v, %v; want %s", tc.command, check, err, tc.state)
		}
	}

	var ce *CommandError
	if err := w.Apply(ctx, "State Bogus", key); !errors.As(err, &ce) || ce.Description == "" {
		t.Errorf("Apply(State Bogus) = %v, want *CommandError", err)
	}

	// The research comments on YPC were written on 2026-10-27 after 13:00 UTC.
	from := timeCursor(time.Date(2026, 10, 27, 13, 0, 0, 0, time.UTC))
	items, next, err := w.Comments(ctx, []string{"YPC"}, from)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) == 0 || !usable(next) || next == from {
		t.Errorf("Comments from %s: %d items, next %q", from, len(items), next)
	}
	for _, c := range items {
		if !strings.HasPrefix(c.IssueKey, "YPC-") || c.ID == "" || c.Created.IsZero() {
			t.Errorf("comment %+v", c)
		}
		if c.Deleted && c.Text != "" {
			t.Errorf("deleted comment with text: %+v", c)
		}
	}
	again, next2, err := w.Comments(ctx, []string{"YPC"}, next)
	if err != nil || !usable(next2) {
		t.Errorf("Comments from %s: %d items, next %q, %v", next, len(again), next2, err)
	}

	boot, err := w.BootstrapCursor(ctx)
	if err != nil {
		t.Fatal(err)
	}
	_, next3, err := w.Comments(ctx, []string{"YPC"}, boot)
	if err != nil || !usable(next3) {
		t.Errorf("Comments from bootstrap %s: next %q, %v", boot, next3, err)
	}
	if _, _, err := w.Comments(ctx, []string{"YPC"}, "garbage"); !errors.Is(err, ErrBadCursor) {
		t.Errorf("garbage cursor: %v, want ErrBadCursor", err)
	}
}
