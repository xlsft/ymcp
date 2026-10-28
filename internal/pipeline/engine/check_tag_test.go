package engine

import (
	"testing"

	"github.com/xlsft/ymcp/internal/pipeline/spec"
	"github.com/xlsft/ymcp/internal/pipeline/store"
)

// TestStageSearchByNewTag: a stage searched by a tag nobody has created yet
// matches nothing, and the other stages are still tracked: YouTrack refuses
// such a query, and that used to stop every stage check of the tick.
func TestStageSearchByNewTag(t *testing.T) {
	h := newHarness(t, func(p *spec.Pipeline) {
		p.Stages.NeedsInput = spec.Stage{Enter: "tag needs-input", Exit: "untag needs-input", Match: "tag: needs-input"}
	})
	is := h.yt.addIssue("Wander", "Open")
	h.start()
	h.waitReview(1, is.Key)
	h.yt.setState(is.Key, "Closed")
	h.waitStatus(1, store.StatusClosed)
}
