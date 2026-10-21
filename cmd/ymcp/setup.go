package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"

	"github.com/charmbracelet/huh"

	"github.com/xlsft/ymcp/internal/clients"
	"github.com/xlsft/ymcp/internal/config"
)

// errKeep is a declined removal.
var errKeep = errors.New("nothing removed")

// Choices of the profile picker besides the profiles themselves.
const (
	pickNew    = "\x00new"
	pickRemove = "\x00remove"
)

func setup(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("setup", flag.ContinueOnError)
	reauth := fs.Bool("reauth", false, "ask for credentials even if some are saved")
	only := fs.String("clients", "", "comma-separated client IDs to register, without asking")
	all := fs.Bool("all", false, "register in every detected client, without asking")
	name := fs.String("name", "", `profile display name, e.g. "YouTrack (work)"; its key is the MCP server name`)
	remove := fs.Bool("remove", false, "remove the --profile profile and its client registrations")
	if err := fs.Parse(args); err != nil {
		return err
	}
	ask := interactive() && *only == "" && !*all && !*remove

	fmt.Println()
	fmt.Println(styleTitle.Render("ymcp") + " " + styleBold.Render("YouTrack MCP setup"))
	fmt.Println()

	env, err := clients.System(runtime.GOOS)
	if err != nil {
		return err
	}
	profiles, err := config.Profiles()
	if err != nil {
		return err
	}

	// 1. Which profile.
	current, removing, err := pickProfile(profiles, ask, *remove)
	if errors.Is(err, errKeep) && !errors.Is(err, huh.ErrUserAborted) {
		fmt.Println(styleDim.Render("Nothing removed"))
		return nil
	}
	if err != nil {
		return err
	}
	if removing {
		return removeProfile(env, current)
	}

	// 2. Its name.
	oldName := current.Name
	display := current.Title()
	if display == "" {
		display = config.DefaultProfile
		if slices.ContainsFunc(profiles, func(p config.Config) bool { return p.Name == display }) {
			display = ""
		}
	}
	if *name != "" {
		display = *name
	}
	if ask {
		if err := nameForm(&display, oldName, profiles).Run(); err != nil {
			return err
		}
	}
	key := config.Slug(display)
	if key != oldName && slices.ContainsFunc(profiles, func(p config.Config) bool { return p.Name == key }) {
		return fmt.Errorf("a profile %q already exists; pick another name or edit that one", key)
	}

	// 3. Its connection; a new profile starts from YOUTRACK_* variables.
	from := oldName
	if from == "" {
		from = key
	}
	cfg, err := config.Load(from)
	if err != nil {
		return err
	}
	cfg.Name, cfg.DisplayName = key, display
	// A new profile is always verified, even when the environment filled it.
	keep := oldName != "" && cfg.Complete() && !*reauth
	if keep && ask {
		if err := huh.NewConfirm().
			Title(fmt.Sprintf("Keep the saved connection %s@%s?", cfg.Login, strings.TrimPrefix(strings.TrimPrefix(cfg.URL, "https://"), "http://"))).
			Affirmative("Keep").Negative("Change").
			Value(&keep).WithTheme(huh.ThemeBase16()).Run(); err != nil {
			return err
		}
	}
	if keep {
		if _, err := config.Save(cfg, oldName); err != nil {
			return err
		}
	} else if cfg, err = authenticate(ctx, cfg, true, oldName); err != nil {
		return err
	}
	if oldName != "" && oldName != key {
		fmt.Println(styleOK.Render("✓") + " Renamed profile " + oldName + " to " + styleBold.Render(key))
	}

	// 4. The clients it is registered in.
	return registerProfile(env, cfg, oldName, *only, *all, ask)
}

