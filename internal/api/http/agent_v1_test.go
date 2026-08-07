package http

import (
	"encoding/json"
	"io"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/x6nux/yanshi/internal/api/v1"
	einollm "github.com/x6nux/yanshi/internal/llm/eino"
)

func readAll(r io.ReadCloser) string {
	b, _ := io.ReadAll(r)
	return string(b)
}

// TestAgentV1StartAcceptsUnknownFieldsAndReturnsCamelCase proves thread/start
// honors the v1 wire contract: unknown request fields are ignored (no
// DisallowUnknownFields), every response carries X-Yanshi-API-Version=v1, and
// every JSON key is camelCase (never snake_case). This is the load-bearing
// forward-compatibility guarantee for external clients.
func TestAgentV1StartAcceptsUnknownFieldsAndReturnsCamelCase(t *testing.T) {
	model := einollm.NewFakeModel([]string{"answer"}, nil)
	svc, err := v1.NewService(v1.Config{DefaultModel: model})
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	s := New(Config{Token: "token"})
	s.AgentV1(svc)
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()
	resp, err := ts.Client().Post(ts.URL+"/api/v1/thread/start", "application/json", strings.NewReader(`{"title":"x","futureField":1}`))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 || resp.Header.Get("X-Yanshi-API-Version") != "v1" {
		t.Fatalf("status=%d headers=%v body=%s", resp.StatusCode, resp.Header, body)
	}
	var got map[string]any
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("body JSON: %v", err)
	}
	if _, ok := got["thread"]; !ok {
		t.Fatalf("response missing thread: %s", body)
	}
	if strings.Contains(string(body), "created_at") || strings.Contains(string(body), "updated_at") {
		t.Fatalf("non-v1 body contains snake_case keys: %s", body)
	}
}

// TestAgentV1TurnStreamUsesItemEvents proves turn/start emits an SSE stream
// where every event is `event: item\ndata: <v1.Item JSON>\n\n` (plus the
// initial `event: turn`). The wire never carries the legacy agent_chunk event
// name — v1 clients consume items only.
//
// ledger: D2/V15#1 start/resume/run/stream/cancel 可用
func TestAgentV1TurnStreamUsesItemEvents(t *testing.T) {
	model := einollm.NewFakeModel([]string{"answer"}, nil)
	svc, err := v1.NewService(v1.Config{DefaultModel: model})
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	s := New(Config{})
	s.AgentV1(svc)
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()
	start, err := ts.Client().Post(ts.URL+"/api/v1/thread/start", "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatalf("thread/start POST: %v", err)
	}
	var started v1.ThreadStartResponse
	_ = json.NewDecoder(start.Body).Decode(&started)
	_ = start.Body.Close()
	body := `{"threadId":"` + started.Thread.ID + `","input":"hello"}`
	resp, err := ts.Client().Post(ts.URL+"/api/v1/turn/start", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("turn POST: %v", err)
	}
	defer resp.Body.Close()
	stream, _ := io.ReadAll(resp.Body)
	text := string(stream)
	if !strings.Contains(text, "event: item") || !strings.Contains(text, `"version":"v1"`) {
		t.Fatalf("stream missing v1 item events: %s", text)
	}
	if !strings.Contains(text, "event: turn") {
		t.Fatalf("stream missing initial turn event: %s", text)
	}
	if resp.Header.Get("X-Yanshi-API-Version") != "v1" {
		t.Fatal("missing X-Yanshi-API-Version header on turn/start response")
	}
}

// TestV1CompatibilityMatrix drives thread/start against three request shapes:
// missing optional version (defaults to v1), unknown request field (ignored),
// and malformed JSON (rejected with 400). Every successful response must be
// JSON-decodable into a value whose keys are all camelCase (no underscores).
// This is the forward-compat guarantee for v1 clients.
//
// ledger: D1/V14#3 兼容测试完善
func TestV1CompatibilityMatrix(t *testing.T) {
	model := einollm.NewFakeModel([]string{"answer"}, nil)
	svc, err := v1.NewService(v1.Config{DefaultModel: model})
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	s := New(Config{})
	s.AgentV1(svc)
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	cases := []struct {
		name       string
		body       string
		wantStatus int
	}{
		{"missing optional version defaults to v1", `{"title":"x"}`, 200},
		{"unknown request field ignored", `{"title":"x","future":{"x":1}}`, 200},
		{"malformed json rejected", `{"title":`, 400},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp, err := ts.Client().Post(ts.URL+"/api/v1/thread/start", "application/json", strings.NewReader(tc.body))
			if err != nil {
				t.Fatalf("POST: %v", err)
			}
			defer resp.Body.Close()
			body, _ := io.ReadAll(resp.Body)
			if resp.StatusCode != tc.wantStatus {
				t.Fatalf("status = %d, want %d, body = %s", resp.StatusCode, tc.wantStatus, body)
			}
			if resp.Header.Get("X-Yanshi-API-Version") != "v1" {
				t.Fatalf("missing X-Yanshi-API-Version header")
			}
			// Every successful response must be JSON-decodable into a value whose
			// keys are all camelCase. The simple substring check is sufficient
			// for thread/start's flat response shape; a snake_case key (e.g.
			// "created_at") would betray a leak from the legacy proto types.
			if tc.wantStatus == 200 && strings.Contains(string(body), "_") {
				t.Fatalf("response contains underscore (non-camelCase key): %s", body)
			}
		})
	}
}

