package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/charmbracelet/huh"

	"github.com/xlsft/ymcp/internal/pipeline/spec"
	"github.com/xlsft/ymcp/internal/pipeline/store"
)

const pipelineUsage = `ymcp pipeline — agents that work on YouTrack issues

Usage:
  ymcp pipeline list                        List pipelines, their runner and active runs
  ymcp pipeline new NAME --profile P --dir D --query Q
                                            Create a pipeline with defaults; also --agent claude|kimi|opencode|acp,
                                            --command CMD (acp), --preset NAME, --host SSH_ALIAS
  ymcp pipeline presets                     List the built-in prompt presets
  ymcp pipeline NAME run [-d]               Run the pipeline in the foreground (ctrl+c stops it), or with -d
                                            in the background
  ymcp pipeline NAME stop [--force]         Stop the background runner; --force kills it
  ymcp pipeline NAME check                  Check the profile, query, stages, git and agent
  ymcp pipeline NAME status                 Runner and active runs
  ymcp pipeline NAME runs [--all]           Runs; --all includes finished ones
  ymcp pipeline NAME logs [RUN] [-f] [-n N] The event feed of a run, or the log of the runner without RUN
  ymcp pipeline NAME answer RUN TEXT        Answer a run that waits for input, or give it feedback
  ymcp pipeline NAME cancel RUN             Cancel a run
  ymcp pipeline NAME show                   Print the pipeline JSON
  ymcp pipeline NAME edit                   Edit the JSON in $VISUAL or $EDITOR
  ymcp pipeline NAME rm [-y]                Delete the pipeline
  ymcp pipeline NAME rename NEW             Rename the pipeline
  ymcp pipeline NAME copy NEW               Copy the pipeline
`

// runnerStale is the heartbeat age after which a runner is shown as gone.
const runnerStale = time.Minute

// The stage commands a new pipeline starts with. They are valid in
// YouTrack's default workflow (the needs-input tag is created on first use)
// and are meant to be edited. The closed stage starts empty.
const (
	defaultWorkingEnter    = "State In Progress"
	defaultWorkingMatch    = "State: {In Progress}"
	defaultReviewEnter     = "State Fixed"
	defaultReviewMatch     = "State: Fixed"
	defaultNeedsInputEnter = "tag needs-input"
	defaultNeedsInputExit  = "untag needs-input"
	defaultNeedsInputMatch = "tag: needs-input"
)

// defaultClaudeMode and defaultClaudeArgs let a new claude pipeline edit
// files and commit (the presets tell the agent to commit) without asking
// about every git command.
var (
	defaultClaudeMode = "acceptEdits"
	defaultClaudeArgs = []string{"--allowedTools", "Bash(git status:*)", "Bash(git diff:*)", "Bash(git add:*)", "Bash(git commit:*)", "Bash(git log:*)"}
)

// pipelineForeground reports whether `ymcp pipeline args…` runs a pipeline
// in the foreground, which logs like serve does.
func pipelineForeground(args []string) bool {
	return len(args) >= 2 && args[1] == "run" && !detachRequested(args[2:])
}

func pipelineCmd(ctx context.Context, args []string, logger *slog.Logger) error {
	if len(args) == 0 || slices.Contains([]string{"help", "-h", "-help", "--help"}, args[0]) {
		fmt.Print(pipelineUsage)
		return nil
	}
	switch args[0] {
	case "list":
		return pipelineList(ctx, args[1:])
	case "new":
		return pipelineNew(args[1:])
	case "presets":
		return pipelinePresets(args[1:])
	}
	name := args[0]
	if strings.HasPrefix(name, "-") {
		fmt.Fprint(os.Stderr, pipelineUsage)
		return fmt.Errorf("unknown flag %q", name)
	}
	if err := spec.CheckName(name); err != nil {
		return err
	}
	if len(args) == 1 {
		fmt.Fprint(os.Stderr, pipelineUsage)
		return fmt.Errorf("missing command for pipeline %q", name)
	}
	verb, rest := args[1], args[2:]
	switch verb {
	case "run":
		return pipelineRunCmd(ctx, name, rest, logger)
	case "stop":
		return pipelineStop(ctx, name, rest)
	case "check":
		return pipelineCheck(ctx, name, rest)
	case "status":
		return pipelineStatus(ctx, name, rest)
	case "runs":
		return pipelineRuns(ctx, name, rest)
	case "logs":
		return pipelineLogs(ctx, name, rest)
	case "answer":
		return pipelineAnswer(ctx, name, rest)
	case "cancel":
		return pipelineCancel(ctx, name, rest)
	case "show":
		return pipelineShow(name, rest)
	case "edit":
		return pipelineEdit(name, rest)
	case "rm":
		return pipelineRemove(ctx, name, rest)
	case "rename":
		return pipelineRename(ctx, name, rest)
	case "copy":
		return pipelineCopy(name, rest)
	}
	fmt.Fprint(os.Stderr, pipelineUsage)
	return fmt.Errorf("unknown pipeline command %q", verb)
}

