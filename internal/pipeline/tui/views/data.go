package views

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/xlsft/ymcp/internal/config"
	"github.com/xlsft/ymcp/internal/pipeline/spec"
	"github.com/xlsft/ymcp/internal/pipeline/store"
	"github.com/xlsft/ymcp/internal/pipeline/tui/theme"
	"github.com/xlsft/ymcp/internal/pipeline/ytwatch"
)

// loadTimeout bounds one reload of the state database.
const loadTimeout = 5 * time.Second

// runnerState is what a pipeline's runner is doing.
type runnerState int

const (
	runnerStopped runnerState = iota
	runnerRunning
	runnerSilent // holds the lock but its heartbeat is stale
)

type runnerInfo struct {
	state runnerState
	pid   int
	host  string
	beat  time.Time
}

func (d *Deps) runner(ctx context.Context, name string) (runnerInfo, error) {
	pid, host, beat, ok, err := d.Store.Runner(ctx, name)
	if err != nil || !ok {
		return runnerInfo{}, err
	}
	r := runnerInfo{state: runnerRunning, pid: pid, host: host, beat: beat}
	if d.Now().Sub(beat) > d.RunnerStale {
		r.state = runnerSilent
	}
	return r, nil
}

// label is the runner as a short coloured word.
func (r runnerInfo) label() string {
	switch r.state {
	case runnerRunning:
		return theme.OK.Render("●") + " running"
	case runnerSilent:
		return theme.Err.Render("●") + " not responding"
	}
	return theme.Dim.Render("○ stopped")
}

// counts are a pipeline's runs per board column.
type counts struct {
	working, input, review, done, failed int
}

func (c *counts) add(r store.Run) {
	switch r.Status {
	case store.StatusQueued, store.StatusWorking:
		c.working++
	case store.StatusNeedsInput:
		c.input++
	case store.StatusReview:
		c.review++
	case store.StatusDone, store.StatusClosed:
		c.done++
	case store.StatusFailed:
		c.failed++
	}
}

// summary is "2 working · 1 needs input · 3 review · 12 done", without the
// zeros, or "idle".
func (c counts) summary() string {
	var parts []string
	for _, p := range []struct {
		n    int
		what string
	}{{c.working, "working"}, {c.input, "needs input"}, {c.review, "review"}, {c.done, "done"}, {c.failed, "failed"}} {
		if p.n > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", p.n, p.what))
		}
	}
	if len(parts) == 0 {
		return "no runs yet"
	}
	return strings.Join(parts, " · ")
}

// inputBadge is the yellow "⚑ 2 need input" flag, or "".
func inputBadge(n int) string {
	switch n {
	case 0:
		return ""
	case 1:
		return theme.Badge.Render("⚑ 1 needs input")
	}
	return theme.Badge.Render(fmt.Sprintf("⚑ %d need input", n))
}

// pipelineRow is a pipeline with its runner and run counts.
type pipelineRow struct {
	p      spec.Pipeline
	runner runnerInfo
	c      counts
}

// loadPipelines reads every pipeline with its runner and counts. A
// pipeline file that cannot be read is skipped by spec.List and reported
// as err along with the rest.
func (d *Deps) loadPipelines(ctx context.Context) ([]pipelineRow, error) {
	list, listErr := d.Pipelines()
	runs, err := d.Store.Runs(ctx, "", false)
	if err != nil {
		return nil, err
	}
	byName := map[string]*counts{}
	for _, r := range runs {
		c := byName[r.Pipeline]
		if c == nil {
			c = &counts{}
			byName[r.Pipeline] = c
		}
		c.add(r)
	}
	rows := make([]pipelineRow, 0, len(list))
	for _, p := range list {
		row := pipelineRow{p: p}
		if c := byName[p.Name]; c != nil {
			row.c = *c
		}
		if row.runner, err = d.runner(ctx, p.Name); err != nil {
			return nil, err
		}
		rows = append(rows, row)
	}
	return rows, listErr
}

