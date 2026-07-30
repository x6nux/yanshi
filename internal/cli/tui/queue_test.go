package tui

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/x6nux/yanshi/internal/cli"
)

// ---- C07 Task 2: enqueue ----

// TestModel_QueueEnqueue_QueueBuffers verifies that in queue mode a message
// submitted mid-turn is buffered (not sent), the input clears, the running turn
// is NOT canceled, and a transcript marker reflects the count.
func TestModel_QueueEnqueue_QueueBuffers(t *testing.T) {
	rec := &recordingSession{}
	m := newModel(rec, "/proj")
	m.streamCh = make(chan cli.StreamEvent) // simulate an in-flight turn
	m.queueMode = QueueModeQueue
	m.input.SetValue("follow-up")

	mm, _ := m.submit()
	m = mm.(model)

	assert.Empty(t, rec.sentText, "queue mode must not send mid-turn")
	assert.Equal(t, []string{"follow-up"}, m.msgQueue)
	assert.Equal(t, 0, rec.canceled, "queue mode must not cancel the running turn")
	assert.Equal(t, "", m.input.Value(), "input must clear after enqueuing")
	qp := m.queuePreview()
	assert.Contains(t, qp, "↳", "queue renders as a preview block above the input")
	assert.Contains(t, qp, "follow-up", "the preview lists the queued message")
}

// TestModel_QueueEnqueue_BatchBuffers is the same guard for batch mode (buffer,
// no cancel); the batch/queue difference is only at drain time.
func TestModel_QueueEnqueue_BatchBuffers(t *testing.T) {
	rec := &recordingSession{}
	m := newModel(rec, "/proj")
	m.streamCh = make(chan cli.StreamEvent)
	m.queueMode = QueueModeBatch

	m.input.SetValue("a")
	mm, _ := m.submit()
	m = mm.(model)
	m.input.SetValue("b")
	mm, _ = m.submit()
	m = mm.(model)

	assert.Equal(t, []string{"a", "b"}, m.msgQueue)
	assert.Equal(t, 0, rec.canceled)
	qp := m.queuePreview()
	assert.Contains(t, qp, "a")
	assert.Contains(t, qp, "b", "the preview lists every queued message")
}

// TestModel_QueueEnqueue_SingleCancels verifies single mode cancels the running
// turn at enqueue time (interrupt semantics) while still buffering the message.
func TestModel_QueueEnqueue_SingleCancels(t *testing.T) {
	rec := &recordingSession{}
	m := newModel(rec, "/proj")
	m.streamCh = make(chan cli.StreamEvent)
	m.queueMode = QueueModeSingle
	m.input.SetValue("interrupt")

	mm, _ := m.submit()
	m = mm.(model)

	assert.Equal(t, []string{"interrupt"}, m.msgQueue, "single still buffers (drain sends latest)")
	assert.Equal(t, 1, rec.canceled, "single must cancel the running turn")
	assert.True(t, m.canceling, "canceling flag dedupes a second cancel")
}

// TestModel_QueueEnqueue_SingleCancelIdempotent verifies a second mid-turn
// submit in single mode does not cancel twice (the first cancel is still in
// flight). Both messages buffer; drain (Task 3) keeps only the latest.
func TestModel_QueueEnqueue_SingleCancelIdempotent(t *testing.T) {
	rec := &recordingSession{}
	m := newModel(rec, "/proj")
	m.streamCh = make(chan cli.StreamEvent)
	m.queueMode = QueueModeSingle

	m.input.SetValue("a")
	mm, _ := m.submit()
	m = mm.(model)
	m.input.SetValue("b")
	mm, _ = m.submit()
	m = mm.(model)

	assert.Equal(t, []string{"a", "b"}, m.msgQueue)
	assert.Equal(t, 1, rec.canceled, "cancel must not be re-issued while already canceling")
}

