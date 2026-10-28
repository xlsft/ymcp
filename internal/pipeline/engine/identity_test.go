package engine

import (
	"testing"

	"github.com/xlsft/ymcp/internal/pipeline/spec"
)

// TestAgentIdentity: with identity on, the agent commits as its own author,
// from the default templates or the pipeline's.
func TestAgentIdentity(t *testing.T) {
	for _, c := range []struct {
		name string
		id   spec.Identity
		want string
	}{
		{"defaults", spec.Identity{Enabled: true}, "Fake · backend #1 <dev+agent_backend_1@xlsft.ru>"},
		{"templates", spec.Identity{Enabled: true, Name: "{{agent.name}} on {{issue.id}}", Tag: "ai-{{issue.id}}"}, "Fake on YPC-1 <dev+ai_ypc_1@xlsft.ru>"},
		{"templates off", spec.Identity{Name: "{{agent.name}}", Tag: "x"}, "Dev <dev@xlsft.ru>"},
	} {
		t.Run(c.name, func(t *testing.T) {
			h := newHarness(t, func(p *spec.Pipeline) { p.Workspace.Identity = c.id })
			is := h.yt.addIssue("Fix the login page", "Open")
			h.ag.script(fixTurn("Fixed the login page."))
			h.start()
			run := h.waitReview(1, is.Key)
			if got := git(t, h.origin, "log", "-1", "--format=%an <%ae>", run.Branch); got != c.want {
				t.Errorf("author = %q, want %q", got, c.want)
			}
		})
	}
}
