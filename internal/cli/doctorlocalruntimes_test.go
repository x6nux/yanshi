package cli

// C3 review's W-C-08 ruling requires an assertion against a genuinely
// assembled system, not a source-text/string-count check on
// doctorlocalruntimes.go itself. Every test below stands up a real
// httptest.Server simulating Ollama's/LM Studio's actual wire protocol (per
// this repo's hard constraint: never start a real host service) and a real
// eino.Cache rooted at a t.TempDir() — never a fake/mock of either. The
// consumption-proof test additionally opens a SECOND, independent
// eino.Cache pointed at the same directory to read back what the first call
// persisted, so the assertion exercises the actual on-disk hand-off between
// a probe and a later reader, not an in-memory field on the struct that
// just wrote it.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/x6nux/yanshi/internal/llm/eino"
)

// newFakeLMStudioServer simulates LM Studio's GET /api/v0/models (one
// model, id=modelID, State "loaded"/"not-loaded" per loaded) and POST
// /v1/chat/completions (always answers "RED", so ProbeImageSupport reads
// back Supported=true). chatCalls counts every chat-completions request
// observed — the mechanism every test below uses to prove whether
// reportLMStudio did or did not send a probe.
func newFakeLMStudioServer(t *testing.T, modelID string, loaded bool, chatCalls *int32) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v0/models", func(w http.ResponseWriter, r *http.Request) {
		state := "not-loaded"
		if loaded {
			state = "loaded"
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"data": []map[string]any{
				{"id": modelID, "type": "llm", "state": state, "max_context_length": 4096},
			},
		})
	})
	mux.HandleFunc("/v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(chatCalls, 1)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{
				{"message": map[string]any{"content": "RED"}},
			},
		})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// TestReportLMStudio_ProbesAndPersistsImageSupportForLoadedModel is the
// W-C-08 property-(2) assertion: a genuinely assembled reportLMStudio call,
// against a real (fake-protocol) HTTP server and a real on-disk cache, must
// leave a probed verdict that a completely independent later reader can
// observe. If PutImageSupport were never reached — the exact "zero
// production consumers" defect the review named — cache2.GetImageSupport
// below would return found=false, not a string that merely LOOKS right in
// reportLMStudio's return value.
func TestReportLMStudio_ProbesAndPersistsImageSupportForLoadedModel(t *testing.T) {
	var chatCalls int32
	srv := newFakeLMStudioServer(t, "vision-model", true, &chatCalls)

	dir := t.TempDir()
	cache, err := eino.NewCache(dir, 0)
	if err != nil {
		t.Fatalf("NewCache: %v", err)
	}
	client := eino.NewLMStudioClient(srv.URL, "", nil)

	msg := reportLMStudio(context.Background(), cache, client)
	if chatCalls != 1 {
		t.Fatalf("chat-completions calls = %d, want 1 (exactly one loaded model, probed exactly once)", chatCalls)
	}
	if !strings.Contains(msg, "vision-model") || !strings.Contains(msg, "image-support probed for 1 loaded model(s)") {
		t.Fatalf("message = %q, want it to mention the model and the probe count", msg)
	}

	// The load-bearing assertion: a SECOND, independent Cache instance
	// pointed at the same directory, standing in for a later `yanshi
	// doctor` run or a future model-picker read.
	cache2, err := eino.NewCache(dir, 0)
	if err != nil {
		t.Fatalf("NewCache (second instance): %v", err)
	}
	verdict, found, err := cache2.GetImageSupport("lmstudio", "vision-model")
	if err != nil {
		t.Fatalf("GetImageSupport: %v", err)
	}
	if !found {
		t.Fatal("image-support verdict was not persisted to disk — PutImageSupport was never reached")
	}
	if !verdict.Supported {
		t.Errorf("verdict.Supported = false, want true (the fake server answers RED)")
	}
	// A disk-loaded verdict is unconditionally downgraded from SourceProbed
	// to SourceDocumented by sanitizeLoadedListing (M-2 review fix,
	// discovery_cache.go): a file cannot vouch for a probe some OTHER
	// process's memory performed, no matter how genuine that probe was.
	if verdict.Source != eino.SourceDocumented {
		t.Errorf("verdict.Source = %q, want %q (M-2's disk round-trip downgrade)", verdict.Source, eino.SourceDocumented)
	}
}

