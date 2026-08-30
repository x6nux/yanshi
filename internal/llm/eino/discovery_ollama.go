package eino

// W-C-03: Ollama deep integration.
//
// Ollama exposes two API surfaces on the same port: a native surface
// (root health ping, /api/tags, /api/pull, ...) and an OpenAI-compatible
// surface (/v1/chat/completions, /v1/models) that config.example.yaml
// already documents operators pointing yanshi's own ProviderConfig.BaseURL
// at. This file only talks to the NATIVE surface — /v1/models is already
// covered by discover.go's M9 preflight for whatever provider is actually
// configured; what's missing, and what W-C-03's acceptance bullets ask for,
// is native-API functionality that surface doesn't expose at all: pulling a
// model with streaming progress, and a two-endpoint reachability probe that
// can tell "daemon is down" apart from "daemon is up but /api/tags itself
// is erroring".

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// DefaultOllamaBaseURL is Ollama's default listen address. Ollama binds
// 127.0.0.1:11434 unless OLLAMA_HOST overrides it; ProviderConfig.BaseURL
// (when kind is "openai" pointed at Ollama's /v1 surface) is the operator's
// existing way to say otherwise for this client too — see NewOllamaClient.
const DefaultOllamaBaseURL = "http://127.0.0.1:11434"

// OllamaClient talks to a local Ollama daemon's native API (not its
// OpenAI-compatible /v1 surface — see the package-level comment above).
type OllamaClient struct {
	baseURL string
	http    *http.Client
}

// NewOllamaClient builds a client for the Ollama daemon at baseURL (no
// trailing /v1 — pass the root, e.g. "http://127.0.0.1:11434"; a trailing
// "/v1" is stripped automatically since that's what operators already have
// in ProviderConfig.BaseURL and copying it verbatim is the likely mistake).
// An empty baseURL defaults to DefaultOllamaBaseURL. httpClient may be nil,
// in which case localHTTPClient(DefaultDiscoveryHTTPTimeout) is used — see
// that function's doc comment for why local discovery never goes through an
// operator's HTTP_PROXY.
func NewOllamaClient(baseURL string, httpClient *http.Client) *OllamaClient {
	if baseURL == "" {
		baseURL = DefaultOllamaBaseURL
	}
	baseURL = strings.TrimSuffix(strings.TrimRight(baseURL, "/"), "/v1")
	baseURL = strings.TrimRight(baseURL, "/")
	if httpClient == nil {
		httpClient = localHTTPClient(DefaultDiscoveryHTTPTimeout)
	}
	return &OllamaClient{baseURL: baseURL, http: httpClient}
}

// Name implements Fetcher.
func (c *OllamaClient) Name() string { return "ollama" }

// Probe checks BOTH of Ollama's relevant endpoints — the acceptance bullet
// is explicit that this is a dual-endpoint probe, not a single ping. The
// root ("/") answers plaintext "Ollama is running" the moment the daemon's
// HTTP server is up, before it has necessarily finished loading anything;
// /api/tags is the endpoint every other method in this file depends on. A
// daemon that answers the root but errors on /api/tags (a corrupted models
// directory, a permissions problem) is reported as unavailable overall —
// nothing downstream can work without a working /api/tags — but
// Endpoints["root"]=true still distinguishes "the process is up and
// something is wrong" from "nothing is listening at all", which the
// all-false case (Available=false, both endpoints false) reports instead.
func (c *OllamaClient) Probe(ctx context.Context) ProbeResult {
	endpoints := map[string]bool{"root": false, "api_tags": false}
	rootDetail := c.probeRoot(ctx, endpoints)
	tagsDetail := c.probeTags(ctx, endpoints)
	if endpoints["root"] && endpoints["api_tags"] {
		return ProbeResult{Available: true, Detail: "ollama daemon reachable, /api/tags responding", Endpoints: endpoints}
	}
	detail := rootDetail
	if endpoints["root"] {
		detail = tagsDetail
	}
	return ProbeResult{Available: false, Detail: detail, Endpoints: endpoints}
}

func (c *OllamaClient) probeRoot(ctx context.Context, endpoints map[string]bool) string {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/", nil)
	if err != nil {
		return fmt.Sprintf("build root request: %v", err)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Sprintf("ollama daemon unreachable at %s: %v", c.baseURL, err)
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<12))
	if resp.StatusCode != http.StatusOK {
		return fmt.Sprintf("ollama daemon at %s answered root with HTTP %d", c.baseURL, resp.StatusCode)
	}
	endpoints["root"] = true
	return ""
}

func (c *OllamaClient) probeTags(ctx context.Context, endpoints map[string]bool) string {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/api/tags", nil)
	if err != nil {
		return fmt.Sprintf("build /api/tags request: %v", err)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Sprintf("/api/tags unreachable: %v", err)
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<12))
	if resp.StatusCode != http.StatusOK {
		return fmt.Sprintf("/api/tags answered HTTP %d", resp.StatusCode)
	}
	endpoints["api_tags"] = true
	return ""
}

// ollamaTagsResponse mirrors GET /api/tags' wire shape: {"models":[{...}]}.
type ollamaTagsResponse struct {
	Models []ollamaTagEntry `json:"models"`
}

