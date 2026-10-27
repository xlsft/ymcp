package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"

	"github.com/xlsft/ymcp/internal/pipeline/engine"
	"github.com/xlsft/ymcp/internal/pipeline/spec"
	"github.com/xlsft/ymcp/internal/pipeline/store"
)

func pipelineRun(ctx context.Context, name string, args []string, logger *slog.Logger) error {
	if err := noArgs(args, "ymcp pipeline NAME run"); err != nil {
		return err
	}
	p, err := spec.Load(name)
	if err != nil {
		return err
	}
	if err := p.Validate(); err != nil {
		return err
	}
	st, err := openStore()
	if err != nil {
		return err
	}
	defer st.Close()

	notes := newRunnerNotifier(p, logger)
	defer notes.close()
	r, err := engine.New(ctx, engine.Options{Pipeline: p, Store: st, Logger: logger, Version: version, Notify: notes.hook()})
	if err != nil {
		return err
	}
	fmt.Fprintln(os.Stderr, styleDim.Render("— running pipeline "+name+", ctrl+c to stop"))
	if err := r.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
		if errors.Is(err, store.ErrRunnerBusy) {
			return fmt.Errorf("%w; check `ymcp pipeline %s status`", err, name)
		}
		notes.stopped(name, err)
		return err
	}
	return nil
}

func pipelineCheck(ctx context.Context, name string, args []string) error {
	if err := noArgs(args, "ymcp pipeline NAME check"); err != nil {
		return err
	}
	p, err := spec.Load(name)
	if err != nil {
		return err
	}
	results, err := engine.Check(ctx, p)
	if err != nil {
		return err
	}
	failed, warned := 0, 0
	for _, r := range results {
		mark := styleOK.Render("✓")
		switch {
		case r.Warning:
			mark = styleWarn.Render("!")
			warned++
		case !r.OK:
			mark = styleWarn.Render("✗")
			failed++
		}
		line := mark + " " + r.Name
		if r.Detail != "" {
			line += styleDim.Render(": " + strings.ReplaceAll(strings.TrimSpace(r.Detail), "\n", "\n    "))
		}
		fmt.Println(line)
	}
	if failed > 0 {
		return fmt.Errorf("%d check(s) failed", failed)
	}
	if warned > 0 {
		fmt.Println(styleDim.Render(fmt.Sprintf("%d warning(s)", warned)))
	}
	return nil
}

// runnerNote tells that an answer or cancellation waits for a runner.
func runnerNote(ctx context.Context, st *store.Store, name string) {
	if _, live, err := runnerLine(ctx, st, name); err == nil && !live {
		fmt.Println(styleDim.Render("  No runner is running; the runner picks this up when `ymcp pipeline " + name + " run` starts."))
	}
}

// activeRun finds a run that can still take an answer or a cancellation.
func activeRun(ctx context.Context, st *store.Store, name, arg string) (store.Run, error) {
	r, err := findRun(ctx, st, name, arg)
	if err != nil {
		return r, err
	}
	if r.Status.Final() {
		return r, fmt.Errorf("run #%d of %s is already %s", r.Number, name, r.Status)
	}
	return r, nil
}

func pipelineAnswer(ctx context.Context, name string, args []string) error {
	const usage = "ymcp pipeline NAME answer RUN TEXT"
	if len(args) > 0 && args[0] == "--" {
		args = args[1:]
	}
	if len(args) == 0 {
		return fmt.Errorf("usage: %s", usage)
	}
	text := strings.TrimSpace(strings.Join(args[1:], " "))
	if text == "" && !interactive() {
		data, err := io.ReadAll(os.Stdin)
		if err != nil {
			return err
		}
		text = strings.TrimSpace(string(data))
	}
	if text == "" {
		return fmt.Errorf("the answer is empty; usage: %s", usage)
	}
	st, err := openStore()
	if err != nil {
		return err
	}
	defer st.Close()
	run, err := activeRun(ctx, st, name, args[0])
	if err != nil {
		return err
	}
	if err := engine.Answer(ctx, st, name, run.Number, text); err != nil {
		return err
	}
	fmt.Println(styleOK.Render("✓") + fmt.Sprintf(" Answer sent to run #%d", run.Number))
	runnerNote(ctx, st, name)
	return nil
}

func pipelineCancel(ctx context.Context, name string, args []string) error {
	const usage = "ymcp pipeline NAME cancel RUN"
	if len(args) != 1 {
		return fmt.Errorf("usage: %s", usage)
	}
	st, err := openStore()
	if err != nil {
		return err
	}
	defer st.Close()
	run, err := activeRun(ctx, st, name, args[0])
	if err != nil {
		return err
	}
	if err := engine.Cancel(ctx, st, name, run.Number); err != nil {
		return err
	}
	fmt.Println(styleOK.Render("✓") + fmt.Sprintf(" Canceled run #%d", run.Number))
	runnerNote(ctx, st, name)
	return nil
}
