package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/x6nux/yanshi/internal/proto"
)

// fakeExecBackend is a deterministic ChatBackend for driver unit tests. Send
// streams a scripted turn (agent_chunk + tool_call + tool_result + status +
// done); SendFrame("restore_session") replies with session_restored. It records
// the Type (and payload) of every call so tests can assert ordering.
type fakeExecBackend struct {
	mode     string
	sendText string
	statusID string // SessionID attached to the status event on Send

	mu     sync.Mutex
	frames []string // recorded "<type>:<payload>" for each Send/SendFrame
}

func (f *fakeExecBackend) Send(_ context.Context, text string) (<-chan StreamEvent, error) {
	f.mu.Lock()
	f.frames = append(f.frames, "user_message:"+text)
	f.mu.Unlock()
	ch := make(chan StreamEvent, 8)
	ch <- StreamEvent{Kind: "agent_chunk", Text: f.sendText}
	ch <- StreamEvent{Kind: "tool_call", ToolName: "fs_read", ToolArgs: "{}"}
	ch <- StreamEvent{Kind: "tool_result", ToolName: "fs_read", ToolStatus: "ok"}
	ch <- StreamEvent{Kind: "status", SessionID: f.statusID}
	ch <- StreamEvent{Kind: "done"}
	close(ch)
	return ch, nil
}

func (f *fakeExecBackend) SendFrame(_ context.Context, fr proto.ClientFrame) (<-chan StreamEvent, error) {
	f.mu.Lock()
	f.frames = append(f.frames, fr.Type+":"+fr.ID)
	f.mu.Unlock()
	ch := make(chan StreamEvent, 4)
	if fr.Type == "restore_session" {
		ch <- StreamEvent{Kind: "session_restored", SessionID: fr.ID}
	}
	close(ch)
	return ch, nil
}

func (f *fakeExecBackend) Cancel() error { return nil }
func (f *fakeExecBackend) Close() error  { return nil }
func (f *fakeExecBackend) Mode() string  { return f.mode }
func (f *fakeExecBackend) recorded() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	cp := make([]string, len(f.frames))
	copy(cp, f.frames)
	return cp
}

// TestExec_TextMode_RendersAgentChunkToStdout proves text mode routes the model
// stream to stdout and tool activity to stderr (stdout stays parseable).
func TestExec_TextMode_RendersAgentChunkToStdout(t *testing.T) {
	var stdout, stderr bytes.Buffer
	b := &fakeExecBackend{mode: "ws", sendText: "hello world", statusID: "sess-1"}
	res, err := execWithBackend(context.Background(), b, ExecOptions{
		Prompt: "hi", Output: ExecOutputText, Stdout: &stdout, Stderr: &stderr,
	})
	require.NoError(t, err)
	assert.Equal(t, "sess-1", res.SessionID, "status event's SessionID surfaces in the result")
	assert.Contains(t, stdout.String(), "hello world", "agent_chunk text -> stdout")
	assert.NotContains(t, stdout.String(), "fs_read", "tool activity must NOT pollute stdout")
	assert.Contains(t, stderr.String(), "fs_read", "tool_call one-liner -> stderr")
	assert.Contains(t, stderr.String(), "[ok]", "tool_result status -> stderr")
}

// TestExec_JSONLMode_OneJSONLinePerEvent proves jsonl mode emits one stable
// HeadlessOutputEvent JSON object per line on stdout (every event, including
// status/done), so the stream is machine-parseable. The wire shape uses the
// lowercase "type" field (not the Go field name "Kind") — the headless JSONL
// contract external consumers depend on.
func TestExec_JSONLMode_OneJSONLinePerEvent(t *testing.T) {
	var stdout, stderr bytes.Buffer
	b := &fakeExecBackend{mode: "ws", sendText: "chunk", statusID: "sess-2"}
	_, err := execWithBackend(context.Background(), b, ExecOptions{
		Prompt: "hi", Output: ExecOutputJSONL, Stdout: &stdout, Stderr: &stderr,
	})
	require.NoError(t, err)
	assert.Empty(t, stderr.String(), "jsonl mode routes all events to stdout")

	out := strings.TrimRight(stdout.String(), "\n")
	require.NotEmpty(t, out, "jsonl mode must emit at least one event")
	lines := strings.Split(out, "\n")
	kinds := make([]string, 0, len(lines))
	for i, ln := range lines {
		var ev map[string]any
		require.NoErrorf(t, json.Unmarshal([]byte(ln), &ev), "line %d not JSON: %q", i, ln)
		// Stable wire key is "type" (lowercase), never the Go field name "Kind".
		kind, _ := ev["type"].(string)
		if kind == "" {
			t.Fatalf("line %d missing stable \"type\" key: %q", i, ln)
		}
		kinds = append(kinds, kind)
	}
	assert.Contains(t, kinds, "agent_chunk")
	assert.Contains(t, kinds, "status")
	assert.Contains(t, kinds, "done")
}

