package eino

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestOllamaClient_Probe_BothEndpointsUp is the happy path: both signals
// answer, Available is true and both Endpoints entries are true.
func TestOllamaClient_Probe_BothEndpointsUp(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/":
			w.Write([]byte("Ollama is running"))
		case "/api/tags":
			json.NewEncoder(w).Encode(ollamaTagsResponse{})
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	c := NewOllamaClient(srv.URL, nil)
	got := c.Probe(context.Background())
	if !got.Available {
		t.Fatalf("Available = false, want true; detail=%q endpoints=%v", got.Detail, got.Endpoints)
	}
	if !got.Endpoints["root"] || !got.Endpoints["api_tags"] {
		t.Fatalf("Endpoints = %v, want both true", got.Endpoints)
	}
}

// TestOllamaClient_Probe_DaemonDown proves the "report truthfully when
// unavailable" acceptance bullet at the Probe layer: pointing at a closed
// port (no listener at all) must report Available=false with both signals
// false, not panic or hang.
func TestOllamaClient_Probe_DaemonDown(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	closedURL := srv.URL
	srv.Close() // now nothing listens at closedURL

	c := NewOllamaClient(closedURL, nil)
	got := c.Probe(context.Background())
	if got.Available {
		t.Fatalf("Available = true against a closed port, want false")
	}
	if got.Endpoints["root"] || got.Endpoints["api_tags"] {
		t.Fatalf("Endpoints = %v, want both false against a closed port", got.Endpoints)
	}
	if got.Detail == "" {
		t.Fatal("Detail is empty, want a truthful reason the daemon is unreachable")
	}
}

// TestOllamaClient_Probe_RootUpTagsBroken proves the dual-endpoint design's
// whole point: a daemon that answers the root ping but errors on /api/tags
// must be reported differently (Endpoints["root"]=true) from a fully-down
// daemon (both false, see TestOllamaClient_Probe_DaemonDown) — collapsing
// the two into the same Available=false result would lose the "the process
// is up, something else is wrong" signal the dual probe exists to capture.
func TestOllamaClient_Probe_RootUpTagsBroken(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/":
			w.Write([]byte("Ollama is running"))
		case "/api/tags":
			http.Error(w, "internal error", http.StatusInternalServerError)
		}
	}))
	defer srv.Close()

	c := NewOllamaClient(srv.URL, nil)
	got := c.Probe(context.Background())
	if got.Available {
		t.Fatalf("Available = true, want false when /api/tags is broken")
	}
	if !got.Endpoints["root"] {
		t.Fatal("Endpoints[\"root\"] = false, want true — the root ping succeeded")
	}
	if got.Endpoints["api_tags"] {
		t.Fatal("Endpoints[\"api_tags\"] = true, want false — /api/tags returned a 500")
	}
}

// TestOllamaClient_ListModels_ReachableButEmpty is THIS batch's headline
// assertion: a daemon that answers successfully with zero pulled models
// must return (nil-or-empty slice, nil error) — never an error. Conflating
// "no listing endpoint" with "listing endpoint returned zero models" is
// exactly discover.go's FetchModelCatalog bug (see that function's
// len(out)==0 error path) that this method deliberately does not repeat.
func TestOllamaClient_ListModels_ReachableButEmpty(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(ollamaTagsResponse{Models: []ollamaTagEntry{}})
	}))
	defer srv.Close()

	c := NewOllamaClient(srv.URL, nil)
	models, err := c.ListModels(context.Background())
	if err != nil {
		t.Fatalf("ListModels err = %v, want nil — the daemon answered successfully with an empty list", err)
	}
	if len(models) != 0 {
		t.Fatalf("models = %v, want empty", models)
	}
}

// TestOllamaClient_ListModels_Unreachable proves the other half of the same
// contract: when the daemon cannot be reached at all, ListModels must
// return a non-nil error, distinguishable in the RETURN VALUE (not just a
// log line) from the reachable-but-empty case above.
func TestOllamaClient_ListModels_Unreachable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	closedURL := srv.URL
	srv.Close()

	c := NewOllamaClient(closedURL, nil)
	models, err := c.ListModels(context.Background())
	if err == nil {
		t.Fatalf("ListModels err = nil, models = %v, want a non-nil error against a closed port", models)
	}
}

// TestOllamaClient_ListModels_ParsesEntries checks name normalization
// (falling back to "model" when "name" is absent, skipping id-less rows).
func TestOllamaClient_ListModels_ParsesEntries(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/tags" {
			t.Errorf("path = %q, want /api/tags", r.URL.Path)
		}
		json.NewEncoder(w).Encode(map[string]any{
			"models": []map[string]any{
				{"name": "llama3:latest", "model": "llama3:latest"},
				{"model": "mistral:7b"}, // no "name" — falls back to "model"
				{"name": ""},            // no usable id at all — skipped
			},
		})
	}))
	defer srv.Close()

	c := NewOllamaClient(srv.URL, nil)
	models, err := c.ListModels(context.Background())
	if err != nil {
		t.Fatalf("ListModels err = %v", err)
	}
	want := []string{"llama3:latest", "mistral:7b"}
	if len(models) != len(want) {
		t.Fatalf("models = %+v, want %d entries", models, len(want))
	}
	for i, w := range want {
		if models[i].ID != w {
			t.Errorf("models[%d].ID = %q, want %q", i, models[i].ID, w)
		}
	}
}

