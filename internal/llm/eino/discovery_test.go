package eino

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestProbeImageSupport_RedAnswer proves the correct-color-name response
// (the actual expected behavior from a real vision model looking at the
// probe's solid-red pixel) classifies as Supported=true, SourceProbed.
func TestProbeImageSupport_RedAnswer(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req chatProbeRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("decode probe request: %v", err)
		}
		if len(req.Messages) != 1 || len(req.Messages[0].Content) != 2 {
			t.Fatalf("request shape = %+v, want one message with text+image_url parts", req)
		}
		if req.Messages[0].Content[1].ImageURL == nil || !strings.HasPrefix(req.Messages[0].Content[1].ImageURL.URL, "data:image/png;base64,") {
			t.Fatalf("image_url = %+v, want a data: URI", req.Messages[0].Content[1].ImageURL)
		}
		json.NewEncoder(w).Encode(chatProbeResponse{
			Choices: []struct {
				Message struct {
					Content string `json:"content"`
				} `json:"message"`
			}{{Message: struct {
				Content string `json:"content"`
			}{Content: "RED"}}},
		})
	}))
	defer srv.Close()

	got := ProbeImageSupport(context.Background(), nil, srv.URL, "", "some-vlm")
	if !got.Supported {
		t.Fatalf("Supported = false, want true; detail=%q", got.Detail)
	}
	if got.Source != SourceProbed {
		t.Fatalf("Source = %q, want %q", got.Source, SourceProbed)
	}
}

// TestProbeImageSupport_NoVisionAnswer proves the model's own honest refusal
// (the explicit escape hatch probeImagePrompt offers) classifies as
// Supported=false, not an error and not a guess.
func TestProbeImageSupport_NoVisionAnswer(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{{"message": map[string]any{"content": "NO_VISION"}}},
		})
	}))
	defer srv.Close()

	got := ProbeImageSupport(context.Background(), nil, srv.URL, "", "text-only-model")
	if got.Supported {
		t.Fatal("Supported = true, want false for an explicit NO_VISION answer")
	}
	if got.Source != SourceProbed {
		t.Fatalf("Source = %q, want %q", got.Source, SourceProbed)
	}
}

// TestProbeImageSupport_AmbiguousAnswer proves the silent-ignore case this
// probe exists to catch: a 200 response that neither names the color nor
// admits it cannot see must be classified unsupported (not guessed
// Supported=true just because the HTTP call succeeded).
func TestProbeImageSupport_AmbiguousAnswer(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{{"message": map[string]any{"content": "I am an AI assistant, how can I help you today?"}}},
		})
	}))
	defer srv.Close()

	got := ProbeImageSupport(context.Background(), nil, srv.URL, "", "some-model")
	if got.Supported {
		t.Fatal("Supported = true, want false for an answer that neither confirms nor denies seeing the image")
	}
	if got.Detail == "" {
		t.Fatal("Detail is empty, want an explanation of the ambiguity")
	}
}

// TestProbeImageSupport_HTTPErrorWithMessage proves a non-200 response whose
// body carries {"error":{"message":...}} surfaces that message in Detail
// rather than the raw (possibly opaque) body text.
func TestProbeImageSupport_HTTPErrorWithMessage(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]any{
			"error": map[string]any{"message": "model does not support image input"},
		})
	}))
	defer srv.Close()

	got := ProbeImageSupport(context.Background(), nil, srv.URL, "", "text-model")
	if got.Supported {
		t.Fatal("Supported = true, want false for a 400 response")
	}
	if !strings.Contains(got.Detail, "model does not support image input") {
		t.Fatalf("Detail = %q, want it to carry the endpoint's error message", got.Detail)
	}
}

// TestProbeImageSupport_HTTPErrorPlainBody proves a non-200 response with a
// body that is not the expected {"error":{"message":...}} shape still
// produces a usable Detail (falls back to the raw body) instead of an empty
// or panicking result.
func TestProbeImageSupport_HTTPErrorPlainBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "internal server error", http.StatusInternalServerError)
	}))
	defer srv.Close()

	got := ProbeImageSupport(context.Background(), nil, srv.URL, "", "some-model")
	if got.Supported {
		t.Fatal("Supported = true, want false for a 500 response")
	}
	if !strings.Contains(got.Detail, "internal server error") {
		t.Fatalf("Detail = %q, want it to carry the raw body", got.Detail)
	}
}

