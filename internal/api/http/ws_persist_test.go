// internal/api/http/ws_persist_test.go
//
// C1: the durable log, and the ordering rule that protects it.
package http

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	einollm "github.com/x6nux/yanshi/internal/llm/eino"
	"github.com/x6nux/yanshi/internal/proto"
	"github.com/x6nux/yanshi/internal/secrets"
	"github.com/x6nux/yanshi/internal/store"
	"github.com/x6nux/yanshi/internal/vcs"
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
	// whole reason the ordering matters. The extra row over `before` is the
	// summary: since INF3 the compacted window is flushed as part of committing
	// the compaction, not left in memory until turn end, because the boundary
	// event is a coordinate into the log and the row it points past has to be
	// there before anything is hidden behind it.
	got, err := st.Messages(sid)
	require.NoError(t, err)
	assert.Len(t, got, before+1)
	assert.Contains(t, got[len(got)-1].Content, "SUMMARY")
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
	// before originals + the summary; see the note in the auto-compaction twin.
	assert.Len(t, got, before+1)
	assert.Contains(t, got[len(got)-1].Content, "SUMMARY")
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

// ---------------------------------------------------------------------------
// INF3 (ADR-0015): the active window survives a reconnect as a PROJECTION
// ---------------------------------------------------------------------------

// msgSig renders the fields that decide whether two windows are the same
// conversation, so a comparison fails on content rather than on a pointer.
func msgSig(m *schema.Message) string {
	if m == nil {
		return "<nil>"
	}
	sig := string(m.Role) + "|" + m.ToolCallID + "|" + m.ToolName + "|" + m.Content
	for _, tc := range m.ToolCalls {
		sig += "|call:" + tc.ID + ":" + tc.Function.Name + ":" + tc.Function.Arguments
	}
	return sig
}

func msgSigs(msgs []*schema.Message) []string {
	out := make([]string, 0, len(msgs))
	for _, m := range msgs {
		out = append(out, msgSig(m))
	}
	return out
}

// compactedFixture drives one real compaction on the persisted path and returns
// the store, the session id and the compacted window. The shape is the same one
// TestMaybeAutoCompact_EvictsWhenPersistSucceeds uses, which is the shape
// measured to actually evict: one user message, a run of plain assistant prose,
// and a closing tool call/result pair.
func compactedFixture(t *testing.T) (*store.Store, string, []*schema.Message, *Server) {
	t.Helper()
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

	wc, client, cleanup := newWSPair(t)
	t.Cleanup(cleanup)
	_ = client
	maybeAutoCompact(context.Background(), srv,
		map[string]model.BaseChatModel{"fm": fm}, wc, cs)
	require.Less(t, len(cs.history), 11, "compaction must actually fire on this fixture")

	cs.persistMessages(srv) // turn end flushes the COMPACTED window
	return st, sid, cs.history, srv
}

// TestReconnectPreservesCompaction: after compaction, restoring the session must
// hand the model the compacted window back, not every original.
//
// C1 made sure the originals get written down; nobody read them through a
// projection, so the restore paths ran a flat Messages() and got the
// pre-compaction originals AND the summary that replaced them. Measured on this
// fixture before the fix: 11 messages in, 4 after compaction, 11 restored — the
// window came back LARGER than it went in, the summary was paid for and thrown
// away, and the next turn compacted the same history again.
//
// The assertion is EQUALITY, message for message (ADR-0015 constraint 5). On
// this fixture the compacted window is [user request, tool call, tool result,
// summary] and the user request sits at seq 0 with eight evicted messages after
// it — a hole in the middle. An earlier cut of the boundary carried a single
// watermark, could only name a suffix, and silently dropped that opening
// request: the model came back from a restore not knowing what it had been
// asked to do. pinned_seqs is what closes the hole.
func TestReconnectPreservesCompaction(t *testing.T) {
	_, sid, compacted, srv := compactedFixture(t)

	fresh := &connSession{perm: &permModeState{}}
	require.NoError(t, fresh.loadSession(srv, sid))

	assertRestoredWindow(t, fresh.history, compacted)
}

// TestRestoreSessionPreservesCompaction covers the same property on the handler
// the TUI and `yanshi exec --resume` actually reach. loadSession above is the
// fork path; restore_session is where a user meets this bug.
func TestRestoreSessionPreservesCompaction(t *testing.T) {
	st, sid, compacted, srv := compactedFixture(t)

	wc, client, cleanup := newWSPair(t)
	defer cleanup()
	_ = client
	fresh := &connSession{perm: &permModeState{}}
	handleRestoreSession(srv, wc, fresh, sid)

	assertRestoredWindow(t, fresh.history, compacted)

	// The durable watermark stays a LOG coordinate even though the window is a
	// subset — commitCompaction derives the next boundary from it.
	all, err := st.Messages(sid)
	require.NoError(t, err)
	assert.Equal(t, len(all), fresh.seq)
	assert.Greater(t, len(all), len(fresh.history),
		"the originals must still be in the log, just out of the window")
}

// assertRestoredWindow holds both restore paths to the same contract: the
// restored window IS the compacted window, message for message.
func assertRestoredWindow(t *testing.T, restored, compacted []*schema.Message) {
	t.Helper()
	// assert, not require: a require here aborts before the pairing check ever
	// runs, and since equality IMPLIES intact pairing (the compacted window is
	// pair-consistent by ctxcompact.EnforceToolCallPairs), that ordering made
	// the second assertion unable to fail at either call site. Both run now, so
	// a broken window reports which of the two properties it broke.
	assert.Equal(t, msgSigs(compacted), msgSigs(restored),
		"the restored window must equal the compacted one message for message: "+
			"anything extra is an evicted message coming back (the bug), anything "+
			"missing is a message ctxcompact.Plan judged least droppable")
	assertToolPairsIntact(t, restored)
}

// rowSigs renders durable rows for comparison, in order.
func rowSigs(rows []store.Message) []string {
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.Role+"|"+r.ToolCallID+"|"+r.ToolName+"|"+r.ToolArgs+"|"+r.Content)
	}
	return out
}

