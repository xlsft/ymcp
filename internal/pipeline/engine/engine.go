// Package engine runs a ymcp pipeline: it watches YouTrack for issues that
// match the pipeline's source, gives each one a run with its own git
// worktree and agent session, publishes the agent's answers as YouTrack
// comments, feeds human replies back to the agent and moves the issue
// through the pipeline's stages. With a pull or merge request strategy it
// opens the change after the first push, reads the feedback left on it,
// answers there and ends the run when the change is merged or closed.
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
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/xlsft/ymcp/internal/config"
	"github.com/xlsft/ymcp/internal/pipeline/agent"
	"github.com/xlsft/ymcp/internal/pipeline/mcpserver"
	"github.com/xlsft/ymcp/internal/pipeline/spec"
	"github.com/xlsft/ymcp/internal/pipeline/store"
	"github.com/xlsft/ymcp/internal/pipeline/text"
	"github.com/xlsft/ymcp/internal/pipeline/vcs"
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
	// NewAgent builds the pipeline's agent, which runs on host. Default:
	// agent.New, with agent.WithHost for an ssh host.
	NewAgent func(a spec.Agent, host workspace.Host, logger *slog.Logger) (agent.Agent, error)
	// Host runs git and the agent. Default: workspace.Local, or
	// workspace.SSH of workspace.host. A host given here is not closed.
	Host workspace.Host
	// Now is the clock used for debouncing. Default: time.Now.
	Now func() time.Time
	// NewStrategy builds the strategy of a pipeline that opens pull or
	// merge requests, for the repository at originURL (the workspace's
	// origin remote). On failure the Remote holds what was found before
	// it. Default: vcs.ParseRemote, vcs.TokenFor and vcs.New.
	NewStrategy func(ctx context.Context, kind vcs.Kind, originURL string) (Remote, error)
	// NewCI builds the CI of the repository at originURL for the branch
	// strategy; the other strategies read their own. Default:
	// vcs.ParseRemote, vcs.TokenFor and vcs.NewCI.
	NewCI func(ctx context.Context, originURL string) (vcs.CI, error)
	// Notify, when set, is told about moments a human may want to know
	// of: kind is one of the Notify* constants, url the page to open
	// (the question, the pull request or the issue). It is called from
	// the runner's goroutines and must not block for long.
	Notify func(ctx context.Context, kind string, run store.Run, title, body, url string)
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
	closeFn func() // closes a host the runner made
	now     func() time.Time

	mcp    *mcpserver.Server
	mcpURL string // http://127.0.0.1:P/<profile>
	// fwd makes the MCP server reachable on a remote host; the agent's
	// URL then has the port on that host.
	fwd *workspace.SSHForward

	// The code host of a strategy with pull or merge requests; vcs is nil
	// for the branch strategy.
	vcs       vcs.Strategy
	vcsMe     string // login on the code host
	vcsRepo   string // host/owner/name
	vcsSource string // sourceGitHub or sourceGitLab
	// ci is the CI watched after each push; nil when ci.watch is off or
	// the host's CI can't be read.
	ci       vcs.CI
	notifyFn func(ctx context.Context, kind string, run store.Run, title, body, url string)

	mu         sync.Mutex
	workers    map[int64]*worker
	compacting map[int64]bool // runs whose summary is being written
	started    bool
	wg         sync.WaitGroup

	// Touched only by the polling loop.
	suppressed map[string]bool     // issue ids not captured again while they stay in the source
	held       map[string]bool     // issue ids of the source another pipeline's run has, logged once
	vcsWait    map[int64]time.Time // runs whose change is not polled until then (rate limits)
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
	logger := opts.Logger
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	logger = logger.With("pipeline", p.Name)
	opts = withDefaults(opts)
	host, closeHost, err := hostFor(p, opts)
	if err != nil {
		return nil, err
	}
	ok := false
	defer func() {
		if !ok {
			closeHost()
		}
	}()
	if strings.TrimSpace(p.Workspace.Worktrees) == "" {
		root, err := workspace.DefaultRoot(ctx, host, p.Name)
		if err != nil {
			return nil, fmt.Errorf("pipeline %s: %w", p.Name, err)
		}
		p.Workspace.Worktrees = root
	} else {
		root, err := workspace.ExpandRoot(ctx, host, strings.TrimSpace(p.Workspace.Worktrees))
		if err != nil {
			return nil, fmt.Errorf("pipeline %s: workspace.worktrees: %w", p.Name, err)
		}
		p.Workspace.Worktrees = root
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
	var rem Remote
	var vcsMe string
	if needsChange(p.Strategy.Kind) {
		if rem, vcsMe, err = connectVCS(ctx, p, host, opts); err != nil {
			return nil, err
		}
		logger.Info("code host", "repo", repoName(rem.Repo), "strategy", p.Strategy.Kind, "as", vcsMe, "token", rem.TokenSource)
	}
	ci := connectCI(ctx, p, host, rem.Strategy, opts, logger)
	ag, err := opts.NewAgent(p.Agent, host, logger)
	if err != nil {
		return nil, err
	}
	profile := cfg.Name
	if profile == "" {
		profile = p.Profile
	}
	ok = true
	return &Runner{
		closeFn:    closeHost,
		vcs:        rem.Strategy,
		vcsMe:      vcsMe,
		vcsRepo:    repoName(rem.Repo),
		vcsSource:  sourceOf(p.Strategy.Kind),
		ci:         ci,
		notifyFn:   opts.Notify,
		vcsWait:    map[int64]time.Time{},
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
		compacting: map[int64]bool{},
		suppressed: map[string]bool{},
		held:       map[string]bool{},
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
	if o.NewStrategy == nil {
		o.NewStrategy = newStrategy
	}
	if o.NewCI == nil {
		o.NewCI = newCI
	}
	return o
}

// hostFor returns the host of the pipeline's workspace and a function that
// closes it (a no-op for Options.Host and this machine).
func hostFor(p spec.Pipeline, o Options) (workspace.Host, func(), error) {
	if o.Host != nil {
		return o.Host, func() {}, nil
	}
	if h := strings.TrimSpace(p.Workspace.Host); h != "" && h != "local" {
		if strings.HasPrefix(h, "-") {
			return nil, func() {}, fmt.Errorf("pipeline %s: workspace host %q is not an ssh host alias", p.Name, h)
		}
		s := workspace.SSH(h)
		return s, func() { s.Close() }, nil
	}
	return workspace.Local(), func() {}, nil
}

// isRemote reports whether h is another machine.
func isRemote(h workspace.Host) bool { return h.Name() != "local" }

// remoteHost is a host on another machine, reached with ssh.
type remoteHost interface {
	workspace.Host
	Forward(ctx context.Context, local int) (*workspace.SSHForward, error)
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
func newAgent(a spec.Agent, host workspace.Host, logger *slog.Logger) (agent.Agent, error) {
	opts := []agent.Setting{agent.WithMode(a.Mode), agent.WithModel(a.Model), agent.WithLogger(logger)}
	if host != nil && isRemote(host) {
		h, ok := host.(agent.Host)
		if !ok {
			return nil, fmt.Errorf("the agent can't run on host %s", host.Name())
		}
		opts = append(opts, agent.WithHost(h))
	}
	return agent.New(a.Provider, a.Command, a.Args, opts...)
}

// Run runs the pipeline until ctx ends. It takes the pipeline's runner lock
// (store.ErrRunnerBusy when a live runner holds it), sends heartbeats,
// serves the agents' MCP endpoint, resumes the active runs of the store and
// polls YouTrack every Source.PollInterval. On return the agents are
// stopped and the lock is released. It returns nil when ctx ended or a stop
// was requested (see RequestStop), and an error when the runner could not
// start or lost its lock.
func (r *Runner) Run(ctx context.Context) error {
	r.mu.Lock()
	if r.started {
		r.mu.Unlock()
		return errors.New("engine: the runner was already run")
	}
	r.started = true
	r.mu.Unlock()
	// Last: stopping workers may still commit and push on the host.
	defer r.closeFn()

	pid := os.Getpid()
	hostname, _ := os.Hostname()
	startedAt := r.now() // stop requests older than this are not ours
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
	if err := r.forwardMCP(ctx, base); err != nil {
		return err
	}

	runCtx, stop := context.WithCancelCause(ctx)
	defer stop(nil)
	defer r.stopWorkers()

	// Being stopped while still starting is a stop, not a failure.
	if err := r.bootstrapCursor(runCtx); err != nil {
		if ctx.Err() != nil {
			return nil
		}
		return err
	}
	r.reapOrphans(runCtx)
	if err := r.resume(runCtx); err != nil {
		if ctx.Err() != nil {
			return nil
		}
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

	watching := make(chan struct{})
	go func() {
		defer close(watching)
		r.watchStop(runCtx, startedAt, stop)
	}()
	defer func() {
		stop(nil)
		<-watching
	}()

	poll := time.NewTicker(r.p.Source.PollInterval.Duration)
	defer poll.Stop()
	for {
		r.tick(runCtx)
		select {
		case <-runCtx.Done():
			if cause := context.Cause(runCtx); ctx.Err() == nil && cause != nil && !errors.Is(cause, errStopRequested) {
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
	// Issues whose last run was canceled by moving them back to the
	// backlog are not captured again until they leave the source. A run
	// canceled otherwise (/cancel, the CLI) doesn't hold its issue: a
	// human who puts it in the source again wants a new run.
	all, err := r.st.Runs(ctx, r.p.Name, false)
	if err != nil {
		return err
	}
	last := map[string]store.Run{}
	for _, run := range all {
		last[run.IssueID] = run
	}
	for id, run := range last {
		if backlogCanceled(run) {
			r.suppressed[id] = true
		}
	}
	// Runs canceled while no runner ran, or while the last one stopped.
	for _, run := range all {
		if run.Status == store.StatusCanceled {
			r.cleanupCanceled(ctx, run.ID)
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

// forwardMCP makes the MCP server at base (http://127.0.0.1:P) reachable
// on a remote host. The forward lives until the host is closed.
func (r *Runner) forwardMCP(ctx context.Context, base string) error {
	rh, ok := r.host.(remoteHost)
	if !ok || !isRemote(r.host) {
		return nil
	}
	u, err := url.Parse(base)
	if err != nil {
		return err
	}
	port, err := strconv.Atoi(u.Port())
	if err != nil {
		return fmt.Errorf("the MCP server address %s: %w", base, err)
	}
	fwd, err := rh.Forward(ctx, port)
	if err != nil {
		return fmt.Errorf("making the MCP server reachable on %s: %w", r.host.Name(), err)
	}
	remote, _ := fwd.Port(ctx)
	r.fwd = fwd
	r.log.Info("MCP server forwarded", "host", r.host.Name(), "local", port, "remote", remote)
	return nil
}

// agentMCPURL is the URL of the MCP server as the agent reaches it: on a
// remote host, through the forward, which is set up again if it was lost.
func (r *Runner) agentMCPURL(ctx context.Context) (string, error) {
	if r.fwd == nil {
		return r.mcpURL, nil
	}
	port, err := r.fwd.Port(ctx)
	if err != nil {
		return "", fmt.Errorf("the MCP forward to %s: %w", r.host.Name(), err)
	}
	return fmt.Sprintf("http://127.0.0.1:%d/%s", port, r.profile), nil
}
