package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/charmbracelet/huh"

	"github.com/xlsft/ymcp/internal/pipeline/spec"
)

// pipelinePresets runs `ymcp pipeline presets [COMMAND …]`; without a
// command it lists the presets.
func pipelinePresets(args []string) error {
	if len(args) == 0 {
		return presetsList()
	}
	cmd, rest := args[0], args[1:]
	switch cmd {
	case "help", "-h", "-help", "--help":
		fmt.Print(pipelineUsage)
		return nil
	case "show":
		return presetsShow(rest)
	case "copy":
		return presetsCopy(rest)
	case "edit":
		return presetsEdit(rest)
	case "rename":
		return presetsRename(rest)
	case "rm":
		return presetsRemove(rest)
	case "save":
		return presetsSave(rest)
	}
	if strings.HasPrefix(cmd, "-") {
		return noArgs(args, "ymcp pipeline presets")
	}
	fmt.Fprint(os.Stderr, pipelineUsage)
	return fmt.Errorf("unknown presets command %q", cmd)
}

func presetsList() error {
	list, err := spec.AllPresets()
	if err != nil {
		fmt.Fprintln(os.Stderr, styleWarn.Render("!"), err)
	}
	rows := make([][]string, 0, len(list))
	user := false
	for _, p := range list {
		name, kind := p.Name, "built-in"
		if p.Name == spec.DefaultPreset {
			name += " (default)"
		}
		if !p.Builtin {
			kind, user = "user", true
		}
		rows = append(rows, []string{name, kind, orDash(oneLine(p.Rationale, 72))})
	}
	fmt.Println(table([]string{"NAME", "TYPE", "ABOUT"}, rows))
	if !user {
		fmt.Println(styleDim.Render("Make one of your own with `ymcp pipeline presets copy fix NAME`."))
	}
	return nil
}

// oneName reads the single NAME argument of a presets command.
func oneName(args []string, usage string) (string, error) {
	if len(args) != 1 || strings.HasPrefix(args[0], "-") {
		return "", fmt.Errorf("usage: %s", usage)
	}
	return args[0], spec.CheckPresetName(args[0])
}

func presetsShow(args []string) error {
	name, err := oneName(args, "ymcp pipeline presets show NAME")
	if err != nil {
		return err
	}
	p, err := spec.LoadPreset(name)
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

func presetsCopy(args []string) error {
	const usage = "ymcp pipeline presets copy FROM TO"
	if len(args) != 2 {
		return fmt.Errorf("usage: %s", usage)
	}
	from, to := args[0], args[1]
	if err := spec.CopyPreset(from, to); err != nil {
		return err
	}
	fmt.Println(styleOK.Render("✓") + " Copied preset " + from + " to " + styleBold.Render(to))
	fmt.Println(styleDim.Render("  Change its prompts with:"))
	fmt.Println("  ymcp pipeline presets edit " + to)
	return nil
}

// builtinHint refuses to change a built-in preset and says how to make a
// copy that can be changed.
func builtinHint(name string) error {
	return fmt.Errorf("preset %q is built in and read-only; copy it first: ymcp pipeline presets copy %s NEW", name, name)
}

// decodePreset reads the JSON of a preset, refusing unknown fields like
// spec.LoadPreset does.
func decodePreset(data []byte) (spec.Preset, error) {
	var p spec.Preset
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&p); err != nil {
		return spec.Preset{}, err
	}
	return p, nil
}

func presetsEdit(args []string) error {
	name, err := oneName(args, "ymcp pipeline presets edit NAME")
	if err != nil {
		return err
	}
	if spec.IsBuiltinPreset(name) {
		return builtinHint(name)
	}
	p, err := spec.LoadPreset(name)
	if err != nil {
		return err
	}
	return editJSON("preset", name, p, func(edited []byte) (func() error, error) {
		np, err := decodePreset(edited)
		if err == nil {
			np.Name = name // the file name is the identity
			err = np.Validate()
		}
		if err != nil {
			return nil, err
		}
		return func() error { return spec.SavePreset(np) }, nil
	})
}

func presetsRename(args []string) error {
	const usage = "ymcp pipeline presets rename OLD NEW"
	if len(args) != 2 {
		return fmt.Errorf("usage: %s", usage)
	}
	oldName, newName := args[0], args[1]
	if spec.IsBuiltinPreset(oldName) {
		return builtinHint(oldName)
	}
	if err := spec.RenamePreset(oldName, newName); err != nil {
		return err
	}
	fmt.Println(styleOK.Render("✓") + " Renamed preset " + oldName + " to " + styleBold.Render(newName))
	warnPresetUsers(oldName, "renamed")
	return nil
}