// TestReportLMStudio_NeverProbesANotLoadedModel is the W-C-08 property-(3)
// assertion: a model LM Studio itself reports as not loaded must never
// receive a chat-completions request, because that request is exactly what
// would cold-load it — the side effect checkMCP's doc comment forbids for a
// diagnostic command and doctorlocalruntimes.go's package comment commits
// to avoiding for this one.
func TestReportLMStudio_NeverProbesANotLoadedModel(t *testing.T) {
	var chatCalls int32
	srv := newFakeLMStudioServer(t, "cold-model", false, &chatCalls)

	cache, err := eino.NewCache(t.TempDir(), 0)
	if err != nil {
		t.Fatalf("NewCache: %v", err)
	}
	client := eino.NewLMStudioClient(srv.URL, "", nil)

	msg := reportLMStudio(context.Background(), cache, client)
	if chatCalls != 0 {
		t.Fatalf("chat-completions calls = %d, want 0 — a not-loaded model must never be probed", chatCalls)
	}
	if strings.Contains(msg, "image-support probed") {
		t.Errorf("message = %q, must not claim a probe happened", msg)
	}
}

// TestReportLMStudio_SkipsAModelWithAnExistingVerdict proves the "don't
// re-probe on every doctor invocation" half of reportLMStudio's caching
// design: once a verdict is on disk (regardless of what Source it reads
// back as — see reportLMStudio's doc comment on why "== SourceProbed" would
// be the wrong gate), a second call must not send a second probe.
func TestReportLMStudio_SkipsAModelWithAnExistingVerdict(t *testing.T) {
	var chatCalls int32
	srv := newFakeLMStudioServer(t, "known-model", true, &chatCalls)

	dir := t.TempDir()
	cache, err := eino.NewCache(dir, 0)
	if err != nil {
		t.Fatalf("NewCache: %v", err)
	}
	client := eino.NewLMStudioClient(srv.URL, "", nil)

	// First call: no cached verdict yet, must probe once.
	reportLMStudio(context.Background(), cache, client)
	if chatCalls != 1 {
		t.Fatalf("after first call: chat-completions calls = %d, want 1", chatCalls)
	}

	// Second call against the SAME cache directory: the verdict the first
	// call persisted must be found and must suppress a second probe.
	cache2, err := eino.NewCache(dir, 0)
	if err != nil {
		t.Fatalf("NewCache (second instance): %v", err)
	}
	reportLMStudio(context.Background(), cache2, client)
	if chatCalls != 1 {
		t.Fatalf("after second call: chat-completions calls = %d, want still 1 — a model with an existing verdict must not be re-probed", chatCalls)
	}
}

// TestReportLMStudio_RetriesAModelWhoseProbePreviouslyFailed proves the
// SourceProbeFailed exemption: M-1's whole point is that a probe failure
// means "unknown, retry later" rather than a negative verdict, and
// reportLMStudio's cache is the first production code that actually acts on
// that distinction — a plain "skip anything already on disk" gate would
// permanently strand a transient failure exactly the way M-1 warned about.
func TestReportLMStudio_RetriesAModelWhoseProbePreviouslyFailed(t *testing.T) {
	var chatCalls int32
	srv := newFakeLMStudioServer(t, "flaky-model", true, &chatCalls)

	dir := t.TempDir()
	cache, err := eino.NewCache(dir, 0)
	if err != nil {
		t.Fatalf("NewCache: %v", err)
	}
	if err := cache.PutImageSupport("lmstudio", "flaky-model", eino.ImageSupport{
		Source: eino.SourceProbeFailed, Detail: "seeded: a previous run's execution failure",
	}); err != nil {
		t.Fatalf("seed PutImageSupport: %v", err)
	}

	client := eino.NewLMStudioClient(srv.URL, "", nil)
	reportLMStudio(context.Background(), cache, client)
	if chatCalls != 1 {
		t.Fatalf("chat-completions calls = %d, want 1 — a SourceProbeFailed verdict must be retried, not treated as final", chatCalls)
	}
}

