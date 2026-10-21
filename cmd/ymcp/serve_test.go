package main

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

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
