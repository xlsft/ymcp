package tui

import (
	"context"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/xlsft/ymcp/internal/pipeline/spec"
	"github.com/xlsft/ymcp/internal/pipeline/tui/wizard"
)

// newWizard opens the pipeline wizard with its real services.
func newWizard(ctx context.Context, existing *spec.Pipeline) tea.Model {
	deps := wizard.DefaultDeps()
	deps.Strategy = newStrategyDetector()
	return wizard.New(ctx, deps, existing)
}

// wizardResult recognises the wizard's last message.
func wizardResult(msg tea.Msg) (wizardOutcome, bool) {
	switch msg := msg.(type) {
	case wizard.Done:
		return wizardOutcome{saved: true, pipeline: msg.Pipeline, runNow: msg.RunNow}, true
	case wizard.Cancelled:
		return wizardOutcome{}, true
	}
	return wizardOutcome{}, false
}
