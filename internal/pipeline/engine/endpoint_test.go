package engine

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/zalando/go-keyring"

	"github.com/xlsft/ymcp/internal/pipeline/agent"
	"github.com/xlsft/ymcp/internal/pipeline/spec"
)

// TestAgentOnEndpoint runs an openai agent: it gets the gateway on the
// runner's MCP server with its run's token, and its requests reach the
// Chat Completions API with the pipeline's key.
func TestAgentOnEndpoint(t *testing.T) {
	keyring.MockInit()
	var mu sync.Mutex
	var seen []string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		seen = append(seen, r.URL.Path+" "+r.Header.Get("Authorization")+" "+string(body))
		mu.Unlock()
		io.WriteString(w, `{"choices":[{"message":{"content":"from the model"},"finish_reason":"stop"}]}`)
	}))
	defer upstream.Close()
	t.Setenv("YMCP_TEST_LLM_KEY", "sk-test")

	h := newHarness(t, func(p *spec.Pipeline) {
		p.Agent = spec.Agent{Provider: "openai", Name: "Fake", Model: "test-model",
			Endpoint: &spec.Endpoint{URL: upstream.URL + "/v1", KeyEnv: "YMCP_TEST_LLM_KEY"}}
	})
	is := h.yt.addIssue("Ask the model", "Open")
	h.ag.script(func(ft *fakeTurn) agent.Result {
		ep := ft.s.opts.Endpoint
		if ep == nil {
			ft.Say("no endpoint")
			return agent.Result{}
		}
		req, _ := http.NewRequest(http.MethodPost, ep.URL+"/v1/messages", strings.NewReader(`{"model":"`+ep.Model+`","max_tokens":5,"messages":[{"role":"user","content":"hi"}]}`))
		req.Header.Set("Authorization", "Bearer "+ep.Token)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			ft.Say("request failed: " + err.Error())
			return agent.Result{}
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		ft.Say(string(b))
		return agent.Result{}
	})
	h.start()
	h.waitReview(1, is.Key)

	prompts := h.ag.promptLog()
	if len(prompts) == 0 {
		t.Fatal("the agent got no prompt")
	}
	mu.Lock()
	defer mu.Unlock()
	if len(seen) != 1 || !strings.HasPrefix(seen[0], "/v1/chat/completions Bearer sk-test ") || !strings.Contains(seen[0], `"model":"test-model"`) {
		t.Errorf("upstream saw %q", seen)
	}
	if got := h.yt.agentComments(is.Key); !strings.Contains(strings.Join(got, "\n"), "from the model") {
		t.Errorf("the agent's answer didn't come through: %q", got)
	}
}

func TestCheckEndpoint(t *testing.T) {
	keyring.MockInit()
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer sk-test" {
			w.WriteHeader(401)
			io.WriteString(w, `{"error":{"message":"bad key"}}`)
			return
		}
		io.WriteString(w, `{"data":[{"id":"a"},{"id":"b"}]}`)
	}))
	defer api.Close()
	t.Setenv("YMCP_TEST_LLM_KEY", "sk-test")
	t.Setenv("YMCP_TEST_NO_KEY", "")
	check := func(model, keyEnv string) []string {
		var out []string
		a := spec.Agent{Provider: "openai", Model: model, Endpoint: &spec.Endpoint{URL: api.URL + "/v1", KeyEnv: keyEnv}}
		checkEndpoint(t.Context(), a,
			func(name string, ok bool, detail string) {
				out = append(out, map[bool]string{true: "ok ", false: "fail "}[ok]+detail)
			},
			func(name, msg string) { out = append(out, "warn "+msg) })
		return out
	}
	got := strings.Join(check("a", "YMCP_TEST_LLM_KEY"), "|")
	if got != "ok "+api.URL+"/v1, key from YMCP_TEST_LLM_KEY, has a" {
		t.Errorf("listed model: %q", got)
	}
	if got := strings.Join(check("c", "YMCP_TEST_LLM_KEY"), "|"); !strings.Contains(got, `warn model "c" is not among the 2 it lists`) {
		t.Errorf("unlisted model: %q", got)
	}
	if got := strings.Join(check("a", "YMCP_TEST_NO_KEY"), "|"); !strings.HasPrefix(got, "fail no API key") {
		t.Errorf("no key: %q", got)
	}
}
