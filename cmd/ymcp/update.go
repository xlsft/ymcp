package main

import (
	"context"
	"flag"
	"fmt"
	"net/http"
	"runtime"
	"time"

	"github.com/charmbracelet/huh/spinner"

	"github.com/xlsft/ymcp/internal/update"
)

const repo = "xlsft/ymcp"

func selfUpdate(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("update", flag.ContinueOnError)
	checkOnly := fs.Bool("check", false, "only report whether an update is available")
	if err := fs.Parse(args); err != nil {
		return err
	}
	u := update.Updater{
		API:  "https://api.github.com",
		Repo: repo,
		HTTP: &http.Client{Timeout: 2 * time.Minute},
		OS:   runtime.GOOS,
		Arch: runtime.GOARCH,
	}
	latest, err := u.Latest(ctx)
	if err != nil {
		return fmt.Errorf("check for updates: %w", err)
	}
	if !update.Newer(latest.Tag, version) {
		fmt.Printf("%s ymcp %s is the latest version\n", styleOK.Render("✓"), version)
		return nil
	}
	if *checkOnly {
		fmt.Printf("ymcp %s is available (you have %s): %s\n", latest.Tag, version, latest.URL)
		return nil
	}
	exe, err := binaryPath()
	if err != nil {
		return err
	}
	install := func() { err = u.Install(ctx, latest, exe) }
	if interactive() {
		_ = spinner.New().Title(" Downloading ymcp " + latest.Tag + "…").Action(install).Run()
	} else {
		install()
	}
	if err != nil {
		return err
	}
	fmt.Printf("%s Updated ymcp %s → %s %s\n", styleOK.Render("✓"), version, latest.Tag, styleDim.Render("(restart your MCP clients to pick it up)"))
	return nil
}
