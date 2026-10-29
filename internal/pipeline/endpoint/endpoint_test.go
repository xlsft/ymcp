package endpoint

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/zalando/go-keyring"
)

func TestPresets(t *testing.T) {
	seen := map[string]bool{}
	for _, p := range Presets {
		if seen[p.Name] {
			t.Errorf("preset %s twice", p.Name)
		}
		seen[p.Name] = true
		if p.Title == "" || p.About == "" {
			t.Errorf("preset %s: no title or about", p.Name)
		}
		for _, u := range []string{p.Anthropic, p.OpenAI} {
			if u == "" {
				continue
			}
			if err := CheckURL(u); err != nil {
				t.Errorf("preset %s: %v", p.Name, err)
			}
			if strings.HasSuffix(u, "/") {
				t.Errorf("preset %s: %s ends with /", p.Name, u)
			}
		}
	}
	if p, ok := Find("openrouter"); !ok || p.URL(OpenAI) != "https://openrouter.ai/api/v1" || p.URL(Anthropic) != "https://openrouter.ai/api" {
		t.Errorf("Find(openrouter) = %+v, %v", p, ok)
	}
	if _, ok := Find("nope"); ok {
		t.Error("Find(nope) found something")
	}
}

func TestCheckURL(t *testing.T) {
	for _, u := range []string{"https://a.example/v1", "http://localhost:11434"} {
		if err := CheckURL(u); err != nil {
			t.Errorf("CheckURL(%q) = %v", u, err)
		}
	}
	for _, u := range []string{"", "a.example", "ftp://a.example", "https://", "https://a.example/v1?x=1"} {
		if CheckURL(u) == nil {
			t.Errorf("CheckURL(%q) = nil", u)
		}
	}
}

func TestPrivate(t *testing.T) {
	for u, want := range map[string]bool{
		"https://api.example/v1": true, "http://localhost:11434/v1": true, "http://127.0.0.1:4000": true,
		"http://[::1]:8000": true, "http://gpu.lan:8000/v1": false, "http://10.0.0.5/v1": false,
	} {
		if got := Private(u); got != want {
			t.Errorf("Private(%q) = %v", u, got)
		}
	}
}

func TestKey(t *testing.T) {
	keyring.MockInit()
	t.Setenv("YMCP_TEST_KEY", "")
	if _, _, err := Key("https://api.example/v1", "YMCP_TEST_KEY"); !errors.Is(err, ErrNoKey) {
		t.Fatalf("no key: %v", err)
	}
	if err := SetKey("https://API.example/v1", " sk-1 "); err != nil {
		t.Fatal(err)
	}
	// Every API on the host shares the key.
	if k, src, err := Key("https://api.example/anthropic", "YMCP_TEST_KEY"); k != "sk-1" || src != "keychain" || err != nil {
		t.Errorf("from keychain: %q %q %v", k, src, err)
	}
	t.Setenv("YMCP_TEST_KEY", "sk-env")
	if k, src, err := Key("https://api.example/v1", "YMCP_TEST_KEY"); k != "sk-env" || src != "YMCP_TEST_KEY" || err != nil {
		t.Errorf("from env: %q %q %v", k, src, err)
	}
	if err := SetKey("https://api.example/v1", ""); err != nil {
		t.Fatal(err)
	}
	if k, _, err := Key("https://api.example/v1", ""); !errors.Is(err, ErrNoKey) {
		t.Errorf("after removal: %q %v", k, err)
	}
	if err := SetKey("https://api.example/v1", ""); err != nil {
		t.Errorf("removing twice: %v", err)
	}
}

