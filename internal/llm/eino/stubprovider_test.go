// internal/llm/eino/stubprovider_test.go
//
// A REAL OpenAI-compatible HTTP server, on loopback, that the REAL provider
// stack talks to over the REAL wire.
//
// Everything else in this package's tests substitutes a fake at the
// model.BaseChatModel seam, which is the right level for testing the decorators
// but structurally cannot see anything below it. The defect that motivated this
// file lived exactly there: go-openai's handleErrorResp discards resp.Header,
// so a 429 carrying "Retry-After: 3" and a 429 carrying nothing produced
// byte-identical errors, and M1's whole cooldown feature silently degraded to a
// blind exponential for the DEFAULT provider kind. Every unit test passed —
// they all constructed the header map by hand and handed it straight to
// ClassifyError, which is the one thing production could never do.
//
// So the rule for tests in this file is: go through BuildProviders (or
// buildOne), point it at a stub over http, and assert on what the stub SAW and
// on how long things actually took. No test here may construct a
// model.BaseChatModel directly.
//
// No external dependency: httptest binds 127.0.0.1:0, so this runs in CI, in an
// air-gapped build, and under -race.
package eino

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cloudwego/eino/components/model"

	"github.com/x6nux/yanshi/internal/config"
)

// capturedRequest is one HTTP request the stub received, decoded far enough to
// assert on.
type capturedRequest struct {
	// Path is the request path ("/v1/chat/completions", "/v1/models").
	Path string
	// At is when the stub received it. Intervals between these are the only
	// honest way to test a backoff schedule.
	At time.Time
	// Body is the decoded JSON request body (nil for GETs).
	Body map[string]any
	// Raw is the undecoded body, for assertions about what is ABSENT (a key
	// that must not appear on the wire cannot be checked through a map that
	// silently omits it either way).
	Raw string
	// Auth is the Authorization header.
	Auth string
	// Header is the full set of request headers, for W-C-02 assertions about
	// custom headers that are not Authorization.
	Header http.Header
}

// stubResponse is what the stub should answer with for one request.
type stubResponse struct {
	// Status is the HTTP status. Zero means 200.
	Status int
	// Header holds extra response headers (Retry-After lives here).
	Header map[string]string
	// Body is the raw response body, used for ERROR responses where the exact
	// provider JSON matters. For successful replies prefer Content: a raw body
	// cannot be rendered as SSE, and a streaming caller handed a completion
	// object sees an empty response rather than an error.
	Body string
	// Content is the assistant text of a successful reply, rendered as either
	// a completion object or an SSE stream depending on what the client asked
	// for. Empty means defaultStubContent.
	Content string
	// Delay is how long to stall before answering, for timeout tests.
	Delay time.Duration
}

// stubProvider is an OpenAI-compatible server plus the log of what it saw.
type stubProvider struct {
	// URL is the base_url to configure a provider with (already ends in /v1).
	URL string

	mu       sync.Mutex
	requests []capturedRequest
	// respond decides the answer for the n-th chat request (1-based).
	respond func(n int, req capturedRequest) stubResponse
	// models is the catalogue GET /v1/models returns.
	models []string
	// modelsStatus overrides the catalogue status (404 for "not implemented").
	modelsStatus int
	// modelsDelay stalls the catalogue endpoint, for the preflight-timeout case.
	modelsDelay time.Duration
	srv         *httptest.Server
}

// newStubProvider starts a stub on loopback and registers cleanup.
//
// respond may be nil, in which case every chat request gets a successful
// canned completion.
func newStubProvider(t *testing.T, respond func(n int, req capturedRequest) stubResponse) *stubProvider {
	t.Helper()
	s := &stubProvider{respond: respond, models: []string{"stub-model-a", "stub-model-b"}}
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/models", s.handleModels)
	mux.HandleFunc("/v1/chat/completions", s.handleChat)
	s.srv = httptest.NewServer(mux)
	s.URL = s.srv.URL + "/v1"
	t.Cleanup(s.srv.Close)
	return s
}