func presetsRemove(args []string) error {
	const usage = "ymcp pipeline presets rm NAME [-y]"
	fs := pipelineFlags("pipeline presets rm")
	yes := fs.Bool("y", false, "do not ask")
	fs.BoolVar(yes, "yes", false, "do not ask")
	pos, err := parseInterspersed(fs, args, usage)
	if err != nil {
		return err
	}
	name, err := oneName(pos, usage)
	if err != nil {
		return err
	}
	if spec.IsBuiltinPreset(name) {
		return fmt.Errorf("preset %q is built in and cannot be removed", name)
	}
	if exists, err := spec.PresetExists(name); err != nil {
		return err
	} else if !exists {
		return fmt.Errorf("preset %q: %w", name, spec.ErrPresetNotFound)
	}
	if !*yes {
		if !interactive() {
			return fmt.Errorf("preset %q is not removed: pass -y to remove it without asking", name)
		}
		warnPresetUsers(name, "removed")
		ok := false
		if err := huh.NewConfirm().Title("Remove preset " + name + "?").
			Description("Pipelines made from it keep their own copies of its prompts.").
			Affirmative("Remove").Negative("Keep").
			Value(&ok).WithTheme(huh.ThemeBase16()).Run(); err != nil {
			return err
		}
		if !ok {
			fmt.Println(styleDim.Render("Nothing removed"))
			return nil
		}
	}
	if err := spec.DeletePreset(name); err != nil {
		return err
	}
	fmt.Println(styleOK.Render("✓") + " Removed preset " + styleBold.Render(name))
	if *yes {
		warnPresetUsers(name, "removed")
	}
	return nil
}

func presetsSave(args []string) error {
	const usage = "ymcp pipeline presets save NAME --from PIPELINE [--rationale TEXT]"
	fs := pipelineFlags("pipeline presets save")
	from := fs.String("from", "", "the pipeline whose prompts are saved")
	rationale := fs.String("rationale", "", "what the preset is for")
	pos, err := parseInterspersed(fs, args, usage)
	if err != nil {
		return err
	}
	name, err := oneName(pos, usage)
	if err != nil {
		return err
	}
	if *from == "" {
		return fmt.Errorf("missing --from; usage: %s", usage)
	}
	if err := spec.CheckName(*from); err != nil {
		return err
	}
	if taken, err := spec.PresetExists(name); err != nil {
		return err
	} else if taken {
		if spec.IsBuiltinPreset(name) {
			return fmt.Errorf("preset %q is built in; choose another name", name)
		}
		return fmt.Errorf("preset %q already exists; choose another name or remove it with `ymcp pipeline presets rm %s`", name, name)
	}
	p, err := spec.Load(*from)
	if err != nil {
		return err
	}
	r := p.ResolvedPrompts()
	why := strings.TrimSpace(*rationale)
	if why == "" {
		why = "The prompts of pipeline " + p.Name + "."
	}
	if err := spec.SavePreset(spec.Preset{Name: name, Rationale: why, Start: r.Start, Feedback: r.Feedback, Answer: r.Answer}); err != nil {
		return err
	}
	fmt.Println(styleOK.Render("✓") + " Saved the prompts of pipeline " + p.Name + " as preset " + styleBold.Render(name))
	fmt.Println(styleDim.Render("  Use it with --preset " + name + " or in the wizard."))
	return nil
}

// warnPresetUsers tells, on stderr, which pipelines name a preset that was
// (or is about to be) renamed or removed: those with their own copies of
// the prompts are fine, those without lose them.
func warnPresetUsers(name, what string) {
	list, _ := spec.List() // a broken pipeline file is reported by `pipeline list`
	var keep, lose []string
	for _, p := range list {
		if p.Prompts.Preset != name {
			continue
		}
		if pr := p.Prompts; pr.Start == "" || pr.Feedback == "" || pr.Answer == "" {
			lose = append(lose, p.Name)
		} else {
			keep = append(keep, p.Name)
		}
	}
	if len(keep) > 0 {
		fmt.Fprintf(os.Stderr, "%s Pipelines made from preset %s keep their own copies of its prompts: %s\n",
			styleWarn.Render("!"), name, strings.Join(keep, ", "))
	}
	if len(lose) > 0 {
		fmt.Fprintf(os.Stderr, "%s Pipelines that take their prompts from preset %s fail once it is %s: %s; edit them with `ymcp pipeline NAME edit`\n",
			styleWarn.Render("!"), name, what, strings.Join(lose, ", "))
	}
}
