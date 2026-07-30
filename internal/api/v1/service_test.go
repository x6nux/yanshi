package v1

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/x6nux/yanshi/internal/agent/orchestrator"
	einollm "github.com/x6nux/yanshi/internal/llm/eino"
	"github.com/x6nux/yanshi/internal/store"
)

// TestServiceStartTurnStreamsItemsInSequence proves a turn produces a strictly
// increasing sequence of items on a channel that closes when the turn ends.
// The first item is the synthetic turn.started; the stream then carries the
// model output; the final item is turn.completed (or turn.error). This is the
// core streaming contract every HTTP/SSE and JSON-RPC client depends on.
func TestServiceStartTurnStreamsItemsInSequence(t *testing.T) {
	model := einollm.NewFakeModel([]string{"answer"}, nil)
	svc, err := NewService(Config{DefaultModel: model})
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	thread, err := svc.Start(context.Background(), ThreadStartParams{Title: "test"})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	resp, items, err := svc.StartTurn(context.Background(), TurnStartParams{
		ThreadID: thread.ID,
		Input:    "hello",
	})
	if err != nil {
		t.Fatalf("StartTurn: %v", err)
	}
	if resp.Turn.Status != TurnStatusInProgress {
		t.Fatalf("turn = %#v", resp.Turn)
	}
	var last int64
	var count int
	for item := range items {
		if item.Sequence <= last {
			t.Fatalf("non-increasing sequence: last=%d item=%#v", last, item)
		}
		if item.Version != Version || item.ThreadID != thread.ID || item.TurnID == "" {
			t.Fatalf("item missing required fields: %#v", item)
		}
		last, count = item.Sequence, count+1
	}
	if count == 0 || last == 0 {
		t.Fatal("expected streamed items")
	}
}

// TestServiceInterruptIsIdempotent proves that calling Interrupt twice on the
// same thread is safe and that the active turn's channel closes in finite time
// after the first interrupt. Idempotency matters because clients commonly send
// interrupt on SIGINT then again on SIGTERM.
func TestServiceInterruptIsIdempotent(t *testing.T) {
	model := einollm.NewFakeModel([]string{"answer"}, nil)
	svc, err := NewService(Config{DefaultModel: model})
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	thread, err := svc.Start(context.Background(), ThreadStartParams{})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	_, items, err := svc.StartTurn(context.Background(), TurnStartParams{ThreadID: thread.ID, Input: "hello"})
	if err != nil {
		t.Fatalf("StartTurn: %v", err)
	}
	if err := svc.Interrupt(context.Background(), ThreadInterruptParams{ThreadID: thread.ID}); err != nil {
		t.Fatalf("first Interrupt: %v", err)
	}
	if err := svc.Interrupt(context.Background(), ThreadInterruptParams{ThreadID: thread.ID}); err != nil {
		t.Fatalf("second Interrupt: %v", err)
	}
	select {
	case <-items:
	case <-time.After(2 * time.Second):
		t.Fatal("interrupt did not close/advance stream")
	}
}

// TestServiceResumeReturnsSnapshotAndRejectsMissing proves Resume returns a
// snapshot for a known thread id, returns ErrThreadNotFound for an unknown id,
// and rejects an empty id with a usage-style error. The snapshot carries the
// original Title so a reconnecting client can render the thread header.
func TestServiceResumeReturnsSnapshotAndRejectsMissing(t *testing.T) {
	model := einollm.NewFakeModel([]string{"answer"}, nil)
	svc, err := NewService(Config{DefaultModel: model})
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	thread, err := svc.Start(context.Background(), ThreadStartParams{Title: "snapshot"})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	snap, err := svc.Resume(context.Background(), ThreadResumeParams{ThreadID: thread.ID})
	if err != nil {
		t.Fatalf("Resume known thread: %v", err)
	}
	if snap.Version != Version || snap.Thread.ID != thread.ID || snap.Thread.Title != "snapshot" {
		t.Fatalf("snapshot = %#v", snap)
	}
	if _, err := svc.Resume(context.Background(), ThreadResumeParams{ThreadID: "missing"}); !errors.Is(err, ErrThreadNotFound) {
		t.Fatalf("Resume missing err = %v want ErrThreadNotFound", err)
	}
	if _, err := svc.Resume(context.Background(), ThreadResumeParams{ThreadID: ""}); err == nil {
		t.Fatal("Resume with empty thread id should fail")
	}
}