// pipelineFlags is a flag set that stays quiet: parseInterspersed reports
// a bad flag together with the usage line.
func pipelineFlags(name string) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	return fs
}

// parseInterspersed parses flags that may come before, between or after the
// positional arguments and returns the positional ones.
func parseInterspersed(fs *flag.FlagSet, args []string, usage string) ([]string, error) {
	var pos []string
	for {
		if err := fs.Parse(args); err != nil {
			if errors.Is(err, flag.ErrHelp) {
				fmt.Println("usage: " + usage)
				return nil, flag.ErrHelp
			}
			return nil, fmt.Errorf("%w; usage: %s", err, usage)
		}
		if fs.NArg() == 0 {
			return pos, nil
		}
		pos = append(pos, fs.Arg(0))
		args = fs.Args()[1:]
	}
}

// noArgs rejects arguments for a command that takes none.
func noArgs(args []string, usage string) error {
	fs := pipelineFlags("pipeline")
	pos, err := parseInterspersed(fs, args, usage)
	if err != nil {
		return err
	}
	if len(pos) > 0 {
		return fmt.Errorf("unexpected argument %q; usage: %s", pos[0], usage)
	}
	return nil
}

// parseRunNumber reads a run number such as 17 or #17.
func parseRunNumber(s string) (int, error) {
	n, err := strconv.Atoi(strings.TrimPrefix(s, "#"))
	if err != nil || n < 1 {
		return 0, fmt.Errorf("%q is not a run number", s)
	}
	return n, nil
}

func openStore() (*store.Store, error) {
	path, err := store.DefaultPath()
	if err != nil {
		return nil, err
	}
	return store.Open(path)
}

// table lays rows out in aligned columns under a dimmed header.
func table(header []string, rows [][]string) string {
	var b strings.Builder
	tw := tabwriter.NewWriter(&b, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, strings.Join(header, "\t"))
	for _, r := range rows {
		fmt.Fprintln(tw, strings.Join(r, "\t"))
	}
	tw.Flush()
	lines := strings.Split(strings.TrimRight(b.String(), "\n"), "\n")
	lines[0] = styleDim.Render(lines[0])
	return strings.Join(lines, "\n")
}

// ago is how long ago t was, in the coarsest sensible unit.
func ago(t time.Time) string {
	if t.IsZero() {
		return "–"
	}
	d := max(time.Since(t), 0)
	switch {
	case d < 5*time.Second:
		return "just now"
	case d < time.Minute:
		return fmt.Sprintf("%ds ago", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	}
	return fmt.Sprintf("%dd ago", int(d.Hours()/24))
}

// oneLine collapses whitespace so a text fits a table cell.
func oneLine(s string, limit int) string {
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); limit > 0 && len(r) > limit {
		s = string(r[:limit-1]) + "…"
	}
	return s
}

// runnerLine describes the runner of a pipeline; live is false when nobody
// holds it or its heartbeat is stale.
func runnerLine(ctx context.Context, st *store.Store, name string) (line string, live bool, err error) {
	pid, host, beat, ok, err := st.Runner(ctx, name)
	if err != nil {
		return "", false, err
	}
	if !ok {
		return "not running", false, nil
	}
	age := time.Since(beat)
	if age > runnerStale {
		return fmt.Sprintf("stopped (pid %d on %s, last heartbeat %s)", pid, host, ago(beat)), false, nil
	}
	return fmt.Sprintf("running (pid %d on %s, heartbeat %s)", pid, host, ago(beat)), true, nil
}