func TestToChat(t *testing.T) {
	var req msgRequest
	err := json.Unmarshal([]byte(`{
		"model": "m", "max_tokens": 100, "stream": true, "stop_sequences": ["X"],
		"system": [{"type": "text", "text": "be brief"}, {"type": "text", "text": "use tools"}],
		"messages": [
			{"role": "user", "content": "hi"},
			{"role": "system", "content": [{"type": "text", "text": "mid"}]},
			{"role": "assistant", "content": [
				{"type": "thinking", "thinking": "hmm", "signature": "s"},
				{"type": "text", "text": "reading"},
				{"type": "tool_use", "id": "t1", "name": "Read", "input": {"path": "a"}},
				{"type": "tool_use", "id": "t2", "name": "Ls", "input": {}}
			]},
			{"role": "user", "content": [
				{"type": "tool_result", "tool_use_id": "t1", "content": [{"type": "text", "text": "A"}, {"type": "image", "source": {"type": "base64", "media_type": "image/png", "data": "QQ=="}}]},
				{"type": "tool_result", "tool_use_id": "t2", "content": "denied", "is_error": true},
				{"type": "text", "text": "go on"}
			]},
			{"role": "assistant", "content": [{"type": "thinking", "thinking": "only thoughts"}]}
		],
		"tools": [
			{"name": "Read", "description": "reads", "input_schema": {"type": "object"}},
			{"type": "web_search_20250305", "name": "web_search"}
		],
		"tool_choice": {"type": "any"}
	}`), &req)
	if err != nil {
		t.Fatal(err)
	}
	chat, err := toChat(req)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := json.Marshal(chat)
	want := `{"model":"m","messages":[` +
		`{"role":"system","content":"be brief\n\nuse tools"},` +
		`{"role":"user","content":"hi"},` +
		`{"role":"system","content":"mid"},` +
		`{"role":"assistant","content":"reading","tool_calls":[{"id":"t1","type":"function","function":{"name":"Read","arguments":"{\"path\":\"a\"}"}},{"id":"t2","type":"function","function":{"name":"Ls","arguments":"{}"}}]},` +
		`{"role":"tool","content":"A","tool_call_id":"t1"},` +
		`{"role":"tool","content":"Error: denied","tool_call_id":"t2"},` +
		`{"role":"user","content":[{"type":"image_url","image_url":{"url":"data:image/png;base64,QQ=="}},{"type":"text","text":"go on"}]}],` +
		`"max_tokens":100,"stop":["X"],"stream":true,"stream_options":{"include_usage":true},` +
		`"tools":[{"type":"function","function":{"name":"Read","description":"reads","parameters":{"type":"object"}}}],` +
		`"tool_choice":"required"}`
	if string(got) != want {
		t.Errorf("toChat:\n got %s\nwant %s", got, want)
	}
}

func TestFromChat(t *testing.T) {
	var cr chatResponse
	json.Unmarshal([]byte(`{"id":"c1","choices":[{"message":{"content":"done","tool_calls":[{"id":"x","function":{"name":"Bash","arguments":"{\"cmd\":\"ls\"}"}},{"function":{"name":"Bad","arguments":"not json"}}]},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":3}}`), &cr)
	m := fromChat(cr, "m")
	got, _ := json.Marshal(m)
	s := string(got)
	for _, want := range []string{`"id":"msg_c1"`, `"model":"m"`, `{"type":"text","text":"done"}`, `{"type":"tool_use","id":"x","name":"Bash","input":{"cmd":"ls"}}`, `"name":"Bad","input":{}`, `"stop_reason":"tool_use"`, `"usage":{"input_tokens":10,"output_tokens":3}`} {
		if !strings.Contains(s, want) {
			t.Errorf("fromChat lacks %s:\n%s", want, s)
		}
	}
	var empty chatResponse
	json.Unmarshal([]byte(`{"choices":[{"message":{"content":""},"finish_reason":"length"}]}`), &empty)
	if m := fromChat(empty, "m"); *m.StopReason != "max_tokens" || len(m.Content) != 0 {
		t.Errorf("empty answer: %+v", m)
	}
}

// fakeChat is a Chat Completions API that records requests and answers
// with reply.
type fakeChat struct {
	mu    sync.Mutex
	reqs  []map[string]any
	auth  []string
	reply func(w http.ResponseWriter, req map[string]any)
}

func (f *fakeChat) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var req map[string]any
	json.NewDecoder(r.Body).Decode(&req)
	f.mu.Lock()
	f.reqs = append(f.reqs, req)
	f.auth = append(f.auth, r.URL.Path+" "+r.Header.Get("Authorization")+" "+r.Header.Get("X-Api-Key"))
	f.mu.Unlock()
	f.reply(w, req)
}

