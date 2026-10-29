package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"runtime"
	"strings"
	"time"

	"github.com/charmbracelet/huh/spinner"
	"golang.org/x/term"

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
		printChanges(ctx, os.Stdout, u, latest.Tag)
		return nil
	}
	exe, err := binaryPath()
	if err != nil {
		return err
	}
	// A package manager keeps track of its files; replacing the binary
	// behind its back would leave it confused.
	if how := install.UpdateCommand(exe); how != "ymcp update" {
		fmt.Printf("ymcp %s is available (you have %s). This copy is managed elsewhere; update it with: %s\n", latest.Tag, version, how)
		printChanges(ctx, os.Stdout, u, latest.Tag)
		return nil
	}
	replace := func() { err = u.Install(ctx, latest, exe) }
	if interactive() {
		_ = spinner.New().Title(" Downloading ymcp " + latest.Tag + "…").Action(replace).Run()
	} else {
		replace()
	}
	if err != nil {
		return err
	}
	fmt.Printf("%s Updated ymcp %s → %s %s\n", styleOK.Render("✓"), version, latest.Tag, styleDim.Render("(restart your MCP clients to pick it up)"))
	printChanges(ctx, os.Stdout, u, latest.Tag)
	return nil
}

// changelogURL is the site's page of what every release changed.
const changelogURL = "https://ymcp.su/changelog"

// printChanges prints what each release after this version, up to latest,
// changed for users, from the What's new of their release notes, and then
// where the whole changelog is. Without the notes (a development build, or
// GitHub didn't answer) it prints only where the changelog is.
func printChanges(ctx context.Context, w io.Writer, u update.Updater, latest string) {
	if version != "dev" {
		ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
		defer cancel()
		if notes, err := u.Changes(ctx, version, latest); err == nil && len(notes) > 0 {
			fmt.Fprint(w, formatChanges(notes, version, termWidth()))
		}
	}
	fmt.Fprintf(w, "\n%s %s#%s\n", styleDim.Render("Changelog:"), changelogURL, latest)
}

// formatChanges lays the notes out for a terminal width columns wide: a
// heading per release, its sections, and each entry as a bullet wrapped
// under itself, with `code` styled.
func formatChanges(notes []update.Notes, since string, width int) string {
	var b strings.Builder
	fmt.Fprintf(&b, "\n%s\n", styleBold.Render("What's new since "+strings.TrimPrefix(since, "v")))
	for _, n := range notes {
		fmt.Fprintf(&b, "\n%s\n", styleTitle.Render(n.Tag))
		for _, sec := range n.Sections {
			fmt.Fprintf(&b, "  %s\n", styleDim.Render(sec.Title))
			for _, e := range sec.Entries {
				for i, line := range wrapEntry(e, width-6) {
					lead := "    "
					if i == 0 {
						lead = "  • "
					}
					b.WriteString(lead + line + "\n")
				}
			}
		}
	}
	return b.String()
}

// wrapEntry breaks a changelog entry into lines of at most width
// characters, at spaces, rendering the text of `code` spans in styleCode.
// Punctuation next to a span stays on its line, and a word longer than a
// line (a URL, a path) is left whole so it can still be copied.
func wrapEntry(text string, width int) []string {
	var lines []string
	var line, word strings.Builder
	n, wn := 0, 0 // the plain widths of line and word
	code := false
	flush := func() {
		if wn == 0 {
			return
		}
		if n > 0 && n+1+wn > width {
			lines = append(lines, line.String())
			line.Reset()
			n = 0
		}
		if n > 0 {
			line.WriteByte(' ')
			n++
		}
		line.WriteString(word.String())
		n += wn
		word.Reset()
		wn = 0
	}
	run := []rune(text)
	for i := 0; i < len(run); {
		switch run[i] {
		case '`':
			code = !code
			i++
		case ' ':
			flush()
			i++
		default:
			j := i
			for j < len(run) && run[j] != '`' && run[j] != ' ' {
				j++
			}
			piece := string(run[i:j])
			if code {
				piece = styleCode.Render(piece)
			}
			word.WriteString(piece)
			wn += j - i
			i = j
		}
	}
	flush()
	if n > 0 {
		lines = append(lines, line.String())
	}
	return lines
}

// termWidth is the width of the terminal on stdout, kept between 40 and
// 100 columns so notes read well; 80 when stdout isn't a terminal.
func termWidth() int {
	w, _, err := term.GetSize(int(os.Stdout.Fd()))
	if err != nil || w <= 0 {
		return 80
	}
	return min(max(w, 40), 100)
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
			exe, _ := os.Executable()
			how := install.UpdateCommand(exe)
			logger.Warn("update available", "current", version, "latest", strings.TrimPrefix(tag, "v"), "run", how)
		default:
			logger.Debug("ymcp is up to date", "version", version)
		}
	}()
	return done
}