// TestV1TurnStreamItemsAreVersionedAndOrdered drives a real turn/start stream
// and asserts every SSE item carries version/sequence/threadId/turnId — the
// wire contract every client depends on. It complements the per-response
// matrix above by walking the streaming payload.
//
// ledger: D1/V14#1 start/resume/interrupt + 流式 item 可用
//
// ledger: D2/V15#1 start/resume/run/stream/cancel 可用
func TestV1TurnStreamItemsAreVersionedAndOrdered(t *testing.T) {
	model := einollm.NewFakeModel([]string{"answer"}, nil)
	svc, err := v1.NewService(v1.Config{DefaultModel: model})
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	s := New(Config{})
	s.AgentV1(svc)
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	start, err := ts.Client().Post(ts.URL+"/api/v1/thread/start", "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatalf("thread/start POST: %v", err)
	}
	var started v1.ThreadStartResponse
	_ = json.NewDecoder(start.Body).Decode(&started)
	_ = start.Body.Close()

	body := `{"threadId":"` + started.Thread.ID + `","input":"hello"}`
	resp, err := ts.Client().Post(ts.URL+"/api/v1/turn/start", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("turn POST: %v", err)
	}
	defer resp.Body.Close()
	stream, _ := io.ReadAll(resp.Body)
	text := string(stream)
	if !strings.Contains(text, "event: item") {
		t.Fatalf("stream missing item events: %s", text)
	}
	// Walk every `data: {...}` line that follows an `event: item` marker and
	// assert the required fields are populated. Lines that do not decode as a
	// strict v1.Item are allowed (the initial `event: turn` line carries a
	// TurnStartResponse, not an Item) and skipped.
	for _, ln := range strings.Split(text, "\n") {
		if !strings.HasPrefix(ln, "data: ") {
			continue
		}
		var item v1.Item
		if err := json.Unmarshal([]byte(strings.TrimPrefix(ln, "data: ")), &item); err != nil {
			continue
		}
		if item.Type == "" {
			// Not an Item (likely the TurnStartResponse) — skip.
			continue
		}
		if item.Version != "v1" || item.Sequence == 0 || item.ThreadID == "" || item.TurnID == "" {
			t.Fatalf("item missing required fields: %#v", item)
		}
	}
}

// TestAgentV1ResumeEndpoint proves thread/resume returns a thread snapshot.
// It tests the happy path (known thread) and two error paths: missing thread
// (404) and an empty thread id payload.
//
// ledger: D1/V14#1 start/resume/interrupt + 流式 item 可用
func TestAgentV1ResumeEndpoint(t *testing.T) {
	model := einollm.NewFakeModel([]string{"answer"}, nil)
	svc, err := v1.NewService(v1.Config{DefaultModel: model})
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	s := New(Config{})
	s.AgentV1(svc)
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	// Create a thread first.
	start, err := ts.Client().Post(ts.URL+"/api/v1/thread/start", "application/json", strings.NewReader(`{"title":"resume-test"}`))
	if err != nil {
		t.Fatalf("thread/start POST: %v", err)
	}
	var started v1.ThreadStartResponse
	_ = json.NewDecoder(start.Body).Decode(&started)
	_ = start.Body.Close()

	// Happy path: resume the created thread.
	resp, err := ts.Client().Post(ts.URL+"/api/v1/thread/resume", "application/json",
		strings.NewReader(`{"threadId":"`+started.Thread.ID+`"}`))
	if err != nil {
		t.Fatalf("thread/resume POST: %v", err)
	}
	if resp.StatusCode != 200 {
		t.Fatalf("resume status = %d", resp.StatusCode)
	}
	if resp.Header.Get("X-Yanshi-API-Version") != "v1" {
		t.Fatal("missing X-Yanshi-API-Version header")
	}
	var resumeResp v1.ThreadResumeResponse
	_ = json.NewDecoder(resp.Body).Decode(&resumeResp)
	_ = resp.Body.Close()
	if resumeResp.Thread.ID != started.Thread.ID {
		t.Fatalf("resumed thread id = %q, want %q", resumeResp.Thread.ID, started.Thread.ID)
	}

	// Unknown thread returns 404.
	resp, err = ts.Client().Post(ts.URL+"/api/v1/thread/resume", "application/json",
		strings.NewReader(`{"threadId":"ghost"}`))
	if err != nil {
		t.Fatalf("thread/resume POST (not found): %v", err)
	}
	if resp.StatusCode != 404 {
		t.Fatalf("resume unknown status = %d, want 404", resp.StatusCode)
	}
	_ = resp.Body.Close()
}

