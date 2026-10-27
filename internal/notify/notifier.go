package notify

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"time"
)

// SendTimeout bounds one provider's Send or Check.
const SendTimeout = 10 * time.Second

// Filter narrows the machine settings for one pipeline (the pipeline
// spec's "notifications" field). Providers keeps only the named providers;
// Events, when set, replaces the providers' event lists for this pipeline.
// A provider turned off in the settings stays off.
type Filter struct {
	Providers []string
	Events    []Kind
}

// FilterOf builds a Filter from a pipeline spec's lists; nil when both are
// empty.
func FilterOf(providers, events []string) *Filter {
	if len(providers) == 0 && len(events) == 0 {
		return nil
	}
	f := &Filter{Providers: slices.Clone(providers)}
	for _, e := range events {
		f.Events = append(f.Events, Kind(e))
	}
	return f
}

// Notifier sends events to the enabled providers. It is safe for
// concurrent use.
type Notifier struct {
	logger  *slog.Logger
	timeout time.Duration
	sinks   []sink

	mu      sync.Mutex
	closed  bool
	onError func(e Event, provider string, err error)
	wg      sync.WaitGroup
}

type sink struct {
	name   string
	p      Provider
	events []Kind
}

// New sets up the enabled providers of s, narrowed by filter (nil: no
// narrowing). Stored settings of providers this build doesn't have are
// ignored with a warning; a provider that rejects its settings, or a filter
// naming an unknown provider or event, is an error. logger may be nil.
func New(s Settings, filter *Filter, logger *slog.Logger) (*Notifier, error) {
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	n := &Notifier{logger: logger, timeout: SendTimeout}
	known := Providers()
	for name := range s.Providers {
		if !slices.Contains(known, name) {
			logger.Warn("notification settings name an unknown provider; ignored", "provider", name)
		}
	}
	if filter != nil {
		for _, name := range filter.Providers {
			if !slices.Contains(known, name) {
				return nil, fmt.Errorf("notifications: unknown provider %q (known: %s)", name, strings.Join(known, ", "))
			}
		}
		for _, k := range filter.Events {
			if _, err := ParseKind(string(k)); err != nil {
				return nil, fmt.Errorf("notifications: %w", err)
			}
		}
	}
	for _, name := range known {
		st, err := s.state(name)
		if err != nil {
			return nil, err
		}
		if !st.Enabled {
			continue
		}
		if filter != nil && len(filter.Providers) > 0 && !slices.Contains(filter.Providers, name) {
			continue
		}
		events := st.Events
		if filter != nil && len(filter.Events) > 0 {
			events = slices.Clone(filter.Events)
		}
		f, _ := factory(name)
		p := f()
		if err := p.Configure(st.Settings); err != nil {
			return nil, fmt.Errorf("notification provider %s: %w", name, err)
		}
		n.sinks = append(n.sinks, sink{name: name, p: p, events: events})
	}
	return n, nil
}

// Enabled lists the providers this notifier sends to.
func (n *Notifier) Enabled() []string {
	names := make([]string, len(n.sinks))
	for i, s := range n.sinks {
		names[i] = s.name
	}
	return names
}

// OnError sets a function called, from the sending goroutine, when an
// asynchronous send fails, e.g. to record it in the run's event feed. The
// failure is logged either way.
func (n *Notifier) OnError(fn func(e Event, provider string, err error)) {
	n.mu.Lock()
	n.onError = fn
	n.mu.Unlock()
}

// Notify sends e in the background to every provider that wants its kind.
// It never blocks: each send has its own timeout, and failures are logged
// (and passed to OnError) rather than returned. After Close it does
// nothing.
func (n *Notifier) Notify(e Event) {
	if n == nil {
		return
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.closed {
		n.logger.Debug("notification dropped after close", "kind", e.Kind)
		return
	}
	for _, s := range n.sinks {
		if !s.wants(e.Kind) {
			continue
		}
		n.wg.Add(1)
		go func() {
			defer n.wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), n.timeout)
			defer cancel()
			if err := send(ctx, s.p, e); err != nil {
				n.logger.Warn("notification failed", "provider", s.name, "kind", e.Kind, "pipeline", e.Pipeline, "run", e.Run, "err", err)
				n.mu.Lock()
				fn := n.onError
				n.mu.Unlock()
				if fn != nil {
					fn(e, s.name, err)
				}
				return
			}
			n.logger.Debug("notification sent", "provider", s.name, "kind", e.Kind, "pipeline", e.Pipeline, "run", e.Run)
		}()
	}
}

// Deliver sends e to every provider that wants it and waits, returning each
// provider's result by name. It is for commands such as a test
// notification, where the caller shows the outcome.
func (n *Notifier) Deliver(ctx context.Context, e Event) map[string]error {
	return n.each(ctx, func(ctx context.Context, s sink) (bool, error) {
		if !s.wants(e.Kind) {
			return false, nil
		}
		return true, send(ctx, s.p, e)
	})
}

// Check runs every enabled provider's Check and returns the results by
// provider name; nil means notifications can be delivered.
func (n *Notifier) Check(ctx context.Context) map[string]error {
	return n.each(ctx, func(ctx context.Context, s sink) (bool, error) {
		return true, call(ctx, func() error { return s.p.Check(ctx) })
	})
}

func (n *Notifier) each(ctx context.Context, do func(context.Context, sink) (bool, error)) map[string]error {
	out := map[string]error{}
	var mu sync.Mutex
	var wg sync.WaitGroup
	for _, s := range n.sinks {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(ctx, n.timeout)
			defer cancel()
			if used, err := do(ctx, s); used {
				mu.Lock()
				out[s.name] = err
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	return out
}

// Close waits for sends in flight and releases the providers (a provider
// that implements io.Closer is closed, e.g. to stop listening for clicks).
// Notify does nothing afterwards.
func (n *Notifier) Close() error {
	if n == nil {
		return nil
	}
	n.mu.Lock()
	if n.closed {
		n.mu.Unlock()
		return nil
	}
	n.closed = true
	n.mu.Unlock()
	n.wg.Wait()
	var errs []error
	for _, s := range n.sinks {
		if c, ok := s.p.(io.Closer); ok {
			if err := c.Close(); err != nil {
				errs = append(errs, fmt.Errorf("%s: %w", s.name, err))
			}
		}
	}
	return errors.Join(errs...)
}

func (s sink) wants(k Kind) bool {
	return k == KindTest || slices.Contains(s.events, k)
}

// send calls p.Send through call.
func send(ctx context.Context, p Provider, e Event) error {
	return call(ctx, func() error { return p.Send(ctx, e) })
}

// call runs fn, turning a panic into an error, and returns when ctx ends
// even if fn doesn't watch it, so a stuck provider can't hold the caller
// past the timeout.
func call(ctx context.Context, fn func() error) error {
	done := make(chan error, 1)
	go func() {
		defer func() {
			if r := recover(); r != nil {
				done <- fmt.Errorf("provider panicked: %v", r)
			}
		}()
		done <- fn()
	}()
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		return fmt.Errorf("timed out: %w", ctx.Err())
	}
}