// TestOllamaClient_FetchModels_ImplementsFetcher exercises the Fetcher
// interface method the disk cache (W-C-06) depends on, confirming it wraps
// ListModels and always reports an empty etag (Ollama's native API sends
// none — see the FetchModels doc comment on the Fetcher interface).
func TestOllamaClient_FetchModels_ImplementsFetcher(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(ollamaTagsResponse{Models: []ollamaTagEntry{{Name: "phi3"}}})
	}))
	defer srv.Close()

	var f Fetcher = NewOllamaClient(srv.URL, nil)
	if f.Name() != "ollama" {
		t.Fatalf("Name() = %q, want ollama", f.Name())
	}
	models, etag, err := f.FetchModels(context.Background())
	if err != nil {
		t.Fatalf("FetchModels err = %v", err)
	}
	if len(models) != 1 || models[0].ID != "phi3" {
		t.Fatalf("models = %+v, want one entry phi3", models)
	}
	if etag != "" {
		t.Fatalf("etag = %q, want empty (Ollama sends none)", etag)
	}
}

// TestOllamaClient_PullModel_StreamsProgress proves the "NDJSON streaming
// pull with progress" acceptance bullet: each line the fake server writes is
// delivered to onProgress as it arrives (accumulated, not just the final
// line), and a "success" status line ends the pull with a nil error.
func TestOllamaClient_PullModel_StreamsProgress(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/pull" {
			t.Errorf("path = %q, want /api/pull", r.URL.Path)
		}
		var body map[string]string
		json.NewDecoder(r.Body).Decode(&body)
		if body["model"] != "llama3" {
			t.Errorf("request model = %q, want llama3", body["model"])
		}
		flusher, ok := w.(http.Flusher)
		if !ok {
			t.Fatal("ResponseWriter does not support flushing — cannot simulate streaming")
		}
		enc := json.NewEncoder(w)
		enc.Encode(PullProgress{Status: "pulling manifest"})
		flusher.Flush()
		enc.Encode(PullProgress{Status: "downloading", Digest: "sha256:abc", Total: 100, Completed: 40})
		flusher.Flush()
		enc.Encode(PullProgress{Status: "downloading", Digest: "sha256:abc", Total: 100, Completed: 100})
		flusher.Flush()
		enc.Encode(PullProgress{Status: "success"})
		flusher.Flush()
	}))
	defer srv.Close()

	c := NewOllamaClient(srv.URL, nil)
	var seen []PullProgress
	err := c.PullModel(context.Background(), "llama3", func(p PullProgress) { seen = append(seen, p) })
	if err != nil {
		t.Fatalf("PullModel err = %v", err)
	}
	if len(seen) != 4 {
		t.Fatalf("saw %d progress lines, want 4: %+v", len(seen), seen)
	}
	if seen[1].Completed != 40 || seen[2].Completed != 100 {
		t.Fatalf("progress not delivered incrementally: %+v", seen)
	}
	if seen[3].Status != "success" {
		t.Fatalf("last line status = %q, want success", seen[3].Status)
	}
}

// TestOllamaClient_PullModel_InlineError proves an inline {"error":"..."}
// object mid-stream (Ollama's documented way of failing a pull that has
// already started, e.g. a bad tag) surfaces as a Go error rather than being
// silently swallowed or reported as onProgress being uninterested.
func TestOllamaClient_PullModel_InlineError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		flusher := w.(http.Flusher)
		enc := json.NewEncoder(w)
		enc.Encode(PullProgress{Status: "pulling manifest"})
		flusher.Flush()
		enc.Encode(PullProgress{Error: "pull model manifest: file does not exist"})
		flusher.Flush()
	}))
	defer srv.Close()

	c := NewOllamaClient(srv.URL, nil)
	err := c.PullModel(context.Background(), "nonexistent:tag", nil)
	if err == nil {
		t.Fatal("PullModel err = nil, want an error from the inline {\"error\":...} line")
	}
	if !strings.Contains(err.Error(), "does not exist") {
		t.Fatalf("err = %v, want it to carry the daemon's message", err)
	}
}

// TestOllamaClient_PullModel_HTTPError proves a non-200 from /api/pull
// itself (before any streaming begins) is reported, not silently treated as
// an empty successful pull.
func TestOllamaClient_PullModel_HTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "model not found", http.StatusNotFound)
	}))
	defer srv.Close()

	c := NewOllamaClient(srv.URL, nil)
	err := c.PullModel(context.Background(), "ghost:latest", nil)
	if err == nil {
		t.Fatal("PullModel err = nil, want an error for a 404 response")
	}
}

// TestNewOllamaClient_StripsV1Suffix proves NewOllamaClient tolerates an
// operator pasting the /v1 base_url already configured in ProviderConfig
// for the OpenAI-compatible surface (see config.example.yaml) instead of
// the bare root this client's native endpoints need.
func TestNewOllamaClient_StripsV1Suffix(t *testing.T) {
	c := NewOllamaClient("http://127.0.0.1:11434/v1", nil)
	if c.baseURL != "http://127.0.0.1:11434" {
		t.Fatalf("baseURL = %q, want the /v1 suffix stripped", c.baseURL)
	}
}
