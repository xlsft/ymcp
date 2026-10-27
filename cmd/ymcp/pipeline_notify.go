package main

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"slices"
	"strings"

	"github.com/xlsft/ymcp/internal/config"
	"github.com/xlsft/ymcp/internal/notify"
	_ "github.com/xlsft/ymcp/internal/notify/toast" // registers the toast provider
)

const pipelineNotifyUsage = `ymcp pipeline notify — how pipelines reach you

Usage:
  ymcp pipeline notify                   Providers, whether they are on, their events and whether they work here
  ymcp pipeline notify test [--provider P]
                                         Send a test notification through the enabled providers
  ymcp pipeline notify set KEY=VALUE...  Change settings: toast.enabled=false, toast.events=needs_input,run_failed,
                                         toast.sound=false; an empty value (toast.events=) restores the default

Events: needs_input, run_failed, review_ready, run_done, runner_stopped (default: needs_input, run_failed).
A pipeline can narrow these with "notifications": {"providers": [...], "events": [...]} in its JSON.
`

// testNotificationURL is what a click on the test notification opens.
const testNotificationURL = "https://github.com/xlsft/ymcp"

func pipelineNotify(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return notifyShow(ctx, true)
	}
	switch args[0] {
	case "help", "-h", "-help", "--help":
		fmt.Print(pipelineNotifyUsage)
		return nil
	case "test":
		return notifyTest(ctx, args[1:])
	case "set":
		return notifySet(args[1:])
	}
	fmt.Fprint(os.Stderr, pipelineNotifyUsage)
	return fmt.Errorf("unknown notify command %q", args[0])
}

// notifyShow prints the providers and, with check, whether the enabled ones
// work on this machine.
func notifyShow(ctx context.Context, check bool) error {
	s, err := notify.Load()
	if err != nil {
		return err
	}
	states, err := s.State()
	if err != nil {
		return err
	}
	var results map[string]error
	if check {
		n, err := notify.New(s, nil, nil)
		if err != nil {
			return err
		}
		results = n.Check(ctx)
		n.Close()
	}
	header := []string{"PROVIDER", "ENABLED", "EVENTS", "SETTINGS"}
	if check {
		header = append(header, "STATUS")
	}
	var rows [][]string
	var problems []string
	for _, st := range states {
		events := make([]string, len(st.Events))
		for i, k := range st.Events {
			events[i] = string(k)
		}
		row := []string{st.Name, onOff(st.Enabled), strings.Join(events, ","), providerSettings(st.Settings)}
		if check {
			switch err, checked := results[st.Name]; {
			case !checked:
				row = append(row, styleDim.Render("–"))
			case err != nil:
				row = append(row, styleWarn.Render("not working"))
				problems = append(problems, fmt.Sprintf("%s: %v", st.Name, err))
			default:
				row = append(row, styleOK.Render("ok"))
			}
		}
		rows = append(rows, row)
	}
	if len(rows) == 0 {
		fmt.Println("No notification providers in this build.")
		return nil
	}
	fmt.Println(table(header, rows))
	for _, p := range problems {
		fmt.Println(styleWarn.Render("!") + " " + p)
	}
	if path, err := config.Path(); err == nil {
		fmt.Println(styleDim.Render("Settings: \"notifications\" in " + path))
	}
	return nil
}

// providerSettings shows a provider's own settings (not enabled and
// events) as key=value pairs.
func providerSettings(raw json.RawMessage) string {
	var m map[string]json.RawMessage
	if json.Unmarshal(raw, &m) != nil {
		return "defaults"
	}
	delete(m, "enabled")
	delete(m, "events")
	if len(m) == 0 {
		return "defaults"
	}
	var parts []string
	for _, k := range slices.Sorted(maps.Keys(m)) {
		parts = append(parts, k+"="+string(m[k]))
	}
	return strings.Join(parts, " ")
}

func onOff(b bool) string {
	if b {
		return "on"
	}
	return "off"
}

func notifyTest(ctx context.Context, args []string) error {
	const usage = "ymcp pipeline notify test [--provider P]"
	fs := pipelineFlags("notify test")
	only := fs.String("provider", "", "")
	pos, err := parseInterspersed(fs, args, usage)
	if err != nil {
		return err
	}
	if len(pos) > 0 {
		return fmt.Errorf("unexpected argument %q; usage: %s", pos[0], usage)
	}
	s, err := notify.Load()
	if err != nil {
		return err
	}
	var filter *notify.Filter
	if *only != "" {
		filter = &notify.Filter{Providers: []string{*only}}
	}
	n, err := notify.New(s, filter, nil)
	if err != nil {
		return err
	}
	defer n.Close()
	if len(n.Enabled()) == 0 {
		if *only != "" {
			return fmt.Errorf("notification provider %s is off; turn it on with `ymcp pipeline notify set %s.enabled=true`", *only, *only)
		}
		return fmt.Errorf("no notification provider is on; turn one on with `ymcp pipeline notify set toast.enabled=true`")
	}
	results := n.Deliver(ctx, notify.Event{
		Kind:  notify.KindTest,
		Title: "ymcp test notification",
		Body:  "Pipeline notifications look like this. A click opens the ymcp repository.",
		URL:   testNotificationURL,
	})
	failed := 0
	for _, name := range slices.Sorted(maps.Keys(results)) {
		if err := results[name]; err != nil {
			failed++
			fmt.Printf("%s %s: %v\n", styleWarn.Render("!"), name, err)
			continue
		}
		fmt.Printf("%s %s: sent\n", styleOK.Render("✓"), name)
	}
	if failed > 0 {
		return fmt.Errorf("%d of %d notification providers failed", failed, len(results))
	}
	return nil
}

func notifySet(args []string) error {
	const usage = "ymcp pipeline notify set KEY=VALUE..."
	if len(args) == 0 {
		return fmt.Errorf("nothing to set; usage: %s (e.g. toast.enabled=false)", usage)
	}
	s, err := notify.Load()
	if err != nil {
		return err
	}
	for _, a := range args {
		key, value, ok := strings.Cut(a, "=")
		if !ok {
			return fmt.Errorf("%q is not KEY=VALUE; usage: %s", a, usage)
		}
		if err := s.Set(strings.TrimSpace(key), value); err != nil {
			return err
		}
	}
	if err := notify.Save(s); err != nil {
		return err
	}
	return notifyShow(context.Background(), false)
}