// assertToolPairsIntact: every tool result in the window must have its call in
// the same window (ADR-0015 constraint 5b). A boundary that cuts between them
// leaves an orphan result, and providers reject the whole request rather than
// the one message — so this fails as a hard error at runtime, not as degraded
// quality.
func assertToolPairsIntact(t *testing.T, msgs []*schema.Message) {
	t.Helper()
	seen := map[string]bool{}
	for _, m := range msgs {
		if m == nil {
			continue
		}
		for _, tc := range m.ToolCalls {
			seen[tc.ID] = true
		}
		if m.Role == schema.Tool {
			assert.True(t, seen[m.ToolCallID],
				"tool result %q has no preceding tool call in the window", m.ToolCallID)
		}
	}
}

// labelledHistory is evictableHistory with per-message distinct text.
//
// evictableHistory repeats one string, which is exactly the shape that makes
// the durable log's dedup keys alias: AppendMessages identifies rows by a hash
// that only distinguishes byte-identical siblings by their ordinal within the
// flushed batch, and compaction changes that batch. Any test that appends a
// SECOND round of history needs distinct text, or it is measuring the aliasing
// rather than the boundary.
func labelledHistory(label string, n int) []*schema.Message {
	out := []*schema.Message{schema.UserMessage("start the " + label + " phase")}
	for i := 0; i < n; i++ {
		out = append(out, schema.AssistantMessage(fmt.Sprintf(
			"%s progress note %d, %s", label, i, strings.Repeat("with detail ", 8)), nil))
	}
	return out
}

// TestSecondCompactionAfterRestoreAdvancesTheBoundary: compaction boundaries
// stack, and the second one is computed from a session that was rebuilt from a
// projection rather than grown in memory.
//
// That is the case where the boundary is easiest to get wrong. cs.seq is a LOG
// coordinate — commitCompaction derives the boundary from it — but a restore
// only loads the WINDOW, so deriving cs.seq from the restored slice (as the
// shared snapshot mapper does) would aim the next boundary into the middle of
// history and un-hide everything the first compaction evicted.
//
// It also pins that the PINS survive a second round: the pin list belongs to the
// event, so an undo has to restore the previous event's pins rather than clear
// them.
func TestSecondCompactionAfterRestoreAdvancesTheBoundary(t *testing.T) {
	st, sid, _, srv := compactedFixture(t)

	first, err := st.HiddenSeq(sid)
	require.NoError(t, err)
	require.Greater(t, first, 0, "the first compaction must have set a boundary")
	firstEvents, err := st.ContextEvents(sid)
	require.NoError(t, err)
	require.Len(t, firstEvents, 1)
	firstPins := firstEvents[0].PinnedSeqs
	require.NotEmpty(t, firstPins, "the opening user request sits below the tail and must be pinned")

	// Reconnect, then run another turn's worth of history on top.
	fresh := &connSession{perm: &permModeState{}}
	require.NoError(t, fresh.loadSession(srv, sid))
	rows, err := st.Messages(sid)
	require.NoError(t, err)
	require.Equal(t, len(rows), fresh.seq, "cs.seq must be a log coordinate, not a window length")

	fresh.history = append(fresh.history, labelledHistory("second", 8)...)
	wc, client, cleanup := newWSPair(t)
	defer cleanup()
	_ = client
	fm := einollm.NewFakeModel([]string{"SECOND SUMMARY"}, nil)
	maybeAutoCompact(context.Background(), srv,
		map[string]model.BaseChatModel{"fm": fm}, wc, fresh)
	compacted := fresh.history

	second, err := st.HiddenSeq(sid)
	require.NoError(t, err)
	assert.Greater(t, second, first,
		"a later compaction must move the boundary forward, never back over "+
			"history an earlier one already superseded")

	// A restore after both rounds still reproduces the window exactly.
	again := &connSession{perm: &permModeState{}}
	require.NoError(t, again.loadSession(srv, sid))
	assertRestoredWindow(t, again.history, compacted)

	// Undo pops exactly one layer — back to the first boundary AND its pins.
	require.NoError(t, st.AppendContextEvent(sid, store.ContextEventUndo, 0, nil))
	back, err := st.HiddenSeq(sid)
	require.NoError(t, err)
	assert.Equal(t, first, back)
	restored := &connSession{perm: &permModeState{}}
	require.NoError(t, restored.loadSession(srv, sid))
	for _, seq := range firstPins {
		assert.Contains(t, msgSigs(restored.history), msgSig(&schema.Message{
			Role: schema.User, Content: rows[seq].Content}),
			"undo must restore the previous event's pins, not clear them")
	}
}

