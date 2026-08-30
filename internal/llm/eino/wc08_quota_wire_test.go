// internal/llm/eino/wc08_quota_wire_test.go
//
// W-C-08 over the wire: does a REAL quota-window header on a REAL (loopback)
// HTTP response actually reach the quota governor, for every adapter kind
// that can carry one?
//
// quota_test.go and ratelimit_test.go already prove the arithmetic
// (ParseQuotaWindows, quotaGovernor.delay) in isolation, by handing header
// maps and QuotaWindow values straight to the code under test. What they
// cannot see is whether the ADAPTERS actually call observeQuotaHeaders on the
// real *http.Response they hold — stubprovider_test.go's own header comment
// explains exactly why that gap matters for the "openai" kind specifically
// (go-openai's SDK drops response headers on error, so a unit test that
// hand-builds a header map tests something production could never produce).
// The same "prove the wire, not just the arithmetic" argument applies to
// anthropic.go and responses.go, which each have two call sites (Generate,
// Stream) that were added by hand and could each be missing.
package eino

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/cloudwego/eino/schema"
)

// TestWC08_OpenAIKindThrottlesRealArrivals fires two sequential calls,
// through the full production stack (BuildProviders → HeaderAwareModel →
// AdaptiveModel → RateLimiter), at a stub that reports 100% quota usage with
// a short reset-after on the FIRST response. It asserts the SECOND request
// does not reach the stub until that reset-after has elapsed — with no QPM
// configured anywhere, proving quotaGovernor is a wholly separate throttle
// from M7's TokenBucket, and that headerCaptureTransport.RoundTrip's new
// observeQuotaHeaders call (retryafter.go) actually runs on a successful
// (200) response, not just on the failures M1 already covered.
func TestWC08_OpenAIKindThrottlesRealArrivals(t *testing.T) {
	const resetAfter = 300 * time.Millisecond
	s := newStubProvider(t, func(n int, _ capturedRequest) stubResponse {
		if n == 1 {
			return stubResponse{Header: map[string]string{
				"X-Codex-Primary-Used-Percent":        "100",
				"X-Codex-Primary-Reset-After-Seconds": "0.3",
			}}
		}
		return stubResponse{}
	})
	inner, _ := buildStubModel(t, s, nil)
	limiter := NewRateLimiter(RateLimitConfig{}, nil) // no QPM configured anywhere
	a := NewAdaptiveModel(inner, AdaptiveConfig{ModelID: "stub-model-a", Limiter: limiter})

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if _, err := a.Generate(ctx, []*schema.Message{schema.UserMessage("first")}); err != nil {
		t.Fatalf("first call: %v", err)
	}
	if _, err := a.Generate(ctx, []*schema.Message{schema.UserMessage("second")}); err != nil {
		t.Fatalf("second call: %v", err)
	}

	reqs := s.chatRequests()
	if len(reqs) != 2 {
		t.Fatalf("stub saw %d requests, want 2", len(reqs))
	}
	gap := reqs[1].At.Sub(reqs[0].At)
	t.Logf("gap between calls = %v (want >= ~%v)", gap, resetAfter)
	if gap < resetAfter-50*time.Millisecond {
		t.Errorf("gap = %v, want the second call held back roughly %v by the quota header the "+
			"first response carried — the header either was not observed off the wire, or the "+
			"observation never reached the rate limiter", gap, resetAfter)
	}
}

// TestWC08_OpenAIKindNoHeadersNoThrottle is the negative control: a stub that
// never sends quota headers must not introduce any delay, so the mechanism
// above cannot be mistaken for some other source of latency.
func TestWC08_OpenAIKindNoHeadersNoThrottle(t *testing.T) {
	s := newStubProvider(t, nil) // canned success, no extra headers
	inner, _ := buildStubModel(t, s, nil)
	limiter := NewRateLimiter(RateLimitConfig{}, nil)
	a := NewAdaptiveModel(inner, AdaptiveConfig{ModelID: "stub-model-a", Limiter: limiter})

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	start := time.Now()
	for range 2 {
		if _, err := a.Generate(ctx, []*schema.Message{schema.UserMessage("x")}); err != nil {
			t.Fatalf("Generate: %v", err)
		}
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("two calls with no quota headers took %v, want no throttling at all", elapsed)
	}
}

// TestWC08_AnthropicGenerateObservesQuotaHeaders proves anthropic.go's
// Generate calls observeQuotaHeaders on its own *http.Response, by binding an
// observer directly to the context passed in (the same seam AdaptiveModel
// uses in production) and checking it fires with the header values the
// httptest server sent — on an ordinary 200, not a failure.
func TestWC08_AnthropicGenerateObservesQuotaHeaders(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Codex-Primary-Used-Percent", "77")
		w.Header().Set("X-Codex-Primary-Reset-After-Seconds", "12")
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": "msg_1", "type": "message", "role": "assistant", "stop_reason": "end_turn",
			"content": []any{map[string]any{"type": "text", "text": "hello"}},
			"usage":   map[string]any{"input_tokens": 10, "output_tokens": 5},
		})
	}))
	defer srv.Close()

	m, err := NewAnthropicModel(context.Background(), &AnthropicModelConfig{
		APIKey: "k", Model: "claude-opus-4-8", BaseURL: srv.URL,
	})
	if err != nil {
		t.Fatal(err)
	}

	var got map[string]QuotaWindow
	ctx := WithQuotaObserver(context.Background(), func(windows map[string]QuotaWindow) { got = windows })
	if _, err := m.Generate(ctx, []*schema.Message{schema.UserMessage("hi")}); err != nil {
		t.Fatal(err)
	}
	w, ok := got["primary"]
	if !ok {
		t.Fatalf("windows = %+v, want a primary window", got)
	}
	if w.UsedPercent != 77 || w.ResetAfter != 12*time.Second {
		t.Errorf("primary = %+v, want {77 0 12s}", w)
	}
}

