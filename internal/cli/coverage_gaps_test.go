package cli

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/x6nux/yanshi/internal/config"
	"github.com/x6nux/yanshi/internal/proto"
)

// ---------------------------------------------------------------------------
// renderHeadlessEvent — JSONL branch
// ---------------------------------------------------------------------------

func TestRenderHeadlessEvent_JSONL(t *testing.T) {
	var stdout, stderr bytes.Buffer

	t.Run("agent_chunk", func(t *testing.T) {
		stdout.Reset()
		stderr.Reset()
		renderHeadlessEvent(&stdout, &stderr, ExecOutputJSONL, StreamEvent{
			Kind: "agent_chunk", Text: "hello",
		})
		assert.Contains(t, stdout.String(), `"type":"agent_chunk"`)
		assert.Contains(t, stdout.String(), `"text":"hello"`)
	})

	t.Run("error_with_err", func(t *testing.T) {
		stdout.Reset()
		renderHeadlessEvent(&stdout, &stderr, ExecOutputJSONL, StreamEvent{
			Kind: "error", Err: errors.New("connection lost"),
		})
		assert.Contains(t, stdout.String(), `"error":"connection lost"`)
	})

	t.Run("status_with_counters", func(t *testing.T) {
		stdout.Reset()
		renderHeadlessEvent(&stdout, &stderr, ExecOutputJSONL, StreamEvent{
			Kind: "status", SessionID: "s1", Model: "gpt4", Turns: 3,
		})
		assert.Contains(t, stdout.String(), `"type":"status"`)
		assert.Contains(t, stdout.String(), `"model":"gpt4"`)
	})
}

// TestRenderHeadlessEvent_TextBranch proves text mode delegates to
// renderExecEvent, which outputs agent_chunk on stdout and errors on stderr.
func TestRenderHeadlessEvent_TextBranch(t *testing.T) {
	var stdout, stderr bytes.Buffer
	renderHeadlessEvent(&stdout, &stderr, ExecOutputText, StreamEvent{
		Kind: "agent_chunk", Text: "hello world",
	})
	assert.Equal(t, "hello world", stdout.String())

	stdout.Reset()
	stderr.Reset()
	renderHeadlessEvent(&stdout, &stderr, ExecOutputText, StreamEvent{
		Kind: "error", Text: "something bad",
	})
	assert.Contains(t, stderr.String(), "something bad")
}

// ---------------------------------------------------------------------------
// renderExecEvent — JSONL branch and text branch
// ---------------------------------------------------------------------------

func TestRenderExecEvent_JSONL(t *testing.T) {
	var stdout, stderr bytes.Buffer
	renderExecEvent(&stdout, &stderr, ExecOutputJSONL, StreamEvent{
		Kind: "tool_call", ToolName: "fs_read", ToolArgs: `{"path":"x"}`,
	})
	assert.Contains(t, stdout.String(), `"type":"tool_call"`)
	assert.Empty(t, stderr.String())
}

func TestRenderExecEvent_TextBranch(t *testing.T) {
	var stdout, stderr bytes.Buffer
	renderExecEvent(&stdout, &stderr, ExecOutputText, StreamEvent{
		Kind: "tool_call", ToolName: "fs_write", ToolArgs: `{"path":"/x"}`,
	})
	assert.Contains(t, stderr.String(), "fs_write")

	stdout.Reset()
	stderr.Reset()
	renderExecEvent(&stdout, &stderr, ExecOutputText, StreamEvent{
		Kind: "tool_result", ToolName: "fs_write", ToolStatus: "ok",
	})
	assert.Contains(t, stderr.String(), "ok")
}

// ---------------------------------------------------------------------------
// execEventText
// ---------------------------------------------------------------------------

func TestExecEventText_WithErr(t *testing.T) {
	s := execEventText(StreamEvent{Err: errors.New("dial failed")})
	assert.Equal(t, "dial failed", s)
}

func TestExecEventText_WithText(t *testing.T) {
	s := execEventText(StreamEvent{Text: "server error: timeout"})
	assert.Equal(t, "server error: timeout", s)
}

func TestExecEventText_NoErrNoText(t *testing.T) {
	s := execEventText(StreamEvent{})
	assert.Empty(t, s)
}

