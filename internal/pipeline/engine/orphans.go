package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/xlsft/ymcp/internal/pipeline/store"
	"github.com/xlsft/ymcp/internal/pipeline/workspace"
)

// stateAgentGroup is the run state key of the process group of the run's
// running agent (agentGroup as JSON). It is written when the agent starts
// and cleared when its session is closed, so one left behind means the
// runner died while the agent ran: the agent may still be running, without
// anyone to stop it, and change the worktree under the next turn.
const stateAgentGroup = "agent_group"

// agentGroup is the process group of an agent, where it runs.
type agentGroup struct {
	PGID int    `json:"pgid"`
	Host string `json:"host"` // the workspace host: "local" or an ssh alias
	// Machine is this machine's name; a "local" group elsewhere is out of
	// reach.
	Machine string `json:"machine"`
	// At is when the group started (unix ms), to tell it from a later
	// group that got the same id.
	At int64 `json:"at"`
}

// recordGroup remembers the process group of a run's agent.
func (r *Runner) recordGroup(ctx context.Context, run store.Run, pgid int) {
	machine, _ := os.Hostname()
	b, _ := json.Marshal(agentGroup{PGID: pgid, Host: r.host.Name(), Machine: machine, At: time.Now().UnixMilli()})
	if err := r.st.SetRunState(context.WithoutCancel(ctx), run.ID, stateAgentGroup, string(b)); err != nil {
		r.log.Warn("recording the agent's process group failed", "run", run.Number, "err", err)
	}
}

// forgetGroup clears the record of a run's agent, whose session ended.
func (r *Runner) forgetGroup(ctx context.Context, run store.Run) {
	if err := r.st.SetRunState(context.WithoutCancel(ctx), run.ID, stateAgentGroup, ""); err != nil {
		r.log.Warn("clearing the agent's process group failed", "run", run.Number, "err", err)
	}
}

// groupMatch bounds how far the age of a recorded group's leader may be
// from the age of the record for the group to count as the recorded one.
const groupMatch = 30 * time.Second

// reapOrphans kills the agents a runner that died left running: every run
// of the pipeline with a recorded process group is checked, and a group
// that is still there, on this machine or on the workspace host, is
// killed. It runs before any run is resumed.
func (r *Runner) reapOrphans(ctx context.Context) {
	runs, err := r.st.Runs(ctx, r.p.Name, false)
	if err != nil {
		r.log.Warn("listing runs for orphaned agents failed", "err", err)
		return
	}
	machine, _ := os.Hostname()
	for _, run := range runs {
		v, ok, err := r.st.RunState(ctx, run.ID, stateAgentGroup)
		if err != nil || !ok || v == "" {
			continue
		}
		var g agentGroup
		if err := json.Unmarshal([]byte(v), &g); err != nil || g.PGID <= 1 {
			r.forgetGroup(ctx, run)
			continue
		}
		switch {
		case g.Host != r.host.Name() || (!isRemote(r.host) && g.Machine != machine):
			r.log.Warn("an agent of an earlier runner may still run where this runner can't reach it", "run", run.Number, "pgid", g.PGID, "host", g.Host, "machine", g.Machine)
		case !isRemote(r.host) && runtime.GOOS == "windows":
			// The agent's job object ended with the runner that held it.
		default:
			r.reap(ctx, run, g)
		}
		r.forgetGroup(ctx, run)
	}
}

// reap kills the recorded group of a run if it is still there.
func (r *Runner) reap(ctx context.Context, run store.Run, g agentGroup) {
	alive, err := groupAlive(ctx, r.host, g, time.Now())
	if err != nil {
		r.log.Warn("checking for an orphaned agent failed; not killing it", "run", run.Number, "pgid", g.PGID, "host", g.Host, "err", err)
		return
	}
	if !alive {
		r.log.Debug("the agent of an earlier runner is gone (its group is empty or the id is another process's now)", "run", run.Number, "pgid", g.PGID, "host", g.Host)
		return
	}
	pg := strconv.Itoa(g.PGID)
	if _, stderr, err := r.host.Run(ctx, "", []string{"sh", "-c", `kill -s KILL -- "-$1"`, "sh", pg}, nil); err != nil {
		r.log.Warn("killing an orphaned agent failed", "run", run.Number, "pgid", g.PGID, "host", g.Host, "err", err, "stderr", strings.TrimSpace(string(stderr)))
		return
	}
	r.log.Info("killed an agent an earlier runner left running", "run", run.Number, "issue", run.IssueKey, "pgid", g.PGID, "host", g.Host)
	r.event(ctx, run.ID, "agent.orphan_killed", map[string]any{"pgid": g.PGID, "host": g.Host})
}

// groupAlive reports whether the recorded group still has processes and
// is the one recorded: a group whose leader is younger or older than the
// record by more than groupMatch got the id later. A group whose leader is
// gone can't have been reused: its id stays taken while it has members.
func groupAlive(ctx context.Context, h workspace.Host, g agentGroup, now time.Time) (bool, error) {
	out, stderr, err := h.Run(ctx, "", []string{"ps", "-A", "-o", "pid=", "-o", "pgid=", "-o", "etime="}, nil)
	if err != nil {
		return false, fmt.Errorf("ps: %w: %s", err, strings.TrimSpace(string(stderr)))
	}
	members, leader := 0, time.Duration(-1)
	for _, line := range strings.Split(string(out), "\n") {
		f := strings.Fields(line)
		if len(f) != 3 {
			continue
		}
		pid, err1 := strconv.Atoi(f[0])
		pgid, err2 := strconv.Atoi(f[1])
		if err1 != nil || err2 != nil || pgid != g.PGID {
			continue
		}
		members++
		if pid == g.PGID {
			if leader, err = parseEtime(f[2]); err != nil {
				return false, err
			}
		}
	}
	if members == 0 {
		return false, nil
	}
	if leader < 0 {
		return true, nil
	}
	age := now.Sub(time.UnixMilli(g.At))
	if d := age - leader; d > groupMatch || d < -groupMatch {
		return false, nil
	}
	return true, nil
}

// parseEtime reads the elapsed time ps prints: [[dd-]hh:]mm:ss.
func parseEtime(s string) (time.Duration, error) {
	bad := fmt.Errorf("ps: unexpected elapsed time %q", s)
	days := 0
	if d, rest, ok := strings.Cut(s, "-"); ok {
		n, err := strconv.Atoi(d)
		if err != nil {
			return 0, bad
		}
		days, s = n, rest
	}
	parts := strings.Split(s, ":")
	if len(parts) < 2 || len(parts) > 3 {
		return 0, bad
	}
	total := 0
	for _, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 {
			return 0, bad
		}
		total = total*60 + n
	}
	return time.Duration(days)*24*time.Hour + time.Duration(total)*time.Second, nil
}