// TestWC08_AnthropicStreamObservesQuotaHeaders is the Stream-side twin of the
// Generate test above: Stream obtains its own *http.Response independently,
// so it needs its own call site and its own proof that call site runs.
func TestWC08_AnthropicStreamObservesQuotaHeaders(t *testing.T) {
	const sseBody = "event: content_block_delta\n" +
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hi"}}` + "\n\n" +
		"event: message_stop\n" +
		`data: {"type":"message_stop"}` + "\n\n"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Codex-Primary-Used-Percent", "88")
		w.Header().Set("X-Codex-Primary-Reset-After-Seconds", "5")
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, sseBody)
	}))
	defer srv.Close()

	m, err := NewAnthropicModel(context.Background(), &AnthropicModelConfig{
		APIKey: "k", Model: "claude-opus-4-8", BaseURL: srv.URL,
	})
	if err != nil {
		t.Fatal(err)
	}

	var got map[string]QuotaWindow
	ctx := WithQuotaObserver(context.Background(), func(windows map[string]QuotaWindow) { got = windows })
	sr, err := m.Stream(ctx, []*schema.Message{schema.UserMessage("hi")})
	if err != nil {
		t.Fatal(err)
	}
	defer sr.Close()
	for {
		_, recvErr := sr.Recv()
		if recvErr != nil {
			break
		}
	}
	w, ok := got["primary"]
	if !ok {
		t.Fatalf("windows = %+v, want a primary window", got)
	}
	if w.UsedPercent != 88 || w.ResetAfter != 5*time.Second {
		t.Errorf("primary = %+v, want {88 0 5s}", w)
	}
}

// TestWC08_ResponsesGenerateObservesQuotaHeaders is the openai-responses
// twin of the anthropic Generate test above.
func TestWC08_ResponsesGenerateObservesQuotaHeaders(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Codex-Primary-Used-Percent", "60")
		w.Header().Set("X-Codex-Primary-Reset-After-Seconds", "20")
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": "resp_1", "object": "response", "status": "completed",
			"output": []any{map[string]any{
				"type": "message", "role": "assistant",
				"content": []any{map[string]any{"type": "output_text", "text": "hello"}},
			}},
			"usage": map[string]any{"input_tokens": 10, "output_tokens": 5, "total_tokens": 15},
		})
	}))
	defer srv.Close()

	m, err := NewOpenAIResponsesModel(context.Background(), &ResponsesConfig{
		APIKey: "k", Model: "gpt-4o", BaseURL: srv.URL,
	})
	if err != nil {
		t.Fatal(err)
	}

	var got map[string]QuotaWindow
	ctx := WithQuotaObserver(context.Background(), func(windows map[string]QuotaWindow) { got = windows })
	if _, err := m.Generate(ctx, []*schema.Message{schema.UserMessage("hi")}); err != nil {
		t.Fatal(err)
	}
	w, ok := got["primary"]
	if !ok {
		t.Fatalf("windows = %+v, want a primary window", got)
	}
	if w.UsedPercent != 60 || w.ResetAfter != 20*time.Second {
		t.Errorf("primary = %+v, want {60 0 20s}", w)
	}
}

// TestWC08_ResponsesStreamObservesQuotaHeaders is the Stream-side twin: the
// Responses adapter's Stream obtains its own *http.Response before checking
// status, exactly like Generate, and needs its own proof.
func TestWC08_ResponsesStreamObservesQuotaHeaders(t *testing.T) {
	const sseBody = "event: response.output_text.delta\n" +
		`data: {"type":"response.output_text.delta","delta":"hi"}` + "\n\n" +
		"event: response.completed\n" +
		`data: {"type":"response.completed","response":{"id":"resp_1","status":"completed"}}` + "\n\n"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Codex-Primary-Used-Percent", "33")
		w.Header().Set("X-Codex-Primary-Reset-After-Seconds", "7")
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, sseBody)
	}))
	defer srv.Close()

	m, err := NewOpenAIResponsesModel(context.Background(), &ResponsesConfig{
		APIKey: "k", Model: "gpt-4o", BaseURL: srv.URL,
	})
	if err != nil {
		t.Fatal(err)
	}

	var got map[string]QuotaWindow
	ctx := WithQuotaObserver(context.Background(), func(windows map[string]QuotaWindow) { got = windows })
	sr, err := m.Stream(ctx, []*schema.Message{schema.UserMessage("hi")})
	if err != nil {
		t.Fatal(err)
	}
	defer sr.Close()
	for {
		_, recvErr := sr.Recv()
		if recvErr != nil {
			break
		}
	}
	w, ok := got["primary"]
	if !ok {
		t.Fatalf("windows = %+v, want a primary window", got)
	}
	if w.UsedPercent != 33 || w.ResetAfter != 7*time.Second {
		t.Errorf("primary = %+v, want {33 0 7s}", w)
	}
}
