package worker

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	apihttp "github.com/x6nux/yanshi/internal/api/http"
	"github.com/x6nux/yanshi/internal/guard"
	"github.com/x6nux/yanshi/internal/store"
	"github.com/x6nux/yanshi/internal/task"
)

// newTestServer stands up an httptest server with the Task API and broker
// backed by an in-memory store. maxRetries controls the broker's retry limit.
func newTestServer(t *testing.T, maxRetries int) (*httptest.Server, *task.Broker) {
	t.Helper()
	st, err := store.Open(":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { st.Close() })

	b := task.NewBroker(st, maxRetries, 0)
	profiles := map[string]guard.PermissionProfile{
		"w1": {Tools: guard.ToolsPerm{Allow: []string{"*"}}},
	}
	s := apihttp.New(apihttp.Config{Token: "tok"})
	s.TaskAPI(b, profiles)
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	return ts, b
}

// failingExecutor always returns an error, simulating a task that fails.
type failingExecutor struct{}

func (failingExecutor) Execute(_ context.Context, _ store.Task) (string, error) {
	return "", errors.New("boom")
}

func TestWorker_RunOnce_Echo(t *testing.T) {
	ts, b := newTestServer(t, 3)
	client := NewClient(ts.URL, "tok")
	w := NewWorker(client, "w1", []string{"echo"}, EchoExecutor{})

	input := `{"msg":"once"}`
	id, err := b.Submit("echo", input, "")
	require.NoError(t, err)

	err = w.RunOnce(context.Background())
	require.NoError(t, err)

	got, err := b.Get(id)
	require.NoError(t, err)
	assert.Equal(t, "completed", got.Status)
	assert.Equal(t, input, got.Result)
}

func TestWorker_RunOnce_NoTasks(t *testing.T) {
	ts, _ := newTestServer(t, 3)
	client := NewClient(ts.URL, "tok")
	w := NewWorker(client, "w1", []string{"echo"}, EchoExecutor{})

	err := w.RunOnce(context.Background())
	assert.ErrorIs(t, err, ErrNoTask) // ErrNoTask = no task available
}

func TestWorker_RunOnce_Failed(t *testing.T) {
	// maxRetries=0 so the first failure is final (not requeued).
	ts, b := newTestServer(t, 0)
	client := NewClient(ts.URL, "tok")
	w := NewWorker(client, "w1", []string{"echo"}, failingExecutor{})

	id, err := b.Submit("echo", "fail-me", "")
	require.NoError(t, err)

	err = w.RunOnce(context.Background())
	require.Error(t, err) // executor returned error

	got, err := b.Get(id)
	require.NoError(t, err)
	assert.Equal(t, "failed", got.Status)
}

func TestWorker_Run_E2E(t *testing.T) {
	ts, b := newTestServer(t, 3)
	client := NewClient(ts.URL, "tok")
	w := NewWorker(client, "w1", []string{"echo"}, EchoExecutor{})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	go w.Run(ctx)

	// Submit a task; the worker should pick it up via SSE, claim, execute,
	// and report completed — all without any direct in-process wiring.
	input := `{"msg":"hello"}`
	id, err := b.Submit("echo", input, "")
	require.NoError(t, err)

	// Poll for completion.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		got, err := b.Get(id)
		require.NoError(t, err)
		if got.Status == "completed" {
			assert.Equal(t, input, got.Result)
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("task did not complete in time")
}

// TestWorker_Run_DrainsMultipleTasks verifies that the worker drains all
// pending tasks on startup (not just one per SSE signal). Two tasks are
// submitted before the worker starts; both must complete.
func TestWorker_Run_DrainsMultipleTasks(t *testing.T) {
	ts, b := newTestServer(t, 3)
	client := NewClient(ts.URL, "tok")
	w := NewWorker(client, "w1", []string{"echo"}, EchoExecutor{}).
		WithPollInterval(100 * time.Millisecond)

	// Submit TWO tasks before starting the worker.
	id1, err := b.Submit("echo", `{"msg":"task1"}`, "")
	require.NoError(t, err)
	id2, err := b.Submit("echo", `{"msg":"task2"}`, "")
	require.NoError(t, err)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	go w.Run(ctx)

	// Both tasks should become completed.
	for _, id := range []string{id1, id2} {
		deadline := time.Now().Add(3 * time.Second)
		for time.Now().Before(deadline) {
			got, err := b.Get(id)
			require.NoError(t, err)
			if got.Status == "completed" {
				break
			}
			time.Sleep(20 * time.Millisecond)
		}
		got, err := b.Get(id)
		require.NoError(t, err)
		assert.Equal(t, "completed", got.Status, "task %s did not complete", id)
	}
}

// TestWorker_Run_PollFallback verifies that a task submitted while the worker
// is running (where the SSE signal may be dropped due to the buffered(1)
// channel) still gets claimed via the periodic poll fallback.
func TestWorker_Run_PollFallback(t *testing.T) {
	ts, b := newTestServer(t, 3)
	client := NewClient(ts.URL, "tok")
	w := NewWorker(client, "w1", []string{"echo"}, EchoExecutor{}).
		WithPollInterval(100 * time.Millisecond)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	go w.Run(ctx)

	// Wait briefly for the worker to start and do its initial drain.
	time.Sleep(100 * time.Millisecond)

	// Submit a task; the SSE signal may be dropped, but the poll ticker
	// should pick it up.
	id, err := b.Submit("echo", `{"msg":"polled"}`, "")
	require.NoError(t, err)

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		got, err := b.Get(id)
		require.NoError(t, err)
		if got.Status == "completed" {
			assert.Equal(t, `{"msg":"polled"}`, got.Result)
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("task did not complete in time via poll fallback")
}

// TestWorker_Run_DrainBackoff verifies that drain does not tight-loop when
// the /claim endpoint returns persistent errors. Without backoff, the worker
// would make thousands of claim attempts per second. With backoff it makes
// at most maxDrainErrors (5) attempts, then yields to the outer Run loop.
func TestWorker_Run_DrainBackoff(t *testing.T) {
	var claimCount atomic.Int64

	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/agent/profile", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte("{}"))
	})
	mux.HandleFunc("POST /api/v1/tasks/claim", func(w http.ResponseWriter, r *http.Request) {
		claimCount.Add(1)
		http.Error(w, "internal error", http.StatusInternalServerError)
	})
	mux.HandleFunc("GET /api/v1/tasks/events", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.WriteHeader(http.StatusOK)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		// Hold the connection open until the request context is done.
		<-r.Context().Done()
	})
	ts := httptest.NewServer(mux)
	defer ts.Close()

	client := NewClient(ts.URL, "tok")
	w := NewWorker(client, "w1", []string{"echo"}, EchoExecutor{}).
		WithPollInterval(10 * time.Second) // long poll so only the initial drain runs

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	go w.Run(ctx)

	// Wait 300ms. Without backoff the drain would have made hundreds of claim
	// attempts in this window. With 100ms initial backoff it makes at most 2.
	time.Sleep(300 * time.Millisecond)
	n := claimCount.Load()
	assert.LessOrEqual(t, n, int64(3), "drain should not tight-loop on persistent errors (got %d claims in 300ms)", n)

	// Wait for the drain to fully yield (5 errors with cumulative backoff
	// ≈ 100+200+400+800 = 1500ms).
	time.Sleep(2 * time.Second)
	n = claimCount.Load()
	assert.Equal(t, int64(5), n, "drain should make exactly 5 attempts then yield (got %d)", n)

	// Verify no more claims arrive after yielding (poll interval is 10s).
	time.Sleep(300 * time.Millisecond)
	assert.Equal(t, int64(5), claimCount.Load(), "no new claims expected after drain yields")
}

