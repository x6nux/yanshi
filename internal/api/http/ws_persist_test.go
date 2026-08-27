// internal/api/http/ws_persist_test.go
//
// C1: the durable log, and the ordering rule that protects it.
package http

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	einollm "github.com/x6nux/yanshi/internal/llm/eino"
	"github.com/x6nux/yanshi/internal/store"
)

func persistStore(t *testing.T) *store.Store {
	t.Helper()
	s, err := store.Open(filepath.Join(t.TempDir(), "p.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// brokenStore returns a Store whose writes all fail, by closing the underlying
// database while keeping the handle. This is the injected write failure the
// persist-before-evict rule is defined against; nothing else in the process is
// affected, so the test observes exactly the branch it means to.
func brokenStore(t *testing.T) *store.Store {
	t.Helper()
	s, err := store.Open(filepath.Join(t.TempDir(), "broken.db"))
	require.NoError(t, err)
	sid, err := s.CreateSession("doomed")
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })
	// Close the DB handle: every subsequent Exec returns sql.ErrConnDone.
	require.NoError(t, s.DB.Close())
	brokenSessionID = sid
	return s
}

// brokenSessionID carries the session id created before brokenStore broke the
// database, so a test can address a session that legitimately exists.
var brokenSessionID string

// evictableHistory builds a window that compaction will actually shrink.
//
// The shape matters and is not arbitrary: ctxcompact.Plan pins every user
// message, every error/diff marker and the recent tail, so a fixture made of
// user turns or "FAIL" text summarises to nothing and reports did=false. One
// user message plus a run of plain assistant prose is the smallest shape where
// eviction genuinely happens — which is the precondition for asserting anything
// about the persist-before-evict ordering.
func evictableHistory(n int) []*schema.Message {
	out := []*schema.Message{schema.UserMessage("kick off the work")}
	for i := 0; i < n; i++ {
		out = append(out, schema.AssistantMessage(
			"an ordinary progress report with enough words in it to matter "+
				strings.Repeat("and more detail ", 8), nil))
	}
	return out
}

func turn(user, assistant string, calls ...schema.ToolCall) []*schema.Message {
	msgs := []*schema.Message{schema.UserMessage(user)}
	if len(calls) > 0 {
		msgs = append(msgs, &schema.Message{Role: schema.Assistant, ToolCalls: calls})
		for _, c := range calls {
			msgs = append(msgs, &schema.Message{
				Role: schema.Tool, ToolCallID: c.ID, ToolName: c.Function.Name,
				Content: "result of " + c.Function.Name,
			})
		}
	}
	if assistant != "" {
		msgs = append(msgs, schema.AssistantMessage(assistant, nil))
	}
	return msgs
}

func toolCall(id, name, args string) schema.ToolCall {
	return schema.ToolCall{ID: id, Function: schema.FunctionCall{Name: name, Arguments: args}}
}

// ---------------------------------------------------------------------------
// C1: tool_call / tool_result reach the database
// ---------------------------------------------------------------------------

// TestPersistMessages_WritesToolCallsAndResults is the headline C1 assertion.
// The pre-C1 persistMessages took (userText, assistantText) and wrote exactly
// two rows, so a turn's tool calls and their output were never written down —
// compaction then evicted them and they were gone. On that code this test
// cannot pass, because the tool rows have no path to the database at all.
func TestPersistMessages_WritesToolCallsAndResults(t *testing.T) {
	st := persistStore(t)
	sid, err := st.CreateSession("s")
	require.NoError(t, err)
	srv := &Server{store: st}
	cs := &connSession{perm: &permModeState{}, sessionID: sid}
	cs.history = turn("run the tests", "one test is red",
		toolCall("c1", "shell_run", `{"cmd":"go test ./..."}`))

	cs.persistMessages(srv)

	got, err := st.Messages(sid)
	require.NoError(t, err)
	require.Len(t, got, 4)

	assert.Equal(t, store.RoleUser, got[0].Role)
	assert.Equal(t, "run the tests", got[0].Content)

	assert.Equal(t, store.RoleToolCall, got[1].Role)
	assert.Equal(t, "shell_run", got[1].ToolName)
	assert.Equal(t, "c1", got[1].ToolCallID)
	assert.Equal(t, `{"cmd":"go test ./..."}`, got[1].ToolArgs)

	assert.Equal(t, store.RoleToolResult, got[2].Role)
	assert.Equal(t, "c1", got[2].ToolCallID)
	assert.Equal(t, "result of shell_run", got[2].Content)

	assert.Equal(t, store.RoleAssistant, got[3].Role)
	assert.Equal(t, "one test is red", got[3].Content)
}

// TestPersistMessages_AcrossTurnsIsIdempotent: every turn flushes the WHOLE
// window, so without deduplication turn two would re-insert turn one. This is
// the property that lets flushHistory work without a watermark.
func TestPersistMessages_AcrossTurnsIsIdempotent(t *testing.T) {
	st := persistStore(t)
	sid, err := st.CreateSession("s")
	require.NoError(t, err)
	srv := &Server{store: st}
	cs := &connSession{perm: &permModeState{}, sessionID: sid}

	cs.history = turn("first question", "first answer")
	cs.persistMessages(srv)
	cs.history = append(cs.history, turn("second question", "second answer")...)
	cs.persistMessages(srv)

	got, err := st.Messages(sid)
	require.NoError(t, err)
	require.Len(t, got, 4)
	assert.Equal(t, []string{"first question", "first answer", "second question", "second answer"},
		[]string{got[0].Content, got[1].Content, got[2].Content, got[3].Content})
	for i, m := range got {
		assert.Equal(t, i, m.Seq)
	}
}

func TestStoreMessagesFor(t *testing.T) {
	cases := []struct {
		name string
		hist []*schema.Message
		want []store.Message
	}{
		{
			name: "system messages are not conversation",
			hist: []*schema.Message{schema.SystemMessage("you are an agent"), schema.UserMessage("hi")},
			want: []store.Message{{Role: store.RoleUser, Content: "hi"}},
		},
		{
			name: "nil entries are skipped",
			hist: []*schema.Message{nil, schema.UserMessage("hi"), nil},
			want: []store.Message{{Role: store.RoleUser, Content: "hi"}},
		},
		{
			name: "assistant prose plus tool calls yields both",
			hist: []*schema.Message{{
				Role: schema.Assistant, Content: "let me check",
				ToolCalls: []schema.ToolCall{toolCall("c1", "fs_read", `{"p":"a"}`)},
			}},
			want: []store.Message{
				{Role: store.RoleAssistant, Content: "let me check"},
				{Role: store.RoleToolCall, ToolCallID: "c1", ToolName: "fs_read", ToolArgs: `{"p":"a"}`},
			},
		},
		{
			name: "parallel tool calls each get a row",
			hist: []*schema.Message{{
				Role: schema.Assistant,
				ToolCalls: []schema.ToolCall{
					toolCall("c1", "fs_read", `{"p":"a"}`),
					toolCall("c2", "fs_read", `{"p":"b"}`),
				},
			}},
			want: []store.Message{
				{Role: store.RoleToolCall, ToolCallID: "c1", ToolName: "fs_read", ToolArgs: `{"p":"a"}`},
				{Role: store.RoleToolCall, ToolCallID: "c2", ToolName: "fs_read", ToolArgs: `{"p":"b"}`},
			},
		},
		{
			name: "an empty assistant message with no calls yields nothing",
			hist: []*schema.Message{schema.AssistantMessage("", nil)},
			want: nil,
		},
		{
			name: "tool result keeps its link",
			hist: []*schema.Message{{
				Role: schema.Tool, ToolCallID: "c7", ToolName: "shell_run", Content: "exit 1",
			}},
			want: []store.Message{
				{Role: store.RoleToolResult, ToolCallID: "c7", ToolName: "shell_run", Content: "exit 1"},
			},
		},
		{
			name: "empty user message is dropped rather than stored blank",
			hist: []*schema.Message{schema.UserMessage("")},
			want: nil,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := storeMessagesFor(tc.hist)
			require.Len(t, got, len(tc.want))
			for i := range tc.want {
				assert.Equal(t, tc.want[i].Role, got[i].Role, "row %d role", i)
				assert.Equal(t, tc.want[i].Content, got[i].Content, "row %d content", i)
				assert.Equal(t, tc.want[i].ToolCallID, got[i].ToolCallID, "row %d call id", i)
				assert.Equal(t, tc.want[i].ToolName, got[i].ToolName, "row %d tool name", i)
				assert.Equal(t, tc.want[i].ToolArgs, got[i].ToolArgs, "row %d tool args", i)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// C1: persist BEFORE evict — the invariant
// ---------------------------------------------------------------------------

// TestFlushHistory_ReportsFailure: a write failure must be visible to the
// caller as `false`. If flushHistory swallowed it (as the old best-effort
// persistMessages did), the eviction gate below could never fire.
func TestFlushHistory_ReportsFailure(t *testing.T) {
	st := brokenStore(t)
	srv := &Server{store: st}
	cs := &connSession{perm: &permModeState{}, sessionID: brokenSessionID}
	cs.history = turn("q", "a")
	assert.False(t, cs.flushHistory(srv), "a failed write must not report success")
}

// TestFlushHistory_NothingToPersistIsSuccess: "nothing was lost" and
// "everything was saved" are the same answer to the caller's question, so the
// no-recording cases must not block compaction.
func TestFlushHistory_NothingToPersistIsSuccess(t *testing.T) {
	cases := []struct {
		name string
		srv  *Server
		cs   *connSession
	}{
		{"no store", &Server{}, &connSession{perm: &permModeState{}, sessionID: "s"}},
		{"no session yet", &Server{store: nil}, &connSession{perm: &permModeState{}}},
		{
			name: "side conversation never writes",
			srv:  &Server{},
			cs: &connSession{perm: &permModeState{}, sessionID: "s",
				sideStack: []sideSnapshot{{}}},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tc.cs.history = turn("q", "a")
			assert.True(t, tc.cs.flushHistory(tc.srv))
		})
	}
}

// TestMaybeAutoCompact_DoesNotEvictWhenPersistFails is the C1 ordering rule,
// stated as the only thing that actually matters: when the conversation cannot
// be written down, it is not thrown away.
//
// Compaction replaces messages with a SUMMARY, which is lossy by construction.
// Evicting first and writing after would mean a full disk silently converts a
// turn's tool output into a paragraph about it, with nothing to recover from.
func TestMaybeAutoCompact_DoesNotEvictWhenPersistFails(t *testing.T) {
	st := brokenStore(t)
	fm := einollm.NewFakeModel([]string{"SUMMARY"}, nil)
	srv := &Server{
		store:      st,
		compaction: CompactionConfig{Model: "fm", Threshold: 0.05, ContextWindow: 4000, KeepRecent: 1},
	}
	cs := &connSession{perm: &permModeState{}, sessionID: brokenSessionID}
	cs.history = append(evictableHistory(8),
		&schema.Message{Role: schema.Assistant,
			ToolCalls: []schema.ToolCall{toolCall("c1", "shell_run", `{"cmd":"go build ./..."}`)}},
		&schema.Message{Role: schema.Tool, ToolCallID: "c1", ToolName: "shell_run",
			Content: "build finished in 3s"})
	before := len(cs.history)

	wc, client, cleanup := newWSPair(t)
	defer cleanup()
	_ = client
	maybeAutoCompact(context.Background(), srv,
		map[string]model.BaseChatModel{"fm": fm}, wc, cs)

	assert.Len(t, cs.history, before,
		"a failed persist must leave the live history intact — evicting it would destroy "+
			"messages that were never written down")
}

// TestMaybeAutoCompact_EvictsWhenPersistSucceeds is the other half, and it is
// what keeps the test above from passing for the wrong reason (a compaction
// that never fires at all would satisfy it trivially).
func TestMaybeAutoCompact_EvictsWhenPersistSucceeds(t *testing.T) {
	st := persistStore(t)
	sid, err := st.CreateSession("s")
	require.NoError(t, err)
	fm := einollm.NewFakeModel([]string{"SUMMARY"}, nil)
	srv := &Server{
		store:      st,
		compaction: CompactionConfig{Model: "fm", Threshold: 0.05, ContextWindow: 4000, KeepRecent: 1},
	}
	cs := &connSession{perm: &permModeState{}, sessionID: sid}
	cs.history = append(evictableHistory(8),
		&schema.Message{Role: schema.Assistant,
			ToolCalls: []schema.ToolCall{toolCall("c1", "shell_run", `{"cmd":"go build ./..."}`)}},
		&schema.Message{Role: schema.Tool, ToolCallID: "c1", ToolName: "shell_run",
			Content: "build finished in 3s"})
	before := len(cs.history)

	wc, client, cleanup := newWSPair(t)
	defer cleanup()
	_ = client
	maybeAutoCompact(context.Background(), srv,
		map[string]model.BaseChatModel{"fm": fm}, wc, cs)

	assert.Less(t, len(cs.history), before, "compaction must actually fire on this fixture")

	// And what was evicted is durable — including the tool rows, which is the
	// whole reason the ordering matters.
	got, err := st.Messages(sid)
	require.NoError(t, err)
	assert.Len(t, got, before)
	var sawCall, sawResult bool
	for _, m := range got {
		switch m.Role {
		case store.RoleToolCall:
			sawCall = true
			assert.Equal(t, `{"cmd":"go build ./..."}`, m.ToolArgs)
		case store.RoleToolResult:
			sawResult = true
		}
	}
	assert.True(t, sawCall, "the evicted tool call must be recoverable")
	assert.True(t, sawResult, "the evicted tool result must be recoverable")
}

// TestCompactNow_DoesNotEvictWhenPersistFails: the manual /compact path obeys
// the same rule. A user asking to shrink the window is not authorising the loss
// of what has not been written down — and it must SAY so, because a bare status
// frame reads as "nothing needed compacting".
func TestCompactNow_DoesNotEvictWhenPersistFails(t *testing.T) {
	st := brokenStore(t)
	fm := einollm.NewFakeModel([]string{"SUMMARY"}, nil)
	srv := &Server{
		store:      st,
		compaction: CompactionConfig{Model: "fm", ContextWindow: 4000, KeepRecent: 1},
	}
	cs := &connSession{perm: &permModeState{}, sessionID: brokenSessionID}
	cs.history = evictableHistory(8)
	before := len(cs.history)

	wc, client, cleanup := newWSPair(t)
	defer cleanup()
	compactNow(context.Background(), srv,
		map[string]model.BaseChatModel{"fm": fm}, wc, cs)

	assert.Len(t, cs.history, before, "manual compaction must not evict unsaved history")

	// The refusal is reported, not silent.
	var sawError bool
	for i := 0; i < 2; i++ {
		var f struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}
		require.NoError(t, client.ReadJSON(&f))
		if f.Type == "error" {
			sawError = true
			assert.Contains(t, f.Text, "could not be saved")
		}
	}
	assert.True(t, sawError, "a refused /compact must tell the user why")
}

// TestCompactNow_EvictsWhenPersistSucceeds is the positive control for the test
// above.
func TestCompactNow_EvictsWhenPersistSucceeds(t *testing.T) {
	st := persistStore(t)
	sid, err := st.CreateSession("s")
	require.NoError(t, err)
	fm := einollm.NewFakeModel([]string{"SUMMARY"}, nil)
	srv := &Server{
		store:      st,
		compaction: CompactionConfig{Model: "fm", ContextWindow: 4000, KeepRecent: 1},
	}
	cs := &connSession{perm: &permModeState{}, sessionID: sid}
	cs.history = evictableHistory(8)
	before := len(cs.history)

	wc, client, cleanup := newWSPair(t)
	defer cleanup()
	_ = client
	compactNow(context.Background(), srv,
		map[string]model.BaseChatModel{"fm": fm}, wc, cs)

	assert.Less(t, len(cs.history), before)
	got, err := st.Messages(sid)
	require.NoError(t, err)
	assert.Len(t, got, before)
}

// TestCompaction_EvictedContentIsRecoverableBySearch closes the C1/C2 loop from
// the operator's point of view: after compaction the text is gone from the live
// window and still findable in the log. Either half alone proves nothing — a
// window that never shrank, or a log nobody can query.
func TestCompaction_EvictedContentIsRecoverableBySearch(t *testing.T) {
	st := persistStore(t)
	sid, err := st.CreateSession("s")
	require.NoError(t, err)
	fm := einollm.NewFakeModel([]string{"SUMMARY"}, nil)
	srv := &Server{
		store:      st,
		compaction: CompactionConfig{Model: "fm", Threshold: 0.05, ContextWindow: 4000, KeepRecent: 1},
	}
	cs := &connSession{perm: &permModeState{}, sessionID: sid}
	cs.history = append([]*schema.Message{
		schema.UserMessage("look into the build please"),
		{Role: schema.Assistant, ToolCalls: []schema.ToolCall{
			toolCall("c1", "shell_run", `{"cmd":"go build ./..."}`)}},
		{Role: schema.Tool, ToolCallID: "c1", ToolName: "shell_run",
			Content: "DISTINCTIVEMARKER completed at line 42"},
	}, evictableHistory(8)[1:]...)

	wc, client, cleanup := newWSPair(t)
	defer cleanup()
	_ = client
	maybeAutoCompact(context.Background(), srv,
		map[string]model.BaseChatModel{"fm": fm}, wc, cs)

	// Gone from the live window...
	for _, m := range cs.history {
		if m != nil {
			assert.NotContains(t, m.Content, "completed at line 42")
		}
	}
	// ...but still in the durable log, and findable.
	hits, err := st.SearchMessages(sid, "DISTINCTIVEMARKER", 0)
	require.NoError(t, err)
	require.NotEmpty(t, hits, "evicted content must remain searchable")
	var sawResult bool
	for _, h := range hits {
		if h.Role == store.RoleToolResult {
			sawResult = true
			assert.Contains(t, h.Content, "completed at line 42")
		}
	}
	assert.True(t, sawResult, "the evicted tool result itself must be recoverable")
}
