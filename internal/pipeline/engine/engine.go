// Package engine runs a ymcp pipeline: it watches YouTrack for issues that
// match the pipeline's source, gives each one a run with its own git
// worktree and agent session, publishes the agent's answers as YouTrack
// comments, feeds human replies back to the agent and moves the issue
// through the pipeline's stages.
//
// One Runner drives one pipeline. Its polling loop is sequential: it reads
// the source, checks stage membership of active runs, reads new comments
// and schedules agent turns. Each run has a worker goroutine that runs at
// most one agent turn at a time. Every state change is written to the
// store before it is acted on, so a restarted runner picks up where the
// previous one stopped.
//
// Answer and Cancel let other processes (the CLI, the TUI) talk to a
// running runner through the store; Check verifies a pipeline without
// running it.
package engine

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/xlsft/ymcp/internal/config"
	"github.com/xlsft/ymcp/internal/pipeline/agent"
	"github.com/xlsft/ymcp/internal/pipeline/mcpserver"
	"github.com/xlsft/ymcp/internal/pipeline/spec"
	"github.com/xlsft/ymcp/internal/pipeline/store"
	"github.com/xlsft/ymcp/internal/pipeline/text"
	"github.com/xlsft/ymcp/internal/pipeline/workspace"
	"github.com/xlsft/ymcp/internal/pipeline/ytwatch"
	"github.com/xlsft/ymcp/internal/tools"
	"github.com/xlsft/ymcp/internal/youtrack"
)

// Options configure a Runner. Pipeline and Store are required; the
// function and interface fields are test seams and default to the real
// thing when nil.
type Options struct {
	Pipeline spec.Pipeline
	Store    *store.Store
	Logger   *slog.Logger
	Version  string

	// Connect builds the YouTrack client of a profile and returns it with
	// the profile's config (its Name is the MCP path, its Tools the
	// enabled ymcp tools). Default: config.Load and youtrack.New.
	Connect func(ctx context.Context, profile string, logger *slog.Logger) (*youtrack.Client, config.Config, error)
	// NewAgent builds the pipeline's agent. Default: agent.New.
	NewAgent func(a spec.Agent, logger *slog.Logger) (agent.Agent, error)
	// Host runs git. Default: workspace.Local (the only host of phase 1).
	Host workspace.Host
	// Now is the clock used for debouncing. Default: time.Now.
	Now func() time.Time
}

// Intervals of the runner lock.
const (
	heartbeatEvery = 10 * time.Second
	staleAfter     = 30 * time.Second
)

// Runner runs one pipeline. Make it with New and start it with Run.
type Runner struct {
	p       spec.Pipeline
	prompts spec.Prompts
	st      *store.Store
	log     *slog.Logger
	version string
	yt      *youtrack.Client
	watch   *ytwatch.Watcher
	profile string // the resolved profile name; the MCP path
	me      string // login of the YouTrack account
	enabled map[string]bool
	agent   agent.Agent
	host    workspace.Host
	now     func() time.Time

	mcp    *mcpserver.Server
	mcpURL string // http://127.0.0.1:P/<profile>

	mu      sync.Mutex
	workers map[int64]*worker
	started bool
	wg      sync.WaitGroup

	// Touched only by the polling loop.
	suppressed map[string]bool // issue ids not captured again while they stay in the source
}

// New prepares a runner: it loads the profile, signs in to YouTrack once
// and builds the watcher and the agent. It checks nothing else; see Check.
func New(ctx context.Context, opts Options) (*Runner, error) {
	if opts.Store == nil {
		return nil, errors.New("engine: a store is required")
	}
	p := opts.Pipeline.WithDefaults()
	if err := p.Validate(); err != nil {
		return nil, err
	}
	if p.Strategy.Kind != "branch" {
		return nil, fmt.Errorf("pipeline %s: strategy %q is not supported yet; use \"branch\"", p.Name, p.Strategy.Kind)
	}
	logger := opts.Logger
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	logger = logger.With("pipeline", p.Name)
	opts = withDefaults(opts)
	host, err := hostFor(p, opts)
	if err != nil {
		return nil, err
	}

	yt, cfg, err := opts.Connect(ctx, p.Profile, logger)
	if err != nil {
		return nil, err
	}
	enabled, err := tools.Select(cfg.Tools)
	if err != nil {
		return nil, fmt.Errorf("profile %s: %w", p.Profile, err)
	}
	w := ytwatch.New(yt)
	me, err := w.Me(ctx)
	if err != nil {
		return nil, fmt.Errorf("signing in to YouTrack (profile %s): %w", p.Profile, err)
	}
	ag, err := opts.NewAgent(p.Agent, logger)
	if err != nil {
		return nil, err
	}
	profile := cfg.Name
	if profile == "" {
		profile = p.Profile
	}
	return &Runner{
		p:          p,
		prompts:    p.ResolvedPrompts(),
		st:         opts.Store,
		log:        logger,
		version:    opts.Version,
		yt:         yt,
		watch:      w,
		profile:    profile,
		me:         me,
		enabled:    enabled,
		agent:      ag,
		host:       host,
		now:        opts.Now,
		workers:    map[int64]*worker{},
		suppressed: map[string]bool{},
	}, nil
}