// TestModel_QueueEnqueue_SlashCommandDropped verifies a slash command submitted
// mid-turn is still a no-op: control frames would clobber the in-flight
// streamCh via sendControlFrame, so C07 queues only plain messages.
func TestModel_QueueEnqueue_SlashCommandDropped(t *testing.T) {
	rec := &recordingSession{}
	m := newModel(rec, "/proj")
	m.streamCh = make(chan cli.StreamEvent)
	m.queueMode = QueueModeQueue
	m.input.SetValue("/model foo")

	mm, _ := m.submit()
	m = mm.(model)

	assert.Empty(t, rec.frames, "slash commands mid-turn must not send a control frame")
	assert.Empty(t, m.msgQueue, "slash commands must not enqueue")
}

// TestModel_QueueSubmit_IdlePathUntouched verifies the idle submit path
// (streamCh == nil) is byte-identical to pre-C07: plain text goes through Send
// as a user_message. Regression guard for the submit() restructure.
func TestModel_QueueSubmit_IdlePathUntouched(t *testing.T) {
	rec := &recordingSession{}
	m := newModel(rec, "/proj")
	m.input.SetValue("hello there")

	mm, _ := m.submit()
	m = mm.(model)

	require.Equal(t, []string{"hello there"}, rec.sentText)
	assert.Empty(t, m.msgQueue, "idle submit never queues")
}

// ---- C07 Task 3: drain ----

// TestModel_QueueDrain_QueueSendsHeadOnly verifies queue mode drains the FIFO
// head only; the tail stays queued for the next turn's done.
func TestModel_QueueDrain_QueueSendsHeadOnly(t *testing.T) {
	rec := &recordingSession{}
	m := newModel(rec, "/proj")
	m.msgQueue = []string{"first", "second"}
	m.queueMode = QueueModeQueue

	m, _ = m.drainQueue()

	require.Equal(t, []string{"first"}, rec.sentText)
	assert.Equal(t, []string{"second"}, m.msgQueue, "tail remains for the next drain")
}

// TestModel_QueueDrain_BatchMergesAll verifies batch mode joins every queued
// message (blank-line separated) into one turn.
func TestModel_QueueDrain_BatchMergesAll(t *testing.T) {
	rec := &recordingSession{}
	m := newModel(rec, "/proj")
	m.msgQueue = []string{"a", "b", "c"}
	m.queueMode = QueueModeBatch

	m, _ = m.drainQueue()

	require.Equal(t, []string{"a\n\nb\n\nc"}, rec.sentText)
	assert.Empty(t, m.msgQueue)
}

// TestModel_QueueDrain_SingleSendsLatest verifies single mode sends only the
// most recent queued message (interrupt semantics: earlier ones are discarded).
func TestModel_QueueDrain_SingleSendsLatest(t *testing.T) {
	rec := &recordingSession{}
	m := newModel(rec, "/proj")
	m.msgQueue = []string{"first", "second"}
	m.queueMode = QueueModeSingle

	m, _ = m.drainQueue()

	require.Equal(t, []string{"second"}, rec.sentText)
	assert.Empty(t, m.msgQueue)
}

// TestModel_QueueDrain_EmptyNoOp verifies a drain on an empty queue is a no-op
// (no send, nil Cmd).
func TestModel_QueueDrain_EmptyNoOp(t *testing.T) {
	rec := &recordingSession{}
	m := newModel(rec, "/proj")

	m, cmd := m.drainQueue()

	assert.Empty(t, rec.sentText)
	assert.Nil(t, cmd, "empty drain returns a nil Cmd (no turn started)")
}

// TestModel_QueueDrain_OnDoneViaUpdate verifies the drain is hooked into the
// done event through Update: a turn ending with a queued batch automatically
// sends the merged message, and the default waitForEvent arming is skipped.
func TestModel_QueueDrain_OnDoneViaUpdate(t *testing.T) {
	rec := &recordingSession{}
	m := newModel(rec, "/proj")
	m.streamCh = make(chan cli.StreamEvent) // in-flight; cleared by done
	m.msgQueue = []string{"x", "y"}
	m.queueMode = QueueModeBatch

	mm, _ := m.Update(streamMsg{ev: cli.StreamEvent{Kind: "done"}})
	m = mm.(model)

	require.Equal(t, []string{"x\n\ny"}, rec.sentText, "done must trigger the drain")
	assert.Empty(t, m.msgQueue)
}

