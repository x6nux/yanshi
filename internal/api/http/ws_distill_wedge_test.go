package http

import (
	"context"
	"testing"
	"time"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
	"github.com/stretchr/testify/require"

	"github.com/x6nux/yanshi/internal/agent/orchestrator"
	einollm "github.com/x6nux/yanshi/internal/llm/eino"
	"github.com/x6nux/yanshi/internal/proto"
	"github.com/x6nux/yanshi/internal/store"

	"net/http/httptest"
)

// stallingDistillModel is a provider that accepts the call and then says
// nothing — the shape W-A-06 exists for, reached here through Generate, which
// ResilientChatModel's stall watchdog does not wrap.
//
// It records the deadline of the context it was handed and blocks until that
// context is done, so a test can assert BOTH halves of the fix: the call is
// bounded, and it is bound to something that a disconnect cancels.
type stallingDistillModel struct {
	entered     chan struct{}
	returned    chan struct{}
	hadDeadline chan bool
}

func newStallingDistillModel() *stallingDistillModel {
	return &stallingDistillModel{
		entered:     make(chan struct{}, 1),
		returned:    make(chan struct{}),
		hadDeadline: make(chan bool, 1),
	}
}

func (m *stallingDistillModel) Generate(
	ctx context.Context, _ []*schema.Message, _ ...model.Option,
) (*schema.Message, error) {
	_, ok := ctx.Deadline()
	select {
	case m.hadDeadline <- ok:
	default:
	}
	select {
	case m.entered <- struct{}{}:
	default:
	}
	<-ctx.Done()
	close(m.returned)
	return nil, ctx.Err()
}

// TestDistillDoesNotWedgeTheControlChannel is the regression test for a
// defect W-A-05 introduced and the per-task review could not see: the
// interactive distillation pass ran synchronously inside the WebSocket frame
// loop under context.Background().
//
// Two consequences, both proved below. context.Background() cannot be
// cancelled by anything, so closing the client did NOT release the handler —
// the frame loop's own `case <-connCtx.Done()` is unreachable while the loop
// is blocked inside the handler, which means one stalled provider call
// wedges the ENTIRE control channel (list_models, /model, /compact, cancel,
// permission replies) for the life of the process. And with no deadline
// there was no upper bound on that either.
//
// The assertions are deliberately on the two mechanisms rather than on wall
// time: "a later frame eventually arrived" would also pass if the handler
// merely happened to be fast, whereas "the model saw a deadline" and "closing
// the client released the call" both go red the moment either half is
// removed.
func TestDistillDoesNotWedgeTheControlChannel(t *testing.T) {
	st, err := store.Open(":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { st.Close() })
	for i := 0; i < 6; i++ {
		_, err := st.WriteMemoryScoped("note", "candidate memory", store.MemoryFilter{})
		require.NoError(t, err)
	}

	stall := newStallingDistillModel()
	o, err := orchestrator.New(orchestrator.Config{Model: einollm.NewFakeModel([]string{"hi"}, nil)})
	require.NoError(t, err)
	s := New(Config{Token: "t", Store: st, DistillModel: stall})
	s.ChatWS(o, nil, nil)
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	c := dial(t, dialWSURL(t, ts))
	require.NoError(t, c.WriteJSON(proto.NewDistillMemories()))

	select {
	case <-stall.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the distillation pass never reached the model")
	}

	require.True(t, <-stall.hadDeadline,
		"the distillation call has no deadline: a provider that connects and "+
			"then stalls holds the WS frame loop for the life of the process")

	// The disconnect half. Nothing else in this test cancels the call, so if
	// it returns it is because the connection context reached it.
	require.NoError(t, c.Close())
	select {
	case <-stall.returned:
	case <-time.After(5 * time.Second):
		t.Fatal("closing the client did not release the in-flight distillation; " +
			"the frame loop stays wedged and its own connCtx.Done() case is " +
			"unreachable while the handler blocks")
	}
}

// TestPostTurnDistillIsBounded covers the detached twin at the end of
// runUserTurn. It must NOT be released by a disconnect (context.WithoutCancel
// is correct there: the pass has to survive turn teardown), so a deadline is
// the only thing standing between it and one leaked goroutine plus one
// in-flight provider request per turn.
func TestPostTurnDistillIsBounded(t *testing.T) {
	restore := distillTimeout
	distillTimeout = 150 * time.Millisecond
	t.Cleanup(func() { distillTimeout = restore })

	st, err := store.Open(":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { st.Close() })

	sid, err := st.CreateSession("wedge")
	require.NoError(t, err)
	for i := 0; i < 6; i++ {
		_, err := st.WriteMemoryScoped("note", "candidate memory", store.MemoryFilter{SessionID: sid})
		require.NoError(t, err)
	}

	stall := newStallingDistillModel()
	reg := newDistillTestRegistry(t)
	require.NoError(t, reg.Set("memory_distill_after_turn", true))

	o, err := orchestrator.New(orchestrator.Config{Model: einollm.NewFakeModel([]string{"hi"}, nil)})
	require.NoError(t, err)
	s := New(Config{Token: "t", Store: st, DistillModel: stall, FeaturesReg: reg})
	s.ChatWS(o, nil, nil)
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	c := dial(t, dialWSURL(t, ts))
	defer c.Close()

	// GB1: restore_session first so the server's cs.sessionID matches the
	// scope the candidate memories were written under; otherwise the pass
	// short-circuits below MinDistillBatch and never reaches the model.
	require.NoError(t, c.WriteJSON(proto.NewRestoreSession(sid)))
	require.Equal(t, "session_restored", readFrame(t, c).Type)

	require.NoError(t, c.WriteJSON(proto.NewUserMessage("hi")))
	turnDone := false
	for i := 0; i < 100; i++ {
		if readFrame(t, c).Type == "done" {
			turnDone = true
			break
		}
	}
	require.True(t, turnDone, "the turn never finished")

	select {
	case <-stall.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the post-turn distillation pass never reached the model")
	}
	select {
	case <-stall.returned:
	case <-time.After(5 * time.Second):
		t.Fatal("the detached post-turn distillation never returned: with no " +
			"deadline it leaks a goroutine and an in-flight request per turn")
	}
}