// TestWindowBoundary is a direct unit test of the split that decides what a
// restore sees. The previous round's equivalent function had NO test: a review
// probe put `return 0` on its first line and the whole package stayed green,
// because every assertion elsewhere was satisfied by the degenerate
// "summary only" window it produced.
func TestWindowBoundary(t *testing.T) {
	for _, tc := range []struct {
		name        string
		kept        []int
		logTop      int
		flushedFrom int
		wantHidden  int
		wantPinned  []int
	}{{
		// The measured production shape: an opening user request at seq 0, eight
		// evicted messages, then the kept tool pair and the summary.
		name: "hole in the middle", kept: []int{0, 9, 10, 11}, logTop: 12, flushedFrom: 11,
		wantHidden: 9, wantPinned: []int{0},
	}, {
		// A clean suffix needs no pins at all — the range says everything.
		name: "contiguous tail", kept: []int{7, 8, 9}, logTop: 10, flushedFrom: 9,
		wantHidden: 7, wantPinned: nil,
	}, {
		// Nothing was evicted: hidden 0 means "the whole log is the window", and
		// ProjectWindow then runs the plain unbounded query.
		name: "everything kept", kept: []int{0, 1, 2}, logTop: 3, flushedFrom: 2,
		wantHidden: 0, wantPinned: nil,
	}, {
		// The fail-safe. A lookup that resolved nothing must not put hidden at
		// the log top, which would project an EMPTY window; it clamps to where
		// the post-compaction flush began, so the summary is always included.
		name: "lookup found nothing", kept: nil, logTop: 12, flushedFrom: 11,
		wantHidden: 11, wantPinned: nil,
	}, {
		// Scattered pins stay scattered; only the contiguous run at the top
		// becomes the range.
		name: "several pins", kept: []int{0, 3, 4, 20, 21}, logTop: 22, flushedFrom: 21,
		wantHidden: 20, wantPinned: []int{0, 3, 4},
	}} {
		t.Run(tc.name, func(t *testing.T) {
			hidden, pinned := windowBoundary(tc.kept, tc.logTop, tc.flushedFrom)
			assert.Equal(t, tc.wantHidden, hidden)
			assert.Equal(t, tc.wantPinned, pinned)

			// The property the two fields exist for: together they select
			// exactly `kept` and nothing else.
			selected := map[int]bool{}
			for _, s := range tc.kept {
				if s >= hidden {
					selected[s] = true
				}
			}
			for _, s := range pinned {
				selected[s] = true
			}
			for _, s := range tc.kept {
				assert.True(t, selected[s], "seq %d was kept but the boundary drops it", s)
			}
			for s := hidden; s < tc.logTop; s++ {
				if len(tc.kept) > 0 {
					assert.Contains(t, tc.kept, s,
						"seq %d is inside the range but was not in the window", s)
				}
			}
		})
	}
}

// TestRestorePreservesOrderWithDuplicateMessages is the constraint-5 regression
// for message IDENTITY, and it is the shape that broke the previous design.
//
// The window here contains byte-identical assistant prose — saying "continue"
// twice is an entirely ordinary conversation — plus a tool call and its result.
// The old boundary located the window's rows by dedup key, whose only
// discriminator between identical siblings is their ordinal within the flushed
// batch, and compaction is exactly the operation that changes that batch. A
// survivor therefore resolved to an EARLIER twin's seq, and since the projection
// is ordered by seq the window came back in a different order than it went out.
//
// Order is not cosmetic here. A review constructed the case where the shifted
// row is a tool_result landing ahead of its tool_call: providers reject that
// request outright rather than degrading it, so the session simply stops working
// after a restore.
func TestRestorePreservesOrderWithDuplicateMessages(t *testing.T) {
	st := persistStore(t)
	sid, err := st.CreateSession("s")
	require.NoError(t, err)
	fm := einollm.NewFakeModel([]string{"SUMMARY"}, nil)
	srv := &Server{
		store:      st,
		compaction: CompactionConfig{Model: "fm", Threshold: 0.05, ContextWindow: 4000, KeepRecent: 2},
	}
	cs := &connSession{perm: &permModeState{}, sessionID: sid}

	// Byte-identical prose on both sides of the evicted region, so a
	// content-addressed boundary has two candidates for each survivor.
	dup := "continue " + strings.Repeat("and more detail ", 8)
	cs.history = []*schema.Message{schema.UserMessage("kick off the work")}
	cs.history = append(cs.history, schema.AssistantMessage(dup, nil))
	cs.history = append(cs.history, labelledHistory("middle", 6)...)
	cs.history = append(cs.history,
		schema.AssistantMessage(dup, nil),
		&schema.Message{Role: schema.Assistant,
			ToolCalls: []schema.ToolCall{toolCall("c1", "shell_run", `{"cmd":"go test ./..."}`)}},
		&schema.Message{Role: schema.Tool, ToolCallID: "c1", ToolName: "shell_run",
			Content: "ok " + strings.Repeat("and more detail ", 8)})
	before := len(cs.history)

	wc, client, cleanup := newWSPair(t)
	defer cleanup()
	_ = client
	maybeAutoCompact(context.Background(), srv,
		map[string]model.BaseChatModel{"fm": fm}, wc, cs)
	require.Less(t, len(cs.history), before, "compaction must fire on this fixture")
	compacted := cs.history
	cs.persistMessages(srv)

	fresh := &connSession{perm: &permModeState{}}
	require.NoError(t, fresh.loadSession(srv, sid))

	// Asserted at the ROW layer, which is where a boundary operates and where
	// the aliasing showed up. The message layer cannot carry this assertion for
	// this fixture: storeMessagesFor splits an assistant's prose and its tool
	// calls into separate rows, and restoreMessages re-joins adjacent ones into
	// a single message, so a window that happened to hold them as two messages
	// legitimately comes back as one. That regrouping preserves content, order
	// and pairing; a shuffled survivor preserves none of them, and this
	// comparison still fails on it.
	assert.Equal(t, rowSigs(storeMessagesFor(compacted)), rowSigs(storeMessagesFor(fresh.history)),
		"duplicate messages must not shuffle the restored window: a boundary "+
			"derived from content cannot tell two identical messages apart, and "+
			"resolves the survivor to its earlier twin's position")
	assertToolPairsIntact(t, fresh.history)
}

// TestAssertToolPairsIntactCanFail keeps the constraint-5(b) checker honest.
//
// The checker is only worth having if it fails on the shape it exists for, and
// at its other two call sites it cannot: they compare the whole window for
// equality first, and an equal window is pair-consistent by construction. This
// hands it the orphan directly.
func TestAssertToolPairsIntactCanFail(t *testing.T) {
	orphan := []*schema.Message{
		{Role: schema.Tool, ToolCallID: "c1", ToolName: "shell_run", Content: "result"},
		{Role: schema.Assistant, ToolCalls: []schema.ToolCall{toolCall("c1", "shell_run", "{}")}},
	}
	var spy testing.T
	assertToolPairsIntact(&spy, orphan)
	assert.True(t, spy.Failed(),
		"a tool result ahead of its call is the exact shape providers reject; "+
			"the checker must not pass it")

	ok := []*schema.Message{
		{Role: schema.Assistant, ToolCalls: []schema.ToolCall{toolCall("c1", "shell_run", "{}")}},
		{Role: schema.Tool, ToolCallID: "c1", ToolName: "shell_run", Content: "result"},
	}
	var clean testing.T
	assertToolPairsIntact(&clean, ok)
	assert.False(t, clean.Failed(), "a properly ordered pair must pass")
}

