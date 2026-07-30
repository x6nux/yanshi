package cli

import (
	"context"
	nethttp "net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/cloudwego/eino/components/model"
	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/x6nux/yanshi/internal/agent/orchestrator"
	"github.com/x6nux/yanshi/internal/api/http"
	einollm "github.com/x6nux/yanshi/internal/llm/eino"
	"github.com/x6nux/yanshi/internal/proto"
)

func newWSServer(t *testing.T) string {
	t.Helper()
	o, _ := orchestrator.New(orchestrator.Config{Model: einollm.NewFakeModel([]string{"ws-reply"}, nil)})
	s := http.New(http.Config{Token: "t"})
	s.ChatWS(o, nil, nil)
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	return "ws" + strings.TrimPrefix(ts.URL, "http") + "/api/v1/chat/ws"
}

// TestWSBackend_MultiTurnMemory proves the persistent connection carries
// server-side history across Send calls: an Echo model's second reply contains
// the first user turn.
func TestWSBackend_MultiTurnMemory(t *testing.T) {
	fm := einollm.NewFakeModel(nil, nil)
	fm.Echo = true // echoes its full input -> proves history reached the model
	o, _ := orchestrator.New(orchestrator.Config{Model: fm})
	s := http.New(http.Config{Token: "t"})
	s.ChatWS(o, nil, nil)
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)

	b, err := newWSBackend(context.Background(), "ws"+strings.TrimPrefix(ts.URL, "http")+"/api/v1/chat/ws")
	require.NoError(t, err)
	defer b.Close()

	drain := func(text string) string {
		ch, err := b.Send(context.Background(), text)
		require.NoError(t, err)
		var out string
		for ev := range ch {
			if ev.Kind == "agent_chunk" {
				out += ev.Text
			}
		}
		return out
	}
	drain("remember OMEGA")
	second := drain("echo history")
	assert.Contains(t, second, "remember OMEGA", "second turn must see first turn (server-held history)")
}

func TestWSBackend_SendReceivesAgentChunk(t *testing.T) {
	b, err := newWSBackend(context.Background(), newWSServer(t))
	require.NoError(t, err)
	defer b.Close()
	assert.Equal(t, "ws", b.Mode())

	ch, err := b.Send(context.Background(), "hi")
	require.NoError(t, err)
	var got string
	var done bool
	for ev := range ch {
		if ev.Kind == "agent_chunk" {
			got += ev.Text
		}
		if ev.Kind == "done" {
			done = true
		}
	}
	assert.Equal(t, "ws-reply", got)
	assert.True(t, done)
}

// newWSServerWithModels builds a WS server with a non-empty model registry so
// list_models has names to return.
func newWSServerWithModels(t *testing.T) string {
	t.Helper()
	o, _ := orchestrator.New(orchestrator.Config{Model: einollm.NewFakeModel([]string{"ok"}, nil)})
	models := map[string]model.BaseChatModel{
		"alpha": einollm.NewFakeModel([]string{"alpha-reply"}, nil),
		"beta":  einollm.NewFakeModel([]string{"beta-reply"}, nil),
	}
	s := http.New(http.Config{Token: "t"})
	s.ChatWS(o, models, nil)
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	return "ws" + strings.TrimPrefix(ts.URL, "http") + "/api/v1/chat/ws"
}

// TestWSBackend_SendFrame_ListModels proves SendFrame on a control REQUEST sets
// up a reply channel: the server's models reply arrives and the channel closes
// (controlMode closes cur on the single-frame reply).
func TestWSBackend_SendFrame_ListModels(t *testing.T) {
	b, err := newWSBackend(context.Background(), newWSServerWithModels(t))
	require.NoError(t, err)
	defer b.Close()

	ch, err := b.SendFrame(context.Background(), proto.NewListModels())
	require.NoError(t, err)
	require.NotNil(t, ch)

	var ev StreamEvent
	var n int
	for e := range ch {
		ev = e
		n++
	}
	assert.Equal(t, 1, n, "exactly one reply frame then close")
	assert.Equal(t, "models", ev.Kind)
	assert.ElementsMatch(t, []string{"alpha", "beta"}, ev.Items)
}