// handleModels serves the M9 catalogue endpoint.
func (s *stubProvider) handleModels(w http.ResponseWriter, r *http.Request) {
	s.record(capturedRequest{Path: r.URL.Path, At: time.Now()})
	if s.modelsDelay > 0 {
		// Wait on the REQUEST's context, not the clock. A bare Sleep makes the
		// handler outlive the client that gave up on it, and httptest.Close
		// then blocks the whole test binary for the full delay waiting for a
		// connection nobody is reading. Returning as soon as the caller
		// disconnects reproduces an unresponsive server just as faithfully.
		select {
		case <-time.After(s.modelsDelay):
		case <-r.Context().Done():
			return
		}
	}
	if s.modelsStatus != 0 {
		w.WriteHeader(s.modelsStatus)
		return
	}
	data := make([]map[string]any, 0, len(s.models))
	for _, m := range s.models {
		data = append(data, map[string]any{"id": m, "object": "model"})
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"object": "list", "data": data})
}

// handleChat serves chat completions, consulting respond for the answer.
func (s *stubProvider) handleChat(w http.ResponseWriter, r *http.Request) {
	raw, _ := io.ReadAll(r.Body)
	cr := capturedRequest{
		Path: r.URL.Path, At: time.Now(), Raw: string(raw), Auth: r.Header.Get("Authorization"),
		Header: r.Header.Clone(),
	}
	_ = json.Unmarshal(raw, &cr.Body)
	n := s.record(cr)

	resp := stubResponse{}
	if s.respond != nil {
		resp = s.respond(n, cr)
	}
	if resp.Delay > 0 {
		time.Sleep(resp.Delay)
	}
	for k, v := range resp.Header {
		w.Header().Set(k, v)
	}
	if resp.Status != 0 && resp.Status != http.StatusOK {
		w.WriteHeader(resp.Status)
		if resp.Body != "" {
			_, _ = io.WriteString(w, resp.Body)
			return
		}
		_, _ = io.WriteString(w, `{"error":{"message":"stub error","type":"stub"}}`)
		return
	}
	// Content first, transport second. A stubResponse.Content is answered in
	// whichever form the CLIENT asked for, because several consumers in this
	// package stream (ctxcompact's summariser does) and answering a streaming
	// request with a completion object makes the SDK see an empty response —
	// which surfaces as "summarizer returned nothing" and looks exactly like a
	// broken compaction. That misdiagnosis cost a debugging round here.
	streaming, _ := cr.Body["stream"].(bool)
	content := resp.Content
	if content == "" {
		content = defaultStubContent
	}
	if streaming {
		s.writeStream(w, content)
		return
	}
	if resp.Body != "" {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, resp.Body)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = io.WriteString(w, completionWith(content))
}

// defaultStubContent is the assistant text a stub reply carries when the test
// did not choose one.
const defaultStubContent = "STUB-OK"

// writeStream emits a minimal but complete SSE completion carrying content,
// including the usage chunk providers send last (which is where M10 reads its
// numbers from).
func (s *stubProvider) writeStream(w http.ResponseWriter, content string) {
	w.Header().Set("Content-Type", "text/event-stream")
	fl, _ := w.(http.Flusher)
	delta, err := json.Marshal(map[string]any{
		"id": "c", "object": "chat.completion.chunk",
		"choices": []map[string]any{{
			"index": 0,
			"delta": map[string]any{"role": "assistant", "content": content},
		}},
	})
	if err != nil {
		panic(err)
	}
	for _, c := range []string{
		string(delta),
		`{"id":"c","object":"chat.completion.chunk","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],` +
			`"usage":{"prompt_tokens":11,"completion_tokens":7,"total_tokens":18}}`,
	} {
		fmt.Fprintf(w, "data: %s\n\n", c)
		if fl != nil {
			fl.Flush()
		}
	}
	fmt.Fprint(w, "data: [DONE]\n\n")
	if fl != nil {
		fl.Flush()
	}
}

