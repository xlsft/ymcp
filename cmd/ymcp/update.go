package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"runtime"
	"strings"
	"time"

	"github.com/charmbracelet/huh/spinner"

	"github.com/xlsft/ymcp/internal/install"
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

// releasesWeb is where the start-up check looks for the latest release.
var releasesWeb = "https://github.com"

// checkForUpdate logs, in the background, whether a newer release exists.
// It never delays or fails the start; YMCP_UPDATE_CHECK=off turns it off.
func checkForUpdate(ctx context.Context, logger *slog.Logger) <-chan struct{} {
	done := make(chan struct{})
	if version == "dev" || strings.EqualFold(os.Getenv("YMCP_UPDATE_CHECK"), "off") {
		close(done)
		return done
	}
	go func() {
		defer close(done)
		ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		u := update.Updater{Repo: repo, HTTP: &http.Client{}}
		tag, err := u.LatestTag(ctx, releasesWeb)
		switch {
		case err != nil:
			logger.Debug("can't check for updates", "err", err)
		case update.Newer(tag, version):
			how := "ymcp update"
			if install.Docker() {
				how = "docker pull xlsoftware/ymcp:latest"
			}
			logger.Warn("update available", "current", version, "latest", strings.TrimPrefix(tag, "v"), "run", how)
		default:
			logger.Debug("ymcp is up to date", "version", version)
		}
	}()
	return done
}