// TestCompleteToolPairs covers the boundary-level half of constraint 5(b): the
// kept set is repaired before it is stored, whatever produced it.
//
// The case it exists for is real rather than defensive. ctxcompact.FoldToolResults
// runs after Assemble and REPLACES the tool results it rewrites, so a folded
// result fails the pointer-identity test that decides what survived — its call
// would be pinned without it, and the window would restore with an orphan call.
func TestCompleteToolPairs(t *testing.T) {
	rows := []store.Message{
		{Seq: 0, Role: store.RoleUser, Content: "go"},
		{Seq: 1, Role: store.RoleToolCall, ToolCallID: "c1", ToolName: "shell_run"},
		{Seq: 2, Role: store.RoleToolResult, ToolCallID: "c1", ToolName: "shell_run"},
		{Seq: 3, Role: store.RoleToolResult, ToolCallID: "", ToolName: "shell_run"},
	}
	assert.Equal(t, []int{1, 2}, completeToolPairs([]int{1}, rows),
		"a kept call must bring its result")
	assert.Equal(t, []int{1, 2}, completeToolPairs([]int{2}, rows),
		"a kept result must bring its call")
	assert.Equal(t, []int{0}, completeToolPairs([]int{0}, rows),
		"a non-tool row pulls nothing in")
	assert.Equal(t, []int{3}, completeToolPairs([]int{3}, rows),
		"an empty tool_call_id cannot be paired up, and guessing would be how an "+
			"orphan gets manufactured rather than avoided")
	assert.Nil(t, completeToolPairs(nil, rows))
}

// ---------------------------------------------------------------------------
// INF3 round 3: the cross-layer invariant, asserted directly
// ---------------------------------------------------------------------------

// assertWindowMatchesLog checks the invariant every boundary calculation rests
// on — THE PROJECTION IS THE ACTIVE WINDOW — using the same predicate production
// gates on, so the test cannot drift from the check it is about.
//
// Content is compared separately and only when the session has no redactor. A
// redacted session's live window legitimately holds the raw secret while its
// rows hold the mask; that difference is the feature, and the invariant the
// boundary needs is structural.
func assertWindowMatchesLog(t *testing.T, st *store.Store, sid string, hist []*schema.Message, redacted bool) {
	t.Helper()
	proj, err := st.ProjectWindow(sid)
	require.NoError(t, err)
	rows := storeMessagesFor(hist)
	require.Equal(t, len(rows), len(proj),
		"the live window has %d rows but projects to %d: every boundary this "+
			"package computes assumes these are the same list", len(rows), len(proj))
	assert.True(t, alignedWithLog(hist, proj),
		"the window and its projection diverged structurally")
	if !redacted {
		assert.Equal(t, rowSigs(proj), rowSigs(rows))
	}
}

// TestWindowMatchesLogAcrossShapes pins the invariant in every shape a session
// reaches it from. It was previously asserted nowhere, and two of these five
// were measured broken.
func TestWindowMatchesLogAcrossShapes(t *testing.T) {
	t.Run("fresh compaction", func(t *testing.T) {
		st, sid, compacted, _ := compactedFixture(t)
		assertWindowMatchesLog(t, st, sid, compacted, false)
	})

	t.Run("after a reconnect", func(t *testing.T) {
		st, sid, _, srv := compactedFixture(t)
		fresh := &connSession{perm: &permModeState{}}
		require.NoError(t, fresh.loadSession(srv, sid))
		assertWindowMatchesLog(t, st, sid, fresh.history, false)
	})

	t.Run("after a second compaction", func(t *testing.T) {
		st, sid, _, srv := compactedFixture(t)
		fresh := &connSession{perm: &permModeState{}}
		require.NoError(t, fresh.loadSession(srv, sid))
		fresh.history = append(fresh.history, labelledHistory("second", 8)...)
		wc, client, cleanup := newWSPair(t)
		defer cleanup()
		_ = client
		fm := einollm.NewFakeModel([]string{"SECOND SUMMARY"}, nil)
		maybeAutoCompact(context.Background(), srv,
			map[string]model.BaseChatModel{"fm": fm}, wc, fresh)
		assertWindowMatchesLog(t, st, sid, fresh.history, false)
	})

	t.Run("a fork of a compacted session", func(t *testing.T) {
		st, sid, _, srv := compactedFixture(t)
		forkID, err := st.ForkSession(sid, -1)
		require.NoError(t, err)
		forked := &connSession{perm: &permModeState{}}
		require.NoError(t, forked.loadSession(srv, forkID))
		assertWindowMatchesLog(t, st, forkID, forked.history, false)

		// And it survives the fork's first flush, which is where the inherited
		// boundary used to be overwritten by a duplicated window.
		forked.persistMessages(srv)
		assertWindowMatchesLog(t, st, forkID, forked.history, false)
	})

	t.Run("a session with a redactor", func(t *testing.T) {
		// The dedup key used to be derived from the RAW text while the row
		// stored the masked text, so a reconnect — which rebuilds the window
		// from the masked rows — hashed something the log had never seen and
		// re-inserted the entire window. Measured growth per reconnect: 2, 3,
		// 4, 5. bootstrap installs a redactor unconditionally, so this is the
		// ordinary configuration rather than an exotic one.
		st := persistStore(t)
		red := secrets.NewRedactor()
		red.Register("sk-live-abcdef123456")
		st.SetRedactor(red)
		sid, err := st.CreateSession("s")
		require.NoError(t, err)
		srv := &Server{store: st}
		cs := &connSession{perm: &permModeState{}, sessionID: sid}
		cs.history = []*schema.Message{
			schema.UserMessage("here is my key sk-live-abcdef123456, use it"),
			schema.AssistantMessage("noted, I will not echo it", nil),
		}
		cs.persistMessages(srv)

		rows, err := st.Messages(sid)
		require.NoError(t, err)
		require.Len(t, rows, 2)
		require.NotContains(t, rows[0].Content, "sk-live-abcdef123456",
			"the secret must not reach the database")

		// Three reconnect + flush cycles. Each one used to add a full copy.
		for i := 0; i < 3; i++ {
			next := &connSession{perm: &permModeState{}}
			require.NoError(t, next.loadSession(srv, sid))
			next.persistMessages(srv)
			rows, err = st.Messages(sid)
			require.NoError(t, err)
			require.Len(t, rows, 2, "reconnect %d duplicated the log", i+1)
			assertWindowMatchesLog(t, st, sid, next.history, true)
		}
	})
}