// successCompletionJSON is the canned non-streaming success body.
const successCompletionJSON = `{"id":"cmpl","object":"chat.completion","model":"stub-model-a",` +
	`"choices":[{"index":0,"message":{"role":"assistant","content":"STUB-OK"},"finish_reason":"stop"}],` +
	`"usage":{"prompt_tokens":11,"completion_tokens":7,"total_tokens":18}}`

// completionWith renders a success body carrying arbitrary content.
//
// It exists because the canned "STUB-OK" is deliberately tiny, and C10's
// summary quality gate REJECTS a summary that short relative to its input —
// correctly, since a model answering "ok" to a summarisation request must not
// be allowed to replace half the history. A test that needs the compaction path
// to complete therefore has to return a plausibly sized summary, the same way a
// real model would.
func completionWith(content string) string {
	b, err := json.Marshal(map[string]any{
		"id": "cmpl", "object": "chat.completion", "model": "stub-model-a",
		"choices": []map[string]any{{
			"index":         0,
			"message":       map[string]any{"role": "assistant", "content": content},
			"finish_reason": "stop",
		}},
		"usage": map[string]any{"prompt_tokens": 11, "completion_tokens": 7, "total_tokens": 18},
	})
	if err != nil {
		panic(err)
	}
	return string(b)
}

// record appends a request and returns its 1-based index among CHAT requests.
func (s *stubProvider) record(r capturedRequest) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.requests = append(s.requests, r)
	n := 0
	for _, q := range s.requests {
		if strings.HasSuffix(q.Path, "/chat/completions") {
			n++
		}
	}
	return n
}

// chatRequests returns the chat requests seen so far, in arrival order.
func (s *stubProvider) chatRequests() []capturedRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]capturedRequest, 0, len(s.requests))
	for _, q := range s.requests {
		if strings.HasSuffix(q.Path, "/chat/completions") {
			out = append(out, q)
		}
	}
	return out
}

// allRequests returns every request the stub saw, chat and catalogue alike.
func (s *stubProvider) allRequests() []capturedRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]capturedRequest(nil), s.requests...)
}

// gaps returns the wall-clock intervals between consecutive chat requests.
// This — not reading the backoff code — is how the retry schedule is tested.
func (s *stubProvider) gaps() []time.Duration {
	reqs := s.chatRequests()
	out := make([]time.Duration, 0, len(reqs))
	for i := 1; i < len(reqs); i++ {
		out = append(out, reqs[i].At.Sub(reqs[i-1].At))
	}
	return out
}

// stubProviderConfig returns a one-provider config pointed at s.
//
// It goes through config.ProviderConfig (not a hand-built adapter config) so
// the test exercises the same field-by-field translation production does.
func stubProviderConfig(s *stubProvider, mutate func(*config.ProviderConfig)) *config.Config {
	p := config.ProviderConfig{
		Name: "stub", Kind: "openai", Model: "stub-model-a",
		APIKey: "stub-key", BaseURL: s.URL,
	}
	if mutate != nil {
		mutate(&p)
	}
	cfg := &config.Config{}
	cfg.LLM.Providers = []config.ProviderConfig{p}
	return cfg
}

// buildStubModel builds the production provider stack against s and returns the
// single model in it, plus the resolved context-window map.
func buildStubModel(t *testing.T, s *stubProvider, mutate func(*config.ProviderConfig)) (
	model.BaseChatModel, map[string]int) {
	t.Helper()
	cfg := stubProviderConfig(s, mutate)
	_, chain, windows, _, err := BuildProviders(cfg)
	if err != nil {
		t.Fatalf("BuildProviders: %v", err)
	}
	if len(chain) != 1 {
		t.Fatalf("chain length = %d, want 1", len(chain))
	}
	return chain[0], windows
}