// TestWSBackend_SendFrame_GetStatus proves a get_status control frame returns
// the status reply and closes.
func TestWSBackend_SendFrame_GetStatus(t *testing.T) {
	b, err := newWSBackend(context.Background(), newWSServerWithModels(t))
	require.NoError(t, err)
	defer b.Close()

	ch, err := b.SendFrame(context.Background(), proto.NewGetStatus())
	require.NoError(t, err)

	var ev StreamEvent
	for e := range ch {
		ev = e
	}
	assert.Equal(t, "status", ev.Kind)
}

// TestWSBackend_SendFrame_ListMCP proves the list_mcp frame returns an mcp_list
// reply (empty when no MCP is configured) and closes.
func TestWSBackend_SendFrame_ListMCP(t *testing.T) {
	b, err := newWSBackend(context.Background(), newWSServerWithModels(t))
	require.NoError(t, err)
	defer b.Close()

	ch, err := b.SendFrame(context.Background(), proto.NewListMCP())
	require.NoError(t, err)

		var ev StreamEvent
		for e := range ch {
			ev = e
		}
		assert.Equal(t, "mcp_status", ev.Kind)
		assert.Empty(t, ev.MCPServers, "mcp_status with no servers")
}

// TestWSBackend_SendFrame_ControlThenTurn proves the backend cleanly transitions
// out of control mode back into turn mode: after a control reply closes cur, a
// subsequent Send (user_message) still receives its full turn stream.
func TestWSBackend_SendFrame_ControlThenTurn(t *testing.T) {
	b, err := newWSBackend(context.Background(), newWSServer(t))
	require.NoError(t, err)
	defer b.Close()

	// Control frame first.
	ch, err := b.SendFrame(context.Background(), proto.NewGetStatus())
	require.NoError(t, err)
	for range ch {
	}

	// Then a regular turn — must still stream to completion (done).
	turnCh, err := b.Send(context.Background(), "hi")
	require.NoError(t, err)
	var sawDone bool
	for ev := range turnCh {
		if ev.Kind == "done" {
			sawDone = true
		}
	}
	assert.True(t, sawDone, "turn after a control frame must complete normally")
}

// TestWSBackend_SendFrame_PermissionResponseReturnsNil proves the mid-turn
// permission_response is written without setting up a reply channel (returns
// nil) so it cannot disturb an active turn's cur channel.
func TestWSBackend_SendFrame_PermissionResponseReturnsNil(t *testing.T) {
	b, err := newWSBackend(context.Background(), newWSServer(t))
	require.NoError(t, err)
	defer b.Close()

	ch, err := b.SendFrame(context.Background(), proto.NewPermissionResponse("req-1", "allow"))
	require.NoError(t, err)
	assert.Nil(t, ch, "permission_response carries no direct reply")
}

// TestSSEBackend_SendFrame_Unsupported proves SSE rejects control frames with
// ErrSSEControlUnsupported (SSE is stateless — no mid-stream control channel).
func TestSSEBackend_SendFrame_Unsupported(t *testing.T) {
	b := newSSEBackend(newSSEServer(t))
	defer b.Close()

	ch, err := b.SendFrame(context.Background(), proto.NewGetStatus())
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrSSEControlUnsupported)
	assert.Nil(t, ch)
}

