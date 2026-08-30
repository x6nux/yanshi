package eino

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestLMStudioClient_Probe_Up proves the happy path: /api/v0/models
// answering 200 marks the single Endpoints entry true.
func TestLMStudioClient_Probe_Up(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v0/models" {
			t.Errorf("path = %q, want /api/v0/models", r.URL.Path)
		}
		json.NewEncoder(w).Encode(lmStudioModelsResponse{})
	}))
	defer srv.Close()

	c := NewLMStudioClient(srv.URL, "", nil)
	got := c.Probe(context.Background())
	if !got.Available {
		t.Fatalf("Available = false, want true; detail=%q", got.Detail)
	}
	if !got.Endpoints["api_v0_models"] {
		t.Fatalf("Endpoints = %v, want api_v0_models true", got.Endpoints)
	}
}

// TestLMStudioClient_Probe_Down proves the "report truthfully when
// unavailable" acceptance bullet: a closed port must report Available=false,
// not panic or hang.
func TestLMStudioClient_Probe_Down(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	closedURL := srv.URL
	srv.Close()

	c := NewLMStudioClient(closedURL, "", nil)
	got := c.Probe(context.Background())
	if got.Available {
		t.Fatal("Available = true against a closed port, want false")
	}
	if got.Endpoints["api_v0_models"] {
		t.Fatal("Endpoints[\"api_v0_models\"] = true, want false")
	}
	if got.Detail == "" {
		t.Fatal("Detail is empty, want a truthful reason")
	}
}

// TestLMStudioClient_ListModels_ReachableButEmpty mirrors the Ollama
// headline assertion for LM Studio: a running instance with no model ever
// downloaded must return (empty slice, nil error), not an error.
func TestLMStudioClient_ListModels_ReachableButEmpty(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(lmStudioModelsResponse{Data: []lmStudioModelEntry{}})
	}))
	defer srv.Close()

	c := NewLMStudioClient(srv.URL, "", nil)
	models, err := c.ListModels(context.Background())
	if err != nil {
		t.Fatalf("ListModels err = %v, want nil", err)
	}
	if len(models) != 0 {
		t.Fatalf("models = %v, want empty", models)
	}
}

// TestLMStudioClient_ListModels_Unreachable is the other half of the same
// contract: an unreachable instance must return a non-nil error.
func TestLMStudioClient_ListModels_Unreachable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	closedURL := srv.URL
	srv.Close()

	c := NewLMStudioClient(closedURL, "", nil)
	models, err := c.ListModels(context.Background())
	if err == nil {
		t.Fatalf("ListModels err = nil, models = %v, want a non-nil error", models)
	}
}

// TestLMStudioClient_ListModels_MapsV0Fields proves the v0 schema mapping:
// type "vlm" -> DeclaredMultimodal, state "loaded" -> Loaded,
// max_context_length -> ContextWindow.
func TestLMStudioClient_ListModels_MapsV0Fields(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"object": "list",
			"data": []map[string]any{
				{"id": "qwen2-vl-7b", "type": "vlm", "state": "loaded", "max_context_length": 32768},
				{"id": "llama-3.1-8b", "type": "llm", "state": "not-loaded", "max_context_length": 8192},
				{"id": "", "type": "llm"}, // no id — skipped
			},
		})
	}))
	defer srv.Close()

	c := NewLMStudioClient(srv.URL, "", nil)
	models, err := c.ListModels(context.Background())
	if err != nil {
		t.Fatalf("ListModels err = %v", err)
	}
	if len(models) != 2 {
		t.Fatalf("models = %+v, want 2 entries", models)
	}
	if models[0].ID != "qwen2-vl-7b" || !models[0].DeclaredMultimodal || !models[0].Loaded || models[0].ContextWindow != 32768 {
		t.Errorf("models[0] = %+v, want vlm/loaded/32768", models[0])
	}
	if models[1].ID != "llama-3.1-8b" || models[1].DeclaredMultimodal || models[1].Loaded || models[1].ContextWindow != 8192 {
		t.Errorf("models[1] = %+v, want llm/not-loaded/8192", models[1])
	}
}

// TestLMStudioClient_FetchModels_ImplementsFetcher exercises the Fetcher
// interface method the disk cache (W-C-06) depends on.
func TestLMStudioClient_FetchModels_ImplementsFetcher(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(lmStudioModelsResponse{Data: []lmStudioModelEntry{{ID: "phi-4"}}})
	}))
	defer srv.Close()

	var f Fetcher = NewLMStudioClient(srv.URL, "", nil)
	if f.Name() != "lmstudio" {
		t.Fatalf("Name() = %q, want lmstudio", f.Name())
	}
	models, etag, err := f.FetchModels(context.Background())
	if err != nil {
		t.Fatalf("FetchModels err = %v", err)
	}
	if len(models) != 1 || models[0].ID != "phi-4" {
		t.Fatalf("models = %+v, want one entry phi-4", models)
	}
	if etag != "" {
		t.Fatalf("etag = %q, want empty (LM Studio sends none)", etag)
	}
}

