package endpoint

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httputil"
	"net/url"
	"slices"
	"strings"
	"time"
)

// Gateway serves the Anthropic Messages API to Claude Code at its root
// (/v1/messages, …) and passes the requests on to the upstream: as they
// are to an Anthropic-compatible API, translated to Chat Completions for
// an OpenAI-compatible one. Whoever mounts it checks the agent's token;
// the gateway drops the agent's credentials and sends the upstream key.
type Gateway struct {
	up     Upstream
	client *http.Client
	proxy  *httputil.ReverseProxy // Anthropic upstreams
	logger *slog.Logger
}

// apiClient sends requests with the key. It follows a redirect only on
// the same host and never from HTTPS to HTTP: Go would pass X-Api-Key on
// to any host, and Authorization over plain HTTP.
var apiClient = &http.Client{CheckRedirect: func(req *http.Request, via []*http.Request) error {
	from := via[0].URL
	switch {
	case len(via) >= 10:
		return errors.New("too many redirects")
	case !strings.EqualFold(req.URL.Host, from.Host) || (from.Scheme == "https" && req.URL.Scheme != "https"):
		return fmt.Errorf("not following the redirect to %s with the API key", req.URL.Redacted())
	}
	return nil
}}

// NewGateway returns the gateway to up. logger may be nil.
func NewGateway(up Upstream, logger *slog.Logger) (*Gateway, error) {
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	if err := CheckURL(up.URL); err != nil {
		return nil, fmt.Errorf("endpoint: %w", err)
	}
	up.URL = strings.TrimRight(strings.TrimSpace(up.URL), "/")
	g := &Gateway{up: up, client: apiClient, logger: logger}
	switch up.Protocol {
	case Anthropic:
		base, _ := url.Parse(up.URL)
		g.proxy = &httputil.ReverseProxy{
			Rewrite: func(r *httputil.ProxyRequest) {
				r.Out.URL.Scheme, r.Out.URL.Host = base.Scheme, base.Host
				r.Out.URL.Path = base.Path + r.In.URL.Path
				r.Out.URL.RawPath = ""
				r.Out.Host = ""
				g.authorize(r.Out.Header)
			},
			FlushInterval: -1, // server-sent events
			ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
				g.logger.Warn("model API unreachable", "err", err)
				writeError(w, http.StatusBadGateway, "api_error", "ymcp could not reach the model API: "+err.Error())
			},
		}
	case OpenAI:
	default:
		return nil, fmt.Errorf("endpoint: unknown protocol %q", up.Protocol)
	}
	return g, nil
}

// authorize replaces the agent's credentials with the upstream key.
func (g *Gateway) authorize(h http.Header) {
	h.Del("Authorization")
	h.Del("X-Api-Key")
	if g.up.Key == "" {
		return
	}
	switch g.up.Protocol {
	case Anthropic:
		// Services differ in which one they read.
		h.Set("X-Api-Key", g.up.Key)
		h.Set("Authorization", "Bearer "+g.up.Key)
	case OpenAI:
		h.Set("Authorization", "Bearer "+g.up.Key)
	}
}

func (g *Gateway) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if g.proxy != nil {
		g.proxy.ServeHTTP(w, r)
		return
	}
	switch {
	case r.Method == http.MethodPost && r.URL.Path == "/v1/messages":
		g.messages(w, r)
	case r.Method == http.MethodPost && r.URL.Path == "/v1/messages/count_tokens":
		countTokens(w, r)
	default:
		writeError(w, http.StatusNotFound, "not_found_error", "ymcp's gateway to a Chat Completions API serves only /v1/messages")
	}
}

// maxBody bounds a request of the agent: images make them large.
const maxBody = 64 << 20

// messages answers a Messages request with a Chat Completions one.
func (g *Gateway) messages(w http.ResponseWriter, r *http.Request) {
	var req msgRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, maxBody)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request_error", "the request is not a Messages request: "+err.Error())
		return
	}
	chat, err := toChat(req)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	resp, err := g.complete(r.Context(), chat)
	if err != nil {
		if r.Context().Err() != nil {
			return
		}
		g.logger.Warn("model API unreachable", "err", err)
		writeError(w, http.StatusBadGateway, "api_error", "ymcp could not reach the model API: "+err.Error())
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		g.logger.Warn("model API refused the request", "status", resp.StatusCode, "body", clip(string(body), 500))
		writeError(w, resp.StatusCode, errorType(resp.StatusCode), upstreamMessage(resp.StatusCode, body))
		return
	}
	if req.Stream {
		streamMessages(w, resp.Body, req.Model, g.logger)
		return
	}
	var cr chatResponse
	if err := json.NewDecoder(resp.Body).Decode(&cr); err != nil {
		writeError(w, http.StatusBadGateway, "api_error", "the model API answered with something other than a chat completion: "+err.Error())
		return
	}
	if cr.Error != nil {
		writeError(w, http.StatusBadGateway, "api_error", cr.Error.text())
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(fromChat(cr, req.Model))
}