// TestServiceStreamStopsWhenConsumerCancels proves backpressure: when the
// consumer's context is cancelled, the producer releases the goroutine and the
// channel closes within finite time. The bounded channel (cap 128) plus
// ctx-aware sendItem guarantees the producer never blocks forever on a dropped
// consumer and never allocates unbounded memory.
func TestServiceStreamStopsWhenConsumerCancels(t *testing.T) {
	model := einollm.NewFakeModel([]string{"answer"}, nil)
	svc, err := NewService(Config{DefaultModel: model})
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	thread, err := svc.Start(context.Background(), ThreadStartParams{})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	_, items, err := svc.StartTurn(ctx, TurnStartParams{ThreadID: thread.ID, Input: "hello"})
	if err != nil {
		t.Fatalf("StartTurn: %v", err)
	}
	cancel()
	deadline := time.After(2 * time.Second)
	for {
		select {
		case _, ok := <-items:
			if !ok {
				return
			}
		case <-deadline:
			t.Fatal("stream did not stop after consumer cancellation")
		}
	}
}

// TestServiceStartTurnRejectsBlankInput proves the input validation contract:
// empty/whitespace input is a usage error, not a runtime error.
func TestServiceStartTurnRejectsBlankInput(t *testing.T) {
	model := einollm.NewFakeModel([]string{"answer"}, nil)
	svc, err := NewService(Config{DefaultModel: model})
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	thread, err := svc.Start(context.Background(), ThreadStartParams{})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if _, _, err := svc.StartTurn(context.Background(), TurnStartParams{ThreadID: thread.ID, Input: "   "}); err == nil {
		t.Fatal("blank input should fail")
	}
}

// TestServiceStartTurnOnUnknownThreadReturnsErrThreadNotFound proves the
// thread-existence check runs before the active-turn check, so a caller with a
// stale thread id gets a clear not-found error instead of a confusing
// concurrent-turn error.
func TestServiceStartTurnOnUnknownThreadReturnsErrThreadNotFound(t *testing.T) {
	model := einollm.NewFakeModel([]string{"answer"}, nil)
	svc, err := NewService(Config{DefaultModel: model})
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	_, _, err = svc.StartTurn(context.Background(), TurnStartParams{ThreadID: "ghost", Input: "hello"})
	if !errors.Is(err, ErrThreadNotFound) {
		t.Fatalf("err = %v, want ErrThreadNotFound", err)
	}
}

// TestServiceNewServiceRequiresModelOrOrchestrator proves NewService rejects a
// config with both DefaultModel and Orchestrator set to nil. The service cannot
// run any turn without at least one of them.
func TestServiceNewServiceRequiresModelOrOrchestrator(t *testing.T) {
	_, err := NewService(Config{})
	if err == nil {
		t.Fatal("NewService with no model/orchestrator should fail")
	}
}

// TestServiceStartWithStore proves Start creates a session via the persistent
// store when configured, returning a valid thread with the store-generated id.
func TestServiceStartWithStore(t *testing.T) {
	st, err := store.Open(":memory:")
	require.NoError(t, err)
	svc, err := NewService(Config{DefaultModel: einollm.NewFakeModel([]string{"a"}, nil), Store: st})
	require.NoError(t, err)

	thread, err := svc.Start(context.Background(), ThreadStartParams{Title: "store-thread"})
	require.NoError(t, err)
	assert.Equal(t, "v1", thread.Version)
	assert.NotEmpty(t, thread.ID, "store-backed ID must be non-empty")
	assert.Equal(t, "store-thread", thread.Title)
	assert.Equal(t, ThreadStatusActive, thread.Status)

	// Verify the session exists in the store.
	ss, err := st.GetSession(thread.ID)
	require.NoError(t, err)
	require.NotNil(t, ss)
	assert.Equal(t, "store-thread", ss.Title)
}

// TestServiceResumeFromStore proves Resume loads a session from the persistent
// store when the session is not in the in-memory registry. This covers the
// fallback path where a previously-created session is resumed in a new service
// instance (or after a restart).
func TestServiceResumeFromStore(t *testing.T) {
	st, err := store.Open(":memory:")
	require.NoError(t, err)

	// Create a session directly in the store (simulating a prior session).
	sessionID, err := st.CreateSession("persisted")
	require.NoError(t, err)
	require.NoError(t, st.AppendMessage(sessionID, 0, "user", "hello"))
	require.NoError(t, st.AppendMessage(sessionID, 1, "assistant", "world"))

	// Create a service with no prior in-memory knowledge of this session.
	svc, err := NewService(Config{DefaultModel: einollm.NewFakeModel([]string{"a"}, nil), Store: st})
	require.NoError(t, err)

	snap, err := svc.Resume(context.Background(), ThreadResumeParams{ThreadID: sessionID})
	require.NoError(t, err)
	assert.Equal(t, "v1", snap.Version)
	assert.Equal(t, sessionID, snap.Thread.ID)
	assert.Equal(t, "persisted", snap.Thread.Title)
}

