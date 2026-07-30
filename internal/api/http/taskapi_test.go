package http

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/x6nux/yanshi/internal/agent/orchestrator"
	"github.com/x6nux/yanshi/internal/guard"
	einollm "github.com/x6nux/yanshi/internal/llm/eino"
	"github.com/x6nux/yanshi/internal/store"
	"github.com/x6nux/yanshi/internal/task"
)

func newTaskTestServer(t *testing.T) (*httptest.Server, *task.Broker) {
	t.Helper()
	st, err := store.Open(":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { st.Close() })

	b := task.NewBroker(st, 3, 0)

	profiles := map[string]guard.PermissionProfile{
		"foo": {
			Tools: guard.ToolsPerm{Allow: []string{"fs.*", "shell"}},
			Shell: guard.ShellPerm{Policy: "allowlist", Patterns: []string{"echo *"}},
		},
	}

	s := New(Config{Token: "tok"})
	s.TaskAPI(b, profiles)
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	return ts, b
}

func authReq(t *testing.T, method, url, token string, body any) *http.Request {
	t.Helper()
	var r io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		require.NoError(t, err)
		r = bytes.NewReader(raw)
	}
	req, err := http.NewRequest(method, url, r)
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer "+token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return req
}

func TestTaskAPI_ClaimAndGet(t *testing.T) {
	ts, b := newTaskTestServer(t)

	// Submit a task directly via the broker.
	id, err := b.Submit("echo", `{"msg":"hi"}`, "")
	require.NoError(t, err)
	require.NotEmpty(t, id)

	// POST /api/v1/tasks/claim — should return the task.
	resp, err := ts.Client().Do(authReq(t, "POST", ts.URL+"/api/v1/tasks/claim", "tok",
		map[string]any{"worker": "w1", "caps": []string{"echo"}}))
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusOK, resp.StatusCode)

	body, _ := io.ReadAll(resp.Body)
	var claimed store.Task
	require.NoError(t, json.Unmarshal(body, &claimed))
	assert.Equal(t, id, claimed.ID)
	assert.Equal(t, "running", claimed.Status)
	assert.Equal(t, "w1", claimed.AssignedTo)

	// GET /api/v1/tasks/{id} — should return the same task.
	resp2, err := ts.Client().Do(authReq(t, "GET", ts.URL+"/api/v1/tasks/"+id, "tok", nil))
	require.NoError(t, err)
	defer resp2.Body.Close()
	assert.Equal(t, http.StatusOK, resp2.StatusCode)

	body2, _ := io.ReadAll(resp2.Body)
	var got store.Task
	require.NoError(t, json.Unmarshal(body2, &got))
	assert.Equal(t, id, got.ID)
	assert.Equal(t, "running", got.Status)
}

func TestTaskAPI_ClaimNoTasks(t *testing.T) {
	ts, _ := newTaskTestServer(t)

	// No tasks submitted — should return 204.
	resp, err := ts.Client().Do(authReq(t, "POST", ts.URL+"/api/v1/tasks/claim", "tok",
		map[string]any{"worker": "w1", "caps": []string{}}))
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusNoContent, resp.StatusCode)
}

func TestTaskAPI_GetMissing(t *testing.T) {
	ts, _ := newTaskTestServer(t)

	resp, err := ts.Client().Do(authReq(t, "GET", ts.URL+"/api/v1/tasks/nonexistent", "tok", nil))
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusNotFound, resp.StatusCode)
}

func TestTaskAPI_Profile(t *testing.T) {
	ts, _ := newTaskTestServer(t)

	// GET /api/v1/agent/profile?worker=foo — should return the configured profile.
	resp, err := ts.Client().Do(authReq(t, "GET", ts.URL+"/api/v1/agent/profile?worker=foo", "tok", nil))
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusOK, resp.StatusCode)

	body, _ := io.ReadAll(resp.Body)
	var prof guard.PermissionProfile
	require.NoError(t, json.Unmarshal(body, &prof))
	assert.Equal(t, []string{"fs.*", "shell"}, prof.Tools.Allow)
	assert.Equal(t, "allowlist", prof.Shell.Policy)
	assert.Equal(t, []string{"echo *"}, prof.Shell.Patterns)
}