// complete sends chat to the upstream. A 400 about max_tokens, which
// Claude Code sets higher than many models allow, is retried once without
// it, so the model's own limit applies.
func (g *Gateway) complete(ctx context.Context, chat chatRequest) (*http.Response, error) {
	resp, err := g.post(ctx, chat)
	if err != nil || resp.StatusCode != http.StatusBadRequest || chat.MaxTokens == 0 {
		return resp, err
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	resp.Body.Close()
	if !strings.Contains(strings.ToLower(string(body)), "max_tokens") && !strings.Contains(strings.ToLower(string(body)), "max_completion_tokens") {
		resp.Body = io.NopCloser(bytes.NewReader(body))
		return resp, nil
	}
	g.logger.Info("model API refused max_tokens; retrying without it", "max_tokens", chat.MaxTokens)
	chat.MaxTokens = 0
	return g.post(ctx, chat)
}

func (g *Gateway) post(ctx context.Context, chat chatRequest) (*http.Response, error) {
	body, err := json.Marshal(chat)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, g.up.URL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if chat.Stream {
		req.Header.Set("Accept", "text/event-stream")
	}
	// OpenRouter's attribution; other services ignore them.
	req.Header.Set("HTTP-Referer", "https://ymcp.su")
	req.Header.Set("X-Title", "ymcp")
	g.authorize(req.Header)
	return g.client.Do(req)
}

// countTokens estimates the tokens of a request at four bytes each: a
// Chat Completions API has no way to count them.
func countTokens(w http.ResponseWriter, r *http.Request) {
	n, _ := io.Copy(io.Discard, io.LimitReader(r.Body, maxBody))
	w.Header().Set("Content-Type", "application/json")
	fmt.Fprintf(w, `{"input_tokens":%d}`, max(n/4, 1))
}

// writeError writes an error in the shape of the Messages API.
func writeError(w http.ResponseWriter, status int, typ, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]any{
		"type":  "error",
		"error": map[string]string{"type": typ, "message": msg},
	})
}

// errorType is the Messages API's error type of an HTTP status.
func errorType(status int) string {
	switch status {
	case http.StatusBadRequest, http.StatusUnprocessableEntity:
		return "invalid_request_error"
	case http.StatusUnauthorized:
		return "authentication_error"
	case http.StatusPaymentRequired, http.StatusForbidden:
		return "permission_error"
	case http.StatusNotFound:
		return "not_found_error"
	case http.StatusRequestEntityTooLarge:
		return "request_too_large"
	case http.StatusTooManyRequests:
		return "rate_limit_error"
	case http.StatusServiceUnavailable, 529:
		return "overloaded_error"
	}
	return "api_error"
}

// upstreamMessage is the message of an error body of an OpenAI-compatible
// API: {"error":{"message":…}}, {"message":…}, or the text itself.
func upstreamMessage(status int, body []byte) string {
	var e struct {
		Error   json.RawMessage `json:"error"`
		Message string          `json:"message"`
	}
	msg := ""
	if json.Unmarshal(body, &e) == nil {
		var inner chatError
		switch {
		case json.Unmarshal(e.Error, &inner) == nil && inner.Message != "":
			msg = inner.text()
		case len(e.Error) > 0 && e.Error[0] == '"':
			json.Unmarshal(e.Error, &msg)
		default:
			msg = e.Message
		}
	}
	if msg == "" {
		msg = strings.TrimSpace(string(body))
	}
	if msg == "" {
		msg = http.StatusText(status)
	}
	return "model API: " + clip(msg, 2000)
}

func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// Models lists the IDs of the models the API offers, sorted:
// GET <url>/models of an OpenAI-compatible API, <url>/v1/models of an
// Anthropic-compatible one. Not every service has the list.
func Models(ctx context.Context, up Upstream) ([]string, error) {
	if err := CheckURL(up.URL); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	u := strings.TrimRight(strings.TrimSpace(up.URL), "/") + "/models"
	if up.Protocol == Anthropic {
		u = strings.TrimRight(strings.TrimSpace(up.URL), "/") + "/v1/models"
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	(&Gateway{up: up}).authorize(req.Header)
	if up.Protocol == Anthropic {
		req.Header.Set("Anthropic-Version", "2023-06-01")
	}
	resp, err := apiClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if resp.StatusCode/100 != 2 {
		return nil, fmt.Errorf("%s: %s", resp.Status, strings.TrimPrefix(upstreamMessage(resp.StatusCode, body), "model API: "))
	}
	var list struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
		Models []struct { // Ollama's /api/tags shape, should a proxy pass it on
			Name string `json:"name"`
		} `json:"models"`
	}
	if err := json.Unmarshal(body, &list); err != nil {
		return nil, fmt.Errorf("the list of models is not JSON: %w", err)
	}
	var out []string
	for _, m := range list.Data {
		if m.ID != "" {
			out = append(out, m.ID)
		}
	}
	for _, m := range list.Models {
		if m.Name != "" {
			out = append(out, m.Name)
		}
	}
	slices.Sort(out)
	return slices.Compact(out), nil
}