// TestCompactionRefusedWhenWindowAndLogDisagree is ADR-0015 constraint 6, driven
// through the real WS path by the conversation that actually causes it.
//
// flushHistory de-duplicates against the WHOLE log, including rows already
// hidden behind a boundary. So a model that repeats a sentence byte-identical to
// an evicted one has that sentence dropped by ON CONFLICT and never written: the
// window has a row the log does not. Compacting from there would compute a
// boundary against the wrong positions, and the previous behaviour — carry on
// and write a boundary with no pins — restored the SUMMARY ALONE, five messages
// down to one.
//
// The required behaviour is to refuse: the context stays oversized but complete,
// which is the direction C1 already chose for a failed flush.
func TestCompactionRefusedWhenWindowAndLogDisagree(t *testing.T) {
	st, sid, compacted, srv := compactedFixture(t)
	hidden, err := st.HiddenSeq(sid)
	require.NoError(t, err)
	require.Greater(t, hidden, 0)

	rows, err := st.Messages(sid)
	require.NoError(t, err)
	var evicted string
	for _, r := range rows {
		if r.Seq < hidden && r.Role == store.RoleAssistant && r.Content != "" {
			evicted = r.Content
			break
		}
	}
	require.NotEmpty(t, evicted, "the fixture must have evicted some assistant prose")

	// Enough new history that a second compaction WOULD fire — the control
	// below proves it does — and then the model says the same thing again, word
	// for word. Without the size, this test passes for the wrong reason: an
	// under-threshold window leaves history and events untouched whether or not
	// anything refuses, which is exactly how the first version of this test
	// stayed green under a probe that deleted the check it was meant to pin.
	cs := &connSession{perm: &permModeState{}}
	require.NoError(t, cs.loadSession(srv, sid))
	cs.history = append(cs.history, labelledHistory("second", 8)...)
	cs.history = append(cs.history, schema.AssistantMessage(evicted, nil))
	cs.persistMessages(srv)

	proj, err := st.ProjectWindow(sid)
	require.NoError(t, err)
	require.Less(t, len(proj), len(storeMessagesFor(cs.history)),
		"the repeated line must have been swallowed by dedup — that is the trigger")

	before := append([]*schema.Message(nil), cs.history...)
	beforeEvents, err := st.ContextEvents(sid)
	require.NoError(t, err)

	wc, client, cleanup := newWSPair(t)
	defer cleanup()
	_ = client
	fm := einollm.NewFakeModel([]string{"SECOND SUMMARY"}, nil)
	maybeAutoCompact(context.Background(), srv,
		map[string]model.BaseChatModel{"fm": fm}, wc, cs)

	assert.Equal(t, msgSigs(before), msgSigs(cs.history),
		"a compaction that cannot locate its window must leave the window alone")
	afterEvents, err := st.ContextEvents(sid)
	require.NoError(t, err)
	assert.Len(t, afterEvents, len(beforeEvents),
		"no boundary may be written when the positions it would be built from "+
			"are not trustworthy")

	// And the restored window is still the conversation, not a lone summary.
	again := &connSession{perm: &permModeState{}}
	require.NoError(t, again.loadSession(srv, sid))
	assert.GreaterOrEqual(t, len(again.history), len(compacted),
		"refusing leaves the context oversized but complete; it must never "+
			"shrink to the summary alone")
	assertToolPairsIntact(t, again.history)

	// POSITIVE CONTROL. The same session, the same amount of new history, but
	// nothing repeated verbatim — so the window and the log still agree and the
	// compaction must go through. Without this, "history unchanged" above is
	// equally satisfied by a fixture too small to compact at all.
	ctl, ctlSid, _, ctlSrv := compactedFixture(t)
	ctlCS := &connSession{perm: &permModeState{}}
	require.NoError(t, ctlCS.loadSession(ctlSrv, ctlSid))
	ctlCS.history = append(ctlCS.history, labelledHistory("second", 8)...)
	ctlCS.history = append(ctlCS.history, schema.AssistantMessage(
		"a line nothing else said "+strings.Repeat("and more detail ", 8), nil))
	ctlCS.persistMessages(ctlSrv)
	ctlBefore := len(ctlCS.history)

	wc2, client2, cleanup2 := newWSPair(t)
	defer cleanup2()
	_ = client2
	maybeAutoCompact(context.Background(), ctlSrv,
		map[string]model.BaseChatModel{"fm": einollm.NewFakeModel([]string{"CTL"}, nil)}, wc2, ctlCS)

	require.Less(t, len(ctlCS.history), ctlBefore,
		"the control must actually compact, or the refusal above proves nothing")
	ctlEvents, err := ctl.ContextEvents(ctlSid)
	require.NoError(t, err)
	assert.Len(t, ctlEvents, 2, "the control's second boundary must be recorded")
}