// activeSummary counts runs by status, e.g. "1 working, 2 needs_input".
func activeSummary(runs []store.Run) string {
	var parts []string
	for _, status := range []store.RunStatus{store.StatusQueued, store.StatusWorking, store.StatusNeedsInput, store.StatusReview} {
		n := 0
		for _, r := range runs {
			if r.Status == status {
				n++
			}
		}
		if n > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", n, status))
		}
	}
	if len(parts) == 0 {
		return "–"
	}
	return strings.Join(parts, ", ")
}

func pipelineList(ctx context.Context, args []string) error {
	if err := noArgs(args, "ymcp pipeline list"); err != nil {
		return err
	}
	list, listErr := spec.List()
	if listErr != nil {
		fmt.Fprintln(os.Stderr, styleWarn.Render("!"), listErr)
	}
	if len(list) == 0 {
		fmt.Println("No pipelines yet. Create one with:")
		fmt.Println("  ymcp pipeline new NAME --profile P --dir D --query Q")
		return nil
	}
	st, err := openStore()
	if err != nil {
		fmt.Fprintln(os.Stderr, styleWarn.Render("!"), "state database unavailable:", err)
		st = nil
	} else {
		defer st.Close()
	}
	rows := make([][]string, 0, len(list))
	for _, p := range list {
		runner, active := "–", "–"
		if st != nil {
			if _, live, err := runnerLine(ctx, st, p.Name); err == nil {
				runner = "stopped"
				if live {
					runner = "running"
				}
			}
			if runs, err := st.Runs(ctx, p.Name, true); err == nil {
				active = activeSummary(runs)
			}
		}
		rows = append(rows, []string{p.Name, p.Profile, p.Agent.Name, runner, active})
	}
	fmt.Println(table([]string{"NAME", "PROFILE", "AGENT", "RUNNER", "ACTIVE RUNS"}, rows))
	return nil
}

func pipelinePresets(args []string) error {
	if err := noArgs(args, "ymcp pipeline presets"); err != nil {
		return err
	}
	for _, p := range spec.Presets() {
		mark := ""
		if p.Name == spec.DefaultPreset {
			mark = styleDim.Render(" (default)")
		}
		fmt.Println(styleBold.Render(p.Name) + mark)
		fmt.Println("  " + oneLine(p.Rationale, 0))
	}
	return nil
}