// TestWSBackend_CancelDuringBurstyStream_NoPanic is the regression test for the
// send-on-closed-channel PANIC in readLoop. readLoop reads b.cur under curMu but
// sends on it WITHOUT the lock; the cancel watcher closes cur AFTER releasing
// curMu. So when a bursty stream fills the 16-frame buffer and readLoop blocks
// on cur<-ev, a concurrent Cancel()/Close() closes cur and the blocked send
// panics — crashing the TUI. safeSend recovers that send.
//
// The test drives the exact window: a raw WS server blasts 200 agent_chunk
// frames (far more than the 16-buffer) while the test does NOT drain ch, so
// readLoop fills the buffer and parks on the 17th cur<-ev; then Cancel()+Close()
// fire the watcher, which closes cur under the parked send. Pre-fix this panics
// out the whole test process; post-fix it shuts down cleanly. Loop is run
// internally and should also be run with -count=N to amplify.
func TestWSBackend_CancelDuringBurstyStream_NoPanic(t *testing.T) {
	const rounds = 10
	const burst = 200
	for round := 0; round < rounds; round++ {
		upgrader := websocket.Upgrader{
			CheckOrigin: func(r *nethttp.Request) bool { return true },
		}
		ts := httptest.NewServer(nethttp.HandlerFunc(func(w nethttp.ResponseWriter, r *nethttp.Request) {
			sc, err := upgrader.Upgrade(w, r, nil)
			if err != nil {
				return
			}
			defer sc.Close()
			// Blast far more than the 16-frame buffer. The single small
			// user_message the client writes fits in the TCP send buffer, so
			// the server need not read it. TCP ordering guarantees readLoop
			// receives these frames before the WS close from sc.Close(), so it
			// fills the buffer and parks on the 17th cur<-ev.
			for i := 0; i < burst; i++ {
				if err := sc.WriteJSON(proto.ServerFrame{Type: "agent_chunk", Text: "x"}); err != nil {
					return
				}
			}
		}))
		url := "ws" + strings.TrimPrefix(ts.URL, "http") + "/api/v1/chat/ws"

		b, err := newWSBackend(context.Background(), url)
		require.NoErrorf(t, err, "round %d: dial", round)

		// Start a turn but do NOT drain ch: readLoop fills the 16-buffer then
		// blocks on the 17th send to cur.
		ch, err := b.Send(context.Background(), "hi")
		require.NoErrorf(t, err, "round %d: send", round)

		// Let readLoop reach the blocked send (burst small frames process in a
		// few ms; the 17th parks immediately after).
		time.Sleep(80 * time.Millisecond)

		// Cancel fires the watcher, which closes cur while readLoop is blocked
		// on cur<-ev. Pre-fix this is a send-on-closed-channel PANIC that
		// crashes the test process; safeSend recovers it.
		require.NoErrorf(t, b.Cancel(), "round %d: cancel", round)
		require.NoErrorf(t, b.Close(), "round %d: close", round)
		ts.Close()

		// Drain whatever was buffered before the close; the channel must close
		// cleanly, not panic. Frames after the close may be dropped by safeSend
		// — that is fine, the invariant under test is "no panic".
		drained := 0
		for range ch {
			drained++
		}
		assert.Greaterf(t, drained, 0, "round %d: buffered frames should arrive before close", round)
	}
}

func TestToStreamEvent_SessionAck(t *testing.T) {
	ev := toStreamEvent(proto.NewSessionAck("renamed", "s1", "new title"))
	assert.Equal(t, "session_ack", ev.Kind)
	assert.Equal(t, "renamed", ev.Action)
	assert.Equal(t, "s1", ev.SessionID)
	assert.Equal(t, "new title", ev.Text)
}

func TestIsControlReply_SessionAck(t *testing.T) {
	assert.True(t, isControlReply("session_ack"),
		"session_ack must close the control-mode reply channel (single-frame reply)")
	// Regression: existing control replies stay recognized.
	assert.True(t, isControlReply("sessions"))
	assert.True(t, isControlReply("status"))
	assert.False(t, isControlReply("agent_chunk"))
}

// TestPermissionControlRepliesMapThroughCLI (Task 9): the new permission
// control-reply frame kinds (permissions, permission_rule_hit) must close the
// control-mode channel and map to StreamEvent with Kind/Permissions populated.
func TestPermissionControlRepliesMapThroughCLI(t *testing.T) {
	f := proto.NewPermissions([]proto.PermissionInfo{{ID: "r1", Action: "shell_run"}})
	ev := toStreamEvent(f)
	if ev.Kind != "permissions" || len(ev.Permissions) != 1 || ev.Permissions[0].ID != "r1" {
		t.Fatalf("event=%#v", ev)
	}
	if !isControlReply("permissions") || !isControlReply("permission_rule_hit") || !isControlReply("session_ack") {
		t.Fatal("permission replies and existing session_ack must close control channel")
	}
}

// TestJobsControlRepliesMapThroughCLI (Task 22): the new jobs control-reply
// frame kinds (jobs, job_event) must close the control-mode channel and map
// to StreamEvent with Kind/Jobs populated.
func TestJobsControlRepliesMapThroughCLI(t *testing.T) {
	f := proto.NewJobs(proto.Jobs{proto.JobInfo{ID: "j-1", State: "running"}})
	ev := toStreamEvent(f)
	if ev.Kind != "jobs" || len(ev.Jobs) != 1 || ev.Jobs[0].ID != "j-1" {
		t.Fatalf("event=%#v", ev)
	}
	if !isControlReply("jobs") || !isControlReply("job_event") {
		t.Fatal("jobs replies must close control channel")
	}
}