// TestAlignedWithLog and TestKeptWindowSeqsRefusesMisalignment cover the two
// guards individually.
//
// They exist because the integration test above cannot distinguish them: each
// guard alone is sufficient, so a probe that deletes either one leaves the other
// catching the same case and the package stays green. That is the right
// behaviour and the wrong coverage, so each gets a direct test.
func TestAlignedWithLog(t *testing.T) {
	hist := []*schema.Message{
		schema.UserMessage("go"),
		{Role: schema.Assistant, ToolCalls: []schema.ToolCall{toolCall("c1", "shell_run", "{}")}},
		{Role: schema.Tool, ToolCallID: "c1", ToolName: "shell_run", Content: "done"},
	}
	rows := storeMessagesFor(hist)
	log := make([]store.Message, len(rows))
	for i, r := range rows {
		log[i] = store.Message{Seq: i, Role: r.Role, ToolCallID: r.ToolCallID, ToolName: r.ToolName,
			Content: r.Content, ToolArgs: r.ToolArgs}
	}
	assert.True(t, alignedWithLog(hist, log))

	// Content may legitimately differ: the store redacts on write, and refusing
	// on that would refuse every compaction of every session holding a secret.
	masked := append([]store.Message(nil), log...)
	masked[0].Content = "[redacted]"
	assert.True(t, alignedWithLog(hist, masked), "redacted content is not a misalignment")

	// A row the log never received — the measured shape, where the model repeats
	// a line byte-identical to an evicted one and dedup swallows it.
	assert.False(t, alignedWithLog(hist, log[:len(log)-1]), "a missing row is a misalignment")

	// Structure differing at the same length.
	shuffled := append([]store.Message(nil), log...)
	shuffled[1].Role = store.RoleUser
	assert.False(t, alignedWithLog(hist, shuffled), "a role mismatch is a misalignment")
}

func TestKeptWindowSeqsRefusesMisalignment(t *testing.T) {
	hist := []*schema.Message{
		schema.UserMessage("go"),
		schema.AssistantMessage("working", nil),
	}
	rows := storeMessagesFor(hist)
	log := make([]store.Message, len(rows))
	for i, r := range rows {
		log[i] = store.Message{Seq: i, Role: r.Role, Content: r.Content}
	}

	kept, ok := keptWindowSeqs(hist, hist, log)
	require.True(t, ok)
	assert.Equal(t, []int{0, 1}, kept)

	// Short log: indexing by a window-derived position would run off the end and
	// panic, taking the WS connection with it. It must report failure instead —
	// and NOT report success with no pins, which would send windowBoundary to
	// the post-compaction flush and restore the summary alone.
	kept, ok = keptWindowSeqs(hist, hist, log[:1])
	assert.False(t, ok, "a short log must be refused, not indexed into")
	assert.Nil(t, kept)

	// Long log: the window does not account for every row, so positions past the
	// window are unexplained and the mapping is not trustworthy either.
	kept, ok = keptWindowSeqs(hist, hist, append(append([]store.Message(nil), log...),
		store.Message{Seq: 2, Role: store.RoleUser, Content: "extra"}))
	assert.False(t, ok, "an unexplained trailing row must be refused")
	assert.Nil(t, kept)
}

// ---------------------------------------------------------------------------
// INF3 round 4: the refusal is visible, and a revert truncates at a ROW seq
// ---------------------------------------------------------------------------

// TestRefusedCompactionIsVisibleOnStatus is the other half of ADR-0015
// constraint 6. "Prefer an oversized context over lost content" is only the safe
// choice while the oversized context can be SEEN: auto-compaction refuses on the
// same silent path an under-threshold turn takes, so a session stuck refusing
// grows without bound and the first thing anyone observes is a provider length
// error, which reads like an unrelated failure.
func TestRefusedCompactionIsVisibleOnStatus(t *testing.T) {
	st, sid, _, srv := compactedFixture(t)
	hidden, err := st.HiddenSeq(sid)
	require.NoError(t, err)
	rows, err := st.Messages(sid)
	require.NoError(t, err)
	var evicted string
	for _, r := range rows {
		if r.Seq < hidden && r.Role == store.RoleAssistant && r.Content != "" {
			evicted = r.Content
			break
		}
	}
	require.NotEmpty(t, evicted)

	cs := &connSession{perm: &permModeState{}}
	require.NoError(t, cs.loadSession(srv, sid))
	cs.history = append(cs.history, labelledHistory("second", 8)...)
	cs.history = append(cs.history, schema.AssistantMessage(evicted, nil))
	cs.persistMessages(srv)

	wc, client, cleanup := newWSPair(t)
	defer cleanup()
	fm := einollm.NewFakeModel([]string{"SECOND SUMMARY"}, nil)
	maybeAutoCompact(context.Background(), srv,
		map[string]model.BaseChatModel{"fm": fm}, wc, cs)

	require.NotEmpty(t, cs.compactionBlocked,
		"a refused compaction must record why")

	// The client is told at the moment it is decided, not several turns later.
	// The refusal happens after the summary has already streamed, so the
	// compact_chunk deltas come first.
	var f proto.ServerFrame
	// D-1: a deadline, so a MISSING frame fails the test instead of hanging it.
	// Without one the refusal-not-reported probe blocks forever in ReadJSON and
	// the run reports a timeout on the whole package, which names no assertion.
	require.NoError(t, client.SetReadDeadline(time.Now().Add(10*time.Second)))
	for i := 0; i < 50; i++ {
		require.NoError(t, client.ReadJSON(&f))
		if f.Type == "status" {
			break
		}
	}
	require.Equal(t, "status", f.Type, "a refusal must still produce a status frame")
	assert.NotEmpty(t, f.CompactionBlocked,
		"the status frame must carry the refusal, or the oversized context is "+
			"invisible and 'prefer oversized' stops being safe")
	assert.False(t, f.Compacted, "a refusal must not also claim it compacted")

	// And it PERSISTS: nothing retries a refusal, so every later status frame
	// has to keep saying so.
	later := cs.statusFrame(srv)
	assert.Equal(t, f.CompactionBlocked, later.CompactionBlocked)

	// A later attempt that does NOT refuse must clear it, or the warning becomes
	// noise nobody reads. Both entry points are checked, because they clear at
	// different places and a probe on either one has to be caught.
	for _, tc := range []struct {
		name string
		run  func(srv *Server, conn *wsConn, cs *connSession, models map[string]model.BaseChatModel)
	}{
		{"auto", func(srv *Server, conn *wsConn, cs *connSession, ms map[string]model.BaseChatModel) {
			maybeAutoCompact(context.Background(), srv, ms, conn, cs)
		}},
		{"manual /compact", func(srv *Server, conn *wsConn, cs *connSession, ms map[string]model.BaseChatModel) {
			compactNow(context.Background(), srv, ms, conn, cs)
		}},
	} {
		t.Run(tc.name+" clears a stale warning", func(t *testing.T) {
			_, sid2, _, srv2 := compactedFixture(t)
			clean := &connSession{perm: &permModeState{}}
			require.NoError(t, clean.loadSession(srv2, sid2))
			clean.compactionBlocked = "stale, from an earlier attempt"
			clean.history = append(clean.history, labelledHistory("third", 8)...)
			clean.persistMessages(srv2)

			wc2, client2, cleanup2 := newWSPair(t)
			defer cleanup2()
			_ = client2
			tc.run(srv2, wc2, clean,
				map[string]model.BaseChatModel{"fm": einollm.NewFakeModel([]string{"OK"}, nil)})

			assert.Empty(t, clean.compactionBlocked)
			assert.Empty(t, clean.statusFrame(srv2).CompactionBlocked,
				"a stale warning must not outlive the attempt that cleared it")
		})
	}
}