// TestModel_QueueDrain_NoQueueDoesNotDrain verifies a normal done with an empty
// queue does not send anything (regression guard on the hook's guard clause).
func TestModel_QueueDrain_NoQueueDoesNotDrain(t *testing.T) {
	rec := &recordingSession{}
	m := newModel(rec, "/proj")
	m.streamCh = make(chan cli.StreamEvent)

	mm, _ := m.Update(streamMsg{ev: cli.StreamEvent{Kind: "done"}})
	m = mm.(model)

	assert.Empty(t, rec.sentText, "empty queue + done must not send")
}

// ---- C07 Task 4: footer queue segment ----

// TestModel_QueueFooter_SegmentVisibility verifies the footer queue segment
// appears when the mode is non-default OR the queue is non-empty, and is hidden
// in the default idle state (queue mode, empty).
func TestModel_QueueFooter_SegmentVisibility(t *testing.T) {
	m := newModel(&fakeSession{}, "/proj")
	// Default: queue mode, empty → hidden.
	assert.NotContains(t, m.statusHeader(), "queue:", "default idle state hides the segment")

	// Non-default mode, empty → shown so the user sees their active mode.
	m.queueMode = QueueModeSingle
	assert.Contains(t, m.statusHeader(), "queue:single")

	// Default mode, non-empty queue → shown with count.
	m.queueMode = QueueModeQueue
	m.msgQueue = []string{"a", "b"}
	hdr := m.statusHeader()
	assert.Contains(t, hdr, "queue:queue")
	assert.Contains(t, hdr, "·2")
}

// TestModel_QueueFooter_CountUpdates verifies the footer count tracks the queue
// as it grows and shrinks.
func TestModel_QueueFooter_CountUpdates(t *testing.T) {
	m := newModel(&fakeSession{}, "/proj")
	m.queueMode = QueueModeQueue
	m.msgQueue = []string{"a"}
	assert.Contains(t, m.statusHeader(), "·1")
	m.msgQueue = append(m.msgQueue, "b", "c")
	assert.Contains(t, m.statusHeader(), "·3")
	m.msgQueue = nil
	assert.NotContains(t, m.statusHeader(), "queue:", "drained queue hides the segment")
}

// ---- C07 Task 5: disconnect/recovery consistency ----

// TestModel_QueueSurvives_DisconnectDone verifies the queue drains when a turn
// ends because the stream closed (a disconnect makes waitForEvent return a
// synthetic "done"). The queue is in-memory model state, so it is consistent
// across reconnect (same TUI process) without explicit persistence.
func TestModel_QueueSurvives_DisconnectDone(t *testing.T) {
	rec := &recordingSession{}
	m := newModel(rec, "/proj")
	m.streamCh = make(chan cli.StreamEvent) // in-flight; a closed channel yields a synthetic done
	m.msgQueue = []string{"queued during disconnect"}
	m.queueMode = QueueModeQueue

	mm, _ := m.Update(streamMsg{ev: cli.StreamEvent{Kind: "done"}})
	m = mm.(model)

	require.Equal(t, []string{"queued during disconnect"}, rec.sentText,
		"queue must drain on the disconnect-induced done")
	assert.Empty(t, m.msgQueue)
}

// TestModel_QueueSurvives_FifoChainAcrossDisconnects verifies that in queue mode
// repeated done events (e.g. a flaky connection producing several synthetic
// dones) unwind the FIFO queue one message per turn, never dropping or reordering.
func TestModel_QueueSurvives_FifoChainAcrossDisconnects(t *testing.T) {
	rec := &recordingSession{}
	m := newModel(rec, "/proj")
	m.streamCh = make(chan cli.StreamEvent)
	m.msgQueue = []string{"one", "two", "three"}
	m.queueMode = QueueModeQueue

	for range []int{0, 1, 2} {
		m.streamCh = make(chan cli.StreamEvent) // each drained turn is "in flight" then done
		mm, _ := m.Update(streamMsg{ev: cli.StreamEvent{Kind: "done"}})
		m = mm.(model)
	}

	assert.Equal(t, []string{"one", "two", "three"}, rec.sentText, "FIFO order preserved across the chain")
	assert.Empty(t, m.msgQueue)
}
