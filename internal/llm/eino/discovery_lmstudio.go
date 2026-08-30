package eino

// W-C-05: LM Studio integration.
//
// LM Studio exposes a native REST API (distinct from the OpenAI-compatible
// /v1/chat/completions surface config.example.yaml already documents
// pointing yanshi's own ProviderConfig at) with two generations this file
// speaks: the legacy but still-served "v0" listing endpoint
// (GET /api/v0/models — richer per-model metadata: type llm/vlm/embeddings,
// loaded/not-loaded state, max_context_length) and the current "v1" load
// endpoint (POST /api/v1/models/load — the warm-up acceptance bullet).
// Endpoint shapes verified against https://lmstudio.ai/docs/developer/rest
// and https://lmstudio.ai/docs/developer/rest/load (2026-08-30).

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// DefaultLMStudioBaseURL is LM Studio's default REST API listen address
// (LM Studio 0.3.6+, server started via `lms server start`).
const DefaultLMStudioBaseURL = "http://127.0.0.1:1234"

// LMStudioClient talks to a local LM Studio instance's native REST API.
type LMStudioClient struct {
	baseURL string
	apiKey  string
	http    *http.Client
}

// NewLMStudioClient builds a client for the LM Studio instance at baseURL
// (empty defaults to DefaultLMStudioBaseURL). apiKey is sent as a Bearer
// token when non-empty — LM Studio's server can optionally require one;
// when it doesn't, an empty apiKey is simply never sent, matching the
// docs' own curl example which only adds the header when a token is
// configured. httpClient may be nil, in which case
// localHTTPClient(DefaultDiscoveryHTTPTimeout) is used.
func NewLMStudioClient(baseURL, apiKey string, httpClient *http.Client) *LMStudioClient {
	if baseURL == "" {
		baseURL = DefaultLMStudioBaseURL
	}
	baseURL = strings.TrimRight(baseURL, "/")
	if httpClient == nil {
		httpClient = localHTTPClient(DefaultDiscoveryHTTPTimeout)
	}
	return &LMStudioClient{baseURL: baseURL, apiKey: apiKey, http: httpClient}
}

// Name implements Fetcher.
func (c *LMStudioClient) Name() string { return "lmstudio" }

func (c *LMStudioClient) newRequest(ctx context.Context, method, path string, body []byte) (*http.Request, error) {
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, reader)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.apiKey)
	}
	return req, nil
}

// Probe checks GET /api/v0/models — the same endpoint ListModels uses — as
// LM Studio's single reachability signal. Unlike Ollama, LM Studio's REST
// server has no separate lightweight root ping documented; /api/v0/models
// itself is the health check LM Studio's own quickstart docs use for this
// purpose, so Endpoints only ever contains one key.
func (c *LMStudioClient) Probe(ctx context.Context) ProbeResult {
	endpoints := map[string]bool{"api_v0_models": false}
	req, err := c.newRequest(ctx, http.MethodGet, "/api/v0/models", nil)
	if err != nil {
		return ProbeResult{Detail: fmt.Sprintf("build request: %v", err), Endpoints: endpoints}
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return ProbeResult{Detail: fmt.Sprintf("lm studio unreachable at %s: %v", c.baseURL, err), Endpoints: endpoints}
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<12))
	if resp.StatusCode != http.StatusOK {
		return ProbeResult{Detail: fmt.Sprintf("/api/v0/models answered HTTP %d", resp.StatusCode), Endpoints: endpoints}
	}
	endpoints["api_v0_models"] = true
	return ProbeResult{Available: true, Detail: "lm studio reachable, /api/v0/models responding", Endpoints: endpoints}
}

// lmStudioModelsResponse mirrors GET /api/v0/models: {"object":"list","data":[...]}.
type lmStudioModelsResponse struct {
	Data []lmStudioModelEntry `json:"data"`
}

type lmStudioModelEntry struct {
	ID               string `json:"id"`
	Type             string `json:"type"` // "llm" | "vlm" | "embeddings"
	State            string `json:"state"`
	MaxContextLength int    `json:"max_context_length"`
}

