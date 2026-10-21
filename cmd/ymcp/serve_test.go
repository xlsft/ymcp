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
	if _, err := newServer(yt, logger).Connect(ctx, st, nil); err != nil {
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
	handler, served, err := httpHandler("http://ymcp.test", "s3cret", "", slog.New(slog.NewTextHandler(&logs, nil)))
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
	_, served, err = httpHandler("http://ymcp.test", "", "work", slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil || strings.Join(served, ",") != "work" {
		t.Errorf("--profiles work: %v %v", served, err)
	}
	if _, _, err := httpHandler("http://ymcp.test", "", "broken", slog.New(slog.NewTextHandler(io.Discard, nil))); err == nil {
		t.Error("serving only a broken profile succeeded")
	}
}