// newFakeOllamaServer simulates Ollama's native GET /api/tags (one model
// named modelID) and — deliberately — nothing else: it has no
// /v1/chat/completions handler at all, so if reportOllama ever tried to
// probe image support, the request would 404 against http.NotFoundHandler
// and be visibly wrong, rather than silently succeeding against a handler
// that happens to also exist.
func newFakeOllamaServer(t *testing.T, modelID string) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/api/tags", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"models": []map[string]any{{"name": modelID}},
		})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// TestReportOllama_NeverProbesImageSupport documents and enforces the other
// half of "never triggers a cold model load": Ollama's /api/tags reports no
// loaded/not-loaded state at all (DiscoveredModel.Loaded is always false
// for Ollama results — see its own doc comment), so reportOllama has no
// safe signal to gate a probe on and must never attempt one, for ANY
// listed model. The fake server has no chat-completions route at all, so a
// stray attempt would surface as a hard failure below rather than a benign
// 200.
func TestReportOllama_NeverProbesImageSupport(t *testing.T) {
	srv := newFakeOllamaServer(t, "llama3:latest")

	cache, err := eino.NewCache(t.TempDir(), 0)
	if err != nil {
		t.Fatalf("NewCache: %v", err)
	}
	client := eino.NewOllamaClient(srv.URL, nil)

	msg := reportOllama(context.Background(), cache, client)
	if !strings.Contains(msg, "llama3:latest") {
		t.Fatalf("message = %q, want it to mention the discovered model", msg)
	}
	if strings.Contains(msg, "probed") {
		t.Errorf("message = %q, reportOllama must never claim an image-support probe", msg)
	}
}

// TestReportOllama_UnreachableIsReportedNotFatal proves the graceful-
// degradation property (W-C-08's property (3)): pointing the client at a
// closed port must produce an informational line, never a panic or an
// error return reportOllama has no way to surface (it returns a plain
// string precisely so checkLocalRuntimes can never fail the whole doctor
// run over an absent local runtime).
func TestReportOllama_UnreachableIsReportedNotFatal(t *testing.T) {
	srv := newFakeOllamaServer(t, "unused")
	unreachableURL := srv.URL
	srv.Close() // close immediately: the URL now refuses connections.

	cache, err := eino.NewCache(t.TempDir(), 0)
	if err != nil {
		t.Fatalf("NewCache: %v", err)
	}
	client := eino.NewOllamaClient(unreachableURL, nil)

	msg := reportOllama(context.Background(), cache, client)
	if !strings.Contains(msg, "ollama:") || !strings.Contains(msg, "unavailable") {
		t.Errorf("message = %q, want an informational ollama:-prefixed unavailable line", msg)
	}
}

// TestRunDoctor_IncludesLocalRuntimesCheck is the wiring-proof half of
// W-C-08's property (1): a real `RunDoctor` call — the same aggregate
// `yanshi doctor` itself calls — must include the "local-runtimes" check,
// and it must never fail or warn on its own account just because this test
// environment (like the overwhelming majority of CI/dev machines) has no
// Ollama or LM Studio actually running.
func TestRunDoctor_IncludesLocalRuntimesCheck(t *testing.T) {
	cfgBody := fmt.Sprintf(`
server: { http_addr: "127.0.0.1:0" }
storage: { sqlite_path: %q }
`, filepath.Join(t.TempDir(), "yanshi.db"))
	rep := RunDoctor(context.Background(), DoctorOptions{ConfigPath: writeTempConfig(t, cfgBody), Root: t.TempDir()})
	c := findCheck(t, rep, "local-runtimes")
	if c.Status != StatusOK {
		t.Errorf("local-runtimes: got %s (%s), want ok (no runtime installed is the common case)", c.Status, c.Message)
	}
	if !strings.Contains(c.Message, "ollama:") || !strings.Contains(c.Message, "lmstudio:") {
		t.Errorf("local-runtimes message = %q, want both runtimes reported", c.Message)
	}
}
