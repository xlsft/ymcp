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

	"github.com/xlsft/ymcp/internal/clients"
	"github.com/xlsft/ymcp/internal/config"
	"github.com/xlsft/ymcp/internal/tools"
)

func setup(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("setup", flag.ContinueOnError)
	reauth := fs.Bool("reauth", false, "ask for credentials even if some are saved (the TUI always can)")
	only := fs.String("clients", "", "comma-separated client IDs to register, without asking")
	all := fs.Bool("all", false, "register in every detected client, without asking")
	name := fs.String("name", "", `profile display name, e.g. "YouTrack (work)"; its key is the MCP server name`)
	remove := fs.Bool("remove", false, "remove the --profile profile and its client registrations")
	toolsFlag := fs.String("tools", "", `tool groups or tools the profile serves, e.g. "issues,articles"; "all" for every tool`)
	confirmFlag := fs.String("confirm", "", `"on" or "off": whether the profile's destructive tools ask for the user's confirmation first (default on)`)
	if err := fs.Parse(args); err != nil {
		return err
	}
	confirm, err := parseSwitch(*confirmFlag)
	if err != nil {
		return fmt.Errorf("--confirm: %w", err)
	}
	if interactive() && *only == "" && !*all && !*remove {
		// The TUI asks for everything; --reauth needs nothing more.
		if *name != "" || *toolsFlag != "" || *confirmFlag != "" {
			return errors.New("--name, --tools and --confirm go with --clients or --all; without them setup opens its TUI, which has all of these")
		}
		return setupTUI(ctx)
	}

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
	current, removing, err := pickProfile(profiles, *remove)
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

	// 4. Its tools, and whether the destructive ones ask first.
	changed, err := pickTools(&cfg, *toolsFlag)
	if err != nil {
		return err
	}
	if confirm != nil && *confirm != cfg.Confirms() {
		cfg.Confirm, changed = confirmSetting(*confirm), true
	}
	if changed {
		if _, err := config.Save(cfg, ""); err != nil {
			return err
		}
	}

	// 5. The clients it is registered in.
	return registerProfile(env, cfg, oldName, *only, *all)
}

// pickTools sets the tools the profile serves from --tools and reports
// whether they changed. Nil means all tools.
func pickTools(cfg *config.Config, spec string) (bool, error) {
	var chosen []string
	switch spec {
	case "":
		return false, nil
	case "all":
	default:
		for _, it := range strings.Split(spec, ",") {
			if it = strings.TrimSpace(it); it != "" {
				chosen = append(chosen, it)
			}
		}
		if _, err := tools.Select(chosen); err != nil {
			return false, err
		}
	}
	changed := !slices.Equal(chosen, cfg.Tools)
	cfg.Tools = chosen
	return changed, nil
}

// confirmSetting is how a profile stores the confirmation switch: nil
// for the default, on.
func confirmSetting(on bool) *bool {
	if on {
		return nil
	}
	return &on
}

// pickProfile chooses the profile to set up: the --profile one or the
// only one. An empty Name means a new profile.
func pickProfile(profiles []config.Config, remove bool) (config.Config, bool, error) {
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
	key, err := config.Resolve("")
	return find(key), false, err
}

// registerProfile adds cfg's server to the clients --clients or --all
// name and removes it, and the server of its old name after a rename,
// from the others.
func registerProfile(env clients.Env, cfg config.Config, oldName, only string, all bool) error {
	detected, missing := detectClients(env)
	var chosen []string
	switch {
	case only != "":
		chosen = strings.Split(only, ",")
	case all:
		for _, c := range detected {
			chosen = append(chosen, c.ID)
		}
	default:
		return errors.New("not a terminal: pass --clients id1,id2 or --all")
	}
	fmt.Println()
	lines, failed, err := register(env, cfg, oldName, chosen, false)
	if err != nil {
		return err
	}
	for _, l := range lines {
		fmt.Println(l)
	}
	if len(missing) > 0 && len(detected) > 0 {
		list := make([]string, 0, len(missing))
		for _, c := range missing {
			list = append(list, c.Name)
		}
		fmt.Println(styleDim.Render("\nNot found on this machine: " + strings.Join(list, ", ")))
	}
	if failed > 0 {
		return fmt.Errorf("%d client(s) failed", failed)
	}
	return nil
}

