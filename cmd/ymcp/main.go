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
	"github.com/xlsft/ymcp/internal/install"
	"github.com/xlsft/ymcp/internal/youtrack"
)

// version is set at build time by the release pipeline.
var version = "dev"

// profile is the --profile flag: which stored YouTrack connection to use.
var profile string

const usage = `ymcp — YouTrack MCP server

Usage:
  ymcp [global flags] [command] [arguments]

Commands:
  serve     Run the MCP server (default). Flags: --transport stdio|http, --addr, --token, --profiles, --tools
  auth      Set or replace the URL, login and password of a profile
  setup     Add, edit or remove YouTrack profiles and register them in MCP clients
  check     Log in and print the current user
  call      Call one tool: ymcp call <tool> ['{"json":"args"}']
  logs      Show the server log; -f follows it, -n N sets the lines, --path prints where it is
  health    Check that a running HTTP server answers; --url, default http://127.0.0.1:8090
  update    Update ymcp to the latest release
  version   Print the version

Global flags:
  --profile KEY  Use this YouTrack profile (YMCP_PROFILE also works); see setup
  --env FILE     Load YOUTRACK_* variables from a dotenv file
  -v             Debug logging, including every YouTrack request (YMCP_LOG_LEVEL also works)
  --version      Print the version, like the version command
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
	showVersion := global.Bool("version", false, "")
	global.StringVar(&profile, "profile", "", "")
	if err := global.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			fmt.Print(usage)
			return nil
		}
		fmt.Fprint(os.Stderr, usage)
		return err
	}
	// Package managers check an install by running ymcp --version, which
	// must work without a profile.
	if *showVersion {
		fmt.Println("ymcp", version)
		return nil
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
	case "health":
		return health(ctx, rest)
	case "version":
		fmt.Println("ymcp", version)
		return nil
	case "help":
		fmt.Print(usage)
		return nil
	default:
		fmt.Fprint(os.Stderr, usage)
		return fmt.Errorf("unknown command %q", cmd)
	}
}

// client builds a YouTrack client from the selected profile and the
// environment, returning the profile with it.
func client(logger *slog.Logger) (*youtrack.Client, config.Config, error) {
	cfg, err := config.Load(profile)
	if err != nil {
		return nil, cfg, err
	}
	if !cfg.Complete() {
		return nil, cfg, fmt.Errorf("YouTrack profile %q is not configured; run `%s` (or set YOUTRACK_URL, YOUTRACK_LOGIN, YOUTRACK_PASSWORD)", cfg.Name, install.AddProfile(nonDefault(cfg.Name)))
	}
	yt, err := youtrack.New(youtrack.Options{BaseURL: cfg.URL, Login: cfg.Login, Password: cfg.Password, Proxy: cfg.Proxy, Logger: logger})
	return yt, cfg, err
}

func check(ctx context.Context) error {
	yt, cfg, err := client(nil)
	if err != nil {
		return err
	}
	name := cfg.Name
	me, err := currentUser(ctx, yt)
	if err != nil {
		return err
	}
	fmt.Printf("ok: %s: logged in to %s as %s (%s)\n", name, yt.BaseURL(), me.Login, me.FullName)
	return nil
}

type user struct{ Login, FullName string }

func currentUser(ctx context.Context, yt *youtrack.Client) (user, error) {
	var me user
	err := yt.Get(ctx, "users/me", youtrack.Fields("login,fullName"), &me)
	return me, err
}

// nonDefault is key, or "" for the default profile, which commands pick
// without --profile.
func nonDefault(key string) string {
	if key == config.DefaultProfile {
		return ""
	}
	return key
}
