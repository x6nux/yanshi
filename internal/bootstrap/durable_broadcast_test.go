package bootstrap_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/require"

	"github.com/x6nux/yanshi/internal/bootstrap"
	"github.com/x6nux/yanshi/internal/proto"
	"github.com/x6nux/yanshi/internal/store"
	"github.com/x6nux/yanshi/internal/task/work"
)

// TestDurableTaskTransitionsReachAConnectedClient is the last hop of the
// durable-task chain, and the one nothing covered.
//
// A2/DT1 was closed on "the state machine is correct", and it is: the mirror
// moves the row and a restart recovers it. But every transition happens on a
// broker WORKER goroutine, minutes after the turn that created the task
// returned, and the only way a frame reached a client was
// TurnOpts.EmitWorkFrame — a callback bound into a turn context that no longer
// exists by then. So the row went pending → running → completed and the TUI
// kept showing "pending" until the user thought to run task_read again. State
// that is correct and invisible is the same defect as state that is wrong,
// from where the user sits.
//
// Everything below is real: a real Build, its real HTTP handler, a real
// WebSocket client, and a real broker Claim on a worker's behalf. The mirror is
// the one under test, so nothing here constructs it.
func TestDurableTaskTransitionsReachAConnectedClient(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")
	cfgPath := filepath.Join(dir, "config.yaml")
	require.NoError(t, os.WriteFile(cfgPath, []byte(`
server:
  http_addr: "127.0.0.1:0"
storage:
  sqlite_path: "`+toYAMLPath(dbPath)+`"
token: "test-token"
`), 0o644))

	app, err := bootstrap.Build(bootstrap.Options{ConfigPath: cfgPath, FakeModel: true})
	require.NoError(t, err)
	t.Cleanup(func() { _ = app.Shutdown(context.Background()) })

	ts := httptest.NewServer(app.Server.Handler)
	t.Cleanup(ts.Close)

	c, _, err := websocket.DefaultDialer.Dial(
		"ws"+strings.TrimPrefix(ts.URL, "http")+"/api/v1/chat/ws",
		http.Header{"Authorization": []string{"Bearer test-token"}},
	)
	require.NoError(t, err)
	t.Cleanup(func() { _ = c.Close() })

	// A durable task, dispatched onto the broker's queue. It goes in through a
	// second Store over the same file for the same reason the recovery test
	// does it: nothing but the database crosses between the test and Build.
	second, err := store.Open(dbPath)
	require.NoError(t, err)
	t.Cleanup(func() { _ = second.Close() })
	ws2, err := work.FromDB(second.DB, second)
	require.NoError(t, err)
	mgr := work.NewManager(ws2, work.BrokerAdapter{Broker: app.Broker}, work.ArtifactPolicy{})
	wt, err := mgr.Create(context.Background(), work.CreateReq{
		Title: "build", Prompt: "run the build", Dispatch: true,
	})
	require.NoError(t, err)
	require.NotEmpty(t, wt.BrokerTaskID, "the task was not dispatched, so no worker can claim it")

	// A worker claims it. This is what the agent-worker CLI does, and it is
	// what fires the mirror.
	claimed, err := app.Broker.Claim("worker-1")
	require.NoError(t, err)
	require.NotNil(t, claimed, "the queue had nothing to claim")

	require.Equal(t, work.StatusRunning, awaitTaskUpdate(t, c, wt.ID),
		"the worker claimed the task and no client was ever told")

	require.NoError(t, app.Broker.RecordResult(claimed.ID, "worker-1", "completed", "build ok"))
	require.Equal(t, work.StatusCompleted, awaitTaskUpdate(t, c, wt.ID))
}

// awaitTaskUpdate reads frames until one is a task_update for taskID and
// returns its status. Other frames are skipped rather than failing: a live
// connection also carries status and heartbeat traffic, and a test that
// insisted on frame ORDER here would be asserting something this feature does
// not promise.
func awaitTaskUpdate(t *testing.T, c *websocket.Conn, taskID string) work.Status {
	t.Helper()
	require.NoError(t, c.SetReadDeadline(time.Now().Add(10*time.Second)))
	for {
		var f proto.ServerFrame
		if err := c.ReadJSON(&f); err != nil {
			t.Fatalf("no task_update for %s before the connection went quiet: %v", taskID, err)
		}
		if f.Type == "task_update" && f.Task != nil && f.Task.ID == taskID {
			return f.Task.Status
		}
	}
}
