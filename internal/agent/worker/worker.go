package worker

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/x6nux/yanshi/internal/guard"
	"github.com/x6nux/yanshi/internal/store"
)

// ErrNoTask is returned by RunOnce when no task was available to claim.
// Callers use errors.Is(err, ErrNoTask) to distinguish "no task" from
// "claimed+executed+reported".
var ErrNoTask = errors.New("worker: no task available")

// Client is an HTTP client for the Task API. It wraps the four endpoints
// the worker needs: profile, claim, result, and events (SSE).
type Client struct {
	BaseURL    string
	Token      string
	httpClient *http.Client
}

// NewClient creates a Client for the given server base URL and bearer token.
func NewClient(baseURL, token string) *Client {
	return &Client{
		BaseURL:    strings.TrimRight(baseURL, "/"),
		Token:      token,
		httpClient: &http.Client{Timeout: 30 * time.Second},
	}
}

// GetProfile fetches the permission profile for a worker name.
func (c *Client) GetProfile(ctx context.Context, name string) (guard.PermissionProfile, error) {
	var prof guard.PermissionProfile
	req, err := http.NewRequestWithContext(ctx, "GET", c.BaseURL+"/api/v1/agent/profile?worker="+name, nil)
	if err != nil {
		return prof, err
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return prof, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return prof, fmt.Errorf("get_profile: unexpected status %d", resp.StatusCode)
	}
	return prof, json.NewDecoder(resp.Body).Decode(&prof)
}

// Claim attempts to claim the next pending task. Returns (nil, nil) if no
// task is available (HTTP 204).
func (c *Client) Claim(ctx context.Context, name string, caps []string) (*store.Task, error) {
	body, _ := json.Marshal(map[string]any{"worker": name, "caps": caps})
	req, err := http.NewRequestWithContext(ctx, "POST", c.BaseURL+"/api/v1/tasks/claim", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNoContent {
		return nil, nil
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("claim: unexpected status %d", resp.StatusCode)
	}
	var t store.Task
	if err := json.NewDecoder(resp.Body).Decode(&t); err != nil {
		return nil, err
	}
	return &t, nil
}

// ReportResult posts the execution outcome for a task. The server-side
// broker decides whether to retry (status "failed" with attempts remaining)
// or record the result as final. The worker field identifies the caller;
// the broker verifies ownership before applying the result.
func (c *Client) ReportResult(ctx context.Context, id, worker, status, result string) error {
	body, _ := json.Marshal(map[string]any{"worker": worker, "status": status, "result": result})
	req, err := http.NewRequestWithContext(ctx, "POST", c.BaseURL+"/api/v1/tasks/"+id+"/result", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("report_result: unexpected status %d", resp.StatusCode)
	}
	return nil
}

// Heartbeat sends a heartbeat for a task, refreshing the server-side
// updated_at timestamp so the sweeper does not requeue the task mid-flight.
func (c *Client) Heartbeat(ctx context.Context, taskID string) error {
	req, err := http.NewRequestWithContext(ctx, "POST", c.BaseURL+"/api/v1/tasks/"+taskID+"/heartbeat", nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("heartbeat: unexpected status %d", resp.StatusCode)
	}
	return nil
}

// Events opens the SSE endpoint and returns a channel that receives a signal
// for each "task_available" event. The channel closes when ctx is cancelled
// or the stream ends.
func (c *Client) Events(ctx context.Context) (<-chan struct{}, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", c.BaseURL+"/api/v1/tasks/events", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)
	req.Header.Set("Accept", "text/event-stream")

	// Use a client without a timeout for the long-lived SSE stream.
	// The request context governs cancellation.
	streamingClient := &http.Client{}
	resp, err := streamingClient.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return nil, fmt.Errorf("events: unexpected status %d", resp.StatusCode)
	}

	ch := make(chan struct{})
	go func() {
		defer resp.Body.Close()
		defer close(ch)
		scanner := bufio.NewScanner(resp.Body)
		for scanner.Scan() {
			if scanner.Text() == "event: task_available" {
				select {
				case ch <- struct{}{}:
				case <-ctx.Done():
					return
				}
			}
		}
	}()
	return ch, nil
}

// Worker runs the claim-execute-report loop against a remote Task API.
type Worker struct {
	client            *Client
	name              string
	caps              []string
	exec              Executor
	pollInterval      time.Duration
	heartbeatInterval time.Duration
	reconnectBackoff  time.Duration
}

// NewWorker creates a Worker that talks to the Task API via client,
// identifies itself as name, advertises caps, and executes tasks with exec.
func NewWorker(client *Client, name string, caps []string, exec Executor) *Worker {
	return &Worker{
		client: client,
		name:   name,
		caps:   caps,
		exec:   exec,
	}
}

// WithPollInterval sets the periodic poll fallback interval. If not called,
// the default is 5 seconds. Useful for tests that need faster polling.
func (w *Worker) WithPollInterval(d time.Duration) *Worker {
	w.pollInterval = d
	return w
}

// WithHeartbeatInterval sets the interval at which the worker sends
// heartbeats during task execution. If not called, the default is 10 seconds.
func (w *Worker) WithHeartbeatInterval(d time.Duration) *Worker {
	w.heartbeatInterval = d
	return w
}