// TestExec_ResumeSendsRestoreBeforeUserMessage proves --resume sends
// restore_session FIRST and the user turn SECOND.
func TestExec_ResumeSendsRestoreBeforeUserMessage(t *testing.T) {
	var stdout, stderr bytes.Buffer
	b := &fakeExecBackend{mode: "ws", sendText: "ok", statusID: "sess-r"}
	res, err := execWithBackend(context.Background(), b, ExecOptions{
		Prompt: "more", Resume: "sess-r", Output: ExecOutputText, Stdout: &stdout, Stderr: &stderr,
	})
	require.NoError(t, err)
	assert.Equal(t, "sess-r", res.SessionID)
	rec := b.recorded()
	require.GreaterOrEqual(t, len(rec), 2, "resume must send restore + user_message")
	assert.Equal(t, "restore_session:sess-r", rec[0], "restore must precede the user turn")
	assert.Equal(t, "user_message:more", rec[1])
}

// TestExec_ServerErrorFrameReturnsError proves a server error frame is surfaced
// as a non-nil error (main maps it to exit 1) and rendered to stderr.
func TestExec_ServerErrorFrameReturnsError(t *testing.T) {
	b := &errExecBackend{fakeExecBackend: fakeExecBackend{mode: "ws"}}
	var stdout, stderr bytes.Buffer
	_, err := execWithBackend(context.Background(), b, ExecOptions{
		Prompt: "hi", Output: ExecOutputText, Stdout: &stdout, Stderr: &stderr,
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "boom")
	assert.Contains(t, stderr.String(), "error: boom")
}

// errExecBackend is a fakeExecBackend whose Send emits a single error frame.
type errExecBackend struct{ fakeExecBackend }

func (e *errExecBackend) Send(_ context.Context, _ string) (<-chan StreamEvent, error) {
	e.mu.Lock()
	e.frames = append(e.frames, "user_message:err")
	e.mu.Unlock()
	ch := make(chan StreamEvent, 1)
	ch <- StreamEvent{Kind: "error", Text: "boom"}
	close(ch)
	return ch, nil
}

// TestExec_CancelledContextReturnsCanceled proves a cancelled ctx makes the
// driver return context.Canceled (cancel/timeout wins over a server error).
// blockExecBackend blocks its Send channel until ctx.Done, mirroring wsBackend's
// ctx wiring (which closes the channel on cancel).
type blockExecBackend struct{ fakeExecBackend }

func (b *blockExecBackend) Send(ctx context.Context, text string) (<-chan StreamEvent, error) {
	b.mu.Lock()
	b.frames = append(b.frames, "user_message:"+text)
	b.mu.Unlock()
	ch := make(chan StreamEvent)
	go func() { <-ctx.Done(); close(ch) }()
	return ch, nil
}

func TestExec_CancelledContextReturnsCanceled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // pre-cancel: the drain sees an empty, closed channel
	b := &blockExecBackend{fakeExecBackend: fakeExecBackend{mode: "ws"}}
	var stdout, stderr bytes.Buffer
	_, err := execWithBackend(ctx, b, ExecOptions{
		Prompt: "hi", Output: ExecOutputText, Stdout: &stdout, Stderr: &stderr,
	})
	require.Error(t, err)
	assert.ErrorIs(t, err, context.Canceled)
}

