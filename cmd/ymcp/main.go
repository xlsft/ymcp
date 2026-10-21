// Command ymcp is an MCP server for YouTrack.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/xlsft/ymcp/internal/config"
	"github.com/xlsft/ymcp/internal/youtrack"
)

// version is set at build time by the release pipeline.
var version = "dev"

const usage = `ymcp — YouTrack MCP server

Usage:
  ymcp [global flags] [command] [arguments]

Commands:
  serve     Run the MCP server (default). Flags: --transport stdio|http, --addr
  auth      Set or replace the YouTrack URL, login and password
  setup     Configure credentials and register ymcp in MCP clients
  check     Log in and print the current user
  call      Call one tool: ymcp call <tool> ['{"json":"args"}']
  logs      Show the server log; -f follows it, -n N sets the lines, --path prints where it is
  update    Update ymcp to the latest release
  version   Print the version

Global flags:
  --env FILE   Load YOUTRACK_* variables from a dotenv file
  -v           Debug logging, including every YouTrack request (YMCP_LOG_LEVEL also works)
`

func main() {
	if err := run(os.Args[1:]); err != nil {
		if !errors.Is(err, flag.ErrHelp) {
			fmt.Fprintln(os.Stderr, "ymcp:", err)
		}
		os.Exit(1)
	}
}

func run(args []string) error {
	global := flag.NewFlagSet("ymcp", flag.ContinueOnError)
	global.SetOutput(io.Discard)
	envFile := global.String("env", "", "")
	verbose := global.Bool("v", false, "")
	if err := global.Parse(args); err != nil {
		fmt.Fprint(os.Stderr, usage)
		return err
	}
	if *envFile != "" {
		if err := loadEnvFile(*envFile); err != nil {
			return err
		}
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	cmd, rest := "serve", global.Args()
	if len(rest) > 0 {
		cmd, rest = rest[0], rest[1:]
	}

	// The server logs what it does to stderr and the log file; interactive
	// commands stay quiet so log lines don't break their forms.
	serving := cmd == "serve" || cmd == "call"
	level := logLevel(*verbose)
	if !serving && !*verbose {
		level = slog.LevelWarn
	}
	logger, logPath, closeLog := newLogger(level, serving)
	defer closeLog()
	// Late replies to the terminal queries made at startup would otherwise
	// end up in a form, in the MCP stream or at the shell prompt.
	if interactive() {
		drainTerminalReplies()
	}
	switch cmd {
	case "serve":
		return serve(ctx, rest, logger, logPath)
	case "auth":
		return auth(ctx, rest)
	case "setup":
		return setup(ctx, rest)
	case "update":
		return selfUpdate(ctx, rest)
	case "check":
		return check(ctx)
	case "call":
		return call(ctx, rest, logger)
	case "logs":
		return logs(ctx, rest)
	case "version", "--version":
		fmt.Println("ymcp", version)
		return nil
	case "help", "-h", "--help":
		fmt.Print(usage)
		return nil
	default:
		fmt.Fprint(os.Stderr, usage)
		return fmt.Errorf("unknown command %q", cmd)
	}
}

// client builds a YouTrack client from the stored config and environment.
func client(logger *slog.Logger) (*youtrack.Client, error) {
	cfg, err := config.Load()
	if err != nil {
		return nil, err
	}
	if !cfg.Complete() {
		return nil, errors.New("YouTrack connection is not configured; run `ymcp auth` (or set YOUTRACK_URL, YOUTRACK_LOGIN, YOUTRACK_PASSWORD)")
	}
	return youtrack.New(youtrack.Options{BaseURL: cfg.URL, Login: cfg.Login, Password: cfg.Password, Proxy: cfg.Proxy, Logger: logger})
}

func check(ctx context.Context) error {
	yt, err := client(nil)
	if err != nil {
		return err
	}
	me, err := currentUser(ctx, yt)
	if err != nil {
		return err
	}
	fmt.Printf("ok: logged in to %s as %s (%s)\n", yt.BaseURL(), me.Login, me.FullName)
	return nil
}

type user struct{ Login, FullName string }

func currentUser(ctx context.Context, yt *youtrack.Client) (user, error) {
	var me user
	err := yt.Get(ctx, "users/me", youtrack.Fields("login,fullName"), &me)
	return me, err
}
