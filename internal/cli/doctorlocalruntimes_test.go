package cli

// C3 review's I-1 ruling requires an assertion against a genuinely
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
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/x6nux/yanshi/internal/llm/eino"
)

// newFakeLMStudioServer simulates LM Studio's GET /api/v0/models (one
// model, id=modelID, State "loaded"/"not-loaded" per loaded, type "vlm" or
// "llm" per declaredMultimodal — the metadata DeclaredMultimodal/
// eino.DocumentedImageSupport's review-whole.md M-1 wiring reads) and POST
// /v1/chat/completions (always answers "RED", so ProbeImageSupport reads
// back Supported=true). chatCalls counts every chat-completions request
// observed — the mechanism every test below uses to prove whether
// reportLMStudio did or did not send a probe.
func newFakeLMStudioServer(t *testing.T, modelID string, loaded, declaredMultimodal bool, chatCalls *int32) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v0/models", func(w http.ResponseWriter, r *http.Request) {
		state := "not-loaded"
		if loaded {
			state = "loaded"
		}
		modelType := "llm"
		if declaredMultimodal {
			modelType = "vlm"
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"data": []map[string]any{
				{"id": modelID, "type": modelType, "state": state, "max_context_length": 4096},
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
// I-1 property-(2) assertion: a genuinely assembled reportLMStudio call,
// against a real (fake-protocol) HTTP server and a real on-disk cache, must
// leave a probed verdict that a completely independent later reader can
// observe. If PutImageSupport were never reached — the exact "zero
// production consumers" defect the review named — cache2.GetImageSupport
// below would return found=false, not a string that merely LOOKS right in
// reportLMStudio's return value.
func TestReportLMStudio_ProbesAndPersistsImageSupportForLoadedModel(t *testing.T) {
	var chatCalls int32
	srv := newFakeLMStudioServer(t, "vision-model", true, false, &chatCalls)

	dir := t.TempDir()
	cache, err := eino.NewCache(dir, 0)
	if err != nil {
		t.Fatalf("NewCache: %v", err)
	}
	client := eino.NewLMStudioClient(srv.URL, "", nil)

	msg := reportLMStudio(context.Background(), cache, client, eino.RefreshAuto)
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

// TestReportLMStudio_NeverProbesANotLoadedModel is the I-1 property-(3)
// assertion: a model LM Studio itself reports as not loaded must never
// receive a chat-completions request, because that request is exactly what
// would cold-load it — the side effect checkMCP's doc comment forbids for a
// diagnostic command and doctorlocalruntimes.go's package comment commits
// to avoiding for this one.
func TestReportLMStudio_NeverProbesANotLoadedModel(t *testing.T) {
	var chatCalls int32
	srv := newFakeLMStudioServer(t, "cold-model", false, false, &chatCalls)

	cache, err := eino.NewCache(t.TempDir(), 0)
	if err != nil {
		t.Fatalf("NewCache: %v", err)
	}
	client := eino.NewLMStudioClient(srv.URL, "", nil)

	msg := reportLMStudio(context.Background(), cache, client, eino.RefreshAuto)
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
	srv := newFakeLMStudioServer(t, "known-model", true, false, &chatCalls)

	dir := t.TempDir()
	cache, err := eino.NewCache(dir, 0)
	if err != nil {
		t.Fatalf("NewCache: %v", err)
	}
	client := eino.NewLMStudioClient(srv.URL, "", nil)

	// First call: no cached verdict yet, must probe once.
	reportLMStudio(context.Background(), cache, client, eino.RefreshAuto)
	if chatCalls != 1 {
		t.Fatalf("after first call: chat-completions calls = %d, want 1", chatCalls)
	}

	// Second call against the SAME cache directory: the verdict the first
	// call persisted must be found and must suppress a second probe.
	cache2, err := eino.NewCache(dir, 0)
	if err != nil {
		t.Fatalf("NewCache (second instance): %v", err)
	}
	reportLMStudio(context.Background(), cache2, client, eino.RefreshAuto)
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
	srv := newFakeLMStudioServer(t, "flaky-model", true, false, &chatCalls)

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
	reportLMStudio(context.Background(), cache, client, eino.RefreshAuto)
	if chatCalls != 1 {
		t.Fatalf("chat-completions calls = %d, want 1 — a SourceProbeFailed verdict must be retried, not treated as final", chatCalls)
	}
}

// TestReportLMStudio_DocumentsImageSupportForANotLoadedModel is
// review-whole.md M-1's wiring proof for eino.DocumentedImageSupport: a
// model LM Studio reports as not loaded (so must never be probed — see
// TestReportLMStudio_NeverProbesANotLoadedModel) still gets a persisted
// SourceDocumented verdict, sourced from the SAME "type":"vlm" metadata the
// initial /api/v0/models fetch already carried (DeclaredMultimodal), at
// zero extra request cost — chatCalls stays 0 throughout.
func TestReportLMStudio_DocumentsImageSupportForANotLoadedModel(t *testing.T) {
	var chatCalls int32
	srv := newFakeLMStudioServer(t, "cold-vlm", false, true, &chatCalls)

	dir := t.TempDir()
	cache, err := eino.NewCache(dir, 0)
	if err != nil {
		t.Fatalf("NewCache: %v", err)
	}
	client := eino.NewLMStudioClient(srv.URL, "", nil)

	msg := reportLMStudio(context.Background(), cache, client, eino.RefreshAuto)
	if chatCalls != 0 {
		t.Fatalf("chat-completions calls = %d, want 0 — the documented half must never probe", chatCalls)
	}
	if !strings.Contains(msg, "image-support documented for 1 unloaded model(s)") {
		t.Fatalf("message = %q, want it to report the documented count", msg)
	}

	// Same consumption-proof shape as the probed case above: an independent
	// second Cache instance must see what the first call persisted.
	cache2, err := eino.NewCache(dir, 0)
	if err != nil {
		t.Fatalf("NewCache (second instance): %v", err)
	}
	verdict, found, err := cache2.GetImageSupport("lmstudio", "cold-vlm")
	if err != nil {
		t.Fatalf("GetImageSupport: %v", err)
	}
	if !found {
		t.Fatal("image-support verdict was not persisted to disk — DocumentedImageSupport's PutImageSupport call was never reached")
	}
	if !verdict.Supported {
		t.Errorf("verdict.Supported = false, want true (server declared type=vlm)")
	}
	if verdict.Source != eino.SourceDocumented {
		t.Errorf("verdict.Source = %q, want %q — a documented verdict must never claim to be probed", verdict.Source, eino.SourceDocumented)
	}
	if verdict.Detail != "LM Studio type=vlm" {
		t.Errorf("verdict.Detail = %q, want the origin string DocumentedImageSupport's doc comment names", verdict.Detail)
	}

	// A second call must not re-document what is already on disk.
	reportLMStudio(context.Background(), cache2, client, eino.RefreshAuto)
	if chatCalls != 0 {
		t.Fatalf("chat-completions calls after second call = %d, want still 0", chatCalls)
	}
}

// TestReportLMStudio_OfflineNeverProbesALoadedModel proves
// eino.RefreshCacheOnly's "never touches the network" contract holds all
// the way through reportLMStudio, not just at the Cache.Get call: a model
// LM Studio reports as loaded, with no cached verdict yet, would normally
// be probed (TestReportLMStudio_ProbesAndPersistsImageSupportForLoadedModel)
// — under the offline policy it must be left unmeasured instead, since
// ProbeImageSupport is itself a live chat-completions round trip.
func TestReportLMStudio_OfflineNeverProbesALoadedModel(t *testing.T) {
	var chatCalls int32
	srv := newFakeLMStudioServer(t, "vision-model", true, false, &chatCalls)

	dir := t.TempDir()
	cache, err := eino.NewCache(dir, 0)
	if err != nil {
		t.Fatalf("NewCache: %v", err)
	}
	client := eino.NewLMStudioClient(srv.URL, "", nil)

	// Seed the listing cache so RefreshCacheOnly has something on disk to
	// read — the models endpoint itself is never contacted live either
	// under this policy (see the offline doctor test below for that half).
	if _, err := cache.Get(context.Background(), client, eino.RefreshAuto); err != nil {
		t.Fatalf("seed listing cache: %v", err)
	}

	msg := reportLMStudio(context.Background(), cache, client, eino.RefreshCacheOnly)
	if chatCalls != 0 {
		t.Fatalf("chat-completions calls = %d, want 0 — RefreshCacheOnly must never probe", chatCalls)
	}
	if strings.Contains(msg, "probed") {
		t.Errorf("message = %q, must not claim a probe happened under RefreshCacheOnly", msg)
	}

	if _, found, _ := cache.GetImageSupport("lmstudio", "vision-model"); found {
		t.Error("an image-support verdict was persisted despite the offline policy skipping the probe")
	}
}

// TestCheckLocalRuntimesWith_OfflineNeverContactsAHungPort is the offline
// counterpart to TestCheckLocalRuntimesWith_DeadlineBoundsAHungPort: under
// eino.RefreshCacheOnly against an empty cache dir (nothing on disk yet),
// checkLocalRuntimesWith must return almost immediately with an
// "unavailable" line for both runtimes, never attempting the TCP connect
// doctorLocalRuntimeProbeTimeout otherwise bounds — proving RefreshCacheOnly
// is actually wired through, not merely accepted as an unused parameter.
func TestCheckLocalRuntimesWith_OfflineNeverContactsAHungPort(t *testing.T) {
	ollamaURL := newHangingListener(t)
	lmstudioURL := newHangingListener(t)

	cache, err := eino.NewCache(t.TempDir(), 0)
	if err != nil {
		t.Fatalf("NewCache: %v", err)
	}
	ollamaClient := eino.NewOllamaClient(ollamaURL, nil)
	lmstudioClient := eino.NewLMStudioClient(lmstudioURL, "", nil)

	start := time.Now()
	result := checkLocalRuntimesWith(context.Background(), cache, ollamaClient, lmstudioClient, eino.RefreshCacheOnly)
	elapsed := time.Since(start)

	// A generous margin well under doctorLocalRuntimeProbeTimeout (3s) x 2:
	// a real network attempt against a hung listener would take close to
	// that; RefreshCacheOnly reading an empty dir returns in microseconds.
	const wantUnder = 500 * time.Millisecond
	if elapsed >= wantUnder {
		t.Fatalf("checkLocalRuntimesWith(RefreshCacheOnly) against two hung ports took %v, want under %v — it must never attempt the network", elapsed, wantUnder)
	}
	if !strings.Contains(result.Message, "ollama:") || !strings.Contains(result.Message, "unavailable") {
		t.Errorf("ollama half of message = %q, want an unavailable line (no cache on disk yet)", result.Message)
	}
	if !strings.Contains(result.Message, "lmstudio:") || !strings.Contains(result.Message, "unavailable") {
		t.Errorf("lmstudio half of message = %q, want an unavailable line (no cache on disk yet)", result.Message)
	}
}

// TestRunDoctor_OfflineOptionReachesLocalRuntimesCheck is the wiring-proof
// half for DoctorOptions.Offline: a real RunDoctor call with Offline: true
// must produce the same "no cache on disk yet" unavailable shape
// TestCheckLocalRuntimesWith_OfflineNeverContactsAHungPort exercises
// directly — proving the CLI flag reaches checkLocalRuntimes's policy
// argument, not just that the argument works in isolation. It cannot use a
// hung listener (checkLocalRuntimes always dials the real default
// loopback ports), so it asserts the check still completes and stays OK,
// the same tolerant assertion TestRunDoctor_IncludesLocalRuntimesCheck
// makes for the non-offline case.
func TestRunDoctor_OfflineOptionReachesLocalRuntimesCheck(t *testing.T) {
	cfgBody := fmt.Sprintf(`
server: { http_addr: "127.0.0.1:0" }
storage: { sqlite_path: %q }
`, filepath.Join(t.TempDir(), "yanshi.db"))
	rep := RunDoctor(context.Background(), DoctorOptions{ConfigPath: writeTempConfig(t, cfgBody), Root: t.TempDir(), Offline: true})
	c := findCheck(t, rep, "local-runtimes")
	if c.Status != StatusOK {
		t.Errorf("local-runtimes: got %s (%s), want ok", c.Status, c.Message)
	}
	if !strings.Contains(c.Message, "ollama:") || !strings.Contains(c.Message, "lmstudio:") {
		t.Errorf("local-runtimes message = %q, want both runtimes reported", c.Message)
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

	msg := reportOllama(context.Background(), cache, client, eino.RefreshAuto)
	if !strings.Contains(msg, "llama3:latest") {
		t.Fatalf("message = %q, want it to mention the discovered model", msg)
	}
	if strings.Contains(msg, "probed") {
		t.Errorf("message = %q, reportOllama must never claim an image-support probe", msg)
	}
}

// TestReportOllama_UnreachableIsReportedNotFatal proves the graceful-
// degradation property (I-1's property (3)): pointing the client at a
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

	msg := reportOllama(context.Background(), cache, client, eino.RefreshAuto)
	if !strings.Contains(msg, "ollama:") || !strings.Contains(msg, "unavailable") {
		t.Errorf("message = %q, want an informational ollama:-prefixed unavailable line", msg)
	}
}

// newHangingListener returns an "http://host:port" URL whose TCP port
// accepts every connection and then never writes a byte back — the I-2
// failure shape (a stuck daemon, a firewall DROP after the SYN, an
// unrelated service squatting on the port) that a closed port
// (TestReportOllama_UnreachableIsReportedNotFatal's ECONNREFUSED case)
// cannot reproduce, because ECONNREFUSED returns immediately while this
// leaves the client's response read blocked indefinitely. Every accepted
// connection is held open (never closed, never read from beyond the OS
// socket buffer) until the test's t.Cleanup runs.
func newHangingListener(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return // listener closed by t.Cleanup.
			}
			t.Cleanup(func() { conn.Close() })
			// Deliberately: read nothing, write nothing, don't close.
		}
	}()
	return "http://" + ln.Addr().String()
}