// ---------------------------------------------------------------------------
// checkConfigVersion — uncovered branches
// ---------------------------------------------------------------------------

func TestCheckConfigVersion_Supported(t *testing.T) {
	r := checkConfigVersion(&config.Config{SchemaVersion: config.SupportedSchemaVersion}, nil, false)
	assert.Equal(t, StatusOK, r.Status)
}

func TestCheckConfigVersion_Unsupported(t *testing.T) {
	r := checkConfigVersion(&config.Config{SchemaVersion: config.SupportedSchemaVersion + 1}, nil, false)
	assert.Equal(t, StatusWarn, r.Status)
}

func TestCheckConfigVersion_ReleaseMode(t *testing.T) {
	r := checkConfigVersion(&config.Config{SchemaVersion: config.SupportedSchemaVersion + 1}, nil, true)
	assert.Equal(t, StatusFail, r.Status)
}

func TestCheckConfigVersion_SchemaVersionError(t *testing.T) {
	r := checkConfigVersion(nil, errors.New("schema_version too high"), false)
	assert.Equal(t, StatusWarn, r.Status)
}

func TestCheckConfigVersion_SchemaVersionErrorRelease(t *testing.T) {
	r := checkConfigVersion(nil, errors.New("schema_version too high"), true)
	assert.Equal(t, StatusFail, r.Status)
}

func TestCheckConfigVersion_OtherError(t *testing.T) {
	r := checkConfigVersion(nil, errors.New("file not found"), false)
	assert.Equal(t, StatusWarn, r.Status)
}

// ---------------------------------------------------------------------------
// runHeadlessWithBackend — nil backend and empty stdin io.Writer defaults
// ---------------------------------------------------------------------------

func TestRunHeadlessWithBackend_NilBackend(t *testing.T) {
	_, err := runHeadlessWithBackend(context.Background(), nil, HeadlessRunOptions{
		Inputs: []HeadlessInput{{Prompt: "hi"}},
	})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "nil")
}

func TestRunHeadlessWithBackend_DefaultStdoutStderr(t *testing.T) {
	b := newFakeBackend([]string{"ok"})
	result, err := runHeadlessWithBackend(context.Background(), b, HeadlessRunOptions{
		Inputs: []HeadlessInput{{Prompt: "hello"}},
	})
	require.NoError(t, err)
	assert.Equal(t, 1, result.Completed)
}

func TestRunHeadlessWithBackend_StreamScriptedEvents(t *testing.T) {
	var stdout, stderr bytes.Buffer
	b := newFakeBackend([]string{"hello", "world"})
	result, err := runHeadlessWithBackend(context.Background(), b, HeadlessRunOptions{
		Inputs: []HeadlessInput{{Prompt: "hi"}},
		Stdout: &stdout,
		Stderr: &stderr,
		Output: ExecOutputText,
	})
	require.NoError(t, err)
	assert.Equal(t, 1, result.Completed)
	assert.Equal(t, "helloworld", stdout.String())
}

func TestRunHeadlessWithBackend_MultiInput(t *testing.T) {
	b := newFakeBackend([]string{"a", "b", "c"})
	result, err := runHeadlessWithBackend(context.Background(), b, HeadlessRunOptions{
		Inputs: []HeadlessInput{
			{Prompt: "first"},
			{Prompt: "second"},
		},
	})
	require.NoError(t, err)
	assert.Equal(t, 2, result.Completed)
}

// ---------------------------------------------------------------------------
// projectHeadlessEvent
// ---------------------------------------------------------------------------

func TestProjectHeadlessEvent_Error(t *testing.T) {
	ev := projectHeadlessEvent(StreamEvent{
		Kind: "error", Text: "msg", Err: errors.New("underlying"),
	})
	assert.Equal(t, "underlying", ev.Error)
}

func TestProjectHeadlessEvent_Normal(t *testing.T) {
	ev := projectHeadlessEvent(StreamEvent{
		Kind: "agent_chunk", Text: "hi", Model: "gpt4",
	})
	assert.Equal(t, "agent_chunk", ev.Type)
	assert.Equal(t, "hi", ev.Text)
	assert.Equal(t, "gpt4", ev.Model)
	assert.Empty(t, ev.Error)
}