// slowExecutor sleeps for a configurable duration before returning, so we can
// verify that heartbeats are sent during execution.
type slowExecutor struct {
	delay time.Duration
}

func (s slowExecutor) Execute(ctx context.Context, _ store.Task) (string, error) {
	select {
	case <-time.After(s.delay):
		return "slow-done", nil
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

// TestWorker_RunOnce_SendsHeartbeat verifies that RunOnce sends at least one
// heartbeat POST to the server during task execution. We use a slow executor
// (250ms) with a 50ms heartbeat interval, then assert ≥1 heartbeat arrived.
func TestWorker_RunOnce_SendsHeartbeat(t *testing.T) {
	ts, b := newTestServer(t, 3)
	client := NewClient(ts.URL, "tok")
	w := NewWorker(client, "w1", []string{"echo"}, slowExecutor{delay: 250 * time.Millisecond}).
		WithHeartbeatInterval(50 * time.Millisecond)

	// We need to count heartbeat calls. Wrap the server with a counting handler.
	var hbCount atomic.Int64
	originalHandler := ts.Config.Handler
	ts.Config.Handler = http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "" && len(r.URL.Path) > len("/api/v1/tasks/") && r.URL.Path[len(r.URL.Path)-len("/heartbeat"):] == "/heartbeat" && r.Method == "POST" {
			hbCount.Add(1)
		}
		originalHandler.ServeHTTP(rw, r)
	})

	id, err := b.Submit("echo", `{"msg":"slow"}`, "")
	require.NoError(t, err)

	err = w.RunOnce(context.Background())
	require.NoError(t, err)

	got, err := b.Get(id)
	require.NoError(t, err)
	assert.Equal(t, "completed", got.Status)

	// At 50ms interval over 250ms execution, we expect at least 1 heartbeat.
	assert.GreaterOrEqual(t, hbCount.Load(), int64(1), "expected at least 1 heartbeat during execution")
}