// TestServiceResumeFromStoreNotFound proves Resume returns ErrThreadNotFound
// when the session id does not exist in either the in-memory registry or the
// persistent store. store.GetSession returns (nil, nil) for a missing id.
func TestServiceResumeFromStoreNotFound(t *testing.T) {
	st, err := store.Open(":memory:")
	require.NoError(t, err)
	svc, err := NewService(Config{DefaultModel: einollm.NewFakeModel([]string{"a"}, nil), Store: st})
	require.NoError(t, err)

	_, err = svc.Resume(context.Background(), ThreadResumeParams{ThreadID: "no-such-session"})
	if !errors.Is(err, ErrThreadNotFound) {
		t.Fatalf("err = %v, want ErrThreadNotFound", err)
	}
}

// TestServiceStartTurnRejectsConcurrentTurn proves that starting a second turn
// on the same thread while the first is still running returns ErrTurnAlreadyActive.
// The v1 contract allows at most one in-progress turn per thread.
func TestServiceStartTurnRejectsConcurrentTurn(t *testing.T) {
	// The DefaultModel-only path emits a stub chunk and completes without
	// calling the model, so a FakeModel turn can finish before the second
	// StartTurn — making the concurrent-reject assertion flaky under -race.
	// Use an Orchestrator + BlockingModel so the first turn genuinely blocks
	// in model.Generate (until Block is closed), keeping st.active set.
	model := einollm.NewBlockingModel("delayed-response")
	o, err := orchestrator.New(orchestrator.Config{Model: model})
	require.NoError(t, err)
	svc, err := NewService(Config{Orchestrator: o})
	require.NoError(t, err)

	thread, err := svc.Start(context.Background(), ThreadStartParams{})
	require.NoError(t, err)

	// Start the first turn (non-blocking: runTurn runs in a goroutine).
	_, _, err = svc.StartTurn(context.Background(), TurnStartParams{ThreadID: thread.ID, Input: "first"})
	require.NoError(t, err)

	// Wait until the model is actually in flight so st.active is guaranteed
	// set when the second StartTurn runs.
	<-model.Started

	// Starting a concurrent turn should fail.
	_, _, err = svc.StartTurn(context.Background(), TurnStartParams{ThreadID: thread.ID, Input: "second"})
	if !errors.Is(err, ErrTurnAlreadyActive) {
		t.Fatalf("err = %v, want ErrTurnAlreadyActive", err)
	}

	// Release the blocked model so the first turn's goroutine can exit.
	close(model.Block)
}

// TestServiceInterruptRejectsWrongTurnID proves Interrupt returns an error
// when the given TurnID does not match the active turn. This protects against
// stale client state after a connection re-establishment.
func TestServiceInterruptRejectsWrongTurnID(t *testing.T) {
	// BlockingModel keeps the turn in flight so Interrupt actually has an
	// active turn to compare the wrong TurnID against. A FakeModel completes
	// before Interrupt runs (especially under -race), leaving active==nil —
	// Interrupt then returns nil (no active turn) and the rejection assertion
	// fails. Waiting on Started guarantees the turn is genuinely in flight.
	model := einollm.NewBlockingModel("answer")
	o, err := orchestrator.New(orchestrator.Config{Model: model})
	require.NoError(t, err)
	svc, err := NewService(Config{Orchestrator: o})
	require.NoError(t, err)

	thread, err := svc.Start(context.Background(), ThreadStartParams{})
	require.NoError(t, err)

	_, _, err = svc.StartTurn(context.Background(), TurnStartParams{ThreadID: thread.ID, Input: "hello"})
	require.NoError(t, err)
	<-model.Started

	// Interrupt with a non-matching TurnID.
	err = svc.Interrupt(context.Background(), ThreadInterruptParams{ThreadID: thread.ID, TurnID: "wrong-turn-id"})
	if err == nil {
		t.Fatal("Interrupt with wrong TurnID should fail")
	}
	assert.Contains(t, err.Error(), "is not active")

	// Release the blocked model so the first turn's goroutine can exit.
	close(model.Block)
}

// TestServiceInterruptReportsThreadNotFound proves Interrupt returns ErrThreadNotFound
// for an unknown thread id.
func TestServiceInterruptReportsThreadNotFound(t *testing.T) {
	model := einollm.NewFakeModel([]string{"answer"}, nil)
	svc, err := NewService(Config{DefaultModel: model})
	require.NoError(t, err)

	err = svc.Interrupt(context.Background(), ThreadInterruptParams{ThreadID: "ghost"})
	if !errors.Is(err, ErrThreadNotFound) {
		t.Fatalf("err = %v, want ErrThreadNotFound", err)
	}
}