// TestTruncationSeqCountsRowsNotMessages pins the message-count / row-count
// distinction that a seam's HistoryLen silently crossed.
//
// THE FIXTURE IS THE TEST. An assistant carrying tool calls expands to several
// durable rows, so a window of N messages is more than N rows — and a fixture
// without tool calls makes the two numbers equal and proves nothing. Passing the
// message count as a row seq truncated at the wrong place, and since round 2 the
// same number also decides which compaction boundaries get compensated, so it
// popped the wrong boundaries too.
func TestTruncationSeqCountsRowsNotMessages(t *testing.T) {
	st := persistStore(t)
	sid, err := st.CreateSession("s")
	require.NoError(t, err)
	srv := &Server{store: st}
	cs := &connSession{perm: &permModeState{}, sessionID: sid}
	cs.history = []*schema.Message{
		schema.UserMessage("go"), // row 0
		{Role: schema.Assistant, Content: "checking", // rows 1 and 2
			ToolCalls: []schema.ToolCall{toolCall("c1", "shell_run", `{"cmd":"ls"}`)}},
		{Role: schema.Tool, ToolCallID: "c1", ToolName: "shell_run", Content: "a.go"}, // row 3
		schema.AssistantMessage("done", nil),                                          // row 4
	}
	cs.persistMessages(srv)

	rows, err := st.Messages(sid)
	require.NoError(t, err)
	require.Len(t, rows, 5, "4 messages expand to 5 rows — that gap is the bug")

	// Keeping the first 2 messages keeps 3 rows, so truncation starts at seq 3.
	// The old code passed 2, deleting a row it had to keep.
	got, err := truncationSeq(srv, cs, 2)
	require.NoError(t, err)
	assert.Equal(t, 3, got,
		"the boundary is a ROW seq; 2 is the message count and would delete the "+
			"tool result belonging to a message the revert keeps")

	assertSeq := func(msgs, wantSeq int) {
		t.Helper()
		n, err := truncationSeq(srv, cs, msgs)
		require.NoError(t, err)
		assert.Equal(t, wantSeq, n)
	}
	assertSeq(0, 0) // keep nothing
	assertSeq(1, 1) // keep the user message
	assertSeq(3, 4) // keep through the tool result
	assertSeq(4, 5) // keep everything: delete from past the end

	// End to end: the revert keeps exactly the rows the message boundary named.
	_, err = st.TruncateSessionForRevert(sid, 3, 1)
	require.NoError(t, err)
	rows, err = st.Messages(sid)
	require.NoError(t, err)
	require.Len(t, rows, 3)
	assert.Equal(t, store.RoleToolCall, rows[2].Role,
		"the kept assistant message's tool call must survive with it")
}

// TestTruncationSeqRefusesAMisalignedWindow: this one deletes rows, so a window
// that no longer matches the log must refuse rather than guess a position.
func TestTruncationSeqRefusesAMisalignedWindow(t *testing.T) {
	st := persistStore(t)
	sid, err := st.CreateSession("s")
	require.NoError(t, err)
	srv := &Server{store: st}
	cs := &connSession{perm: &permModeState{}, sessionID: sid}
	cs.history = []*schema.Message{schema.UserMessage("go")}
	cs.persistMessages(srv)

	// A message the log never received — the A-1 shape.
	cs.history = append(cs.history, schema.AssistantMessage("never written", nil))

	_, err = truncationSeq(srv, cs, 1)
	require.Error(t, err, "a guessed truncation point deletes the wrong rows irreversibly")
	assert.Contains(t, err.Error(), "no longer line up")
}

