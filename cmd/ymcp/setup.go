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

func setup(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("setup", flag.ContinueOnError)
	reauth := fs.Bool("reauth", false, "ask for credentials even if some are saved")
	only := fs.String("clients", "", "comma-separated client IDs to register, without asking")
	all := fs.Bool("all", false, "register in every detected client, without asking")
	if err := fs.Parse(args); err != nil {
		return err
	}

	fmt.Println()
	fmt.Println(styleTitle.Render("ymcp") + " " + styleBold.Render("YouTrack MCP setup"))
	fmt.Println()

	// 1. Credentials.
	cfg, err := config.Load("")
	if err != nil {
		return err
	}
	keep := cfg.Complete() && !*reauth
	if keep && interactive() && *only == "" && !*all {
		if err := huh.NewConfirm().
			Title(fmt.Sprintf("Keep the saved connection %s@%s?", cfg.Login, strings.TrimPrefix(strings.TrimPrefix(cfg.URL, "https://"), "http://"))).
			Affirmative("Keep").Negative("Change").
			Value(&keep).WithTheme(huh.ThemeBase16()).Run(); err != nil {
			return err
		}
	}
	if !keep {
		if _, err := authenticate(ctx, cfg, true); err != nil {
			return err
		}
	}

	// 2. Clients.
	bin, err := binaryPath()
	if err != nil {
		return err
	}
	env, err := clients.System(runtime.GOOS)
	if err != nil {
		return err
	}
	var detected, missing []clients.Client
	for _, c := range clients.All() {
		if c.Detected(env) {
			detected = append(detected, c)
		} else {
			missing = append(missing, c)
		}
	}
	if len(detected) == 0 {
		fmt.Println(styleWarn.Render("!") + " No MCP clients found. Register " + bin + " as a stdio server manually.")
		return nil
	}

	var chosen []string
	switch {
	case *only != "":
		chosen = strings.Split(*only, ",")
	case *all:
		for _, c := range detected {
			chosen = append(chosen, c.ID)
		}
	case interactive():
		anyInstalled := slices.ContainsFunc(detected, func(c clients.Client) bool { return c.Installed(env) })
		opts := make([]huh.Option[string], 0, len(detected))
		for _, c := range detected {
			label := c.Name
			installed := c.Installed(env)
			if installed {
				label += styleDim.Render("  · added")
			}
			opts = append(opts, huh.NewOption(label, c.ID).Selected(installed || !anyInstalled))
		}
		if err := huh.NewMultiSelect[string]().
			Title("Register ymcp in").
			Description("space to toggle · enter to confirm · unchecked clients get ymcp removed").
			Options(opts...).
			Value(&chosen).WithTheme(huh.ThemeBase16()).Run(); err != nil {
			return err
		}
	default:
		return errors.New("not a terminal: pass --clients id1,id2 or --all")
	}

	// 3. Apply.
	fmt.Println()
	var failed int
	for _, c := range detected {
		want := slices.Contains(chosen, c.ID)
		had := c.Installed(env)
		switch {
		case want:
			if err := c.Install(env, bin); err != nil {
				failed++
				fmt.Printf("%s %s: %v\n", styleWarn.Render("✗"), c.Name, err)
				continue
			}
			note := ""
			if c.Note != "" {
				note = styleDim.Render(" — " + c.Note)
			}
			fmt.Printf("%s %s%s\n", styleOK.Render("✓"), c.Name, note)
		case had:
			if err := c.Uninstall(env); err != nil {
				failed++
				fmt.Printf("%s %s: %v\n", styleWarn.Render("✗"), c.Name, err)
				continue
			}
			fmt.Printf("%s %s %s\n", styleDim.Render("–"), c.Name, styleDim.Render("removed"))
		}
	}
	if len(missing) > 0 {
		names := make([]string, 0, len(missing))
		for _, c := range missing {
			names = append(names, c.Name)
		}
		fmt.Println(styleDim.Render("\nNot found on this machine: " + strings.Join(names, ", ")))
	}
	fmt.Println(styleDim.Render("\nServer: " + bin + " (stdio, name \"" + clients.ServerName + "\")"))
	if failed > 0 {
		return fmt.Errorf("%d client(s) failed", failed)
	}
	return nil
}

// binaryPath is the absolute, symlink-free path of the running binary.
func binaryPath() (string, error) {
	exe, err := os.Executable()
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
