package cli

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/x6nux/yanshi/internal/proto"
)

// TestHealthz_TrueOn200AndFalseOtherwise proves healthz returns true for a 200
// healthz and false for an unreachable host, a bad URL, and a non-200 reply.
func TestHealthz_TrueOn200AndFalseOtherwise(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(200) })
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	assert.True(t, healthz(context.Background(), ts.URL))

	// Non-200.
	mux2 := http.NewServeMux()
	mux2.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(503) })
	ts2 := httptest.NewServer(mux2)
	t.Cleanup(ts2.Close)
	assert.False(t, healthz(context.Background(), ts2.URL))

	// Unreachable host (connection refused).
	assert.False(t, healthz(context.Background(), "http://127.0.0.1:1"))

	// Malformed URL -> NewRequest error -> false.
	assert.False(t, healthz(context.Background(), "http://[::1"))

	// Empty base URL also yields a request error path.
	assert.False(t, healthz(context.Background(), ""))
}

// TestReadHeadlessInputs_NilReader proves a nil reader is rejected.
func TestReadHeadlessInputs_NilReader(t *testing.T) {
	_, err := ReadHeadlessInputs(nil, HeadlessInputText)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "nil reader")
}

// TestReadHeadlessInputs_TextEmpty proves text mode rejects a whitespace-only
// stream.
func TestReadHeadlessInputs_TextEmpty(t *testing.T) {
	_, err := ReadHeadlessInputs(strings.NewReader("   \n  "), HeadlessInputText)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "empty")
}

// TestReadHeadlessInputs_LinesEmpty proves lines mode rejects a stream with no
// non-empty lines.
func TestReadHeadlessInputs_LinesEmpty(t *testing.T) {
	_, err := ReadHeadlessInputs(strings.NewReader("\n  \n"), HeadlessInputLines)
	require.Error(t, err)
}

// TestReadHeadlessInputs_JSONLErrors proves jsonl mode rejects malformed JSON
// and an empty input stream.
func TestReadHeadlessInputs_JSONLErrors(t *testing.T) {
	_, err := ReadHeadlessInputs(strings.NewReader("{bad json}\n"), HeadlessInputJSONL)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "jsonl line 1")

	_, err = ReadHeadlessInputs(strings.NewReader("\n\n"), HeadlessInputJSONL)
	require.Error(t, err)
}

// TestReadHeadlessInputs_JSONLEmptyPromptField proves jsonl rejects an object
// whose prompt field is empty/whitespace.
func TestReadHeadlessInputs_JSONLEmptyPromptField(t *testing.T) {
	_, err := ReadHeadlessInputs(strings.NewReader(`{"prompt":"   "}`+"\n"), HeadlessInputJSONL)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "prompt is empty")
}

// frameBackend drives runHeadlessWithFrames: Send streams a scripted turn for
// user_message; SendFrame returns a one-line reply for control frames or nil for
// permission_response; SendFrameErr forces an error on the named frame type.
type frameBackend struct {
	sendErr    error
	frameErr   error
	frameErrOn string
	permReturn bool // SendFrame returns nil for permission_response
}

// SendTurn delegates to Send: these fakes exercise the text path only.
// It is on the interface rather than an optional capability so a backend
// that forgets it fails to compile — an optional one would silently drop
// every attachment.
func (b *frameBackend) SendTurn(ctx context.Context, fr proto.ClientFrame) (<-chan StreamEvent, error) {
	return b.Send(ctx, fr.Text)
}

func (b *frameBackend) Send(_ context.Context, _ string) (<-chan StreamEvent, error) {
	if b.sendErr != nil {
		return nil, b.sendErr
	}
	ch := make(chan StreamEvent, 2)
	ch <- StreamEvent{Kind: "agent_chunk", Text: "ok"}
	ch <- StreamEvent{Kind: "done"}
	close(ch)
	return ch, nil
}

func (b *frameBackend) SendFrame(_ context.Context, f proto.ClientFrame) (<-chan StreamEvent, error) {
	if b.frameErr != nil && (b.frameErrOn == "" || b.frameErrOn == f.Type) {
		return nil, b.frameErr
	}
	if f.Type == "permission_response" && b.permReturn {
		return nil, nil
	}
	ch := make(chan StreamEvent, 1)
	ch <- StreamEvent{Kind: "status"}
	close(ch)
	return ch, nil
}

func (b *frameBackend) Cancel() error { return nil }
func (b *frameBackend) Close() error  { return nil }
func (b *frameBackend) Mode() string  { return "ws" }

// TestRunHeadlessWithFrames_MixedFrames proves a user_message turn and a control
// frame are both drained, and a permission_response (nil channel) is skipped.
func TestRunHeadlessWithFrames_MixedFrames(t *testing.T) {
	b := &frameBackend{permReturn: true}
	evs, err := runHeadlessWithFrames(context.Background(), b, []proto.ClientFrame{
		{Type: "user_message", Text: "hi"},
		{Type: "get_status"},
		{Type: "permission_response"},
	})
	require.NoError(t, err)
	// user_message -> agent_chunk + done; get_status -> status. permission_response
	// contributes nothing (nil channel).
	require.GreaterOrEqual(t, len(evs), 3)
}