// register adds cfg's server to the chosen clients and removes it, and the
// server of its old name after a rename, from the other detected ones.
// quit lets it quit and reopen running clients that would drop the
// change. It returns what happened, line by line, and how many clients
// failed.
func register(env clients.Env, cfg config.Config, oldName string, chosen []string, quit bool) (lines []string, failed int, err error) {
	bin, err := binaryPath()
	if err != nil {
		return nil, 0, err
	}
	server := clients.Server{Name: cfg.Name, Command: bin, Args: []string{"--profile", cfg.Name}}
	names := []string{cfg.Name}
	if oldName != "" && oldName != cfg.Name {
		names = append(names, oldName)
	}
	installed := func(c clients.Client) bool {
		return slices.ContainsFunc(names, func(n string) bool { return c.Installed(env, n) })
	}

	detected, _ := detectClients(env)
	if len(detected) == 0 {
		return []string{styleWarn.Render("!") + " No MCP clients found. Register " + bin + " --profile " + cfg.Name + " as a stdio server manually."}, 0, nil
	}

	reopen, closed := closeApps(env, detected, func(c clients.Client) bool { return slices.Contains(chosen, c.ID) || installed(c) }, quit)
	lines = append(lines, closed...)
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
			lines = append(lines, fmt.Sprintf("%s %s: %v", styleWarn.Render("✗"), c.Name, err))
			continue
		}
		switch {
		case want && c.Note != "":
			lines = append(lines, fmt.Sprintf("%s %s%s", styleOK.Render("✓"), c.Name, styleDim.Render(" — "+c.Note)))
		case want:
			lines = append(lines, fmt.Sprintf("%s %s", styleOK.Render("✓"), c.Name))
		default:
			lines = append(lines, fmt.Sprintf("%s %s %s", styleDim.Render("–"), c.Name, styleDim.Render("removed")))
		}
	}
	lines = append(lines, reopenApps(env, reopen)...)
	lines = append(lines, styleDim.Render("Server \""+cfg.Name+"\": "+bin+" --profile "+cfg.Name+" (stdio)"))
	return lines, failed, nil
}

// removeProfile unregisters a profile from every client and deletes it.
func removeProfile(env clients.Env, p config.Config) error {
	lines, failed, err := unregister(env, p.Name, false)
	for _, l := range lines {
		fmt.Println(l)
	}
	if err != nil {
		return err
	}
	if failed > 0 {
		return fmt.Errorf("%d client(s) failed", failed)
	}
	return nil
}

// unregister removes a profile from every client and deletes it; quit
// is as for register.
func unregister(env clients.Env, key string, quit bool) (lines []string, failed int, err error) {
	detected, _ := detectClients(env)
	reopen, closed := closeApps(env, detected, func(c clients.Client) bool { return c.Installed(env, key) }, quit)
	lines = append(lines, closed...)
	for _, c := range detected {
		if !c.Installed(env, key) {
			continue
		}
		if err := c.Uninstall(env, key); err != nil {
			failed++
			lines = append(lines, fmt.Sprintf("%s %s: %v", styleWarn.Render("✗"), c.Name, err))
			continue
		}
		lines = append(lines, fmt.Sprintf("%s %s %s", styleDim.Render("–"), c.Name, styleDim.Render("removed")))
	}
	lines = append(lines, reopenApps(env, reopen)...)
	if err := config.Delete(key); err != nil {
		return lines, failed, err
	}
	return append(lines, styleOK.Render("✓")+" Removed profile "+styleBold.Render(key)), failed, nil
}

// closeApps handles clients that write their config back from memory
// while running, which would drop ymcp's edit: with quit it quits those
// about to be edited and returns the ones to reopen afterwards; without,
// it says to quit them.
func closeApps(env clients.Env, list []clients.Client, edited func(clients.Client) bool, quit bool) (reopen []clients.Client, lines []string) {
	for _, c := range list {
		if c.App() == "" || !edited(c) || !c.Running(env) {
			continue
		}
		why := c.Name + " is running and writes its config back from memory, which drops ymcp's change"
		switch {
		case insideApp(c.App()):
			lines = append(lines, styleWarn.Render("!")+" "+why+". Quit it and run ymcp setup from another terminal.")
		case !quit:
			lines = append(lines, styleWarn.Render("!")+" "+why+". Quit it, then run ymcp setup again.")
		default:
			if err := c.Quit(env); err != nil {
				lines = append(lines, fmt.Sprintf("%s %s: %v", styleWarn.Render("!"), why, err))
				continue
			}
			reopen = append(reopen, c)
		}
	}
	return reopen, lines
}

func reopenApps(env clients.Env, list []clients.Client) (lines []string) {
	for _, c := range list {
		if err := c.Open(env); err != nil {
			lines = append(lines, fmt.Sprintf("%s %s: %v", styleWarn.Render("!"), c.Name, err))
			continue
		}
		lines = append(lines, fmt.Sprintf("%s %s %s", styleOK.Render("✓"), c.Name, styleDim.Render("reopened")))
	}
	return lines
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