func TestTaskAPI_ProfileDefault(t *testing.T) {
	ts, _ := newTaskTestServer(t)

	// GET /api/v1/agent/profile?worker=unknown — should return a deny-all
	// profile (fail-closed for unknown workers).
	resp, err := ts.Client().Do(authReq(t, "GET", ts.URL+"/api/v1/agent/profile?worker=unknown", "tok", nil))
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusOK, resp.StatusCode)

	body, _ := io.ReadAll(resp.Body)
	var prof guard.PermissionProfile
	require.NoError(t, json.Unmarshal(body, &prof))
	assert.Empty(t, prof.Tools.Allow, "unknown worker should have no tool permissions")
	assert.False(t, prof.Net.Allow, "unknown worker should have network denied")
	assert.Equal(t, "deny", prof.Shell.Policy, "unknown worker should have shell policy deny")
}

func TestTaskAPI_Unauthorized(t *testing.T) {
	ts, _ := newTaskTestServer(t)

	// Use a non-loopback RemoteAddr so the request does not bypass auth.
	req := httptest.NewRequest("GET", "/api/v1/tasks/nonexistent", nil)
	req.RemoteAddr = "10.0.0.5:1234"
	rec := httptest.NewRecorder()
	ts.Config.Handler.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusUnauthorized, rec.Code)
}

func TestTaskAPI_Result(t *testing.T) {
	ts, b := newTaskTestServer(t)

	id, err := b.Submit("echo", `{"msg":"hi"}`, "")
	require.NoError(t, err)

	// Claim the task first (result only makes sense for running tasks).
	_, err = b.Claim("w1")
	require.NoError(t, err)

	// POST /api/v1/tasks/{id}/result with {worker, status, result}.
	resp, err := ts.Client().Do(authReq(t, "POST", ts.URL+"/api/v1/tasks/"+id+"/result", "tok",
		map[string]any{"worker": "w1", "status": "completed", "result": "done"}))
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusOK, resp.StatusCode)

	// Verify via GET that the task is now completed.
	resp2, err := ts.Client().Do(authReq(t, "GET", ts.URL+"/api/v1/tasks/"+id, "tok", nil))
	require.NoError(t, err)
	defer resp2.Body.Close()
	assert.Equal(t, http.StatusOK, resp2.StatusCode)

	body, _ := io.ReadAll(resp2.Body)
	var got store.Task
	require.NoError(t, json.Unmarshal(body, &got))
	assert.Equal(t, "completed", got.Status)
	assert.Equal(t, "done", got.Result)
}

func TestTaskAPI_ResultNotFound(t *testing.T) {
	ts, _ := newTaskTestServer(t)

	resp, err := ts.Client().Do(authReq(t, "POST", ts.URL+"/api/v1/tasks/nonexistent/result", "tok",
		map[string]any{"worker": "w1", "status": "completed", "result": "done"}))
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusNotFound, resp.StatusCode)
}

func TestTaskAPI_Heartbeat(t *testing.T) {
	ts, b := newTaskTestServer(t)

	id, err := b.Submit("echo", `{"msg":"hi"}`, "")
	require.NoError(t, err)

	// Claim so the task is running.
	_, err = b.Claim("w1")
	require.NoError(t, err)

	resp, err := ts.Client().Do(authReq(t, "POST", ts.URL+"/api/v1/tasks/"+id+"/heartbeat", "tok", nil))
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusOK, resp.StatusCode)
}

func TestTaskAPI_Progress(t *testing.T) {
	ts, b := newTaskTestServer(t)

	id, err := b.Submit("echo", `{"msg":"hi"}`, "")
	require.NoError(t, err)

	// POST /api/v1/tasks/{id}/progress — no-op store for M5, just ack.
	resp, err := ts.Client().Do(authReq(t, "POST", ts.URL+"/api/v1/tasks/"+id+"/progress", "tok",
		map[string]any{"note": "50% done"}))
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusOK, resp.StatusCode)
}

func TestTaskAPI_SSEEvents(t *testing.T) {
	ts, b := newTaskTestServer(t)

	// Submit a task — this fires the notify channel.
	id, err := b.Submit("echo", `{"msg":"hi"}`, "")
	require.NoError(t, err)
	require.NotEmpty(t, id)

	// Connect to the SSE endpoint with a timeout.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, "GET", ts.URL+"/api/v1/tasks/events", nil)
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer tok")

	resp, err := ts.Client().Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, "text/event-stream", resp.Header.Get("Content-Type"))

	// Read until we get a task_available event, then close.
	scanner := bufio.NewScanner(resp.Body)
	gotEvent := false
	for scanner.Scan() {
		line := scanner.Text()
		if line == "event: task_available" {
			gotEvent = true
			break
		}
	}
	assert.True(t, gotEvent, "expected to receive a task_available event")
}