func withDefaults(o Options) Options {
	if o.Connect == nil {
		o.Connect = connect
	}
	if o.NewAgent == nil {
		o.NewAgent = newAgent
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	return o
}

func hostFor(p spec.Pipeline, o Options) (workspace.Host, error) {
	if o.Host != nil {
		return o.Host, nil
	}
	if h := strings.TrimSpace(p.Workspace.Host); h != "" && h != "local" {
		return nil, fmt.Errorf("pipeline %s: workspace host %q: ssh hosts are not supported yet", p.Name, h)
	}
	return workspace.Local(), nil
}

// connect is the real Options.Connect.
func connect(_ context.Context, profile string, logger *slog.Logger) (*youtrack.Client, config.Config, error) {
	cfg, err := config.Load(profile)
	if err != nil {
		return nil, cfg, err
	}
	if !cfg.Complete() {
		return nil, cfg, fmt.Errorf("YouTrack profile %q is not configured; run `ymcp setup` (or set YOUTRACK_URL, YOUTRACK_LOGIN, YOUTRACK_PASSWORD)", cfg.Name)
	}
	yt, err := youtrack.New(youtrack.Options{BaseURL: cfg.URL, Login: cfg.Login, Password: cfg.Password, Proxy: cfg.Proxy, Logger: logger})
	return yt, cfg, err
}

// newAgent is the real Options.NewAgent.
func newAgent(a spec.Agent, logger *slog.Logger) (agent.Agent, error) {
	return agent.New(a.Provider, a.Command, a.Args, agent.WithMode(a.Mode), agent.WithModel(a.Model), agent.WithLogger(logger))
}

// Run runs the pipeline until ctx ends. It takes the pipeline's runner lock
// (store.ErrRunnerBusy when a live runner holds it), sends heartbeats,
// serves the agents' MCP endpoint, resumes the active runs of the store and
// polls YouTrack every Source.PollInterval. On return the agents are
// stopped and the lock is released. It returns nil when ctx ended, and an
// error when the runner could not start or lost its lock.
func (r *Runner) Run(ctx context.Context) error {
	r.mu.Lock()
	if r.started {
		r.mu.Unlock()
		return errors.New("engine: the runner was already run")
	}
	r.started = true
	r.mu.Unlock()

	pid := os.Getpid()
	hostname, _ := os.Hostname()
	if err := r.st.AcquireRunner(ctx, r.p.Name, pid, hostname, staleAfter); err != nil {
		return err
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := r.st.ReleaseRunner(ctx, r.p.Name, pid); err != nil {
			r.log.Warn("releasing the runner lock failed", "err", err)
		}
	}()

	r.mcp = mcpserver.New(r.yt, r.profile, r.enabled, r.decorate, callbacks{r}, r.log)
	base, err := r.mcp.Start("127.0.0.1:0")
	if err != nil {
		return err
	}
	r.mcpURL = base + "/" + r.profile
	defer r.mcp.Close()

	runCtx, stop := context.WithCancelCause(ctx)
	defer stop(nil)
	defer r.stopWorkers()

	if err := r.bootstrapCursor(runCtx); err != nil {
		return err
	}
	if err := r.resume(runCtx); err != nil {
		return err
	}
	r.log.Info("pipeline runner started", "version", r.version, "profile", r.profile, "as", r.me, "poll", r.p.Source.PollInterval.Duration)

	beating := make(chan struct{})
	go func() {
		defer close(beating)
		r.heartbeat(runCtx, pid, stop)
	}()
	defer func() {
		stop(nil)
		<-beating
	}()

	poll := time.NewTicker(r.p.Source.PollInterval.Duration)
	defer poll.Stop()
	for {
		r.tick(runCtx)
		select {
		case <-runCtx.Done():
			if cause := context.Cause(runCtx); ctx.Err() == nil && cause != nil {
				return cause
			}
			r.log.Info("pipeline runner stopping")
			return nil
		case <-poll.C:
		}
	}
}

// heartbeat keeps the runner lock fresh and stops the runner when it is
// lost.
func (r *Runner) heartbeat(ctx context.Context, pid int, stop context.CancelCauseFunc) {
	t := time.NewTicker(heartbeatEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		err := r.st.Heartbeat(ctx, r.p.Name, pid)
		switch {
		case errors.Is(err, store.ErrRunnerLost):
			r.log.Error("another runner took over the pipeline; stopping")
			stop(fmt.Errorf("pipeline %s: %w", r.p.Name, err))
			return
		case err != nil && ctx.Err() == nil:
			r.log.Warn("heartbeat failed", "err", err)
		}
	}
}

// bootstrapCursor starts the comment feed at YouTrack's current time when
// the store has no cursor yet. A failure is retried by the first poll.
func (r *Runner) bootstrapCursor(ctx context.Context) error {
	cur, err := r.st.Cursor(ctx, r.p.Name, cursorSource, cursorKey)
	if err != nil || cur != "" {
		return err
	}
	cur, err = r.watch.BootstrapCursor(ctx)
	if err != nil {
		r.log.Warn("reading the YouTrack clock failed", "err", err)
		return nil
	}
	return r.st.SetCursor(ctx, r.p.Name, cursorSource, cursorKey, cur)
}

// resume starts a worker for every active run of the store. A run that
// was queued when the last runner stopped is captured again.
func (r *Runner) resume(ctx context.Context) error {
	runs, err := r.st.Runs(ctx, r.p.Name, true)
	if err != nil {
		return err
	}
	for _, run := range runs {
		if run.Status == store.StatusQueued {
			r.capture(ctx, run, run.IssueKey)
			continue
		}
		w := r.startWorker(run)
		w.leftSource = run.Status == store.StatusReview
	}
	// Issues whose last run was canceled are not captured again until they
	// leave the source (a human moved them back to the backlog to stop).
	all, err := r.st.Runs(ctx, r.p.Name, false)
	if err != nil {
		return err
	}
	last := map[string]store.RunStatus{}
	for _, run := range all {
		last[run.IssueID] = run.Status
	}
	for id, status := range last {
		if status == store.StatusCanceled {
			r.suppressed[id] = true
		}
	}
	return nil
}

// decorate marks text the agent writes to YouTrack through the MCP tools.
func (r *Runner) decorate(ri mcpserver.RunInfo, body string) string {
	return text.Decorate(body, ri.Mention, text.Mark{Pipeline: ri.Pipeline, Run: ri.Number, Msg: text.NewMsgID()})
}

// mention is how the agent's messages name the user it works for.
func (r *Runner) mention() string { return "@" + r.me }

// worker returns the worker of a run, or nil.
func (r *Runner) worker(runID int64) *worker {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.workers[runID]
}

// liveWorkers lists the current workers.
func (r *Runner) liveWorkers() []*worker {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]*worker, 0, len(r.workers))
	for _, w := range r.workers {
		out = append(out, w)
	}
	return out
}