func gateway(t *testing.T, protocol string, upstream http.Handler) *httptest.Server {
	t.Helper()
	up := httptest.NewServer(upstream)
	t.Cleanup(up.Close)
	g, err := NewGateway(Upstream{Protocol: protocol, URL: up.URL + "/base/", Key: "sk-up"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(g)
	t.Cleanup(srv.Close)
	return srv
}

func post(t *testing.T, url, body string) (*http.Response, string) {
	t.Helper()
	req, _ := http.NewRequestWithContext(t.Context(), http.MethodPost, url, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer agent-token")
	req.Header.Set("X-Api-Key", "agent-key")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp, string(b)
}

func TestGatewayOpenAI(t *testing.T) {
	f := &fakeChat{reply: func(w http.ResponseWriter, req map[string]any) {
		io.WriteString(w, `{"id":"c1","choices":[{"message":{"content":"hello"},"finish_reason":"stop"}],"usage":{"prompt_tokens":5,"completion_tokens":1}}`)
	}}
	srv := gateway(t, OpenAI, f)
	resp, body := post(t, srv.URL+"/v1/messages", `{"model":"m","max_tokens":10,"messages":[{"role":"user","content":"hi"}]}`)
	if resp.StatusCode != 200 || !strings.Contains(body, `"text":"hello"`) || !strings.Contains(body, `"stop_reason":"end_turn"`) {
		t.Fatalf("%d %s", resp.StatusCode, body)
	}
	if f.auth[0] != "/base/chat/completions Bearer sk-up " {
		t.Errorf("upstream got %q", f.auth[0])
	}
	if f.reqs[0]["model"] != "m" {
		t.Errorf("upstream request %v", f.reqs[0])
	}

	resp, body = post(t, srv.URL+"/v1/messages/count_tokens", `{"model":"m","messages":[{"role":"user","content":"hello there"}]}`)
	if resp.StatusCode != 200 || !strings.Contains(body, `"input_tokens":`) {
		t.Errorf("count_tokens: %d %s", resp.StatusCode, body)
	}
	if resp, _ = post(t, srv.URL+"/v1/other", `{}`); resp.StatusCode != 404 {
		t.Errorf("other path: %d", resp.StatusCode)
	}
}

func TestGatewayOpenAIRetriesMaxTokens(t *testing.T) {
	f := &fakeChat{reply: func(w http.ResponseWriter, req map[string]any) {
		if _, ok := req["max_tokens"]; ok {
			w.WriteHeader(400)
			io.WriteString(w, `{"error":{"message":"max_tokens is too large: 32000 > 8192"}}`)
			return
		}
		io.WriteString(w, `{"choices":[{"message":{"content":"ok"},"finish_reason":"stop"}]}`)
	}}
	srv := gateway(t, OpenAI, f)
	resp, body := post(t, srv.URL+"/v1/messages", `{"model":"m","max_tokens":32000,"messages":[{"role":"user","content":"hi"}]}`)
	if resp.StatusCode != 200 || !strings.Contains(body, `"text":"ok"`) || len(f.reqs) != 2 {
		t.Fatalf("%d %s after %d requests", resp.StatusCode, body, len(f.reqs))
	}
}

func TestGatewayOpenAIError(t *testing.T) {
	f := &fakeChat{reply: func(w http.ResponseWriter, req map[string]any) {
		w.WriteHeader(429)
		io.WriteString(w, `{"error":{"message":"slow down","code":"rate_limited"}}`)
	}}
	srv := gateway(t, OpenAI, f)
	resp, body := post(t, srv.URL+"/v1/messages", `{"model":"m","messages":[{"role":"user","content":"hi"}]}`)
	if resp.StatusCode != 429 || !strings.Contains(body, `"type":"rate_limit_error"`) || !strings.Contains(body, "slow down (rate_limited)") {
		t.Errorf("%d %s", resp.StatusCode, body)
	}
}

// events reads a Messages stream as event names and data.
func events(t *testing.T, body string) (names []string, data []map[string]any) {
	t.Helper()
	sc := bufio.NewScanner(strings.NewReader(body))
	for sc.Scan() {
		if n, ok := strings.CutPrefix(sc.Text(), "event: "); ok {
			names = append(names, n)
		}
		if d, ok := strings.CutPrefix(sc.Text(), "data: "); ok {
			var m map[string]any
			if err := json.Unmarshal([]byte(d), &m); err != nil {
				t.Fatalf("data %q: %v", d, err)
			}
			data = append(data, m)
		}
	}
	return names, data
}

func TestGatewayOpenAIStream(t *testing.T) {
	f := &fakeChat{reply: func(w http.ResponseWriter, req map[string]any) {
		if req["stream"] != true {
			t.Errorf("not streamed: %v", req)
		}
		for _, c := range []string{
			`{"choices":[{"delta":{"role":"assistant","content":""}}]}`,
			`{"choices":[{"delta":{"content":"Hel"}}]}`,
			`{"choices":[{"delta":{"content":"lo"}}]}`,
			`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"a","function":{"name":"Read","arguments":""}}]}}]}`,
			`{"choices":[{"delta":{"tool_calls":[{"index":1,"id":"b","function":{"name":"Ls","arguments":"{}"}}]}}]}`,
			`{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"path\":"}}]}}]}`,
			`{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"x\"}"}}]}}]}`,
			`{"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`,
			`{"choices":[],"usage":{"prompt_tokens":7,"completion_tokens":2}}`,
			`[DONE]`,
		} {
			io.WriteString(w, "data: "+c+"\n\n")
		}
	}}
	srv := gateway(t, OpenAI, f)
	resp, body := post(t, srv.URL+"/v1/messages", `{"model":"m","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	if resp.StatusCode != 200 || resp.Header.Get("Content-Type") != "text/event-stream" {
		t.Fatalf("%d %s", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	names, data := events(t, body)
	want := "message_start content_block_start content_block_delta content_block_delta content_block_stop " +
		"content_block_start content_block_delta content_block_stop content_block_start content_block_delta content_block_stop " +
		"message_delta message_stop"
	if strings.Join(names, " ") != want {
		t.Fatalf("events:\n got %s\nwant %s", strings.Join(names, " "), want)
	}
	if d := data[6]["delta"].(map[string]any); d["partial_json"] != `{"path":"x"}` || data[5]["content_block"].(map[string]any)["id"] != "a" {
		t.Errorf("first tool: %v %v", data[5], data[6])
	}
	if data[8]["index"] != float64(2) || data[8]["content_block"].(map[string]any)["name"] != "Ls" {
		t.Errorf("second tool: %v", data[8])
	}
	md := data[11]
	if md["delta"].(map[string]any)["stop_reason"] != "tool_use" || md["usage"].(map[string]any)["output_tokens"] != float64(2) {
		t.Errorf("message_delta: %v", md)
	}
}

func TestGatewayOpenAIStreamError(t *testing.T) {
	f := &fakeChat{reply: func(w http.ResponseWriter, req map[string]any) {
		io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"a\"}}]}\n\n")
		io.WriteString(w, "data: {\"error\":{\"message\":\"overloaded\"}}\n\n")
	}}
	srv := gateway(t, OpenAI, f)
	_, body := post(t, srv.URL+"/v1/messages", `{"model":"m","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	names, data := events(t, body)
	if names[len(names)-1] != "error" || !strings.Contains(data[len(data)-1]["error"].(map[string]any)["message"].(string), "overloaded") {
		t.Errorf("events %v %v", names, data)
	}
}

func TestGatewayAnthropic(t *testing.T) {
	f := &fakeChat{reply: func(w http.ResponseWriter, req map[string]any) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "event: message_stop\ndata: {}\n\n")
	}}
	srv := gateway(t, Anthropic, f)
	resp, body := post(t, srv.URL+"/v1/messages?beta=true", `{"model":"m"}`)
	if resp.StatusCode != 200 || !strings.Contains(body, "message_stop") {
		t.Fatalf("%d %s", resp.StatusCode, body)
	}
	if f.auth[0] != "/base/v1/messages Bearer sk-up sk-up" {
		t.Errorf("upstream got %q", f.auth[0])
	}
}

func TestModels(t *testing.T) {
	var got []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = append(got, r.URL.Path+" "+r.Header.Get("Authorization"))
		if r.URL.Path == "/bad/models" {
			w.WriteHeader(401)
			io.WriteString(w, `{"error":{"message":"invalid key"}}`)
			return
		}
		io.WriteString(w, `{"data":[{"id":"b"},{"id":"a"},{"id":"a"}]}`)
	}))
	defer srv.Close()
	ms, err := Models(context.Background(), Upstream{Protocol: OpenAI, URL: srv.URL + "/v1", Key: "k"})
	if err != nil || strings.Join(ms, ",") != "a,b" {
		t.Errorf("openai: %v %v", ms, err)
	}
	if _, err := Models(context.Background(), Upstream{Protocol: Anthropic, URL: srv.URL}); err != nil {
		t.Error(err)
	}
	if _, err := Models(context.Background(), Upstream{Protocol: OpenAI, URL: srv.URL + "/bad"}); err == nil || !strings.Contains(err.Error(), "invalid key") {
		t.Errorf("bad key: %v", err)
	}
	if strings.Join(got, "|") != "/v1/models Bearer k|/v1/models |/bad/models " {
		t.Errorf("requests %q", got)
	}
}
