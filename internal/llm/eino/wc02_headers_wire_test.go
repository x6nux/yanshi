// internal/llm/eino/wc02_headers_wire_test.go
//
// W-C-02 over the wire: does a configured Headers value actually leave the
// process on the outgoing request, for all three adapter kinds, and does it
// override a built-in header name rather than losing silently to it?
//
// This file proves the INJECTION half. The REDACTION half — that the same
// value, once it comes back embedded in a genuine provider error, does not
// survive into whatever the redactor's consumers see — is proved in
// internal/bootstrap's TestWC02_HeaderValuesAreRedacted, fed by the real
// error TestWC02_HeaderValueSurvivesIntoAGenuineProviderError produces below
// (not a hand-built string): "the redactor exists" and "the redactor covers
// this path" are two different claims, and only a value that actually made
// the round trip through real adapter code can tell them apart.
package eino

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/cloudwego/eino/schema"

	"github.com/x6nux/yanshi/internal/config"
)

// wcCanaryHeader / wcCanaryValue are the header name/value used across this
// file's tests. The value is deliberately distinctive so a grep for it in
// bootstrap's redaction test cannot accidentally match anything else.
const (
	wcCanaryHeader = "X-Gateway-Token"
	wcCanaryValue  = "wc02-canary-9f3a7c1e"
)

// TestWC02_HeaderReachesTheWireForOpenAIKind drives a real request through
// BuildProviders (the "openai" default kind, go-openai) and asserts the
// STUB — a real loopback HTTP server, not a mock at the model.BaseChatModel
// seam — actually received the configured header. This is the injection
// point wired in retryafter.go's headerCaptureTransport, since go-openai owns
// request construction end to end and gives the adapter no other seam.
func TestWC02_HeaderReachesTheWireForOpenAIKind(t *testing.T) {
	s := newStubProvider(t, nil) // always succeeds
	m, _ := buildStubModel(t, s, func(p *config.ProviderConfig) {
		p.Headers = map[string]string{wcCanaryHeader: wcCanaryValue}
	})
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if _, err := m.Generate(ctx, []*schema.Message{schema.UserMessage("hi")}); err != nil {
		t.Fatalf("Generate: %v", err)
	}
	reqs := s.chatRequests()
	if len(reqs) != 1 {
		t.Fatalf("chat requests = %d, want 1", len(reqs))
	}
	if got := reqs[0].Header.Get(wcCanaryHeader); got != wcCanaryValue {
		t.Errorf("stub saw %s=%q, want %q — the configured header never reached the wire", wcCanaryHeader, got, wcCanaryValue)
	}
}

// TestWC02_HeaderOverridesBuiltinAuthorizationForOpenAIKind pins the ordering
// decision documented on headerCaptureTransport: an operator entry applies
// AFTER go-openai's own headers, so naming "Authorization" lets an operator
// route through a proxy expecting its own scheme rather than losing silently
// to the SDK's "Bearer <api_key>".
func TestWC02_HeaderOverridesBuiltinAuthorizationForOpenAIKind(t *testing.T) {
	s := newStubProvider(t, nil)
	m, _ := buildStubModel(t, s, func(p *config.ProviderConfig) {
		p.APIKey = "stub-key"
		p.Headers = map[string]string{"Authorization": "Bearer overridden-by-operator"}
	})
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if _, err := m.Generate(ctx, []*schema.Message{schema.UserMessage("hi")}); err != nil {
		t.Fatalf("Generate: %v", err)
	}
	reqs := s.chatRequests()
	if len(reqs) != 1 {
		t.Fatalf("chat requests = %d, want 1", len(reqs))
	}
	if got := reqs[0].Auth; got != "Bearer overridden-by-operator" {
		t.Errorf("stub saw Authorization=%q, want the operator's override — go-openai's own %q won silently", got, "Bearer stub-key")
	}
}

