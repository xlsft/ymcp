package main

import (
	"cmp"
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/xlsft/ymcp/internal/config"
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

// logRequests logs each new client and every tool call with its arguments,
// duration and outcome; other methods are logged at debug level.
func logRequests(logger *slog.Logger) mcp.Middleware {
	var seen sync.Map // client + protocol already logged
	return func(next mcp.MethodHandler) mcp.MethodHandler {
		return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
			start := time.Now()
			res, err := next(ctx, method, req)
			took := time.Since(start).Round(time.Millisecond)

			// Clients on the 2026-07-28 protocol skip initialize and send
			// their info with every request, so look at all of them.
			if name, protocol, ok := clientOf(req); ok {
				if _, logged := seen.LoadOrStore(name+"|"+protocol, true); !logged {
					logger.Info("client connected", "client", name, "protocol", protocol)
				}
			}

			switch p := req.GetParams().(type) {
			case *mcp.CallToolParamsRaw:
				args := shorten(redact(p.Arguments), 300)
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

// clientOf names the client behind a request and its protocol version.
func clientOf(req mcp.Request) (name, protocol string, ok bool) {
	r, isServerReq := req.(interface {
		ClientInfo() *mcp.Implementation
		ProtocolVersion() string
	})
	if !isServerReq {
		return "", "", false
	}
	info, protocol := r.ClientInfo(), r.ProtocolVersion()
	if p, isInit := req.GetParams().(*mcp.InitializeParams); isInit {
		info, protocol = p.ClientInfo, p.ProtocolVersion
	}
	if info == nil {
		return "", "", false
	}
	return strings.TrimSpace(info.Name + " " + info.Version), protocol, true
}

func toolText(r *mcp.CallToolResult) string {
	for _, c := range r.Content {
		if t, ok := c.(*mcp.TextContent); ok {
			return shorten(t.Text, 300)
		}
	}
	return ""
}

// secretKey matches argument names whose values never go to the log.
var secretKey = regexp.MustCompile(`(?i)pass|secret|token|authorization|api[-_]?key|credential|cookie`)

// redact renders tool arguments for the log with secrets masked: values
// of secret-looking keys, and the value of objects typed as passwords,
// like Hub's {"type":"PlainpasswordJSON","value":…}.
func redact(raw json.RawMessage) string {
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return string(raw)
	}
	var walk func(any) any
	walk = func(v any) any {
		switch x := v.(type) {
		case map[string]any:
			typ, _ := x["type"].(string)
			for k, val := range x {
				if secretKey.MatchString(k) || (k == "value" && secretKey.MatchString(typ)) {
					if _, nested := val.(map[string]any); !nested {
						x[k] = "***"
						continue
					}
				}
				x[k] = walk(val)
			}
		case []any:
			for i := range x {
				x[i] = walk(x[i])
			}
		}
		return v
	}
	out, err := json.Marshal(walk(v))
	if err != nil {
		return string(raw)
	}
	return string(out)
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
	token := fs.String("token", "", "bearer token HTTP clients must send (YMCP_HTTP_TOKEN also works)")
	only := fs.String("profiles", "", "comma-separated profiles to serve over HTTP (default: all, or --profile)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *token == "" {
		*token = os.Getenv("YMCP_HTTP_TOKEN")
	}

	switch *transport {
	case "stdio":
		if *only != "" {
			return errors.New("--profiles needs --transport http; over stdio pick one with --profile")
		}
		if *token != "" {
			logger.Warn("the HTTP token is ignored over stdio")
		}
		// Servers of several profiles share the log file; tag each line.
		if name, err := config.Resolve(profile); err == nil {
			logger = logger.With("profile", name)
		}
		yt, _, err := client(logger)
		if err != nil {
			logger.Error("can't start", "err", err)
			return err
		}
		logger.Info("ymcp started", "version", version, "transport", "stdio", "youtrack", yt.BaseURL(), "log", logPath)
		defer logger.Info("ymcp stopped")
		return newServer(yt, logger).Run(ctx, &mcp.StdioTransport{})
	case "http":
		ln, err := net.Listen("tcp", *addr)
		if err != nil {
			return err
		}
		// Log the address as given, unless the port was left to the system.
		base := "http://" + *addr
		if _, port, _ := net.SplitHostPort(*addr); port == "0" {
			base = "http://" + ln.Addr().String()
		}
		handler, served, err := httpHandler(base, *token, *only, logger)
		if err != nil {
			ln.Close()
			logger.Error("can't start", "err", err)
			return err
		}
		auth := "none"
		if *token != "" {
			auth = "bearer token"
		} else if host, _, _ := net.SplitHostPort(*addr); !isLoopback(host) {
			logger.Warn("serving without a token on a non-loopback address: anyone who reaches it acts as these YouTrack accounts; set --token or YMCP_HTTP_TOKEN")
		}
		logger.Info("ymcp started", "version", version, "transport", "http", "addr", base, "auth", auth,
			"profiles", strings.Join(served, ", "), "log", cmp.Or(logPath, "off"))
		defer logger.Info("ymcp stopped")

		httpSrv := &http.Server{Handler: handler}
		go func() {
			<-ctx.Done()
			httpSrv.Shutdown(context.Background())
		}()
		if err := httpSrv.Serve(ln); !errors.Is(err, http.ErrServerClosed) {
			return err
		}
		return nil
	default:
		return fmt.Errorf("unknown transport %q", *transport)
	}
}

// httpHandler serves each profile at /<key>, and the default one at / too.
// Profiles that can't sign in are skipped with a warning; it returns the
// keys it serves and logs the URL of each.
func httpHandler(base, token, only string, logger *slog.Logger) (http.Handler, []string, error) {
	names, def, err := httpProfiles(only)
	if err != nil {
		return nil, nil, err
	}
	mux := http.NewServeMux()
	var served []string
	for _, name := range names {
		l := logger.With("profile", name)
		cfg, err := config.Load(name)
		if err == nil && !cfg.Complete() {
			err = errors.New("not configured; run ymcp setup")
		}
		var yt *youtrack.Client
		if err == nil {
			yt, err = youtrack.New(youtrack.Options{BaseURL: cfg.URL, Login: cfg.Login, Password: cfg.Password, Proxy: cfg.Proxy, Logger: l})
		}
		if err != nil {
			l.Warn("profile skipped", "err", err)
			continue
		}
		srv := newServer(yt, l)
		h := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return srv }, nil)
		mux.Handle("/"+name, h)
		mux.Handle("/"+name+"/", h)
		urls := base + "/" + name
		if name == def {
			mux.Handle("/{$}", h)
			urls += " and " + base + "/"
		}
		l.Info("serving profile", "name", cfg.Title(), "youtrack", yt.BaseURL(), "url", urls)
		served = append(served, name)
	}
	if len(served) == 0 {
		return nil, nil, errors.New("no profile to serve; run ymcp setup")
	}
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "no such profile; served: /"+strings.Join(served, ", /"), http.StatusNotFound)
	})
	if token == "" {
		return mux, served, nil
	}
	return requireToken(token, mux, logger), served, nil
}

