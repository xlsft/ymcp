// Package notify tells a human that a pipeline needs them.
//
// The runner only publishes events (an agent asked a question, a run
// failed, a review is ready); this package decides which providers get
// them. A provider is one way of reaching the human, such as system
// notifications ("toast", in internal/notify/toast). It registers itself
// with Register from an init function, so adding one is a new package and
// an import, without changes to the runner.
//
// Settings are per machine and live in the "notifications" section of
// internal/config's config.json:
//
//	"notifications": {
//	  "providers": {
//	    "toast": {"enabled": true, "events": ["needs_input", "run_failed"], "sound": true}
//	  }
//	}
//
// enabled and events are common to all providers (see Common); the other
// fields belong to the provider. A pipeline may narrow them with a Filter.
package notify

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"sort"
	"strings"
	"sync"
)

// Kind is what happened.
type Kind string

// The kinds of events.
const (
	KindNeedsInput    Kind = "needs_input"    // an agent asked a question
	KindRunFailed     Kind = "run_failed"     // a run failed
	KindReviewReady   Kind = "review_ready"   // work is ready for review
	KindRunDone       Kind = "run_done"       // a run finished
	KindRunnerStopped Kind = "runner_stopped" // a pipeline's runner stopped
	KindTest          Kind = "test"           // a test notification; ignores event filters
)

// Kinds are the kinds settings and filters may name, in a stable order.
// KindTest is not among them: test notifications always go out.
var Kinds = []Kind{KindNeedsInput, KindRunFailed, KindReviewReady, KindRunDone, KindRunnerStopped}

// DefaultEvents are the kinds a provider gets when its settings name none.
var DefaultEvents = []Kind{KindNeedsInput, KindRunFailed}

// ParseKind checks that s names one of Kinds.
func ParseKind(s string) (Kind, error) {
	k := Kind(strings.TrimSpace(s))
	if slices.Contains(Kinds, k) {
		return k, nil
	}
	names := make([]string, len(Kinds))
	for i, k := range Kinds {
		names[i] = string(k)
	}
	return "", fmt.Errorf("unknown notification event %q: use one of %s", s, strings.Join(names, ", "))
}

// Event is one thing a human may want to hear about.
type Event struct {
	Kind     Kind
	Pipeline string
	Run      int    // run number within the pipeline; 0 for pipeline-wide events
	Issue    string // readable issue ID, e.g. BE-123
	Title    string // empty: Heading builds one
	Body     string
	URL      string // what a click opens: the question, the PR/MR or the issue
}

var kindText = map[Kind]string{
	KindNeedsInput:    "needs input",
	KindRunFailed:     "failed",
	KindReviewReady:   "ready for review",
	KindRunDone:       "done",
	KindRunnerStopped: "runner stopped",
	KindTest:          "test",
}

// Heading is the notification title: Title if set, otherwise built from the
// event, e.g. "BE-123 · backend #17: needs input".
func (e Event) Heading() string {
	if e.Title != "" {
		return e.Title
	}
	what := kindText[e.Kind]
	if what == "" {
		what = strings.ReplaceAll(string(e.Kind), "_", " ")
	}
	var parts []string
	if e.Issue != "" {
		parts = append(parts, e.Issue)
	}
	switch {
	case e.Pipeline != "" && e.Run > 0:
		parts = append(parts, fmt.Sprintf("%s #%d", e.Pipeline, e.Run))
	case e.Pipeline != "":
		parts = append(parts, e.Pipeline)
	case e.Run > 0:
		parts = append(parts, fmt.Sprintf("#%d", e.Run))
	}
	if len(parts) == 0 {
		return "ymcp: " + what
	}
	return strings.Join(parts, " · ") + ": " + what
}

// Provider delivers events one way, e.g. as system notifications.
type Provider interface {
	// Name is the key the provider is registered and configured under.
	Name() string
	// Configure applies the provider's settings section (nil when there is
	// none). The common fields of Common are part of it.
	Configure(settings json.RawMessage) error
	// Check reports whether notifications can be delivered here, e.g. an
	// error when macOS notifications are turned off for ymcp or there is
	// no D-Bus session bus. The error says what to do.
	Check(ctx context.Context) error
	// Send delivers one event.
	Send(ctx context.Context, e Event) error
}

// DefaultEnabler is implemented by providers that work without any
// settings and so are on unless turned off. Others are off until their
// settings say "enabled": true.
type DefaultEnabler interface {
	EnabledByDefault() bool
}

var (
	registryMu sync.RWMutex
	registry   = map[string]func() Provider{}
)

// Register makes a provider available under name. It panics on an empty
// name or a duplicate, like other registries do at init time.
func Register(name string, factory func() Provider) {
	registryMu.Lock()
	defer registryMu.Unlock()
	if name == "" || factory == nil {
		panic("notify: Register with an empty name or nil factory")
	}
	if _, dup := registry[name]; dup {
		panic("notify: provider " + name + " registered twice")
	}
	registry[name] = factory
}

// Providers lists the registered provider names, sorted.
func Providers() []string {
	registryMu.RLock()
	defer registryMu.RUnlock()
	names := make([]string, 0, len(registry))
	for name := range registry {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func factory(name string) (func() Provider, bool) {
	registryMu.RLock()
	defer registryMu.RUnlock()
	f, ok := registry[name]
	return f, ok
}