func TestTaskAPI_ResultWrongWorker(t *testing.T) {
	ts, b := newTaskTestServer(t)

	id, err := b.Submit("echo", `{"msg":"hi"}`, "")
	require.NoError(t, err)

	_, err = b.Claim("w1")
	require.NoError(t, err)

	// POST result as a different worker → 409 Conflict.
	resp, err := ts.Client().Do(authReq(t, "POST", ts.URL+"/api/v1/tasks/"+id+"/result", "tok",
		map[string]any{"worker": "w2", "status": "completed", "result": "stolen"}))
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusConflict, resp.StatusCode)

	// Task should still be running, assigned to w1.
	got, err := b.Get(id)
	require.NoError(t, err)
	assert.Equal(t, "running", got.Status)
	assert.Equal(t, "w1", got.AssignedTo)
}

func TestTaskAPI_ResultInvalidStatus(t *testing.T) {
	ts, b := newTaskTestServer(t)

	id, err := b.Submit("echo", `{"msg":"hi"}`, "")
	require.NoError(t, err)

	_, err = b.Claim("w1")
	require.NoError(t, err)

	// POST result with status "pending" → 400 Bad Request.
	resp, err := ts.Client().Do(authReq(t, "POST", ts.URL+"/api/v1/tasks/"+id+"/result", "tok",
		map[string]any{"worker": "w1", "status": "pending", "result": ""}))
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)

	// Task should still be running.
	got, err := b.Get(id)
	require.NoError(t, err)
	assert.Equal(t, "running", got.Status)
}

func TestTaskAPI_StaleWorkerRejected(t *testing.T) {
	ts, b := newTaskTestServer(t)

	id, err := b.Submit("echo", "in", "")
	require.NoError(t, err)

	// Worker A claims and fails → task requeued to pending.
	_, err = b.Claim("A")
	require.NoError(t, err)
	require.NoError(t, b.RecordResult(id, "A", "failed", "err"))

	// Worker B claims the requeued task.
	_, err = b.Claim("B")
	require.NoError(t, err)

	// Worker A tries to POST result via HTTP → 409.
	resp, err := ts.Client().Do(authReq(t, "POST", ts.URL+"/api/v1/tasks/"+id+"/result", "tok",
		map[string]any{"worker": "A", "status": "completed", "result": "stale"}))
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusConflict, resp.StatusCode)
}

func TestHTTP_BodyLimit(t *testing.T) {
	ts, b := newTaskTestServer(t)

	// Build a JSON body larger than 1 MiB.
	big := strings.Repeat("x", 2<<20) // 2 MiB
	body := `{"worker":"w1","status":"completed","result":"` + big + `"}`

	req, err := http.NewRequest("POST", ts.URL+"/api/v1/tasks/claim", strings.NewReader(body))
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer tok")
	req.Header.Set("Content-Type", "application/json")

	resp, err := ts.Client().Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)

	// Also verify the chat endpoint rejects oversized bodies.
	_ = b // keep compiler happy
	o, err := orchestrator.New(orchestrator.Config{Model: einollm.NewFakeModel([]string{"ok"}, nil)})
	require.NoError(t, err)
	s2 := New(Config{Token: "tok"})
	s2.Chat(o, nil, nil)
	ts2 := httptest.NewServer(s2.Handler())
	defer ts2.Close()

	bigChat := `{"message":"` + strings.Repeat("x", 2<<20) + `"}`
	req2, err := http.NewRequest("POST", ts2.URL+"/api/v1/chat", strings.NewReader(bigChat))
	require.NoError(t, err)
	req2.Header.Set("Authorization", "Bearer tok")
	req2.Header.Set("Content-Type", "application/json")

	resp2, err := ts2.Client().Do(req2)
	require.NoError(t, err)
	defer resp2.Body.Close()
	assert.Equal(t, http.StatusBadRequest, resp2.StatusCode)
}