// TestProbeImageSupport_NoChoices proves a 200 response with an empty
// choices array (a malformed but not-erroring backend) is reported as
// unsupported with an explanatory detail, not a panic on index 0.
func TestProbeImageSupport_NoChoices(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(chatProbeResponse{})
	}))
	defer srv.Close()

	got := ProbeImageSupport(context.Background(), nil, srv.URL, "", "some-model")
	if got.Supported {
		t.Fatal("Supported = true, want false when choices is empty")
	}
	if got.Detail == "" {
		t.Fatal("Detail is empty, want an explanation")
	}
}

// TestProbeImageSupport_Unreachable proves a connection failure produces an
// unsupported verdict with a truthful detail rather than hanging or
// panicking — the probe itself must degrade gracefully like the Probe
// methods do.
//
// Before M-1 (C3 review), this asserted Source == SourceProbed — the same
// value a genuine "model looked at the image and said NO_VISION" result
// carries. That collapse is exactly the M-1 bug: combined with Cache having
// no TTL/invalidation for ImageSupport entries, one transient connection
// failure at probe time became a PERMANENT false verdict once written to
// disk, indistinguishable from a real measurement. A closed port is a
// probe that never got to ask the model anything, so it must carry
// SourceProbeFailed instead.
func TestProbeImageSupport_Unreachable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	closedURL := srv.URL
	srv.Close()

	got := ProbeImageSupport(context.Background(), nil, closedURL, "", "some-model")
	if got.Supported {
		t.Fatal("Supported = true against a closed port, want false")
	}
	if got.Source != SourceProbeFailed {
		t.Fatalf("Source = %q, want %q (M-1: a connection failure is not a measurement)", got.Source, SourceProbeFailed)
	}
	if got.Detail == "" {
		t.Fatal("Detail is empty, want a truthful reason")
	}
}

// TestProbeImageSupport_BuildRequestError proves a request the standard
// library itself refuses to build (a malformed URL, here via an ASCII
// control character http.NewRequestWithContext's own validation rejects)
// also carries SourceProbeFailed — this never even reaches the network, so
// it is even less of a "measurement" than the closed-port case above.
func TestProbeImageSupport_BuildRequestError(t *testing.T) {
	got := ProbeImageSupport(context.Background(), nil, "http://\x7f/", "", "some-model")
	if got.Supported {
		t.Fatal("Supported = true for a malformed request URL, want false")
	}
	if got.Source != SourceProbeFailed {
		t.Fatalf("Source = %q, want %q (M-1: a request-build failure is not a measurement)", got.Source, SourceProbeFailed)
	}
	if got.Detail == "" {
		t.Fatal("Detail is empty, want a truthful reason")
	}
}

// probeBrokenBodyTransport is an http.RoundTripper whose response body
// fails on Read after headers are already sent — the shape a well-formed
// 200 response with a connection that drops mid-body produces, exercising
// ProbeImageSupport's io.ReadAll(resp.Body) failure branch specifically
// (distinct from BuildRequestError and Unreachable, which fail before any
// response exists at all).
type probeBrokenBodyTransport struct{}

type brokenReadCloser struct{}

func (brokenReadCloser) Read(p []byte) (int, error) {
	return 0, fmt.Errorf("simulated body read failure")
}
func (brokenReadCloser) Close() error { return nil }

func (probeBrokenBodyTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	return &http.Response{
		StatusCode: http.StatusOK,
		Body:       brokenReadCloser{},
		Header:     make(http.Header),
	}, nil
}

// TestProbeImageSupport_ReadResponseError proves a response whose body
// fails mid-read also carries SourceProbeFailed — the probe got a live HTTP
// response but never got far enough to read what the model said, so it
// still hasn't measured anything.
func TestProbeImageSupport_ReadResponseError(t *testing.T) {
	client := &http.Client{Transport: probeBrokenBodyTransport{}}
	got := ProbeImageSupport(context.Background(), client, "http://example.invalid/v1/chat/completions", "", "some-model")
	if got.Supported {
		t.Fatal("Supported = true for a response-read failure, want false")
	}
	if got.Source != SourceProbeFailed {
		t.Fatalf("Source = %q, want %q (M-1: a response-read failure is not a measurement)", got.Source, SourceProbeFailed)
	}
	if got.Detail == "" {
		t.Fatal("Detail is empty, want a truthful reason")
	}
}

// TestProbeImageSupport_SendsAPIKey proves apiKey, when non-empty, is sent
// as a Bearer token.
func TestProbeImageSupport_SendsAPIKey(t *testing.T) {
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{{"message": map[string]any{"content": "RED"}}},
		})
	}))
	defer srv.Close()

	ProbeImageSupport(context.Background(), nil, srv.URL, "sk-probe-key", "some-model")
	if gotAuth != "Bearer sk-probe-key" {
		t.Fatalf("Authorization = %q, want Bearer sk-probe-key", gotAuth)
	}
}

