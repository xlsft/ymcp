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

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/xlsft/ymcp/internal/tools"
	"github.com/xlsft/ymcp/internal/youtrack"
)

func newServer(yt *youtrack.Client, logger *slog.Logger) *mcp.Server {
	s := mcp.NewServer(&mcp.Implementation{Name: "ymcp", Version: version}, &mcp.ServerOptions{
		Instructions: "Tools for the YouTrack instance at " + yt.BaseURL() + ". Issues are addressed by readable ID (e.g. PRJ-123).",
		Logger:       logger,
	})
	tools.Register(s, yt)
	return s
}

func serve(ctx context.Context, args []string, logger *slog.Logger) error {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	transport := fs.String("transport", "stdio", "MCP transport: stdio or http")
	addr := fs.String("addr", "127.0.0.1:8090", "listen address for --transport=http")
	if err := fs.Parse(args); err != nil {
		return err
	}
	yt, err := client()
	if err != nil {
		return err
	}

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
		logger.Warn("listening", "addr", "http://"+*addr, "youtrack", yt.BaseURL())
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