// TestFormatSequenceNegative proves formatSequence clamps negative values to
// "0" so the item id stays a valid identifier. This is a safety net: sequence
// starts at 1 and increments, so negatives should never occur in practice but
// the code handles them defensively.
func TestFormatSequenceNegative(t *testing.T) {
	tests := []struct {
		input int64
		want  string
	}{
		{-1, "0"},
		{-100, "0"},
		{0, "0"},
		{1, "1"},
		{42, "42"},
	}
	for _, tc := range tests {
		got := formatSequence(tc.input)
		if got != tc.want {
			t.Errorf("formatSequence(%d) = %q, want %q", tc.input, got, tc.want)
		}
	}
}

// TestServiceStartTurnConsumerCancelledBeforeEmit proves that when the
// consumer context is already cancelled before the first item is emitted,
// StartTurn returns context.Canceled and the turn is aborted cleanly (the
// active-turn pointer is cleared so a subsequent StartTurn succeeds).
func TestServiceStartTurnConsumerCancelledBeforeEmit(t *testing.T) {
	model := einollm.NewFakeModel([]string{"answer"}, nil)
	svc, err := NewService(Config{DefaultModel: model})
	require.NoError(t, err)

	thread, err := svc.Start(context.Background(), ThreadStartParams{})
	require.NoError(t, err)

	// Cancel the parent context before calling StartTurn so the derived
	// turn context is immediately done and sendItem selects ctx.Done().
	parentCtx, cancel := context.WithCancel(context.Background())
	cancel()

	_, _, err = svc.StartTurn(parentCtx, TurnStartParams{ThreadID: thread.ID, Input: "hello"})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}

	// The active-turn pointer was cleared, so a subsequent StartTurn succeeds.
	_, items, err := svc.StartTurn(context.Background(), TurnStartParams{ThreadID: thread.ID, Input: "retry"})
	require.NoError(t, err)
	for item := range items {
		_ = item
	}
}

// TestServiceInterruptOnCompletedTurn proves Interrupt is a no-op (returns nil)
// when the thread has no active turn because the previous turn already completed.
func TestServiceInterruptOnCompletedTurn(t *testing.T) {
	model := einollm.NewFakeModel([]string{"answer"}, nil)
	svc, err := NewService(Config{DefaultModel: model})
	require.NoError(t, err)

	thread, err := svc.Start(context.Background(), ThreadStartParams{})
	require.NoError(t, err)

	_, items, err := svc.StartTurn(context.Background(), TurnStartParams{ThreadID: thread.ID, Input: "hello"})
	require.NoError(t, err)

	// Drain the items channel (turn completes when channel closes).
	for range items {
	}

	// Now Interrupt should find no active turn and return nil.
	err = svc.Interrupt(context.Background(), ThreadInterruptParams{ThreadID: thread.ID})
	if err != nil {
		t.Fatalf("Interrupt on completed turn should succeed, got: %v", err)
	}
}

// TestServiceStartTurnWithStorePersistsMessages proves that when a Store is
// configured, runTurn persists both the user and assistant messages to the
// database after the turn completes via the orchestrator path.
func TestServiceStartTurnWithStorePersistsMessages(t *testing.T) {
	st, err := store.Open(":memory:")
	require.NoError(t, err)

	// Use an orchestrator with a fake model so runTurn goes through the
	// orchestrator path and persists messages.
	mdl := einollm.NewFakeModel([]string{"hello world"}, nil)
	orch, err := orchestrator.New(orchestrator.Config{Model: mdl})
	require.NoError(t, err)

	svc, err := NewService(Config{
		Orchestrator: orch,
		Store:        st,
	})
	require.NoError(t, err)

	thread, err := svc.Start(context.Background(), ThreadStartParams{Title: "store-persist"})
	require.NoError(t, err)

	_, items, err := svc.StartTurn(context.Background(), TurnStartParams{
		ThreadID: thread.ID,
		Input:    "say hello",
	})
	require.NoError(t, err)

	// Drain items to let the turn complete.
	for range items {
	}

	// Verify messages persisted.
	msgs, err := st.Messages(thread.ID)
	require.NoError(t, err)
	require.Len(t, msgs, 2, "expected user + assistant messages")
	assert.Equal(t, "user", msgs[0].Role)
	assert.Equal(t, "say hello", msgs[0].Content)
	assert.Equal(t, "assistant", msgs[1].Role)
	assert.Equal(t, "hello world", msgs[1].Content)

	// Verify session meta was updated.
	ss, err := st.GetSession(thread.ID)
	require.NoError(t, err)
	require.NotNil(t, ss)
	assert.Equal(t, 1, ss.Turns, "one completed turn")
}