// TestDocumentedImageSupport proves the documentation-side constructor tags
// its verdict SourceDocumented (never SourceProbed) and carries the origin
// string through to Detail — the field a caller distinguishes "asserted"
// from "measured" on, per W-C-15's source-marker requirement.
func TestDocumentedImageSupport(t *testing.T) {
	got := DocumentedImageSupport(true, "ProviderConfig.Multimodal")
	if !got.Supported {
		t.Fatal("Supported = false, want true")
	}
	if got.Source != SourceDocumented {
		t.Fatalf("Source = %q, want %q", got.Source, SourceDocumented)
	}
	if got.Detail != "ProviderConfig.Multimodal" {
		t.Fatalf("Detail = %q, want the origin string", got.Detail)
	}

	gotFalse := DocumentedImageSupport(false, "LM Studio type=vlm")
	if gotFalse.Supported {
		t.Fatal("Supported = true, want false")
	}
	if gotFalse.Source != SourceDocumented {
		t.Fatalf("Source = %q, want %q", gotFalse.Source, SourceDocumented)
	}
}

// TestMultimodalSource_DistinctValues locks the three source values as
// non-empty and pairwise distinct, guarding against a future edit collapsing
// the "documented vs. probed vs. probe-failed" distinction back into fewer
// indistinguishable strings (or worse, a bool) — this is what W-C-15 and M-1
// (C3 review) actually require: each marker must be a real, distinct value a
// caller can branch on, not a note in a log line.
func TestMultimodalSource_DistinctValues(t *testing.T) {
	values := map[MultimodalSource]string{
		SourceDocumented:  "SourceDocumented",
		SourceProbed:      "SourceProbed",
		SourceProbeFailed: "SourceProbeFailed",
	}
	if len(values) != 3 {
		t.Fatalf("only %d distinct source values among SourceDocumented/SourceProbed/SourceProbeFailed, want 3 — two collapsed onto the same string", len(values))
	}
	for v, name := range values {
		if v == "" {
			t.Fatalf("%s is empty, want a non-empty marker", name)
		}
	}
}

// TestNoProxyTransport_NilFallsBackToNoProxy proves the L-3(b) fix directly:
// when a caller's http.Client has a nil Transport (built with only Timeout
// set, e.g. NewOllamaClient(url, &http.Client{Timeout: t})),
// noProxyTransport must NOT return nil (which `&http.Client{Transport: nil}`
// would silently resolve to http.DefaultTransport — the operator-proxy leak
// localHTTPClient's doc comment says this package must never have).
func TestNoProxyTransport_NilFallsBackToNoProxy(t *testing.T) {
	got := noProxyTransport(nil)
	tr, ok := got.(*http.Transport)
	if !ok {
		t.Fatalf("noProxyTransport(nil) = %T, want *http.Transport", got)
	}
	if tr.Proxy != nil {
		t.Fatal("noProxyTransport(nil).Proxy is set, want nil")
	}
}

// TestNoProxyTransport_NonNilPassthrough proves the other half: a caller
// that DID set an explicit Transport (the normal case via NewOllamaClient's
// own default, localHTTPClient) gets that exact instance back unchanged,
// not silently replaced.
func TestNoProxyTransport_NonNilPassthrough(t *testing.T) {
	want := &http.Transport{Proxy: nil}
	got := noProxyTransport(want)
	if got != http.RoundTripper(want) {
		t.Fatalf("noProxyTransport returned a different instance, want the exact one passed in")
	}
}

// TestLocalHTTPClient_NoProxy proves the discovery HTTP client never routes
// through an operator-configured proxy — the transport's Proxy func must be
// nil, matching internal/netpolicy/proxy.go's identical choice for the same
// "this is loopback traffic" reason documented on localHTTPClient itself.
func TestLocalHTTPClient_NoProxy(t *testing.T) {
	c := localHTTPClient(DefaultDiscoveryHTTPTimeout)
	tr, ok := c.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("Transport = %T, want *http.Transport", c.Transport)
	}
	if tr.Proxy != nil {
		t.Fatal("Transport.Proxy is set, want nil so loopback discovery never goes through HTTP_PROXY")
	}
	if c.Timeout != DefaultDiscoveryHTTPTimeout {
		t.Fatalf("Timeout = %v, want %v", c.Timeout, DefaultDiscoveryHTTPTimeout)
	}
}
