package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/xlsft/ymcp/internal/tools"
	"github.com/xlsft/ymcp/internal/youtrack"
)

func newServer(yt *youtrack.Client, logger *slog.Logger) *mcp.Server {
	// The SDK logs session plumbing at info; keep that for -v only.
	sdkLogger := logger
	if !logger.Enabled(context.Background(), slog.LevelDebug) {
		sdkLogger = slog.New(atLeast{logger.Handler(), slog.LevelWarn})
	}
	s := mcp.NewServer(&mcp.Implementation{Name: "ymcp", Version: version}, &mcp.ServerOptions{
		Instructions: "Tools for the YouTrack instance at " + yt.BaseURL() + ". Issues are addressed by readable ID (e.g. PRJ-123).",
		Logger:       sdkLogger,
	})
	tools.Register(s, yt)
	s.AddReceivingMiddleware(logRequests(logger))
	return s
}

// logRequests logs connecting clients and every tool call with its
// arguments, duration and outcome; other methods are logged at debug level.
func logRequests(logger *slog.Logger) mcp.Middleware {
	return func(next mcp.MethodHandler) mcp.MethodHandler {
		return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
			start := time.Now()
			res, err := next(ctx, method, req)
			took := time.Since(start).Round(time.Millisecond)

			switch p := req.GetParams().(type) {
			case *mcp.InitializeParams:
				client := "unknown"
				if p.ClientInfo != nil {
					client = strings.TrimSpace(p.ClientInfo.Name + " " + p.ClientInfo.Version)
				}
				logger.Info("client connected", "client", client, "protocol", p.ProtocolVersion)
			case *mcp.CallToolParamsRaw:
				args := shorten(string(p.Arguments), 300)
				switch r, _ := res.(*mcp.CallToolResult); {
				case err != nil:
					logger.Error("tool failed", "tool", p.Name, "args", args, "took", took, "err", err)
				case r != nil && r.IsError:
					logger.Warn("tool returned an error", "tool", p.Name, "args", args, "took", took, "err", toolText(r))
				default:
					logger.Info("tool", "tool", p.Name, "args", args, "took", took)
				}
			default:
				if err != nil {
					logger.Warn("request failed", "method", method, "took", took, "err", err)
				} else {
					logger.Debug("request", "method", method, "took", took)
				}
			}
			return res, err
		}
	}
}

func toolText(r *mcp.CallToolResult) string {
	for _, c := range r.Content {
		if t, ok := c.(*mcp.TextContent); ok {
			return shorten(t.Text, 300)
		}
	}
	return ""
}

func shorten(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n]) + "…"
	}
	return s
}

func serve(ctx context.Context, args []string, logger *slog.Logger, logPath string) error {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	transport := fs.String("transport", "stdio", "MCP transport: stdio or http")
	addr := fs.String("addr", "127.0.0.1:8090", "listen address for --transport=http")
	if err := fs.Parse(args); err != nil {
		return err
	}
	yt, err := client()
	if err != nil {
		logger.Error("can't start", "err", err)
		return err
	}
	logger.Info("ymcp started", "version", version, "transport", *transport, "youtrack", yt.BaseURL(), "log", logPath)
	defer logger.Info("ymcp stopped")

	switch *transport {
	case "stdio":
		return newServer(yt, logger).Run(ctx, &mcp.StdioTransport{})
	case "http":
		srv := newServer(yt, logger)
		handler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return srv }, nil)
		httpSrv := &http.Server{Addr: *addr, Handler: handler}
		go func() {
			<-ctx.Done()
			httpSrv.Shutdown(context.Background())
		}()
		logger.Info("listening", "addr", "http://"+*addr)
		if err := httpSrv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
			return err
		}
		return nil
	default:
		return fmt.Errorf("unknown transport %q", *transport)
	}
}

// call runs a single tool through an in-memory MCP session, so the result
// is exactly what a client would get.
func call(ctx context.Context, args []string, logger *slog.Logger) error {
	if len(args) == 0 || len(args) > 2 {
		return errors.New(`usage: ymcp call <tool> ['{"json":"args"}']`)
	}
	name, rawArgs := args[0], "{}"
	if len(args) == 2 {
		rawArgs = args[1]
	}
	var toolArgs map[string]any
	if err := json.Unmarshal([]byte(rawArgs), &toolArgs); err != nil {
		return fmt.Errorf("tool arguments: %w", err)
	}
	yt, err := client()
	if err != nil {
		return err
	}

	ct, st := mcp.NewInMemoryTransports()
	if _, err := newServer(yt, logger).Connect(ctx, st, nil); err != nil {
		return err
	}
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "ymcp-call", Version: version}, nil).Connect(ctx, ct, nil)
	if err != nil {
		return err
	}
	defer cs.Close()

	res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: toolArgs})
	if err != nil {
		return err
	}
	for _, c := range res.Content {
		switch c := c.(type) {
		case *mcp.TextContent:
			fmt.Println(c.Text)
		case *mcp.ImageContent:
			fmt.Fprintf(os.Stderr, "[image %s, %d bytes]\n", c.MIMEType, len(c.Data))
		}
	}
	if res.IsError {
		return fmt.Errorf("tool %s failed", name)
	}
	return nil
}