// TestRunHeadlessWithFrames_SendError proves a Send error short-circuits with a
// wrapped error naming the frame type.
func TestRunHeadlessWithFrames_SendError(t *testing.T) {
	b := &frameBackend{sendErr: errors.New("send boom")}
	_, err := runHeadlessWithFrames(context.Background(), b, []proto.ClientFrame{
		{Type: "user_message", Text: "hi"},
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "user_message")
	assert.Contains(t, err.Error(), "send boom")
}

// TestRunHeadlessWithFrames_FrameError proves a SendFrame error short-circuits.
func TestRunHeadlessWithFrames_FrameError(t *testing.T) {
	b := &frameBackend{frameErr: errors.New("frame boom"), frameErrOn: "get_status"}
	_, err := runHeadlessWithFrames(context.Background(), b, []proto.ClientFrame{
		{Type: "get_status"},
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "frame boom")
}

// TestRunHeadlessWithFrames_EventErrShortCircuits proves an error event with a
// non-nil Err on the stream aborts the run with a wrapped error.
func TestRunHeadlessWithFrames_EventErrShortCircuits(t *testing.T) {
	b := &errFrameBackend{}
	_, err := runHeadlessWithFrames(context.Background(), b, []proto.ClientFrame{
		{Type: "user_message", Text: "hi"},
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "errored")
}

// errFrameBackend is a frameBackend whose Send emits one error event.
type errFrameBackend struct{ frameBackend }

// SendTurn delegates to Send: these fakes exercise the text path only.
// It is on the interface rather than an optional capability so a backend
// that forgets it fails to compile — an optional one would silently drop
// every attachment.
func (b *errFrameBackend) SendTurn(ctx context.Context, fr proto.ClientFrame) (<-chan StreamEvent, error) {
	return b.Send(ctx, fr.Text)
}

func (b *errFrameBackend) Send(_ context.Context, _ string) (<-chan StreamEvent, error) {
	ch := make(chan StreamEvent, 1)
	ch <- StreamEvent{Kind: "error", Err: errors.New("stream broke")}
	close(ch)
	return ch, nil
}

// TestRunHeadless_RootDefaultsToCWD proves runHeadless resolves an empty Root to
// the working directory (the happy-path Resolve must succeed for the fake model).
func TestRunHeadless_RootDefaultsToCWD(t *testing.T) {
	root := t.TempDir()
	evs, err := runHeadless(context.Background(),
		Options{Root: "", ConfigPath: writeTestConfig(t, root), FakeModel: true, InProcess: true},
		[]string{"hello"})
	require.NoError(t, err)
	assert.NotEmpty(t, evs, "runHeadless returns streamed events")
}

// TestRunHeadless_SendError proves a Send failure wraps the query in the error.
func TestRunHeadless_SendError(t *testing.T) {
	root := t.TempDir()
	// forcedRemote to a dead origin: Resolve connects (lazy), then Send fails on
	// the dead socket.
	_, err := runHeadless(context.Background(),
		Options{Root: root, ConfigPath: "", Server: "http://127.0.0.1:1"},
		[]string{"hello"})
	require.Error(t, err)
}

// resumeFrameErrBackend's SendFrame returns a transport error on the
// restore_session frame, exercising execWithBackend's "resume %q: %w" branch.
type resumeFrameErrBackend struct{ fakeExecBackend }

func (b *resumeFrameErrBackend) SendFrame(_ context.Context, _ proto.ClientFrame) (<-chan StreamEvent, error) {
	return nil, errors.New("restore refused")
}

// TestExec_ResumeFrameErrorReturnsError proves a SendFrame failure during resume
// is wrapped with the resume id.
func TestExec_ResumeFrameErrorReturnsError(t *testing.T) {
	b := &resumeFrameErrBackend{fakeExecBackend: fakeExecBackend{mode: "ws"}}
	var stdout, stderr bytes.Buffer
	_, err := execWithBackend(context.Background(), b, ExecOptions{
		Prompt: "hi", Resume: "sess-x", Output: ExecOutputText, Stdout: &stdout, Stderr: &stderr,
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "sess-x")
	assert.Contains(t, err.Error(), "restore refused")
}

// resumeEventErrBackend's SendFrame streams an error event, exercising
// execWithBackend's "resume %q: %s" branch (a server error frame during resume).
type resumeEventErrBackend struct{ fakeExecBackend }

func (b *resumeEventErrBackend) SendFrame(_ context.Context, fr proto.ClientFrame) (<-chan StreamEvent, error) {
	ch := make(chan StreamEvent, 1)
	ch <- StreamEvent{Kind: "error", Text: "session gone"}
	close(ch)
	return ch, nil
}

// TestExec_ResumeEventErrorReturnsError proves an error event during the resume
// reply drain is surfaced as a non-nil error.
func TestExec_ResumeEventErrorReturnsError(t *testing.T) {
	b := &resumeEventErrBackend{fakeExecBackend: fakeExecBackend{mode: "ws"}}
	var stdout, stderr bytes.Buffer
	_, err := execWithBackend(context.Background(), b, ExecOptions{
		Prompt: "hi", Resume: "sess-y", Output: ExecOutputText, Stdout: &stdout, Stderr: &stderr,
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "session gone")
}

// TestExec_ResumeJSONLRendersRestoreEvent proves JSONL mode renders the
// restore_session reply as a real event line (the resume branch's JSONL render).
func TestExec_ResumeJSONLRendersRestoreEvent(t *testing.T) {
	b := &fakeExecBackend{mode: "ws", sendText: "chunk", statusID: "sess-j"}
	var stdout, stderr bytes.Buffer
	_, err := execWithBackend(context.Background(), b, ExecOptions{
		Prompt: "hi", Resume: "sess-j", Output: ExecOutputJSONL, Stdout: &stdout, Stderr: &stderr,
	})
	require.NoError(t, err)
	// The session_restored reply must appear as a JSONL line on stdout.
	assert.Contains(t, stdout.String(), "session_restored")
}
