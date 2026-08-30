package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/x6nux/yanshi/internal/llm/eino"
)

// TestRunModelsPull_RequiresModel proves the flag-parsing-adjacent guard: an
// empty Model is a usage error, not a nil-pointer panic or a silent no-op
// HTTP call to a client built from a blank string.
func TestRunModelsPull_RequiresModel(t *testing.T) {
	if err := RunModelsPull(context.Background(), ModelsPullOptions{}); err == nil {
		t.Fatal("RunModelsPull with empty Model: err = nil, want a usage error")
	}
}

// TestRunModelsPreheat_RequiresModel is TestRunModelsPull_RequiresModel's
// LM Studio counterpart.
func TestRunModelsPreheat_RequiresModel(t *testing.T) {
	if _, err := RunModelsPreheat(context.Background(), ModelsPreheatOptions{}); err == nil {
		t.Fatal("RunModelsPreheat with empty Model: err = nil, want a usage error")
	}
}

// TestRunModelsPullWith_StreamsProgressAndForceRefreshes is the review-
// whole.md M-1 wiring proof for both OllamaClient.PullModel and
// eino.RefreshForce in one call: it drives POST /api/pull through a fake
// Ollama daemon exactly the way OllamaClient's own unit tests do, then
// checks two things neither of those tests can: (1) the streamed progress
// lines reach the caller's io.Writer, and (2) the on-disk cache's listing
// gets re-fetched afterward even though it was seeded moments earlier and
// is nowhere near its TTL — the only way that second fetch happens is if
// runModelsPullWith actually called Cache.Get with RefreshForce, not
// RefreshAuto (which would have served the fresh seed and skipped the
// network entirely).
func TestRunModelsPullWith_StreamsProgressAndForceRefreshes(t *testing.T) {
	var tagsHits int32
	mux := http.NewServeMux()
	mux.HandleFunc("/api/tags", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&tagsHits, 1)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"models": []map[string]any{{"name": "llama3.1:8b"}},
		})
	})
	mux.HandleFunc("/api/pull", func(w http.ResponseWriter, r *http.Request) {
		flusher := w.(http.Flusher)
		enc := json.NewEncoder(w)
		_ = enc.Encode(eino.PullProgress{Status: "pulling manifest"})
		flusher.Flush()
		_ = enc.Encode(eino.PullProgress{Status: "downloading", Total: 100, Completed: 100})
		flusher.Flush()
		_ = enc.Encode(eino.PullProgress{Status: "success"})
		flusher.Flush()
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	cache, err := eino.NewCache(t.TempDir(), 0)
	if err != nil {
		t.Fatalf("NewCache: %v", err)
	}
	client := eino.NewOllamaClient(srv.URL, nil)

	// Seed a fresh cache entry — well within DefaultCacheTTL — so a
	// subsequent RefreshAuto call would serve it from disk with zero
	// additional /api/tags requests. If runModelsPullWith's post-pull
	// refresh used RefreshAuto instead of RefreshForce, tagsHits would stay
	// at 1 (the seed) instead of reaching 2.
	if _, err := cache.Get(context.Background(), client, eino.RefreshAuto); err != nil {
		t.Fatalf("seed cache.Get: %v", err)
	}
	if got := atomic.LoadInt32(&tagsHits); got != 1 {
		t.Fatalf("tagsHits after seed = %d, want 1", got)
	}

	var progress bytes.Buffer
	if err := runModelsPullWith(context.Background(), cache, client, "llama3.1:8b", &progress); err != nil {
		t.Fatalf("runModelsPullWith: %v", err)
	}

	if !strings.Contains(progress.String(), "pulling manifest") || !strings.Contains(progress.String(), "success") {
		t.Fatalf("progress = %q, want it to contain both pull status lines", progress.String())
	}
	if !strings.Contains(progress.String(), "100/100") {
		t.Fatalf("progress = %q, want the completed/total counter", progress.String())
	}
	if got := atomic.LoadInt32(&tagsHits); got != 2 {
		t.Fatalf("tagsHits after pull = %d, want 2 (RefreshForce must re-fetch even within TTL)", got)
	}
}

// TestRunModelsPullWith_PropagatesPullError proves a failed pull (bad tag,
// daemon error) surfaces as a Go error and never reaches the
// RefreshForce step — refreshing the cache after a pull that never
// happened would misreport the pull as having changed something.
func TestRunModelsPullWith_PropagatesPullError(t *testing.T) {
	var tagsHits int32
	mux := http.NewServeMux()
	mux.HandleFunc("/api/tags", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&tagsHits, 1)
		_ = json.NewEncoder(w).Encode(map[string]any{"models": []map[string]any{}})
	})
	mux.HandleFunc("/api/pull", func(w http.ResponseWriter, r *http.Request) {
		flusher := w.(http.Flusher)
		enc := json.NewEncoder(w)
		_ = enc.Encode(eino.PullProgress{Error: "manifest not found"})
		flusher.Flush()
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	cache, err := eino.NewCache(t.TempDir(), 0)
	if err != nil {
		t.Fatalf("NewCache: %v", err)
	}
	client := eino.NewOllamaClient(srv.URL, nil)

	err = runModelsPullWith(context.Background(), cache, client, "nonexistent:tag", nil)
	if err == nil {
		t.Fatal("runModelsPullWith err = nil, want the inline pull error surfaced")
	}
	if got := atomic.LoadInt32(&tagsHits); got != 0 {
		t.Fatalf("tagsHits = %d, want 0 (a failed pull must not trigger a cache refresh)", got)
	}
}

// TestRunModelsPreheatWith_LoadsAndForceRefreshes is
// TestRunModelsPullWith_StreamsProgressAndForceRefreshes' LM Studio
// counterpart: proves LoadModel's result reaches the caller and that the
// post-load refresh actually re-fetched (RefreshForce), not served a
// just-seeded, still-fresh cache entry (RefreshAuto).
func TestRunModelsPreheatWith_LoadsAndForceRefreshes(t *testing.T) {
	var modelsHits int32
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v0/models", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&modelsHits, 1)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"data": []map[string]any{{"id": "qwen2-vl-7b", "state": "loaded", "type": "vlm"}},
		})
	})
	mux.HandleFunc("/api/v1/models/load", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(eino.LoadResult{
			Type: "llm", InstanceID: "qwen2-vl-7b:1", Status: "loaded",
		})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	cache, err := eino.NewCache(t.TempDir(), 0)
	if err != nil {
		t.Fatalf("NewCache: %v", err)
	}
	client := eino.NewLMStudioClient(srv.URL, "", nil)

	if _, err := cache.Get(context.Background(), client, eino.RefreshAuto); err != nil {
		t.Fatalf("seed cache.Get: %v", err)
	}
	if got := atomic.LoadInt32(&modelsHits); got != 1 {
		t.Fatalf("modelsHits after seed = %d, want 1", got)
	}

	result, err := runModelsPreheatWith(context.Background(), cache, client, "qwen2-vl-7b", eino.LoadOptions{})
	if err != nil {
		t.Fatalf("runModelsPreheatWith: %v", err)
	}
	if result.Status != "loaded" || result.InstanceID != "qwen2-vl-7b:1" {
		t.Fatalf("result = %+v, want status=loaded instance_id=qwen2-vl-7b:1", result)
	}
	if got := atomic.LoadInt32(&modelsHits); got != 2 {
		t.Fatalf("modelsHits after preheat = %d, want 2 (RefreshForce must re-fetch even within TTL)", got)
	}
}

// TestRunModelsPreheatWith_PropagatesLoadError mirrors
// TestRunModelsPullWith_PropagatesPullError for the LM Studio path.
func TestRunModelsPreheatWith_PropagatesLoadError(t *testing.T) {
	var modelsHits int32
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v0/models", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&modelsHits, 1)
		_ = json.NewEncoder(w).Encode(map[string]any{"data": []map[string]any{}})
	})
	mux.HandleFunc("/api/v1/models/load", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte("model not found"))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	cache, err := eino.NewCache(t.TempDir(), 0)
	if err != nil {
		t.Fatalf("NewCache: %v", err)
	}
	client := eino.NewLMStudioClient(srv.URL, "", nil)

	_, err = runModelsPreheatWith(context.Background(), cache, client, "ghost-model", eino.LoadOptions{})
	if err == nil {
		t.Fatal("runModelsPreheatWith err = nil, want the HTTP 404 surfaced")
	}
	if got := atomic.LoadInt32(&modelsHits); got != 0 {
		t.Fatalf("modelsHits = %d, want 0 (a failed load must not trigger a cache refresh)", got)
	}
}