// profileURL is the YouTrack address of a profile, or "".
func profileURL(profiles []config.Config, name string) string {
	for _, c := range profiles {
		if c.Name == name {
			return c.URL
		}
	}
	return ""
}

// issueURL links an issue of a profile, or is "" when the profile has no
// address.
func issueURL(profiles []config.Config, profile, key string) string {
	base := profileURL(profiles, profile)
	if base == "" || key == "" {
		return ""
	}
	return ytwatch.IssueURL(base, key)
}

// runInfo is what a card shows beyond the run itself; it is built from the
// run's events and kept between reloads.
type runInfo struct {
	lastEvent int64
	summary   string
	activity  string // what the agent did last: a tool title, "thinking", …
	turnStart time.Time
	inTurn    bool
	pending   int  // inbox items not yet given to the agent
	final     bool // read after the run ended: nothing changes any more
}

// follow reads the events of a run that arrived since the last call and
// its pending inbox, and returns the new events.
func (d *Deps) follow(ctx context.Context, r store.Run, info *runInfo) ([]store.Event, error) {
	events, err := d.Store.Events(ctx, r.ID, info.lastEvent, 0)
	if err != nil {
		return nil, err
	}
	info.apply(events)
	info.final = r.Status.Final()
	if info.final {
		info.pending, info.inTurn = 0, false
		return events, nil
	}
	items, err := d.Store.PendingInbox(ctx, r.ID)
	if err != nil {
		return nil, err
	}
	info.pending = len(items)
	return events, nil
}

// apply updates the info with new events.
func (info *runInfo) apply(events []store.Event) {
	for _, e := range events {
		info.lastEvent = e.ID
		switch e.Kind {
		case "run.captured":
			if s := fields(e.Data)["summary"]; s != "" {
				info.summary = s
			}
		case "turn.start":
			info.inTurn, info.turnStart, info.activity = true, e.At, "starting"
		case "turn.end":
			info.inTurn, info.activity = false, ""
		case "agent.thought":
			info.activity = "thinking"
		case "agent.text":
			info.activity = "writing"
		case "agent.tool_call":
			if t := toolTitle(e.Data); t != "" {
				info.activity = t
			}
		case "agent.permission":
			info.activity = "asking permission"
		}
	}
}

// fields reads the string fields of an event's data.
func fields(data json.RawMessage) map[string]string {
	var m map[string]any
	if json.Unmarshal(data, &m) != nil {
		return nil
	}
	out := make(map[string]string, len(m))
	for k, v := range m {
		if s, ok := v.(string); ok {
			out[k] = s
		}
	}
	return out
}

func toolTitle(data json.RawMessage) string {
	var v struct {
		Tool struct{ Title, Status string } `json:"tool"`
	}
	if json.Unmarshal(data, &v) != nil {
		return ""
	}
	return oneLine(v.Tool.Title)
}

// statusLabel names a run status for people.
func statusLabel(s store.RunStatus) string {
	switch s {
	case store.StatusNeedsInput:
		return "needs input"
	case store.StatusCanceled:
		return "canceled"
	}
	return string(s)
}

// statusStyle renders a run status in its colour.
func statusStyle(s store.RunStatus) string {
	label := strings.ToUpper(statusLabel(s))
	switch s {
	case store.StatusNeedsInput:
		return theme.Badge.Render("⚑ " + label)
	case store.StatusDone, store.StatusClosed, store.StatusReview:
		return theme.OK.Render(label)
	case store.StatusFailed:
		return theme.Err.Render(label)
	case store.StatusCanceled:
		return theme.Dim.Render(label)
	}
	return theme.Bold.Render(label)
}

// waitingSince is when a run started waiting: its last update.
func waitingSince(r store.Run) time.Time { return r.UpdatedAt }

// homeShort shortens a path under the home directory to ~/….
func homeShort(path, home string) string {
	if home != "" && strings.HasPrefix(path, home) {
		return "~" + strings.TrimPrefix(path, home)
	}
	return path
}