// stopWorkers stops every worker and waits for them.
func (r *Runner) stopWorkers() {
	for _, w := range r.liveWorkers() {
		w.stop(errShutdown)
	}
	r.wg.Wait()
}

// event adds an entry to a run's timeline, logging a failure.
func (r *Runner) event(ctx context.Context, runID int64, kind string, data any) {
	if err := r.st.AddEvent(context.WithoutCancel(ctx), runID, kind, data); err != nil {
		r.log.Warn("recording an event failed", "kind", kind, "err", err)
	}
}

// errFinal is returned by updateRun for a run that already ended.
var errFinal = errors.New("the run has ended")

// updateRun reads the run, applies fn and saves it in one transaction. It
// returns errFinal, without calling fn, when the run has a final status,
// so a run canceled from the CLI is never revived by a stale copy.
func (r *Runner) updateRun(ctx context.Context, id int64, fn func(run *store.Run)) (store.Run, error) {
	ctx = context.WithoutCancel(ctx)
	var out store.Run
	err := r.st.Tx(ctx, func(tx *store.Store) error {
		run, err := tx.Run(ctx, id)
		if err != nil {
			return err
		}
		if run.Status.Final() {
			out = run
			return errFinal
		}
		fn(&run)
		if err := tx.UpdateRun(ctx, run); err != nil {
			return err
		}
		out, err = tx.Run(ctx, id)
		return err
	})
	return out, err
}