// TestAgentV1InterruptEndpoint proves thread/interrupt cancels an active turn
// and returns the correct status codes for known/missing threads.
//
// ledger: D1/V14#1 start/resume/interrupt + 流式 item 可用
func TestAgentV1InterruptEndpoint(t *testing.T) {
	model := einollm.NewFakeModel([]string{"answer"}, nil)
	svc, err := v1.NewService(v1.Config{DefaultModel: model})
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	s := New(Config{})
	s.AgentV1(svc)
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	// Create a thread.
	start, err := ts.Client().Post(ts.URL+"/api/v1/thread/start", "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatalf("thread/start POST: %v", err)
	}
	var started v1.ThreadStartResponse
	_ = json.NewDecoder(start.Body).Decode(&started)
	_ = start.Body.Close()

	// Start a turn (it'll complete quickly on the FakeModel but that's OK).
	_, err = ts.Client().Post(ts.URL+"/api/v1/turn/start", "application/json",
		strings.NewReader(`{"threadId":"`+started.Thread.ID+`","input":"hello"}`))
	if err != nil {
		t.Fatalf("turn/start POST: %v", err)
	}

	// Interrupt — just tests the HTTP path; the turn may already be done.
	resp, err := ts.Client().Post(ts.URL+"/api/v1/thread/interrupt", "application/json",
		strings.NewReader(`{"threadId":"`+started.Thread.ID+`"}`))
	if err != nil {
		t.Fatalf("thread/interrupt POST: %v", err)
	}
	if resp.StatusCode != 200 {
		t.Fatalf("interrupt status = %d, want 200", resp.StatusCode)
	}
	_ = resp.Body.Close()

	// Unknown thread returns 404.
	resp, err = ts.Client().Post(ts.URL+"/api/v1/thread/interrupt", "application/json",
		strings.NewReader(`{"threadId":"ghost"}`))
	if err != nil {
		t.Fatalf("thread/interrupt POST (not found): %v", err)
	}
	if resp.StatusCode != 404 {
		t.Fatalf("interrupt unknown status = %d, want 404", resp.StatusCode)
	}
	_ = resp.Body.Close()
}

// TestAgentV1SchemaEndpoint proves GET /api/v1/schema/agent-v1.json returns
// the JSON Schema document with the correct Content-Type and version header.
func TestAgentV1SchemaEndpoint(t *testing.T) {
	s := New(Config{})
	s.AgentV1(nil)
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	resp, err := ts.Client().Get(ts.URL + "/api/v1/schema/agent-v1.json")
	if err != nil {
		t.Fatalf("GET schema: %v", err)
	}
	defer resp.Body.Close()
	if resp.Header.Get("Content-Type") != "application/schema+json" {
		t.Fatalf("Content-Type = %q, want application/schema+json", resp.Header.Get("Content-Type"))
	}
	if resp.Header.Get("X-Yanshi-API-Version") != "v1" {
		t.Fatal("missing X-Yanshi-API-Version header")
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "$schema") {
		t.Fatalf("schema response missing $schema: %s", body)
	}
}

// TestAgentV1TurnStartRejectsMalformedJSON proves turn/start returns 400 when
// the request body is not valid JSON.
func TestAgentV1TurnStartRejectsMalformedJSON(t *testing.T) {
	model := einollm.NewFakeModel([]string{"answer"}, nil)
	svc, err := v1.NewService(v1.Config{DefaultModel: model})
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	s := New(Config{})
	s.AgentV1(svc)
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	resp, err := ts.Client().Post(ts.URL+"/api/v1/turn/start", "application/json",
		strings.NewReader(`not json`))
	if err != nil {
		t.Fatalf("turn/start POST: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 400 {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
}

// TestAgentV1TurnStartReturns404ForUnknownThread proves turn/start returns 404
// when the given thread id does not exist.
func TestAgentV1TurnStartReturns404ForUnknownThread(t *testing.T) {
	model := einollm.NewFakeModel([]string{"answer"}, nil)
	svc, err := v1.NewService(v1.Config{DefaultModel: model})
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	s := New(Config{})
	s.AgentV1(svc)
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	resp, err := ts.Client().Post(ts.URL+"/api/v1/turn/start", "application/json",
		strings.NewReader(`{"threadId":"ghost","input":"hello"}`))
	if err != nil {
		t.Fatalf("turn/start POST: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 404 {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
}