// ListModels calls GET /api/v0/models and normalizes the result.
//
// Per ADR-0025's return-value contract (identical to OllamaClient.
// ListModels): a non-nil error means "unreachable or unparsable"; a nil
// error with a possibly-empty slice means "reachable, and this is
// genuinely every model LM Studio currently knows about" — a machine with
// LM Studio installed but no model ever downloaded returns (nil, nil), not
// an error.
func (c *LMStudioClient) ListModels(ctx context.Context) ([]DiscoveredModel, error) {
	req, err := c.newRequest(ctx, http.MethodGet, "/api/v0/models", nil)
	if err != nil {
		return nil, fmt.Errorf("lmstudio: build /api/v0/models request: %w", err)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("lmstudio: unreachable at %s: %w", c.baseURL, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<22))
	if err != nil {
		return nil, fmt.Errorf("lmstudio: read /api/v0/models response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("lmstudio: /api/v0/models returned HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var parsed lmStudioModelsResponse
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, fmt.Errorf("lmstudio: parse /api/v0/models response: %w", err)
	}
	models := make([]DiscoveredModel, 0, len(parsed.Data))
	for _, entry := range parsed.Data {
		if entry.ID == "" {
			continue
		}
		models = append(models, DiscoveredModel{
			ID:                 entry.ID,
			ContextWindow:      entry.MaxContextLength,
			Loaded:             entry.State == "loaded",
			DeclaredMultimodal: entry.Type == "vlm",
		})
	}
	return models, nil
}

// FetchModels implements Fetcher for the disk cache (W-C-06). LM Studio's
// REST API documents no ETag/Last-Modified header on /api/v0/models, so
// etag is always empty — Cache substitutes its own content-hash ETag (see
// discovery_cache.go's putListing).
func (c *LMStudioClient) FetchModels(ctx context.Context) (models []DiscoveredModel, etag string, err error) {
	models, err = c.ListModels(ctx)
	return models, "", err
}

// LoadOptions carries the llama.cpp-only tuning knobs POST /api/v1/models/load
// accepts, all optional. Left at their zero value they are omitted from the
// request entirely (omitempty) rather than sent as explicit zeros/falses,
// so a caller that only wants "load this model" doesn't have to know
// llama.cpp's defaults to avoid overriding them.
//
// ponytail: only the fields LM Studio's docs list are modeled here — no
// speculative "future knob" placeholders. Add a field when the docs add one.
type LoadOptions struct {
	ContextLength       int  `json:"context_length,omitempty"`
	EvalBatchSize       int  `json:"eval_batch_size,omitempty"`
	FlashAttention      bool `json:"flash_attention,omitempty"`
	NumExperts          int  `json:"num_experts,omitempty"`
	OffloadKVCacheToGPU bool `json:"offload_kv_cache_to_gpu,omitempty"`
	EchoLoadConfig      bool `json:"echo_load_config,omitempty"`
}

// LoadResult is POST /api/v1/models/load's response body.
type LoadResult struct {
	Type            string          `json:"type"` // "llm" | "embedding"
	InstanceID      string          `json:"instance_id"`
	LoadTimeSeconds float64         `json:"load_time_seconds"`
	Status          string          `json:"status"`
	LoadConfig      json.RawMessage `json:"load_config,omitempty"`
}

// LoadModel issues POST /api/v1/models/load to warm the given model into
// memory — the acceptance bullet's "load_model 预热". opts may be the zero
// LoadOptions to request every llama.cpp default.
//
// A cold load can take many seconds (LoadResult.LoadTimeSeconds in the
// docs' own sample response is under 10s for a mid-size model, but a large
// one on slow storage can run far longer); ctx governs how long this waits,
// not DefaultDiscoveryHTTPTimeout — callers pass a context with whatever
// deadline is appropriate, the same contract as OllamaClient.PullModel.
func (c *LMStudioClient) LoadModel(ctx context.Context, model string, opts LoadOptions) (LoadResult, error) {
	reqBody := struct {
		Model string `json:"model"`
		LoadOptions
	}{Model: model, LoadOptions: opts}
	payload, err := json.Marshal(reqBody)
	if err != nil {
		return LoadResult{}, fmt.Errorf("lmstudio: encode load request: %w", err)
	}
	req, err := c.newRequest(ctx, http.MethodPost, "/api/v1/models/load", payload)
	if err != nil {
		return LoadResult{}, fmt.Errorf("lmstudio: build load request: %w", err)
	}
	// Loading has no fixed upper bound (see the doc comment above) — like
	// OllamaClient.PullModel, use a client that shares the transport (still
	// bypasses HTTP_PROXY) but has no timeout of its own; ctx is the only
	// deadline.
	loadClient := &http.Client{Transport: c.http.Transport}
	resp, err := loadClient.Do(req)
	if err != nil {
		return LoadResult{}, fmt.Errorf("lmstudio: load request failed: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return LoadResult{}, fmt.Errorf("lmstudio: read load response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return LoadResult{}, fmt.Errorf("lmstudio: load returned HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var result LoadResult
	if err := json.Unmarshal(body, &result); err != nil {
		return LoadResult{}, fmt.Errorf("lmstudio: parse load response: %w", err)
	}
	return result, nil
}

// ProbeImageSupport sends a real image to model over LM Studio's
// OpenAI-compatible /v1/chat/completions endpoint (a different surface than
// /api/v1/models/load above — LM Studio serves the OpenAI-compatible layer
// unprefixed at /v1, not under /api/v1) and classifies whether it was
// actually processed. See the package-level ProbeImageSupport function for
// the classification rules.
func (c *LMStudioClient) ProbeImageSupport(ctx context.Context, model string) ImageSupport {
	return ProbeImageSupport(ctx, c.http, c.baseURL+"/v1/chat/completions", c.apiKey, model)
}
