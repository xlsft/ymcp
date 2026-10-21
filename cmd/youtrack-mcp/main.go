// Command youtrack-mcp is an MCP server for a self-hosted YouTrack instance.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/xlsft/youtrack-mcp/internal/tools"
	"github.com/xlsft/youtrack-mcp/internal/youtrack"
)

var version = "dev"

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "youtrack-mcp:", err)
		os.Exit(1)
	}
}

func run() error {
	transport := flag.String("transport", "stdio", "MCP transport: stdio or http")
	addr := flag.String("addr", "127.0.0.1:8090", "listen address for -transport=http")
	envFile := flag.String("env", "", "load YOUTRACK_* variables from this file (existing env wins)")
	check := flag.Bool("check", false, "log in, print the current user and exit")
	verbose := flag.Bool("v", false, "verbose logging")
	flag.Parse()

	if *envFile != "" {
		if err := loadEnvFile(*envFile); err != nil {
			return err
		}
	}

	// stdout belongs to the stdio transport, so logs go to stderr.
	level := slog.LevelWarn
	if *verbose {
		level = slog.LevelInfo
	}
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))

	yt, err := youtrack.New(youtrack.Options{
		BaseURL:  os.Getenv("YOUTRACK_URL"),
		Login:    os.Getenv("YOUTRACK_LOGIN"),
		Password: os.Getenv("YOUTRACK_PASSWORD"),
		Proxy:    os.Getenv("YOUTRACK_PROXY"),
	})
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if *check {
		var me struct{ Login, FullName string }
		if err := yt.Get(ctx, "users/me", youtrack.Fields("login,fullName"), &me); err != nil {
			return err
		}
		fmt.Printf("ok: logged in to %s as %s (%s)\n", yt.BaseURL(), me.Login, me.FullName)
		return nil
	}

	newServer := func() *mcp.Server {
		s := mcp.NewServer(&mcp.Implementation{Name: "youtrack-mcp", Version: version}, &mcp.ServerOptions{
			Instructions: "Tools for the YouTrack instance at " + yt.BaseURL() + ". Issues are addressed by readable ID (e.g. PRJ-123).",
			Logger:       logger,
		})
		tools.Register(s, yt)
		return s
	}

	switch *transport {
	case "stdio":
		return newServer().Run(ctx, &mcp.StdioTransport{})
	case "http":
		srv := newServer()
		handler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return srv }, nil)
		httpSrv := &http.Server{Addr: *addr, Handler: handler}
		go func() {
			<-ctx.Done()
			httpSrv.Shutdown(context.Background())
		}()
		logger.Warn("listening", "addr", "http://"+*addr, "youtrack", yt.BaseURL())
		if err := httpSrv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
			return err
		}
		return nil
	default:
		return fmt.Errorf("unknown transport %q", *transport)
	}
}