// WithReconnectBackoff sets the initial backoff for SSE reconnect retries.
// If not called, the default is 1 second. The backoff doubles on each failed
// attempt, capped at 30 seconds. Useful for tests that need faster retries.
func (w *Worker) WithReconnectBackoff(d time.Duration) *Worker {
	w.reconnectBackoff = d
	return w
}

// RunOnce claims a single task, executes it, and reports the result.
// Returns ErrNoTask if no task was available. If the executor returns an
// error, the worker reports status "failed" and returns the executor error.
// During execution, a heartbeat ticker goroutine periodically calls
// c.Heartbeat so the server-side sweeper does not requeue the task mid-flight.
func (w *Worker) RunOnce(ctx context.Context) error {
	t, err := w.client.Claim(ctx, w.name, w.caps)
	if err != nil {
		return fmt.Errorf("claim: %w", err)
	}
	if t == nil {
		return ErrNoTask
	}

	// Start heartbeat ticker to keep the task alive during long executions.
	hbInterval := w.heartbeatInterval
	if hbInterval <= 0 {
		hbInterval = 10 * time.Second
	}
	hbCtx, hbCancel := context.WithCancel(ctx)
	defer hbCancel()
	ticker := time.NewTicker(hbInterval)
	defer ticker.Stop()
	go func() {
		for {
			select {
			case <-hbCtx.Done():
				return
			case <-ticker.C:
				_ = w.client.Heartbeat(hbCtx, t.ID)
			}
		}
	}()

	result, execErr := w.exec.Execute(ctx, *t)
	if execErr != nil {
		if rerr := w.client.ReportResult(ctx, t.ID, w.name, "failed", execErr.Error()); rerr != nil {
			return fmt.Errorf("report failed: %w (exec error: %v)", rerr, execErr)
		}
		return execErr
	}
	return w.client.ReportResult(ctx, t.ID, w.name, "completed", result)
}

// Run starts the worker loop: fetch the profile once (logged/ignored for M5),
// then listen for SSE task_available signals. On each signal and on a periodic
// ticker, it drains the pending queue (loops RunOnce until ErrNoTask).
// Returns when ctx is cancelled.
func (w *Worker) Run(ctx context.Context) error {
	// Fetch profile once (M5: log and ignore errors — the worker can still
	// claim and execute without a profile).
	_, _ = w.client.GetProfile(ctx, w.name)

	events, err := w.connectEvents(ctx)
	if err != nil {
		return err
	}

	pollInterval := w.pollInterval
	if pollInterval <= 0 {
		pollInterval = 5 * time.Second
	}
	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()

	// Initial drain: claim any tasks already pending before the first signal.
	w.drain(ctx)

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case _, ok := <-events:
			if !ok {
				// SSE stream closed; reconnect with bounded exponential backoff.
				events, err = w.connectEvents(ctx)
				if err != nil {
					return err
				}
				continue
			}
			// Drain: claim-execute-report until no more tasks.
			w.drain(ctx)
		case <-ticker.C:
			// Periodic poll fallback for dropped SSE signals.
			w.drain(ctx)
		}
	}
}

// drain calls RunOnce repeatedly until no task is available (ErrNoTask) or
// the context is cancelled. On non-ErrNoTask errors it applies exponential
// backoff (100ms → 200ms → 400ms → …, capped at 2s, reset on success) to
// avoid tight-looping against a persistent claim or report failure. After
// maxDrainErrors (5) consecutive errors the drain yields, letting the
// outer Run loop's poll ticker retry later.
func (w *Worker) drain(ctx context.Context) {
	const (
		initialBackoff = 100 * time.Millisecond
		maxBackoff     = 2 * time.Second
		maxDrainErrors = 5
	)
	backoff := initialBackoff
	errCount := 0

	for {
		if err := ctx.Err(); err != nil {
			return
		}
		err := w.RunOnce(ctx)
		if errors.Is(err, ErrNoTask) {
			return
		}
		if err != nil {
			errCount++
			if errCount >= maxDrainErrors {
				return
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(backoff):
			}
			backoff *= 2
			if backoff > maxBackoff {
				backoff = maxBackoff
			}
		} else {
			// Success — reset backoff and error count.
			backoff = initialBackoff
			errCount = 0
		}
	}
}

// connectEvents opens the SSE stream with bounded exponential backoff retry.
// It keeps retrying until the stream connects or ctx is cancelled; the only
// error returned is ctx.Err(). This mirrors the drain method's backoff
// pattern so that transient server errors (e.g. during a restart) do not
// permanently kill the worker.
func (w *Worker) connectEvents(ctx context.Context) (<-chan struct{}, error) {
	const maxBackoff = 30 * time.Second

	backoff := w.reconnectBackoff
	if backoff <= 0 {
		backoff = 1 * time.Second
	}
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		ch, err := w.client.Events(ctx)
		if err == nil {
			return ch, nil
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(backoff):
		}
		if backoff < maxBackoff {
			backoff *= 2
		}
	}
}