// TestKeptWindowSeqsCompletesToolPairs guards the PRODUCTION call site of
// completeToolPairs (ADR-0015 constraint 5(b)).
//
// The helper had its own unit test, but deleting its single call inside
// keptWindowSeqs left the whole repository green — so the pairing repair was
// only ever exercised through a function nothing in production had to call.
//
// The situation it exists for is reachable: ctxcompact.FoldToolResults runs
// after Assemble and REPLACES the tool results it rewrites, so a folded result
// fails the pointer-identity test that decides what survived while its call
// still passes. That leaves a pinned tool_call with no result, and providers
// reject the whole request rather than the one message. This drives that shape
// straight in — newHist keeps the assistant's call but not the tool message.
func TestKeptWindowSeqsCompletesToolPairs(t *testing.T) {
	call := &schema.Message{Role: schema.Assistant,
		ToolCalls: []schema.ToolCall{toolCall("c1", "shell_run", `{"cmd":"ls"}`)}}
	result := &schema.Message{Role: schema.Tool, ToolCallID: "c1",
		ToolName: "shell_run", Content: "a.go"}
	oldHist := []*schema.Message{schema.UserMessage("go"), call, result}

	rows := storeMessagesFor(oldHist)
	log := make([]store.Message, len(rows))
	for i, r := range rows {
		log[i] = store.Message{Seq: i, Role: r.Role, ToolCallID: r.ToolCallID,
			ToolName: r.ToolName, ToolArgs: r.ToolArgs, Content: r.Content}
	}

	// The fold replaced the result, so only the call survives identity.
	kept, ok := keptWindowSeqs(oldHist, []*schema.Message{call}, log)
	require.True(t, ok)
	assert.Equal(t, []int{1, 2}, kept,
		"a surviving tool_call must drag its result into the boundary; pinning "+
			"seq 1 alone restores an orphan call and the provider rejects the turn")
}

// TestRefusedCompactionWritesNoSummaryRow guards the ORDER of the alignment
// check, which is the half of constraint 6 with the worst failure mode.
//
// The verdict is the same either way — a later structural check also refuses —
// so a test that only asserts "refused" cannot see this. What differs is that
// checking after the flush has ALREADY WRITTEN THE SUMMARY to the log. That row
// is not in the window and never can be, so alignedWithLog fails from then on
// and this session can never compact again. One trigger, permanent lock.
func TestRefusedCompactionWritesNoSummaryRow(t *testing.T) {
	st, sid, _, srv := compactedFixture(t)
	hidden, err := st.HiddenSeq(sid)
	require.NoError(t, err)
	rows, err := st.Messages(sid)
	require.NoError(t, err)
	var evicted string
	for _, r := range rows {
		if r.Seq < hidden && r.Role == store.RoleAssistant && r.Content != "" {
			evicted = r.Content
			break
		}
	}
	require.NotEmpty(t, evicted)

	cs := &connSession{perm: &permModeState{}}
	require.NoError(t, cs.loadSession(srv, sid))
	cs.history = append(cs.history, labelledHistory("second", 8)...)
	cs.history = append(cs.history, schema.AssistantMessage(evicted, nil))
	cs.persistMessages(srv)

	before, err := st.Messages(sid)
	require.NoError(t, err)

	wc, client, cleanup := newWSPair(t)
	defer cleanup()
	_ = client
	maybeAutoCompact(context.Background(), srv,
		map[string]model.BaseChatModel{"fm": einollm.NewFakeModel([]string{"SUMMARY2"}, nil)}, wc, cs)

	after, err := st.Messages(sid)
	require.NoError(t, err)
	require.Len(t, after, len(before),
		"a refused compaction must not leave its summary in the log: that row "+
			"can never enter the window, so the alignment check fails forever "+
			"and this session is locked out of compaction permanently")
	for _, r := range after {
		assert.NotContains(t, r.Content, "SUMMARY2")
	}
}

// TestRestoreTurnTruncatesAtARowSeq guards the WIRING of truncationSeq into
// handleRestoreTurn.
//
// The conversion and its refusal are unit-tested, but restoring the handler's
// argument to truncLen — the whole bug this round fixed — left the repository
// green, because nothing drove the handler far enough to perform a truncation.
//
// THE FIXTURE IS THE TEST, again: the window's second message carries a tool
// call, so 3 messages expand to 4 rows. Reverting to a 2-message boundary must
// keep 3 rows. Passing the message count keeps 2 and destroys the tool call
// belonging to a message the revert is keeping — and since the round-2 change,
// the same number also decides which compaction boundaries get compensated.
func TestRestoreTurnTruncatesAtARowSeq(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "repo")
	require.NoError(t, os.MkdirAll(root, 0o755))
	st, err := store.Open(filepath.Join(base, "state.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = st.Close() })

	v := vcs.New(st, filepath.Join(base, "worktrees"))
	repoID, err := v.InitRepo(root)
	require.NoError(t, err)
	f := filepath.Join(root, "a.txt")
	require.NoError(t, os.WriteFile(f, []byte("v0"), 0o644))
	require.NoError(t, v.RecordEditMain(repoID, "test", f, []byte("v0")))
	_, err = v.CommitMain(repoID, "test", "seed")
	require.NoError(t, err)

	sid, err := st.CreateSession("s")
	require.NoError(t, err)
	srv := &Server{store: st, vcs: v, repoID: repoID}
	cs := &connSession{perm: &permModeState{}, sessionID: sid, turns: 2}
	cs.history = []*schema.Message{
		schema.UserMessage("go"), // row 0
		{Role: schema.Assistant, Content: "checking", // rows 1 and 2
			ToolCalls: []schema.ToolCall{toolCall("c1", "shell_run", `{"cmd":"ls"}`)}},
		{Role: schema.Tool, ToolCallID: "c1", ToolName: "shell_run", Content: "a.go"}, // row 3
	}
	cs.persistMessages(srv)
	rows, err := st.Messages(sid)
	require.NoError(t, err)
	require.Len(t, rows, 4, "3 messages expand to 4 rows — that gap is the bug")

	// A seam whose HistoryLen is 2 MESSAGES.
	seamID, err := v.SealMainTurnSeam(repoID, sid, 1, 2, vcs.SeamPreTurn, "turn 1")
	require.NoError(t, err)

	wc, client, cleanup := newWSPair(t)
	defer cleanup()
	_ = client
	handleRestoreTurn(srv, wc, cs, seamID, srv.fullHead())

	got, err := st.Messages(sid)
	require.NoError(t, err)
	require.Len(t, got, 3,
		"a 2-MESSAGE boundary is 3 rows; keeping 2 would delete the tool call "+
			"belonging to the assistant message the revert keeps")
	assert.Equal(t, store.RoleToolCall, got[2].Role)
	assert.Equal(t, 3, cs.seq, "cs.seq is a log coordinate, not a message count")
}