// TestWorker_Run_SSEReconnectRetries verifies that the worker retries the SSE
// connection with exponential backoff instead of fataling on transient errors.
// The test server's /events endpoint returns 503 on the first 2 attempts, then
// succeeds on the 3rd. The worker (with a 20ms initial backoff) should survive
// the bad-connect window, eventually connect, and process a task.
func TestWorker_Run_SSEReconnectRetries(t *testing.T) {
	ts, b := newTestServer(t, 3)

	// Intercept /events: fail the first 2 attempts with 503, then delegate.
	var eventsAttempts atomic.Int64
	realHandler := ts.Config.Handler
	ts.Config.Handler = http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/tasks/events" {
			n := eventsAttempts.Add(1)
			if n <= 2 {
				http.Error(rw, "service unavailable", http.StatusServiceUnavailable)
				return
			}
		}
		realHandler.ServeHTTP(rw, r)
	})

	client := NewClient(ts.URL, "tok")
	w := NewWorker(client, "w1", []string{"echo"}, EchoExecutor{}).
		WithReconnectBackoff(20 * time.Millisecond).
		WithPollInterval(100 * time.Millisecond)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	runErr := make(chan error, 1)
	go func() {
		runErr <- w.Run(ctx)
	}()

	// Wait for the 2 failures + 3rd successful connect (~60ms with 20ms backoff).
	// Give generous time for the connection to establish.
	time.Sleep(300 * time.Millisecond)

	// The worker should still be alive — Run must NOT have returned a fatal error.
	select {
	case err := <-runErr:
		t.Fatalf("worker Run returned error during reconnect window: %v", err)
	default:
	}

	// At least 3 attempts should have been made (2 failures + 1 success).
	assert.GreaterOrEqual(t, eventsAttempts.Load(), int64(3),
		"should have retried at least 3 times (got %d)", eventsAttempts.Load())

	// Submit a task; the worker should pick it up and complete it.
	input := `{"msg":"after-reconnect"}`
	id, err := b.Submit("echo", input, "")
	require.NoError(t, err)

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		got, err := b.Get(id)
		require.NoError(t, err)
		if got.Status == "completed" {
			assert.Equal(t, input, got.Result)
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("task did not complete after SSE reconnect")
}

// ---------------------------------------------------------------------------
// Coverage: Client HTTP error paths, Events ctx cancel, RunOnce report failure,
// Run/drain/connectEvents backoff and context cancellation.
// ---------------------------------------------------------------------------

func TestClientGetProfile_DialError(t *testing.T) {
	client := NewClient("http://127.255.255.255:1", "tok")
	_, err := client.GetProfile(context.Background(), "w1")
	require.Error(t, err)
}

func TestClientClaim_Non200(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/v1/tasks/claim", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})
	ts := httptest.NewServer(mux)
	defer ts.Close()

	client := NewClient(ts.URL, "tok")
	_, err := client.Claim(context.Background(), "w1", nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unexpected status")
}

func TestClientClaim_DecodeBodyError(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/v1/tasks/claim", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{invalid`))
	})
	ts := httptest.NewServer(mux)
	defer ts.Close()

	client := NewClient(ts.URL, "tok")
	_, err := client.Claim(context.Background(), "w1", nil)
	require.Error(t, err)
}

func TestClientReportResult_Non200(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/v1/tasks/", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})
	ts := httptest.NewServer(mux)
	defer ts.Close()

	client := NewClient(ts.URL, "tok")
	err := client.ReportResult(context.Background(), "t1", "w1", "failed", "bad")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unexpected status")
}

func TestClientHeartbeat_Non200(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/v1/tasks/", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})
	ts := httptest.NewServer(mux)
	defer ts.Close()

	client := NewClient(ts.URL, "tok")
	err := client.Heartbeat(context.Background(), "t1")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unexpected status")
}