// TestWC02_HeaderReachesTheWireForAnthropicKind mirrors the openai-kind test
// for the hand-rolled Anthropic adapter, which owns setHeaders itself rather
// than relying on a shared transport.
func TestWC02_HeaderReachesTheWireForAnthropicKind(t *testing.T) {
	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Get(wcCanaryHeader)
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
		Headers: map[string]string{wcCanaryHeader: wcCanaryValue},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.Generate(context.Background(), []*schema.Message{schema.UserMessage("hi")}); err != nil {
		t.Fatal(err)
	}
	if got != wcCanaryValue {
		t.Errorf("stub saw %s=%q, want %q", wcCanaryHeader, got, wcCanaryValue)
	}
}

// TestWC02_HeaderOverridesBuiltinAnthropicVersion pins the same
// override-after-built-ins ordering as the openai-kind test, but for a
// built-in the Anthropic adapter itself sets (anthropic-version) rather than
// one a shared transport injects — proving the ordering decision in
// setHeaders (loop runs AFTER the three Set calls) actually holds.
func TestWC02_HeaderOverridesBuiltinAnthropicVersion(t *testing.T) {
	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Get("anthropic-version")
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
		Headers: map[string]string{"anthropic-version": "2099-01-01-operator-pinned"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.Generate(context.Background(), []*schema.Message{schema.UserMessage("hi")}); err != nil {
		t.Fatal(err)
	}
	if got != "2099-01-01-operator-pinned" {
		t.Errorf("stub saw anthropic-version=%q, want the operator override, not the built-in default", got)
	}
}

// TestWC02_HeaderReachesTheWireForResponsesKind mirrors the same proof for
// the hand-rolled OpenAI Responses adapter.
func TestWC02_HeaderReachesTheWireForResponsesKind(t *testing.T) {
	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Get(wcCanaryHeader)
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
		Headers: map[string]string{wcCanaryHeader: wcCanaryValue},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.Generate(context.Background(), []*schema.Message{schema.UserMessage("hi")}); err != nil {
		t.Fatal(err)
	}
	if got != wcCanaryValue {
		t.Errorf("stub saw %s=%q, want %q", wcCanaryHeader, got, wcCanaryValue)
	}
}

// TestWC02_HeaderValueSurvivesIntoAGenuineProviderError is the negative
// control: it proves the leak this feature could cause is REAL, not a straw
// man, before anything claims to have closed it.
//
// The stub plays a realistic misbehaving gateway: on rejection, it echoes
// back the header value it received in the error body — the same pattern a
// misconfigured corporate proxy uses when it reports "invalid token: <what
// you sent>". anthropic.go's non-200 branch (`"anthropic: API error (HTTP
// %d): %s"`, response body verbatim) then puts that value into a genuine
// *error* this test never hand-constructs. internal/bootstrap's
// TestWC02_HeaderValuesAreRedacted reruns this exact stub-and-adapter
// pattern (it cannot import this _test.go file's helpers across packages)
// and asserts the redactor scrubs the same kind of real error text; this
// test is what proves that text is a genuine leak, not a straw man, so if
// this assertion ever starts failing the bootstrap one would be vacuous —
// asserting that redaction removes a value that was never actually present.
func TestWC02_HeaderValueSurvivesIntoAGenuineProviderError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen := r.Header.Get(wcCanaryHeader)
		w.WriteHeader(http.StatusUnauthorized)
		body, _ := json.Marshal(map[string]any{
			"error": map[string]any{"message": "invalid gateway token: " + seen, "type": "authentication_error"},
		})
		_, _ = w.Write(body)
	}))
	defer srv.Close()

	m, err := NewAnthropicModel(context.Background(), &AnthropicModelConfig{
		APIKey: "k", Model: "claude-opus-4-8", BaseURL: srv.URL,
		Headers: map[string]string{wcCanaryHeader: wcCanaryValue},
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = m.Generate(context.Background(), []*schema.Message{schema.UserMessage("hi")})
	if err == nil {
		t.Fatal("want an error from the 401 stub")
	}
	if !strings.Contains(err.Error(), wcCanaryValue) {
		t.Fatalf("err.Error() = %q, want it to contain %q — the stub's gateway-style echo did not "+
			"reach the real error, so this test is not exercising the leak it claims to", err.Error(), wcCanaryValue)
	}
}