func pipelineNew(args []string) error {
	const usage = "ymcp pipeline new NAME --profile P --dir D --query Q [--agent claude] [--command CMD] [--preset NAME] [--host SSH_ALIAS]"
	fs := pipelineFlags("pipeline new")
	prof := fs.String("profile", "", "YouTrack profile the pipeline works with")
	dir := fs.String("dir", "", "the git repository the agent works in")
	query := fs.String("query", "", "YouTrack search that finds issues for the agent")
	agentFlag := fs.String("agent", spec.DefaultProvider, "claude, kimi, opencode or acp")
	command := fs.String("command", "", "the command that starts the ACP server (acp)")
	preset := fs.String("preset", spec.DefaultPreset, "built-in prompt preset")
	host := fs.String("host", "", "ssh host alias to work on; empty is this machine")
	pos, err := parseInterspersed(fs, args, usage)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return fmt.Errorf("a pipeline needs exactly one name; usage: %s", usage)
	}
	name := pos[0]
	if err := spec.CheckName(name); err != nil {
		return err
	}
	if *prof == "" {
		*prof = profile // the global --profile, given before the command
	}
	var missing []string
	for _, f := range []struct{ flag, value string }{{"--profile", *prof}, {"--dir", *dir}, {"--query", *query}} {
		if strings.TrimSpace(f.value) == "" {
			missing = append(missing, f.flag)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("missing %s; usage: %s", strings.Join(missing, ", "), usage)
	}
	if _, ok := spec.LookupPreset(*preset); !ok {
		return fmt.Errorf("unknown preset %q; see `ymcp pipeline presets`", *preset)
	}
	if exists, err := pipelineExists(name); err != nil {
		return err
	} else if exists {
		return fmt.Errorf("pipeline %q already exists; edit it with `ymcp pipeline %s edit`", name, name)
	}

	repo := expandHome(*dir)
	if *host == "" && !filepath.IsAbs(repo) {
		if repo, err = filepath.Abs(repo); err != nil {
			return err
		}
	}
	p := spec.Pipeline{
		Name:    name,
		Profile: *prof,
		Source:  spec.Source{Query: *query},
		Stages: spec.Stages{
			Working:    spec.Stage{Enter: defaultWorkingEnter, Match: defaultWorkingMatch},
			Review:     spec.Stage{Enter: defaultReviewEnter, Match: defaultReviewMatch},
			NeedsInput: spec.Stage{Enter: defaultNeedsInputEnter, Exit: defaultNeedsInputExit, Match: defaultNeedsInputMatch},
		},
		Workspace: spec.Workspace{Host: *host, Dir: repo},
		Agent:     spec.Agent{Provider: *agentFlag, Command: *command},
		Prompts:   spec.Prompts{Preset: *preset},
	}.WithDefaults()
	if p.Agent.Provider == "claude" {
		p.Agent.Mode = defaultClaudeMode
		p.Agent.Args = slices.Clone(defaultClaudeArgs)
	}
	if err := p.Validate(); err != nil {
		return err
	}
	if err := spec.Save(p); err != nil {
		return err
	}
	fmt.Println(styleOK.Render("✓") + " Created pipeline " + styleBold.Render(name))
	fmt.Println(styleDim.Render("  The stage commands are examples; adjust them to your workflow:"))
	fmt.Println("  ymcp pipeline " + name + " edit")
	fmt.Println("  ymcp pipeline " + name + " check")
	return nil
}

