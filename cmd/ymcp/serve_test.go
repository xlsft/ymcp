package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/xlsft/ymcp/internal/config"
	"github.com/xlsft/ymcp/internal/youtrack"
	"github.com/xlsft/ymcp/internal/youtrack/youtracktest"
)

func TestLogsClientAndToolCalls(t *testing.T) {
	f := youtracktest.New(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"login":"alice","fullName":"Alice"}`))
	}))
	yt, err := youtrack.New(youtrack.Options{BaseURL: f.URL, Login: youtracktest.Login, Password: youtracktest.Password, Proxy: "none"})
	if err != nil {
		t.Fatal(err)
	}
	var logs bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logs, nil))

	ctx := context.Background()
	ct, st := mcp.NewInMemoryTransports()
	if _, err := newServer(yt, logger, nil).Connect(ctx, st, nil); err != nil {
		t.Fatal(err)
	}
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "9.9"}, nil).Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close()
	for range 2 {
		if _, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "get_current_user", Arguments: map[string]any{}}); err != nil {
			t.Fatal(err)
		}
	}

	out := logs.String()
	if n := strings.Count(out, `msg="client connected" client="test-client 9.9"`); n != 1 {
		t.Errorf("client connected logged %d times:\n%s", n, out)
	}
	if n := strings.Count(out, "tool=get_current_user"); n != 2 {
		t.Errorf("tool call logged %d times:\n%s", n, out)
	}
}

// bearer adds an Authorization header to every request.
type bearer struct{ token string }

func (b bearer) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+b.token)
	return http.DefaultTransport.RoundTrip(r)
}

func TestHTTPServesEveryProfileBehindAToken(t *testing.T) {
	isolate(t)
	for _, name := range []string{"work", "home"} {
		f := youtracktest.New(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `{"login":%q,"fullName":"At %s"}`, name, name)
		}))
		if _, err := config.Save(config.Config{Name: name, DisplayName: "YouTrack (" + name + ")", URL: f.URL, Login: youtracktest.Login, Password: youtracktest.Password, Proxy: "none"}, ""); err != nil {
			t.Fatal(err)
		}
	}
	config.Save(config.Config{Name: "broken", URL: "https://broken.example.com", Login: "x"}, "")

	var logs bytes.Buffer
	handler, served, err := httpHandler("http://ymcp.test", "s3cret", "", "", slog.New(slog.NewTextHandler(&logs, nil)))
	if err != nil || strings.Join(served, ",") != "home,work" {
		t.Fatalf("served %v, %v", served, err)
	}
	for _, want := range []string{
		`msg="serving profile" profile=home name="YouTrack (home)"`, "url=http://ymcp.test/home",
		`msg="serving profile" profile=work`, `msg="profile skipped" profile=broken`,
	} {
		if !strings.Contains(logs.String(), want) {
			t.Errorf("log lacks %q:\n%s", want, logs.String())
		}
	}
	ts := httptest.NewServer(handler)
	defer ts.Close()

	if resp, err := http.Post(ts.URL+"/home", "application/json", strings.NewReader("{}")); err != nil || resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("no token: %v %v", resp.StatusCode, err)
	}
	authed := &http.Client{Transport: bearer{"s3cret"}}
	for _, name := range []string{"home", "work"} {
		ctx := context.Background()
		cs, err := mcp.NewClient(&mcp.Implementation{Name: "http-test"}, nil).Connect(ctx,
			&mcp.StreamableClientTransport{Endpoint: ts.URL + "/" + name, HTTPClient: authed}, nil)
		if err != nil {
			t.Fatal(err)
		}
		res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "get_current_user", Arguments: map[string]any{}})
		cs.Close()
		if err != nil || res.IsError || !strings.Contains(res.Content[0].(*mcp.TextContent).Text, `"login": "`+name+`"`) {
			t.Errorf("%s: %+v %v", name, res, err)
		}
	}
	// /healthz needs no token, and ymcp health reads it.
	if resp, err := http.Get(ts.URL + "/healthz"); err != nil || resp.StatusCode != http.StatusOK {
		t.Errorf("/healthz: %v %v", resp, err)
	}
	if out, err := captureStdout(t, func() error { return health(context.Background(), []string{"--url", ts.URL}) }); err != nil || !strings.Contains(out, `"status":"ok"`) {
		t.Errorf("ymcp health = %q, %v", out, err)
	}
	if err := health(context.Background(), []string{"--url", "http://127.0.0.1:1"}); err == nil || !strings.Contains(err.Error(), "unhealthy") {
		t.Errorf("ymcp health on a closed port = %v", err)
	}

	// Two profiles and no default: / lists what is served.
	resp, err := authed.Get(ts.URL + "/")
	if err != nil || resp.StatusCode != http.StatusNotFound {
		t.Fatalf("/: %v %v", resp.StatusCode, err)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "/home, /work") {
		t.Errorf("/ body = %q", body)
	}

	// --profiles work: only that one, at / as well.
	_, served, err = httpHandler("http://ymcp.test", "", "work", "", slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil || strings.Join(served, ",") != "work" {
		t.Errorf("--profiles work: %v %v", served, err)
	}
	if _, _, err := httpHandler("http://ymcp.test", "", "broken", "", slog.New(slog.NewTextHandler(io.Discard, nil))); err == nil {
		t.Error("serving only a broken profile succeeded")
	}
}

func TestRedactMasksSecrets(t *testing.T) {
	in := `{"method":"POST","path":"users","api":"hub","body":{"login":"bob","details":[{"type":"LoginuserdetailsJSON","login":"bob",` +
		`"password":{"type":"PlainpasswordJSON","value":"hunter2"}}]},"query":{"token":"abc"},"headers":{"Authorization":"Bearer xyz"}}`
	out := redact([]byte(in))
	for _, secret := range []string{"hunter2", "abc", "xyz"} {
		if strings.Contains(out, secret) {
			t.Errorf("%q leaked: %s", secret, out)
		}
	}
	for _, kept := range []string{`"login":"bob"`, `"path":"users"`, `"type":"PlainpasswordJSON"`} {
		if !strings.Contains(out, kept) {
			t.Errorf("lost %s: %s", kept, out)
		}
	}
	if got := redact([]byte("not json")); got != "not json" {
		t.Errorf("non-JSON = %q", got)
	}
}

func TestToolSelection(t *testing.T) {
	cfg := config.Config{Tools: []string{"projects"}}
	enabled, summary, err := toolSelection("", cfg)
	if err != nil || len(enabled) != 2 || summary != "2 of 56: projects" {
		t.Errorf("profile tools: %v %q %v", enabled, summary, err)
	}
	enabled, summary, err = toolSelection("-api", cfg)
	if err != nil || enabled["call_api"] || summary != "55 of 56: -api" {
		t.Errorf("--tools overrides the profile: %d %q %v", len(enabled), summary, err)
	}
	if enabled, summary, _ := toolSelection("", config.Config{}); enabled != nil || summary != "all 56" {
		t.Errorf("default: %v %q", enabled, summary)
	}
	if _, _, err := toolSelection("nope", cfg); err == nil {
		t.Error("unknown group accepted")
	}
}