func TestClientEvents_Non200(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/tasks/events", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	})
	ts := httptest.NewServer(mux)
	defer ts.Close()

	client := NewClient(ts.URL, "tok")
	_, err := client.Events(context.Background())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unexpected status")
}

func TestClientEvents_ScannerCtxCancel(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/tasks/events", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		w.Write([]byte("event: task_available\ndata: {}\n\n"))
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		<-r.Context().Done()
	})
	ts := httptest.NewServer(mux)
	defer ts.Close()

	client := NewClient(ts.URL, "tok")
	ctx, cancel := context.WithCancel(context.Background())
	ch, err := client.Events(ctx)
	require.NoError(t, err)
	// Give the scanner goroutine time to read the event and block in the select.
	time.Sleep(50 * time.Millisecond)
	// Cancel the context so the scanner goroutine sees ctx.Done().
	cancel()
	// The channel should close (scanner sees ctx.Done() and returns).
	_, ok := <-ch
	assert.False(t, ok, "channel should be closed when ctx is cancelled")
}

func TestWorker_RunOnce_ExecFailedReportFails(t *testing.T) {
	ts, b := newTestServer(t, 0)
	// Intercept ReportResult to return 500 so both exec and report fail.
	realHandler := ts.Config.Handler
	ts.Config.Handler = http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		if r.Method == "POST" && strings.Contains(r.URL.Path, "/result") {
			rw.WriteHeader(http.StatusInternalServerError)
			return
		}
		realHandler.ServeHTTP(rw, r)
	})

	client := NewClient(ts.URL, "tok")
	w := NewWorker(client, "w1", []string{"echo"}, failingExecutor{})

	_, err := b.Submit("echo", "boom", "")
	require.NoError(t, err)

	err = w.RunOnce(context.Background())
	require.Error(t, err)
	// Should wrap both the report error and the exec error.
	assert.Contains(t, err.Error(), "report failed")
	assert.Contains(t, err.Error(), "boom")
}

func TestWorker_Run_ConnectEventsFailsThenCancel(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/tasks/events", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	})
	mux.HandleFunc("GET /api/v1/agent/profile", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte("{}"))
	})
	ts := httptest.NewServer(mux)
	defer ts.Close()

	client := NewClient(ts.URL, "tok")
	w := NewWorker(client, "w1", []string{"echo"}, EchoExecutor{}).
		WithReconnectBackoff(10 * time.Millisecond).
		WithPollInterval(1 * time.Hour)

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	err := w.Run(ctx)
	require.Error(t, err)
	assert.ErrorIs(t, err, context.DeadlineExceeded)
}

func TestWorker_Run_SSEStreamClosedReconnects(t *testing.T) {
	ts, b := newTestServer(t, 3)

	var eventsClosed atomic.Bool
	realHandler := ts.Config.Handler
	ts.Config.Handler = http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/tasks/events" {
			if eventsClosed.Load() {
				realHandler.ServeHTTP(rw, r)
				return
			}
			rw.Header().Set("Content-Type", "text/event-stream")
			rw.WriteHeader(http.StatusOK)
			if f, ok := rw.(http.Flusher); ok {
				f.Flush()
			}
			eventsClosed.Store(true)
			return
		}
		realHandler.ServeHTTP(rw, r)
	})

	client := NewClient(ts.URL, "tok")
	w := NewWorker(client, "w1", []string{"echo"}, EchoExecutor{}).
		WithReconnectBackoff(20 * time.Millisecond).
		WithPollInterval(100 * time.Millisecond)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	go w.Run(ctx)

	time.Sleep(300 * time.Millisecond)

	input := `{"msg":"after-reconnect"}`
	id, err := b.Submit("echo", input, "")
	require.NoError(t, err)

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		got, err := b.Get(id)
		require.NoError(t, err)
		if got.Status == "completed" {
			assert.Equal(t, input, got.Result)
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("task did not complete after SSE reconnect")
}

func TestWorker_Drain_ContextCancel(t *testing.T) {
	var claimCount atomic.Int64
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/agent/profile", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte("{}"))
	})
	mux.HandleFunc("POST /api/v1/tasks/claim", func(w http.ResponseWriter, r *http.Request) {
		claimCount.Add(1)
		http.Error(w, "transient error", http.StatusInternalServerError)
	})
	mux.HandleFunc("GET /api/v1/tasks/events", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		<-r.Context().Done()
	})
	ts := httptest.NewServer(mux)
	defer ts.Close()

	client := NewClient(ts.URL, "tok")
	w := NewWorker(client, "w1", []string{"echo"}, EchoExecutor{}).
		WithPollInterval(300 * time.Millisecond)

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	go w.Run(ctx)

	time.Sleep(600 * time.Millisecond)
	n := claimCount.Load()
	assert.Greater(t, n, int64(0), "expected at least one claim attempt")
	assert.LessOrEqual(t, n, int64(10), "drain should not tight-loop")
}

