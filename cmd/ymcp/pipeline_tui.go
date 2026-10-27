package main

import (
	"context"

	"github.com/xlsft/ymcp/internal/pipeline/tui"
)

// pipelineTUI opens the pipeline TUI on the state database. Runners keep
// running when it closes.
func pipelineTUI(ctx context.Context) error {
	st, err := openStore()
	if err != nil {
		return err
	}
	defer st.Close()
	return tui.Run(ctx, tui.Options{Deps: tui.Defaults(st)})
}