// expandHome turns a leading ~ into the home directory.
func expandHome(p string) string {
	if p == "~" || strings.HasPrefix(p, "~/") || strings.HasPrefix(p, `~\`) {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, p[1:])
		}
	}
	return p
}

// pipelineExists reports whether a pipeline file is there, readable or not.
func pipelineExists(name string) (bool, error) {
	dir, err := spec.Dir()
	if err != nil {
		return false, err
	}
	if err := spec.CheckName(name); err != nil {
		return false, err
	}
	_, err = os.Stat(filepath.Join(dir, name+".json"))
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	return err == nil, err
}

func pipelineShow(name string, args []string) error {
	if err := noArgs(args, "ymcp pipeline NAME show"); err != nil {
		return err
	}
	p, err := spec.Load(name)
	if err != nil {
		return err
	}
	data, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return err
	}
	fmt.Println(string(data))
	return nil
}

// editorCommand is the editor to open files with.
func editorCommand() []string {
	for _, k := range []string{"VISUAL", "EDITOR"} {
		if f := strings.Fields(os.Getenv(k)); len(f) > 0 {
			return f
		}
	}
	if runtime.GOOS == "windows" {
		return []string{"notepad"}
	}
	return []string{"vi"}
}

// decodePipeline reads the JSON of a pipeline file, refusing unknown fields
// like spec.Load does.
func decodePipeline(data []byte) (spec.Pipeline, error) {
	var p spec.Pipeline
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&p); err != nil {
		return spec.Pipeline{}, err
	}
	return p, nil
}

func pipelineEdit(name string, args []string) error {
	if err := noArgs(args, "ymcp pipeline NAME edit"); err != nil {
		return err
	}
	p, err := spec.Load(name)
	if err != nil {
		return err
	}
	data, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	tmp, err := os.CreateTemp("", "ymcp-pipeline-"+name+"-*.json")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}

	for {
		ed := editorCommand()
		cmd := exec.Command(ed[0], append(ed[1:], tmp.Name())...)
		cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("editor %s: %w", ed[0], err)
		}
		edited, err := os.ReadFile(tmp.Name())
		if err != nil {
			return err
		}
		if bytes.Equal(edited, data) {
			fmt.Println(styleDim.Render("No changes"))
			return nil
		}
		np, err := decodePipeline(edited)
		if err == nil {
			np.Name = name // the file name is the identity
			err = np.Validate()
		}
		if err == nil {
			if err := spec.Save(np); err != nil {
				return err
			}
			fmt.Println(styleOK.Render("✓") + " Saved pipeline " + styleBold.Render(name))
			return nil
		}
		fmt.Fprintln(os.Stderr, styleWarn.Render("✗"), err)
		if !interactive() {
			return fmt.Errorf("not saved: %w", err)
		}
		again := true
		if ferr := huh.NewConfirm().Title("Edit again?").
			Affirmative("Edit again").Negative("Discard").
			Value(&again).WithTheme(huh.ThemeBase16()).Run(); ferr != nil || !again {
			return errors.New("not saved")
		}
	}
}

// pipelineGuard refuses to change a pipeline a live runner is working on.
func pipelineGuard(ctx context.Context, name string) (*store.Store, bool, error) {
	st, err := openStore()
	if err != nil {
		return nil, false, err
	}
	_, live, err := runnerLine(ctx, st, name)
	if err != nil {
		st.Close()
		return nil, false, err
	}
	return st, live, nil
}

func pipelineRemove(ctx context.Context, name string, args []string) error {
	const usage = "ymcp pipeline NAME rm [-y]"
	fs := pipelineFlags("pipeline rm")
	yes := fs.Bool("y", false, "do not ask")
	fs.BoolVar(yes, "yes", false, "do not ask")
	pos, err := parseInterspersed(fs, args, usage)
	if err != nil {
		return err
	}
	if len(pos) > 0 {
		return fmt.Errorf("unexpected argument %q; usage: %s", pos[0], usage)
	}
	if exists, err := pipelineExists(name); err != nil {
		return err
	} else if !exists {
		return fmt.Errorf("pipeline %q: %w", name, spec.ErrNotFound)
	}
	st, live, err := pipelineGuard(ctx, name)
	if err != nil {
		return err
	}
	defer st.Close()
	if live {
		return fmt.Errorf("pipeline %q has a running runner; stop it first", name)
	}
	if !*yes {
		if !interactive() {
			return fmt.Errorf("pipeline %q is not removed: pass -y to remove it without asking", name)
		}
		ok := false
		if err := huh.NewConfirm().Title("Remove pipeline " + name + "?").
			Description("The run history stays in the state database.").
			Affirmative("Remove").Negative("Keep").
			Value(&ok).WithTheme(huh.ThemeBase16()).Run(); err != nil {
			return err
		}
		if !ok {
			fmt.Println(styleDim.Render("Nothing removed"))
			return nil
		}
	}
	if err := spec.Delete(name); err != nil {
		return err
	}
	fmt.Println(styleOK.Render("✓") + " Removed pipeline " + styleBold.Render(name))
	return nil
}

func pipelineRename(ctx context.Context, name string, args []string) error {
	const usage = "ymcp pipeline NAME rename NEW"
	if len(args) != 1 {
		return fmt.Errorf("usage: %s", usage)
	}
	newName := args[0]
	if err := spec.CheckName(newName); err != nil {
		return err
	}
	if exists, err := pipelineExists(name); err != nil {
		return err
	} else if !exists {
		return fmt.Errorf("pipeline %q: %w", name, spec.ErrNotFound)
	}
	// Runs are filed under the pipeline name, so they cannot follow it.
	st, err := openStore()
	if err != nil {
		return err
	}
	defer st.Close()
	active, err := st.Runs(ctx, name, true)
	if err != nil {
		return err
	}
	if len(active) > 0 {
		return fmt.Errorf("pipeline %q has %d active run(s), and runs stay filed under the old name; cancel or finish them before renaming", name, len(active))
	}
	if err := spec.Rename(name, newName); err != nil {
		return err
	}
	fmt.Println(styleOK.Render("✓") + " Renamed pipeline " + name + " to " + styleBold.Render(newName))
	return nil
}

func pipelineCopy(name string, args []string) error {
	const usage = "ymcp pipeline NAME copy NEW"
	if len(args) != 1 {
		return fmt.Errorf("usage: %s", usage)
	}
	newName := args[0]
	if err := spec.CheckName(newName); err != nil {
		return err
	}
	p, err := spec.Load(name)
	if err != nil {
		return err
	}
	if exists, err := pipelineExists(newName); err != nil {
		return err
	} else if exists {
		return fmt.Errorf("pipeline %q already exists", newName)
	}
	p.Name = newName
	if err := spec.Save(p); err != nil {
		return err
	}
	fmt.Println(styleOK.Render("✓") + " Copied pipeline " + name + " to " + styleBold.Render(newName))
	return nil
}

// knownPipeline fails for a name that has neither a file nor any runs, which
// is almost always a typo.
func knownPipeline(name string, runs []store.Run) error {
	if len(runs) > 0 {
		return nil
	}
	exists, err := pipelineExists(name)
	if err != nil {
		return err
	}
	if !exists {
		return fmt.Errorf("pipeline %q: %w", name, spec.ErrNotFound)
	}
	return nil
}

// runsTable lists runs, status first: a run that waits for input keeps
// the stage it waits in (usually working), so the stage alone would hide
// it. Every run waiting for input follows with its reason and how to
// answer it.
func runsTable(runs []store.Run) string {
	rows := make([][]string, 0, len(runs))
	for _, r := range runs {
		rows = append(rows, []string{
			"#" + strconv.Itoa(r.Number), orDash(r.IssueKey), string(r.Status), orDash(r.Stage),
			orDash(r.Branch), ago(r.UpdatedAt),
		})
	}
	out := table([]string{"RUN", "ISSUE", "STATUS", "STAGE", "BRANCH", "UPDATED"}, rows)
	waiting := 0
	for _, r := range runs {
		if r.Status != store.StatusNeedsInput {
			continue
		}
		waiting++
		reason := oneLine(r.NeedsInputReason, 200)
		if reason == "" {
			reason = "waiting for an answer"
		}
		out += fmt.Sprintf("\n%s #%d needs input: %s", styleWarn.Render("?"), r.Number, reason)
	}
	if waiting > 0 {
		out += "\n" + styleDim.Render("  answer with: ymcp pipeline "+runs[0].Pipeline+" answer RUN TEXT")
	}
	return out
}

func orDash(s string) string {
	if s == "" {
		return "–"
	}
	return s
}

func pipelineRuns(ctx context.Context, name string, args []string) error {
	const usage = "ymcp pipeline NAME runs [--all]"
	fs := pipelineFlags("pipeline runs")
	all := fs.Bool("all", false, "include finished runs")
	pos, err := parseInterspersed(fs, args, usage)
	if err != nil {
		return err
	}
	if len(pos) > 0 {
		return fmt.Errorf("unexpected argument %q; usage: %s", pos[0], usage)
	}
	st, err := openStore()
	if err != nil {
		return err
	}
	defer st.Close()
	runs, err := st.Runs(ctx, name, !*all)
	if err != nil {
		return err
	}
	if len(runs) == 0 {
		if err := knownPipeline(name, nil); err != nil {
			return err
		}
		if *all {
			fmt.Println("No runs yet.")
		} else {
			fmt.Println("No active runs. `--all` shows finished ones.")
		}
		return nil
	}
	fmt.Println(runsTable(runs))
	return nil
}

func pipelineStatus(ctx context.Context, name string, args []string) error {
	if err := noArgs(args, "ymcp pipeline NAME status"); err != nil {
		return err
	}
	st, err := openStore()
	if err != nil {
		return err
	}
	defer st.Close()
	line, _, err := runnerLine(ctx, st, name)
	if err != nil {
		return err
	}
	runs, err := st.Runs(ctx, name, true)
	if err != nil {
		return err
	}
	if err := knownPipeline(name, runs); err != nil {
		return err
	}
	fmt.Printf("%s  %s\n", styleBold.Render("Runner"), line)
	if len(runs) == 0 {
		fmt.Printf("%s    none active\n", styleBold.Render("Runs"))
		return nil
	}
	fmt.Printf("%s    %s\n\n", styleBold.Render("Runs"), activeSummary(runs))
	fmt.Println(runsTable(runs))
	return nil
}

// findRun looks a run up by its number in the pipeline.
func findRun(ctx context.Context, st *store.Store, name, arg string) (store.Run, error) {
	n, err := parseRunNumber(arg)
	if err != nil {
		return store.Run{}, err
	}
	r, err := st.RunByNumber(ctx, name, n)
	if errors.Is(err, store.ErrNotFound) {
		return store.Run{}, fmt.Errorf("pipeline %q has no run #%d; see `ymcp pipeline %s runs --all`", name, n, name)
	}
	return r, err
}

func pipelineLogs(ctx context.Context, name string, args []string) error {
	const usage = "ymcp pipeline NAME logs [RUN] [-f] [-n N]"
	fs := pipelineFlags("pipeline logs")
	follow := fs.Bool("f", false, "follow new events")
	fs.BoolVar(follow, "follow", false, "follow new events")
	lines := fs.Int("n", 100, "how many of the latest events to show; 0 shows all")
	pos, err := parseInterspersed(fs, args, usage)
	if err != nil {
		return err
	}
	if len(pos) > 1 {
		return fmt.Errorf("usage: %s", usage)
	}
	if *lines < 0 {
		return fmt.Errorf("-n must not be negative; usage: %s", usage)
	}
	if len(pos) == 0 {
		return pipelineRunnerLog(ctx, name, *lines, *follow)
	}
	st, err := openStore()
	if err != nil {
		return err
	}
	defer st.Close()
	run, err := findRun(ctx, st, name, pos[0])
	if err != nil {
		return err
	}

	events, err := st.Events(ctx, run.ID, 0, 0)
	if err != nil {
		return err
	}
	var last int64
	if len(events) > 0 {
		last = events[len(events)-1].ID
	}
	if *lines > 0 && len(events) > *lines {
		events = events[len(events)-*lines:]
	}
	if len(events) == 0 && !*follow {
		fmt.Println("No events yet.")
		return nil
	}
	for _, e := range events {
		fmt.Println(formatEvent(e))
	}
	if !*follow {
		return nil
	}
	fmt.Fprintln(os.Stderr, styleDim.Render(fmt.Sprintf("— following run #%d, ctrl+c to stop", run.Number)))
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(500 * time.Millisecond):
		}
		fresh, err := st.Events(ctx, run.ID, last, 0)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		for _, e := range fresh {
			fmt.Println(formatEvent(e))
			last = e.ID
		}
		if len(fresh) > 0 {
			continue
		}
		cur, err := st.Run(ctx, run.ID)
		if err != nil {
			return err
		}
		if cur.Status.Final() {
			return nil
		}
	}
}

// eventTextKeys are the fields of an event's data that hold its text, in
// the order they are looked for.
var eventTextKeys = []string{"text", "message", "question", "reason", "error", "title", "summary", "command", "prompt"}

// formatEvent prints one event as "15:04:05  kind  text". The text comes
// from the first well-known field of the event data, or is the data itself.
func formatEvent(e store.Event) string {
	head := fmt.Sprintf("%s  %s", styleDim.Render(e.At.Local().Format("15:04:05")), styleBold.Render(e.Kind))
	body := eventText(e.Data)
	if body == "" {
		return head
	}
	body = strings.ReplaceAll(strings.TrimSpace(body), "\n", "\n    ")
	return head + "  " + body
}

func eventText(data json.RawMessage) string {
	if len(data) == 0 || string(data) == "null" {
		return ""
	}
	var s string
	if json.Unmarshal(data, &s) == nil {
		return s
	}
	var m map[string]any
	if json.Unmarshal(data, &m) == nil {
		for _, k := range eventTextKeys {
			if v, ok := m[k].(string); ok && strings.TrimSpace(v) != "" {
				return v
			}
		}
	}
	var buf bytes.Buffer
	if json.Compact(&buf, data) == nil {
		return buf.String()
	}
	return string(data)
}