type ollamaTagEntry struct {
	Name    string `json:"name"`
	Model   string `json:"model"`
	Details struct {
		Families []string `json:"families"`
	} `json:"details"`
}

// ListModels calls GET /api/tags and normalizes the result.
//
// Per ADR-0025's return-value contract: a non-nil error means "unreachable
// or unparsable — we don't know what Ollama has"; a nil error with a
// possibly-empty slice means "reachable, and this is genuinely everything
// it has pulled" — an empty slice on success is never turned into an error,
// unlike discover.go's FetchModelCatalog, which is the shape this method
// deliberately does not repeat (see the package-level ADR-0025 discussion
// in discovery.go).
func (c *OllamaClient) ListModels(ctx context.Context) ([]DiscoveredModel, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/api/tags", nil)
	if err != nil {
		return nil, fmt.Errorf("ollama: build /api/tags request: %w", err)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("ollama: daemon unreachable at %s: %w", c.baseURL, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<22))
	if err != nil {
		return nil, fmt.Errorf("ollama: read /api/tags response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("ollama: /api/tags returned HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var parsed ollamaTagsResponse
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, fmt.Errorf("ollama: parse /api/tags response: %w", err)
	}
	models := make([]DiscoveredModel, 0, len(parsed.Models))
	for _, entry := range parsed.Models {
		id := entry.Name
		if id == "" {
			id = entry.Model
		}
		if id == "" {
			continue
		}
		models = append(models, DiscoveredModel{ID: id})
	}
	return models, nil
}

// FetchModels implements Fetcher for the disk cache (W-C-06). Ollama's
// native API has no ETag/Last-Modified concept, so etag is always empty —
// Cache falls back to its own content-hash ETag in that case (see
// discovery_cache.go's putListing).
func (c *OllamaClient) FetchModels(ctx context.Context) (models []DiscoveredModel, etag string, err error) {
	models, err = c.ListModels(ctx)
	return models, "", err
}

// PullProgress is one line of Ollama's /api/pull NDJSON stream.
//
// Ollama's own docs describe the stream as "a series of JSON objects" — one
// object per line, no enclosing array — which is exactly what
// json.Decoder.Decode called in a loop consumes; PullModel does not need to
// split on newlines itself.
type PullProgress struct {
	Status    string `json:"status"`
	Digest    string `json:"digest,omitempty"`
	Total     int64  `json:"total,omitempty"`
	Completed int64  `json:"completed,omitempty"`
	Error     string `json:"error,omitempty"`
}

// PullModel issues POST /api/pull for model and streams progress to
// onProgress as each NDJSON object arrives. onProgress may be nil to
// discard progress and just wait for completion or the first error.
//
// A pull can legitimately take many minutes (multi-gigabyte weights over a
// slow connection); ctx — not DefaultDiscoveryHTTPTimeout — governs how
// long this runs, so callers must pass a context with whatever deadline (or
// none) is appropriate for the model being pulled, not rely on this method
// picking one.
func (c *OllamaClient) PullModel(ctx context.Context, model string, onProgress func(PullProgress)) error {
	payload, err := json.Marshal(map[string]string{"model": model})
	if err != nil {
		return fmt.Errorf("ollama: encode pull request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/api/pull", strings.NewReader(string(payload)))
	if err != nil {
		return fmt.Errorf("ollama: build pull request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	// The pull client has no request timeout of its own (see the doc
	// comment above) — a bare c.http.Do would still be bound by
	// c.http.Timeout (DefaultDiscoveryHTTPTimeout, 10s) if it were set on
	// the shared client, which would abort a real multi-minute pull. Build
	// a dedicated client that shares the transport (so it still bypasses
	// any HTTP_PROXY, per localHTTPClient) but has no timeout of its own —
	// ctx is the only deadline that governs this call.
	pullClient := &http.Client{Transport: c.http.Transport}
	resp, err := pullClient.Do(req)
	if err != nil {
		return fmt.Errorf("ollama: pull request failed: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
		return fmt.Errorf("ollama: pull returned HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	dec := json.NewDecoder(resp.Body)
	for {
		var line PullProgress
		if err := dec.Decode(&line); err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return fmt.Errorf("ollama: parse pull progress: %w", err)
		}
		if line.Error != "" {
			return fmt.Errorf("ollama: pull failed: %s", line.Error)
		}
		if onProgress != nil {
			onProgress(line)
		}
		if strings.EqualFold(line.Status, "success") {
			return nil
		}
	}
}

// ProbeImageSupport sends a real image to model over Ollama's
// OpenAI-compatible /v1/chat/completions endpoint and classifies whether it
// was actually processed — see the package-level ProbeImageSupport function
// for the classification rules. Ollama serves both its native API and this
// OpenAI-compatible surface on the same port, so the request goes to
// c.baseURL+"/v1/chat/completions" — no separate configuration.
func (c *OllamaClient) ProbeImageSupport(ctx context.Context, model string) ImageSupport {
	return ProbeImageSupport(ctx, c.http, c.baseURL+"/v1/chat/completions", "", model)
}
