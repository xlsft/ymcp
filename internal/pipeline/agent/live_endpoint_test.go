package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/xlsft/ymcp/internal/pipeline/endpoint"
)

// TestLiveClaudeOnEndpoint runs the real claude on a model API: ymcp's
// gateway in front of a scripted Chat Completions API, which has claude
// run one Bash command and then answers with its output. It bills
// nothing, but needs claude, so it runs only with YMCP_PIPELINE_LIVE=1.
// The anthropic case puts a passthrough gateway in front of the
// translating one.
func TestLiveClaudeOnEndpoint(t *testing.T) {
	if os.Getenv("YMCP_PIPELINE_LIVE") != "1" {
		t.Skip("set YMCP_PIPELINE_LIVE=1 to run the real claude")
	}
	if _, err := exec.LookPath("claude"); err != nil {
		t.Skip("claude not installed")
	}
	for _, provider := range []string{endpoint.OpenAI, endpoint.Anthropic} {
		t.Run(provider, func(t *testing.T) {
			chat := &scriptedChat{t: t}
			upstream := httptest.NewServer(chat)
			defer upstream.Close()
			gw, err := endpoint.NewGateway(endpoint.Upstream{Protocol: endpoint.OpenAI, URL: upstream.URL + "/v1", Key: "sk-upstream"}, nil)
			if err != nil {
				t.Fatal(err)
			}
			srv := httptest.NewServer(gw)
			defer srv.Close()
			if provider == endpoint.Anthropic {
				pass, err := endpoint.NewGateway(endpoint.Upstream{Protocol: endpoint.Anthropic, URL: srv.URL}, nil)
				if err != nil {
					t.Fatal(err)
				}
				srv = httptest.NewServer(pass)
				defer srv.Close()
			}

			a, err := New(provider, "", nil)
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
			defer cancel()
			s, err := a.Start(ctx, StartOptions{
				Dir: t.TempDir(), Model: "scripted-model", Mode: "bypassPermissions",
				Endpoint: &Endpoint{URL: srv.URL, Token: "run-token", Model: "scripted-model"},
			})
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			r := record(s)
			res, err := s.Prompt(ctx, "Run `echo gateway-ok` and tell me what it printed.")
			if err != nil {
				t.Fatalf("Prompt: %v (%+v)", err, res)
			}
			t.Logf("result %+v; events %s", res, kinds(r.all()))
			if res.IsError || !strings.Contains(res.Text, "the command printed: gateway-ok") {
				t.Errorf("result = %+v", res)
			}
			chat.mu.Lock()
			defer chat.mu.Unlock()
			t.Logf("%d requests to the model API, models %q", chat.requests, chat.models)
			if !chat.sawToolResult {
				t.Error("the Bash output never came back to the model")
			}
		})
	}
}

// scriptedChat is a Chat Completions API that asks for `echo gateway-ok`
// through the Bash tool and then reports what the tool result said.
// Requests without tools (claude's titles and summaries) get plain text.
type scriptedChat struct {
	t             *testing.T
	mu            sync.Mutex
	requests      int
	models        []string
	sawToolResult bool
}

func (c *scriptedChat) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/v1/chat/completions" || r.Header.Get("Authorization") != "Bearer sk-upstream" {
		c.t.Errorf("unexpected request %s %s auth %q", r.Method, r.URL.Path, r.Header.Get("Authorization"))
		http.Error(w, "no", 404)
		return
	}
	var req struct {
		Model    string `json:"model"`
		Stream   bool   `json:"stream"`
		Messages []struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
		Tools []struct {
			Function struct {
				Name string `json:"name"`
			} `json:"function"`
		} `json:"tools"`
	}
	body, _ := io.ReadAll(r.Body)
	if err := json.Unmarshal(body, &req); err != nil {
		c.t.Errorf("request body: %v", err)
	}
	c.mu.Lock()
	c.requests++
	c.models = append(c.models, req.Model)
	c.mu.Unlock()

	hasBash := false
	for _, tl := range req.Tools {
		hasBash = hasBash || tl.Function.Name == "Bash"
	}
	var chunks []string
	last := req.Messages[len(req.Messages)-1]
	switch {
	case last.Role == "tool":
		var out string
		json.Unmarshal(last.Content, &out)
		c.mu.Lock()
		c.sawToolResult = strings.Contains(out, "gateway-ok")
		c.mu.Unlock()
		chunks = textChunks("Done, the command printed: " + strings.TrimSpace(out))
	case hasBash:
		args, _ := json.Marshal(map[string]string{"command": "echo gateway-ok", "description": "Print a marker"})
		chunks = []string{
			`{"choices":[{"delta":{"role":"assistant","content":"Running it."}}]}`,
			fmt.Sprintf(`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"Bash","arguments":""}}]}}]}`),
			fmt.Sprintf(`{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":%q}}]}}]}`, string(args)),
			`{"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`,
		}
	default:
		chunks = textChunks("Gateway check")
	}
	if !req.Stream {
		// Join the script into one completion.
		io.WriteString(w, `{"choices":[{"message":{"content":"Gateway check"},"finish_reason":"stop"}]}`)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	for _, ch := range append(chunks, `{"choices":[],"usage":{"prompt_tokens":100,"completion_tokens":10}}`, "[DONE]") {
		fmt.Fprintf(w, "data: %s\n\n", ch)
		w.(http.Flusher).Flush()
	}
}

func textChunks(s string) []string {
	b, _ := json.Marshal(s)
	return []string{
		fmt.Sprintf(`{"choices":[{"delta":{"content":%s}}]}`, b),
		`{"choices":[{"delta":{},"finish_reason":"stop"}]}`,
	}
}