// TestLMStudioClient_AuthorizationHeader proves apiKey is sent as a Bearer
// token when configured, and proves it is NOT sent at all when empty (LM
// Studio's own curl examples only add the header when a token exists).
func TestLMStudioClient_AuthorizationHeader(t *testing.T) {
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		json.NewEncoder(w).Encode(lmStudioModelsResponse{})
	}))
	defer srv.Close()

	c := NewLMStudioClient(srv.URL, "sk-test-token", nil)
	if _, err := c.ListModels(context.Background()); err != nil {
		t.Fatalf("ListModels err = %v", err)
	}
	if gotAuth != "Bearer sk-test-token" {
		t.Fatalf("Authorization = %q, want Bearer sk-test-token", gotAuth)
	}

	c2 := NewLMStudioClient(srv.URL, "", nil)
	gotAuth = "unset"
	if _, err := c2.ListModels(context.Background()); err != nil {
		t.Fatalf("ListModels err = %v", err)
	}
	if gotAuth != "" {
		t.Fatalf("Authorization = %q, want empty when apiKey is empty", gotAuth)
	}
}

// TestLMStudioClient_LoadModel_Warmup proves the "load_model 预热"
// acceptance bullet: LoadModel POSTs to /api/v1/models/load with the model
// name and any LoadOptions, and parses the response including the optional
// LoadConfig echo.
func TestLMStudioClient_LoadModel_Warmup(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/models/load" {
			t.Errorf("path = %q, want /api/v1/models/load", r.URL.Path)
		}
		if r.Method != http.MethodPost {
			t.Errorf("method = %q, want POST", r.Method)
		}
		var body struct {
			Model               string `json:"model"`
			ContextLength       int    `json:"context_length"`
			FlashAttention      bool   `json:"flash_attention"`
			EvalBatchSize       int    `json:"eval_batch_size"`
			NumExperts          int    `json:"num_experts"`
			OffloadKVCacheToGPU bool   `json:"offload_kv_cache_to_gpu"`
			EchoLoadConfig      bool   `json:"echo_load_config"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode request body: %v", err)
		}
		if body.Model != "qwen2-vl-7b" {
			t.Errorf("request model = %q, want qwen2-vl-7b", body.Model)
		}
		if body.ContextLength != 16384 {
			t.Errorf("request context_length = %d, want 16384", body.ContextLength)
		}
		if !body.FlashAttention {
			t.Error("request flash_attention = false, want true")
		}
		json.NewEncoder(w).Encode(LoadResult{
			Type:            "llm",
			InstanceID:      "qwen2-vl-7b:2",
			LoadTimeSeconds: 3.14,
			Status:          "loaded",
			LoadConfig:      json.RawMessage(`{"context_length":16384}`),
		})
	}))
	defer srv.Close()

	c := NewLMStudioClient(srv.URL, "", nil)
	result, err := c.LoadModel(context.Background(), "qwen2-vl-7b", LoadOptions{
		ContextLength:  16384,
		FlashAttention: true,
	})
	if err != nil {
		t.Fatalf("LoadModel err = %v", err)
	}
	if result.Status != "loaded" || result.InstanceID != "qwen2-vl-7b:2" {
		t.Fatalf("result = %+v, want status=loaded instance_id=qwen2-vl-7b:2", result)
	}
	if len(result.LoadConfig) == 0 {
		t.Fatal("result.LoadConfig is empty, want the echoed config")
	}
}

// TestLMStudioClient_LoadModel_HTTPError proves a non-200 load response
// (e.g. model not found) surfaces as a Go error rather than a zero-value
// success.
func TestLMStudioClient_LoadModel_HTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "model not found", http.StatusNotFound)
	}))
	defer srv.Close()

	c := NewLMStudioClient(srv.URL, "", nil)
	_, err := c.LoadModel(context.Background(), "ghost-model", LoadOptions{})
	if err == nil {
		t.Fatal("LoadModel err = nil, want an error for a 404 response")
	}
}

// TestNewLMStudioClient_DefaultsBaseURL proves an empty baseURL falls back
// to DefaultLMStudioBaseURL rather than producing a broken client.
func TestNewLMStudioClient_DefaultsBaseURL(t *testing.T) {
	c := NewLMStudioClient("", "", nil)
	if c.baseURL != DefaultLMStudioBaseURL {
		t.Fatalf("baseURL = %q, want %q", c.baseURL, DefaultLMStudioBaseURL)
	}
}