// pickProfile chooses the profile to set up: the --profile one, the only
// one, or the one picked from a list that can also add or remove profiles.
// An empty Name means a new profile.
func pickProfile(profiles []config.Config, ask, remove bool) (config.Config, bool, error) {
	find := func(key string) config.Config {
		for _, p := range profiles {
			if p.Name == key {
				return p
			}
		}
		return config.Config{}
	}
	if profile != "" {
		p := find(profile)
		if remove && p.Name == "" {
			return p, false, fmt.Errorf("no profile %q", profile)
		}
		if p.Name == "" {
			p.DisplayName = profile
		}
		return p, remove, nil
	}
	if remove {
		return config.Config{}, false, errors.New("pass the profile to remove: ymcp --profile <key> setup --remove")
	}
	if len(profiles) == 0 {
		return config.Config{}, false, nil
	}
	if !ask {
		key, err := config.Resolve("")
		return find(key), false, err
	}

	opts := make([]huh.Option[string], 0, len(profiles)+2)
	for _, p := range profiles {
		opts = append(opts, huh.NewOption(profileLabel(p), p.Name))
	}
	opts = append(opts, huh.NewOption("+ Add a profile", pickNew), huh.NewOption("− Remove a profile", pickRemove))
	var choice string
	if err := huh.NewSelect[string]().
		Title("Profiles").
		Description("Each profile is a YouTrack instance, registered in clients under its own name").
		Options(opts...).Value(&choice).WithTheme(huh.ThemeBase16()).Run(); err != nil {
		return config.Config{}, false, err
	}
	switch choice {
	case pickNew:
		return config.Config{}, false, nil
	case pickRemove:
		opts = opts[:len(profiles)]
		if err := huh.NewSelect[string]().Title("Remove which profile?").
			Options(opts...).Value(&choice).WithTheme(huh.ThemeBase16()).Run(); err != nil {
			return config.Config{}, false, err
		}
		sure := false
		if err := huh.NewConfirm().
			Title("Remove " + choice + " and its client registrations?").
			Affirmative("Remove").Negative("Cancel").
			Value(&sure).WithTheme(huh.ThemeBase16()).Run(); err != nil || !sure {
			return config.Config{}, false, errors.Join(err, errKeep)
		}
		return find(choice), true, nil
	}
	return find(choice), false, nil
}

func profileLabel(p config.Config) string {
	label := p.Title()
	if label != p.Name {
		label += styleDim.Render(" · " + p.Name)
	}
	host := strings.TrimPrefix(strings.TrimPrefix(p.URL, "https://"), "http://")
	return label + styleDim.Render("  "+p.Login+"@"+host)
}

func nameForm(display *string, oldName string, profiles []config.Config) *huh.Form {
	return huh.NewForm(huh.NewGroup(
		huh.NewInput().
			Title("Profile name").
			Placeholder("e.g. YouTrack (work)").
			DescriptionFunc(func() string {
				if strings.TrimSpace(*display) == "" {
					return "MCP clients list the YouTrack under a key made from it"
				}
				return "MCP clients list this YouTrack as " + config.Slug(*display)
			}, display).
			Value(display).
			Validate(func(s string) error {
				key := config.Slug(s)
				if strings.TrimSpace(s) == "" {
					return errors.New("name is required")
				}
				if key != oldName && slices.ContainsFunc(profiles, func(p config.Config) bool { return p.Name == key }) {
					return fmt.Errorf("profile %s already exists", key)
				}
				return nil
			}),
	)).WithTheme(huh.ThemeBase16())
}