func TestConnectEvents_RetryThenCancel(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/tasks/events", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	})
	ts := httptest.NewServer(mux)
	defer ts.Close()

	client := NewClient(ts.URL, "tok")
	w := NewWorker(client, "w1", []string{"echo"}, EchoExecutor{}).
		WithReconnectBackoff(20 * time.Millisecond)

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	_, err := w.connectEvents(ctx)
	require.Error(t, err)
	assert.ErrorIs(t, err, context.DeadlineExceeded)
}

func TestGetProfile_Non200(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/agent/profile", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	})
	ts := httptest.NewServer(mux)
	defer ts.Close()

	client := NewClient(ts.URL, "tok")
	_, err := client.GetProfile(context.Background(), "w1")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unexpected status")
}

// TestClientRequestCreationErrors covers the http.NewRequestWithContext error
// paths by using a malformed base URL that cannot be parsed.
func TestClientRequestCreationErrors(t *testing.T) {
	client := NewClient("http://host with spaces", "tok")
	ctx := context.Background()

	_, err := client.GetProfile(ctx, "w1")
	require.Error(t, err)

	_, err = client.Claim(ctx, "w1", nil)
	require.Error(t, err)

	err = client.ReportResult(ctx, "t1", "w1", "failed", "nope")
	require.Error(t, err)

	err = client.Heartbeat(ctx, "t1")
	require.Error(t, err)

	_, err = client.Events(ctx)
	require.Error(t, err)
}

// TestConnectEvents_PreCancel covers the ctx.Err() check at the top of the
// connectEvents retry loop (line 362 in worker.go).
func TestConnectEvents_PreCancel(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/tasks/events", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	})
	ts := httptest.NewServer(mux)
	defer ts.Close()

	client := NewClient(ts.URL, "tok")
	w := NewWorker(client, "w1", []string{"echo"}, EchoExecutor{})

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := w.connectEvents(ctx)
	require.Error(t, err)
	assert.ErrorIs(t, err, context.Canceled)
}

// TestClientDialErrors covers the HTTP Do error paths for Claim, ReportResult,
// Heartbeat, and Events by using an unreachable address.
func TestClientDialErrors(t *testing.T) {
	client := NewClient("http://127.255.255.255:1", "tok")
	ctx := context.Background()

	_, err := client.Claim(ctx, "w1", nil)
	require.Error(t, err)

	err = client.ReportResult(ctx, "t1", "w1", "failed", "nope")
	require.Error(t, err)

	err = client.Heartbeat(ctx, "t1")
	require.Error(t, err)

	_, err = client.Events(ctx)
	require.Error(t, err)
}

// TestWorker_Run_SSEReconnectFails verifies that Run returns an error when the
// SSE stream closes and the reconnect attempt fails (line 290 in worker.go).
func TestWorker_Run_SSEReconnectFails(t *testing.T) {
	var callCount atomic.Int64
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/tasks/events", func(w http.ResponseWriter, r *http.Request) {
		n := callCount.Add(1)
		if n == 1 {
			// First call: succeed then close immediately so the events
			// channel closes and Run enters the reconnect branch.
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
			return
		}
		// Second (and all subsequent) calls: fail so connectEvents never
		// succeeds, and Run returns the connectEvents error.
		w.WriteHeader(http.StatusServiceUnavailable)
	})
	mux.HandleFunc("GET /api/v1/agent/profile", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte("{}"))
	})
	mux.HandleFunc("POST /api/v1/tasks/claim", func(w http.ResponseWriter, r *http.Request) {
		// Return NoContent so the initial drain exits immediately
		// and the main loop can reach the reconnect branch.
		w.WriteHeader(http.StatusNoContent)
	})
	ts := httptest.NewServer(mux)
	defer ts.Close()

	client := NewClient(ts.URL, "tok")
	w := NewWorker(client, "w1", []string{"echo"}, EchoExecutor{}).
		WithReconnectBackoff(20 * time.Millisecond).
		WithPollInterval(1 * time.Hour)

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	err := w.Run(ctx)
	require.Error(t, err) // Run should return an error after reconnect failure
}