// TestExec_InProcessFakeModelTurn is the composition proof: the real Exec path
// (bootstrap.Build via Resolve, in-process store, bootstrap fake model) runs one
// turn and surfaces a non-empty session id. Depends on Task 1's statusFrame change.
func TestExec_InProcessFakeModelTurn(t *testing.T) {
	root := t.TempDir()
	var stdout, stderr bytes.Buffer
	res, err := Exec(context.Background(), ExecOptions{
		Options: Options{Root: root, ConfigPath: writeTestConfig(t, root), FakeModel: true},
		Prompt:  "hello",
		Output:  ExecOutputText,
		Stdout:  &stdout,
		Stderr:  &stderr,
	})
	require.NoError(t, err)
	// The bootstrap fake model (bootstrap.go:114) emits this exact text.
	assert.Contains(t, stdout.String(), "(no real model configured)")
	assert.NotEmpty(t, res.SessionID, "store-backed in-process backend must surface a session id")
}

// recordingBackend records prompts and restore counts for multi-prompt tests.
// It implements the full ChatBackend interface so it can stand in for a real
// backend across the headless runner's lifecycle (Send + SendFrame + Cancel +
// Close + Mode) without pulling in a mock framework.
type recordingBackend struct {
	prompts  []string
	restores int
	mu       sync.Mutex
}

func (r *recordingBackend) Send(_ context.Context, text string) (<-chan StreamEvent, error) {
	r.mu.Lock()
	r.prompts = append(r.prompts, text)
	r.mu.Unlock()
	ch := make(chan StreamEvent, 4)
	ch <- StreamEvent{Kind: "agent_chunk", Text: "ok"}
	ch <- StreamEvent{Kind: "status", SessionID: "sess-rec"}
	ch <- StreamEvent{Kind: "done"}
	close(ch)
	return ch, nil
}

func (r *recordingBackend) SendFrame(_ context.Context, fr proto.ClientFrame) (<-chan StreamEvent, error) {
	r.mu.Lock()
	r.restores++
	r.mu.Unlock()
	ch := make(chan StreamEvent, 2)
	if fr.Type == "restore_session" {
		ch <- StreamEvent{Kind: "session_restored", SessionID: fr.ID}
	}
	close(ch)
	return ch, nil
}

func (r *recordingBackend) Cancel() error { return nil }
func (r *recordingBackend) Close() error  { return nil }
func (r *recordingBackend) Mode() string  { return "fake" }

// TestRenderHeadlessJSONLEventUsesStableKeys proves the JSONL encoder emits
// stable lowercase camelCase keys (type/text) and never leaks Go field names
// (Kind) — the contract external pipe consumers depend on.
func TestRenderHeadlessJSONLEventUsesStableKeys(t *testing.T) {
	var out, errOut bytes.Buffer
	renderHeadlessEvent(&out, &errOut, ExecOutputJSONL, StreamEvent{
		Kind: "agent_chunk",
		Text: "hello",
	})
	var got map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(out.Bytes()), &got); err != nil {
		t.Fatalf("JSONL event is invalid JSON: %v", err)
	}
	if got["type"] != "agent_chunk" || got["text"] != "hello" {
		t.Fatalf("JSONL event = %#v", got)
	}
	if _, ok := got["Kind"]; ok {
		t.Fatal("JSONL must not expose Go field name Kind")
	}
}

// TestRunHeadlessWithBackendResumesOnlyOnce proves a multi-prompt run restores
// at most once: the CLI resume flag (or the first record's resume) is applied
// before turn 1, and subsequent records continue the same session without
// re-restoring. The recordingBackend's restores counter asserts "once total".
func TestRunHeadlessWithBackendResumesOnlyOnce(t *testing.T) {
	backend := &recordingBackend{}
	var stdout, stderr bytes.Buffer
	result, err := runHeadlessWithBackend(context.Background(), backend, HeadlessRunOptions{
		Inputs: []HeadlessInput{{Prompt: "one"}, {Prompt: "two"}},
		Output: ExecOutputJSONL,
		Stdout: &stdout,
		Stderr: &stderr,
		Resume: "sess-1",
	})
	if err != nil {
		t.Fatalf("runHeadlessWithBackend: %v", err)
	}
	if result.Completed != 2 || result.SessionID == "" {
		t.Fatalf("result = %#v", result)
	}
	backend.mu.Lock()
	restores := backend.restores
	prompts := append([]string(nil), backend.prompts...)
	backend.mu.Unlock()
	if restores != 1 {
		t.Fatalf("backend restores = %d, want 1", restores)
	}
	if len(prompts) != 2 || !strings.Contains(prompts[0], "one") || !strings.Contains(prompts[1], "two") {
		t.Fatalf("backend prompts = %#v", prompts)
	}
}
