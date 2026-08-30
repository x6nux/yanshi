package eino

import (
	"context"
	"encoding/json"
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
func TestProbeImageSupport_Unreachable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	closedURL := srv.URL
	srv.Close()

	got := ProbeImageSupport(context.Background(), nil, closedURL, "", "some-model")
	if got.Supported {
		t.Fatal("Supported = true against a closed port, want false")
	}
	if got.Source != SourceProbed {
		t.Fatalf("Source = %q, want %q", got.Source, SourceProbed)
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

// TestMultimodalSource_DistinctValues locks the two source values as
// non-empty and distinct from each other, guarding against a future edit
// collapsing the "documented vs. probed" distinction back into an
// indistinguishable pair of strings (or worse, a bool) — this is what W-C-15
// actually requires: the marker must be a real field a caller can branch on,
// not a note in a log line.
func TestMultimodalSource_DistinctValues(t *testing.T) {
	if SourceDocumented == "" || SourceProbed == "" {
		t.Fatalf("source values must be non-empty: documented=%q probed=%q", SourceDocumented, SourceProbed)
	}
	if SourceDocumented == SourceProbed {
		t.Fatalf("SourceDocumented and SourceProbed must be distinct, both are %q", SourceDocumented)
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