// ---------------------------------------------------------------------------
// toStreamEvent — seam_restored maps ID to UndoSeamID
// ---------------------------------------------------------------------------

func TestToStreamEvent_SeamRestored(t *testing.T) {
	ev := toStreamEvent(proto.NewSeamRestored("undo-id", "abc123", "fullhash", "reverted"))
	assert.Equal(t, "seam_restored", ev.Kind)
	assert.Equal(t, "undo-id", ev.UndoSeamID)
}

// ---------------------------------------------------------------------------
// isControlReply — all reply types enumerated
// ---------------------------------------------------------------------------

func TestIsControlReply_FullList(t *testing.T) {
	for _, kind := range []string{
		"models", "status", "mcp_list", "mcp_status", "sessions",
		"session_restored", "session_ack", "permissions",
		"permission_rule_hit", "jobs", "job_event", "seams",
		"seam_restored", "session_forked", "side_state",
		"skills_list", "skill_ack", "features",
		// The memory pair. Both were missing when W-D-12 added the second one:
		// a reply type absent from isControlReply never closes the client's
		// reply channel, so the slash command that sent it appears to hang
		// while the server has already answered. THE LIST IS HAND-MAINTAINED
		// and nothing compares it to the switch, so it goes stale silently —
		// which is how memories_distilled sat outside it since W-A-05.
		"memories_distilled", "memories_cleared",
	} {
		assert.True(t, isControlReply(kind), "kind=%q must be a control reply", kind)
	}
	assert.False(t, isControlReply("agent_chunk"))
	assert.False(t, isControlReply("tool_call"))
	assert.False(t, isControlReply(""))
}

// ---------------------------------------------------------------------------
// mustMarshal — always returns valid JSON
// ---------------------------------------------------------------------------

func TestMustMarshal(t *testing.T) {
	b := mustMarshal(proto.NewUserMessage("hi"))
	assert.Contains(t, string(b), `"text":"hi"`)
	assert.Contains(t, string(b), `"type":"user_message"`)
}

// TestExecWithBackend_OutputDefaults proves nil stdout/stderr default to
// io.Discard (no panic).
func TestExecWithBackend_OutputDefaults(t *testing.T) {
	b := &fakeExecBackend{mode: "ws", sendText: "reply", statusID: "sess-1"}
	result, err := execWithBackend(context.Background(), b, ExecOptions{
		Prompt: "hi",
	})
	require.NoError(t, err)
	assert.Equal(t, "sess-1", result.SessionID)
}

func TestExecWithBackend_JSONLOutput(t *testing.T) {
	var stdout, stderr bytes.Buffer
	b := &fakeExecBackend{mode: "ws", sendText: "reply", statusID: "sess-2"}
	result, err := execWithBackend(context.Background(), b, ExecOptions{
		Prompt: "hi",
		Stdout: &stdout,
		Stderr: &stderr,
		Output: ExecOutputJSONL,
	})
	require.NoError(t, err)
	assert.Equal(t, "sess-2", result.SessionID)
	assert.Contains(t, stdout.String(), "agent_chunk")
}

func TestExecWithBackend_ResumeError(t *testing.T) {
	b := &fakeExecBackend{mode: "ws", sendText: "reply", statusID: "sess-3"}
	result, err := execWithBackend(context.Background(), b, ExecOptions{
		Prompt: "hi",
		Resume: "session-abc",
	})
	require.NoError(t, err)
	assert.NotEmpty(t, result.SessionID)
}

// ---------------------------------------------------------------------------
// TestNewClientThreadID_Format proves the format is always "sse-" + 12 hex chars.
func TestNewClientThreadID_Format(t *testing.T) {
	for i := 0; i < 10; i++ {
		id := newClientThreadID()
		if !strings.HasPrefix(id, "sse-") || len(id) != 16 {
			t.Fatalf("bad id: %q", id)
		}
	}
}

// TestHeadlessRun_NoInputs proves RunHeadless returns an error for empty inputs.
func TestHeadlessRun_NoInputs(t *testing.T) {
	_, err := RunHeadless(context.Background(), Options{}, HeadlessRunOptions{})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "no input")
}