// TestCheckLocalRuntimesWith_DeadlineBoundsAHungPort is I-2's acceptance
// test: it proves doctorLocalRuntimeProbeTimeout, not the two clients'
// 10s-default http.Client.Timeout (eino.DefaultDiscoveryHTTPTimeout, via
// NewOllamaClient/NewLMStudioClient's httpClient==nil fallback), is what
// actually bounds a hung local-runtime port when checkLocalRuntimesWith
// runs — the exact function checkLocalRuntimes (RunDoctor's real check)
// calls with production clients. If doctorLocalRuntimeProbeTimeout's
// context.WithTimeout wrapping were ever removed or miswired, this test
// would take close to I-2's identified worst case (2 x 10s = 20s) instead
// of finishing in a few seconds.
func TestCheckLocalRuntimesWith_DeadlineBoundsAHungPort(t *testing.T) {
	ollamaURL := newHangingListener(t)
	lmstudioURL := newHangingListener(t)

	cache, err := eino.NewCache(t.TempDir(), 0)
	if err != nil {
		t.Fatalf("NewCache: %v", err)
	}
	ollamaClient := eino.NewOllamaClient(ollamaURL, nil)
	lmstudioClient := eino.NewLMStudioClient(lmstudioURL, "", nil)

	start := time.Now()
	result := checkLocalRuntimesWith(context.Background(), cache, ollamaClient, lmstudioClient, eino.RefreshAuto)
	elapsed := time.Since(start)

	// Real margin, not a tight pin to 2 x doctorLocalRuntimeProbeTimeout —
	// a tight pin would make this flaky under CI scheduling jitter. The
	// property under test is "closer to the short deadline than to the
	// 10s-per-client default", so anything comfortably under half of
	// I-2's 20s worst case proves the fix and rules out the regression.
	const wantUnder = 10 * time.Second
	if elapsed >= wantUnder {
		t.Fatalf("checkLocalRuntimesWith against two hung ports took %v, want under %v (doctorLocalRuntimeProbeTimeout must bound the request, not eino.DefaultDiscoveryHTTPTimeout)", elapsed, wantUnder)
	}
	if !strings.Contains(result.Message, "ollama:") || !strings.Contains(result.Message, "unavailable") {
		t.Errorf("ollama half of message = %q, want an unavailable line", result.Message)
	}
	if !strings.Contains(result.Message, "lmstudio:") || !strings.Contains(result.Message, "unavailable") {
		t.Errorf("lmstudio half of message = %q, want an unavailable line", result.Message)
	}
}

// TestRunDoctor_IncludesLocalRuntimesCheck is the wiring-proof half of
// I-1's property (1): a real `RunDoctor` call — the same aggregate
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