// httpProfiles lists the profiles to serve and the default one (served at
// /, empty if none): --profile alone, the --profiles list, or every stored
// profile; with none stored, the one YOUTRACK_* variables describe.
func httpProfiles(only string) (names []string, def string, err error) {
	switch {
	case profile != "":
		return []string{profile}, profile, nil
	case only != "":
		for _, n := range strings.Split(only, ",") {
			if n = strings.TrimSpace(n); n != "" {
				names = append(names, n)
			}
		}
	default:
		list, err := config.Profiles()
		if err != nil {
			return nil, "", err
		}
		for _, p := range list {
			names = append(names, p.Name)
		}
		if len(names) == 0 {
			names = []string{config.DefaultProfile}
		}
	}
	if key, err := config.Resolve(""); err == nil && slices.Contains(names, key) {
		def = key
	} else if len(names) == 1 {
		def = names[0]
	}
	return names, def, nil
}

// requireToken rejects requests without "Authorization: Bearer <token>".
func requireToken(token string, next http.Handler, logger *slog.Logger) http.Handler {
	want := []byte("Bearer " + token)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), want) != 1 {
			logger.Warn("rejected HTTP request without a valid token", "remote", r.RemoteAddr, "path", r.URL.Path)
			w.Header().Set("WWW-Authenticate", `Bearer realm="ymcp"`)
			http.Error(w, "missing or wrong bearer token", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func isLoopback(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
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
	yt, _, err := client(logger)
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