// registerProfile adds cfg's server to the chosen clients and removes it,
// and the server of its old name after a rename, from the others.
func registerProfile(env clients.Env, cfg config.Config, oldName, only string, all, ask bool) error {
	bin, err := binaryPath()
	if err != nil {
		return err
	}
	server := clients.Server{Name: cfg.Name, Command: bin, Args: []string{"--profile", cfg.Name}}
	names := []string{cfg.Name}
	if oldName != "" && oldName != cfg.Name {
		names = append(names, oldName)
	}
	installed := func(c clients.Client) bool {
		return slices.ContainsFunc(names, func(n string) bool { return c.Installed(env, n) })
	}

	detected, missing := detectClients(env)
	if len(detected) == 0 {
		fmt.Println(styleWarn.Render("!") + " No MCP clients found. Register " + bin + " --profile " + cfg.Name + " as a stdio server manually.")
		return nil
	}

	var chosen []string
	switch {
	case only != "":
		chosen = strings.Split(only, ",")
	case all:
		for _, c := range detected {
			chosen = append(chosen, c.ID)
		}
	case ask:
		anyInstalled := slices.ContainsFunc(detected, installed)
		opts := make([]huh.Option[string], 0, len(detected))
		for _, c := range detected {
			label := c.Name
			had := installed(c)
			if had {
				label += styleDim.Render("  · added")
			}
			opts = append(opts, huh.NewOption(label, c.ID).Selected(had || !anyInstalled))
		}
		if err := huh.NewMultiSelect[string]().
			Title("Register " + cfg.Title() + " in").
			Description("space to toggle · enter to confirm · unchecked clients get it removed").
			Options(opts...).
			Value(&chosen).WithTheme(huh.ThemeBase16()).Run(); err != nil {
			return err
		}
	default:
		return errors.New("not a terminal: pass --clients id1,id2 or --all")
	}

	fmt.Println()
	var failed int
	for _, c := range detected {
		want := slices.Contains(chosen, c.ID)
		had := installed(c)
		if !want && !had {
			continue
		}
		var errs []error
		for _, n := range names {
			if (!want || n != cfg.Name) && c.Installed(env, n) {
				errs = append(errs, c.Uninstall(env, n))
			}
		}
		if want {
			errs = append(errs, c.Install(env, server))
		}
		if err := errors.Join(errs...); err != nil {
			failed++
			fmt.Printf("%s %s: %v\n", styleWarn.Render("✗"), c.Name, err)
			continue
		}
		switch {
		case want && c.Note != "":
			fmt.Printf("%s %s%s\n", styleOK.Render("✓"), c.Name, styleDim.Render(" — "+c.Note))
		case want:
			fmt.Printf("%s %s\n", styleOK.Render("✓"), c.Name)
		default:
			fmt.Printf("%s %s %s\n", styleDim.Render("–"), c.Name, styleDim.Render("removed"))
		}
	}
	if len(missing) > 0 {
		list := make([]string, 0, len(missing))
		for _, c := range missing {
			list = append(list, c.Name)
		}
		fmt.Println(styleDim.Render("\nNot found on this machine: " + strings.Join(list, ", ")))
	}
	fmt.Println(styleDim.Render("\nServer \"" + cfg.Name + "\": " + bin + " --profile " + cfg.Name + " (stdio)"))
	if failed > 0 {
		return fmt.Errorf("%d client(s) failed", failed)
	}
	return nil
}

// removeProfile unregisters a profile from every client and deletes it.
func removeProfile(env clients.Env, p config.Config) error {
	detected, _ := detectClients(env)
	var failed int
	for _, c := range detected {
		if !c.Installed(env, p.Name) {
			continue
		}
		if err := c.Uninstall(env, p.Name); err != nil {
			failed++
			fmt.Printf("%s %s: %v\n", styleWarn.Render("✗"), c.Name, err)
			continue
		}
		fmt.Printf("%s %s %s\n", styleDim.Render("–"), c.Name, styleDim.Render("removed"))
	}
	if err := config.Delete(p.Name); err != nil {
		return err
	}
	fmt.Println(styleOK.Render("✓") + " Removed profile " + styleBold.Render(p.Name))
	if failed > 0 {
		return fmt.Errorf("%d client(s) failed", failed)
	}
	return nil
}

func detectClients(env clients.Env) (detected, missing []clients.Client) {
	for _, c := range clients.All() {
		if c.Detected(env) {
			detected = append(detected, c)
		} else {
			missing = append(missing, c)
		}
	}
	return detected, missing
}

// executable is os.Executable; tests replace it.
var executable = os.Executable

// binaryPath is the absolute, symlink-free path of the running binary.
func binaryPath() (string, error) {
	exe, err := executable()
	if err != nil {
		return "", err
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}
	if strings.Contains(exe, "go-build") {
		return "", errors.New("setup was started with `go run`; build or install ymcp first so clients get a stable path")
	}
	return exe, nil
}
