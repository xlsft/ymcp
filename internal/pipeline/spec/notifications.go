package spec

import (
	"fmt"
	"slices"
	"strings"
)

// Notifications narrows the machine-wide notification settings (see
// internal/notify) for one pipeline: Providers keeps only the named
// providers, and Events, when set, are the events this pipeline notifies
// about. A provider turned off on the machine stays off.
type Notifications struct {
	Providers []string `json:"providers,omitempty"` // the providers to use, e.g. ["toast"]; empty for all enabled
	Events    []string `json:"events,omitempty"`    // the events to tell, e.g. ["needs_input", "run_failed"]; empty for each provider's own
}

// NotificationEvents are the event names Notifications.Events may use; they
// match internal/notify's kinds.
var NotificationEvents = []string{"needs_input", "run_failed", "review_ready", "run_done", "runner_stopped"}

// problems lists what is wrong with the field; provider names are checked
// against the registered providers only when the pipeline runs.
func (n *Notifications) problems() []string {
	if n == nil {
		return nil
	}
	var out []string
	for _, p := range n.Providers {
		if strings.TrimSpace(p) == "" {
			out = append(out, "notifications.providers must not contain an empty name")
			break
		}
	}
	for _, e := range n.Events {
		if !slices.Contains(NotificationEvents, e) {
			out = append(out, fmt.Sprintf("notifications.events: unknown event %q: use %s", e, strings.Join(NotificationEvents, ", ")))
		}
	}
	return out
}
