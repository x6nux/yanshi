package http

import (
	"testing"

	"github.com/cloudwego/eino/schema"
	"github.com/stretchr/testify/require"

	"github.com/x6nux/yanshi/internal/ctxcompact"
	"github.com/x6nux/yanshi/internal/store"
)

// liveTurn is the in-memory ReAct history for one tool-using exchange: a user
// ask, an assistant message carrying a single tool call, the tool result, and
// the assistant's final answer.
//
// storedTurn derives the durable-log fixture from this via storeMessagesFor —
// the actual production writer (ws_compaction.go) — instead of hand-typing
// store.Message rows. An earlier version of this file hand-typed
// Role: "tool" and put ToolCallID on Role: "assistant", neither of which any
// production writer ever produces (the real vocabulary is
// store.RoleUser/RoleAssistant/RoleToolCall/RoleToolResult), so its 4 tests
// passed against a shape that never occurs in the durable log. Deriving the
// fixture through storeMessagesFor makes that class of drift impossible: the
// fixture is, by construction, whatever production actually writes.
func liveTurn() []*schema.Message {
	return []*schema.Message{
		{Role: schema.User, Content: "list the files"},
		{Role: schema.Assistant, ToolCalls: []schema.ToolCall{{
			ID:       "call_1",
			Function: schema.FunctionCall{Name: "fs_list", Arguments: `{"path":"."}`},
		}}},
		{Role: schema.Tool, Content: "a.go\nb.go", ToolCallID: "call_1", ToolName: "fs_list"},
		{Role: schema.Assistant, Content: "there are two files"},
	}
}

// storedTurn is what the durable log holds after liveTurn() is persisted.
func storedTurn() []store.Message {
	return storeMessagesFor(liveTurn())
}

// parallelLiveTurn is one assistant message carrying two parallel tool calls
// (no prose), followed by both tool results and a final answer — the shape a
// ReAct turn takes when the model asks for two independent tools in a single
// step. storeMessagesFor persists this as two CONSECUTIVE store.RoleToolCall
// rows (its doc comment: "one store.RoleToolCall row per call"), not one row
// carrying both calls.
func parallelLiveTurn() []*schema.Message {
	return []*schema.Message{
		{Role: schema.User, Content: "list the files and check git status"},
		{Role: schema.Assistant, ToolCalls: []schema.ToolCall{
			{ID: "call_1", Function: schema.FunctionCall{Name: "fs_list", Arguments: `{"path":"."}`}},
			{ID: "call_2", Function: schema.FunctionCall{Name: "vcs_status", Arguments: `{}`}},
		}},
		{Role: schema.Tool, Content: "a.go\nb.go", ToolCallID: "call_1", ToolName: "fs_list"},
		{Role: schema.Tool, Content: "clean", ToolCallID: "call_2", ToolName: "vcs_status"},
		{Role: schema.Assistant, Content: "two files, working tree clean"},
	}
}

// plainLiveTurn has no tool calls at all — restoreMessages must leave this
// shape untouched.
func plainLiveTurn() []*schema.Message {
	return []*schema.Message{
		{Role: schema.User, Content: "hello"},
		{Role: schema.Assistant, Content: "hi"},
	}
}

// ledger: A2/W-A-04#1 恢复后的历史包含 tool 角色的消息
func TestRestoreMessagesKeepsToolRole(t *testing.T) {
	got := restoreMessages(storedTurn())

	require.Len(t, got, 4)
	require.Equal(t, schema.User, got[0].Role)
	require.Equal(t, schema.Assistant, got[1].Role)
	require.Equal(t, schema.Tool, got[2].Role,
		"a tool message restored as user makes the model read its own tool output as the operator speaking")
	require.Equal(t, schema.Assistant, got[3].Role)
}

// ledger: A2/W-A-04#2 每条 tool 消息的 ToolCallID 能在同一历史中找到对应的 assistant ToolCalls
func TestRestoreMessagesPairsToolCallsWithResults(t *testing.T) {
	got := restoreMessages(storedTurn())

	calls := map[string]bool{}
	for _, m := range got {
		for _, tc := range m.ToolCalls {
			calls[tc.ID] = true
		}
	}
	require.True(t, calls["call_1"], "the assistant's ToolCalls were dropped on restore")

	for _, m := range got {
		if m.Role != schema.Tool {
			continue
		}
		require.NotEmpty(t, m.ToolCallID, "a tool message without ToolCallID is an orphan")
		require.Truef(t, calls[m.ToolCallID],
			"tool result %q has no matching assistant tool call in the restored history", m.ToolCallID)
	}
}

// ledger: A2/W-A-04#2 每条 tool 消息的 ToolCallID 能在同一历史中找到对应的 assistant ToolCalls
//
// This is the parallel-call sibling of TestRestoreMessagesPairsToolCallsWithResults.
// storeMessagesFor writes N parallel tool calls as N consecutive
// store.RoleToolCall rows. A restore that turned each row back into its own
// one-call assistant message would hand the provider N separate assistant
// messages before any tool result appears — not a shape providers accept (a
// turn's tool_calls must live on ONE assistant message, immediately followed
// by their results). restoreMessages instead merges a run of RoleToolCall
// rows into the single preceding assistant message. This test asserts that
// merge actually happens: both call_1 and call_2 must resolve from the SAME
// restored assistant message, not from two different ones.
func TestRestoreMessagesRegroupsParallelToolCalls(t *testing.T) {
	got := restoreMessages(storeMessagesFor(parallelLiveTurn()))

	require.Len(t, got, 5, "two RoleToolCall rows must merge into one assistant message")
	require.Equal(t, schema.User, got[0].Role)

	require.Equal(t, schema.Assistant, got[1].Role)
	require.Len(t, got[1].ToolCalls, 2, "both parallel calls must land on the same assistant message")
	require.Equal(t, "call_1", got[1].ToolCalls[0].ID)
	require.Equal(t, "call_2", got[1].ToolCalls[1].ID)

	require.Equal(t, schema.Tool, got[2].Role)
	require.Equal(t, "call_1", got[2].ToolCallID)
	require.Equal(t, schema.Tool, got[3].Role)
	require.Equal(t, "call_2", got[3].ToolCallID)

	require.Equal(t, schema.Assistant, got[4].Role)
	require.Equal(t, "two files, working tree clean", got[4].Content)
}

// ledger: A2/W-A-04#3 恢复后的消息序列通过 EnforceToolCallPairs 不产生删除
//
// ctxcompact.EnforceToolCallPairs does not return a trimmed slice: it mutates
// a pinned-index set in place, pulling in an index's counterpart or dropping
// the index as an orphan if the counterpart is missing (see
// internal/ctxcompact/pairs.go). This test pins each side of the pair
// independently — first only the tool result, then only the assistant tool
// call — exactly as ctxcompact.Plan's tail-window heuristic can pin either
// side without the other. If restoreMessages had wired ToolCallID onto one
// side but not matched it to the other (the "restore only half a pair" bug
// this task guards against), the fixpoint would remove the seeded index as
// an orphan instead of pulling in its counterpart, and this test would fail.
func TestRestoreMessagesSurvivesPairEnforcement(t *testing.T) {
	toolIdx, assistantCallIdx := -1, -1
	got := restoreMessages(storedTurn())
	for i, m := range got {
		if m.Role == schema.Tool {
			toolIdx = i
		}
		if len(m.ToolCalls) > 0 {
			assistantCallIdx = i
		}
	}
	require.GreaterOrEqual(t, toolIdx, 0, "fixture must contain a tool message")
	require.GreaterOrEqual(t, assistantCallIdx, 0, "fixture must contain an assistant tool call")

	t.Run("pin tool result only", func(t *testing.T) {
		pinned := map[int]bool{toolIdx: true}
		ctxcompact.EnforceToolCallPairs(got, pinned)
		require.True(t, pinned[toolIdx],
			"the tool result must not be dropped as an orphan when its assistant tool call exists")
		require.True(t, pinned[assistantCallIdx],
			"pinning the tool result must pull in its matching assistant tool call")
	})

	t.Run("pin assistant tool call only", func(t *testing.T) {
		pinned := map[int]bool{assistantCallIdx: true}
		ctxcompact.EnforceToolCallPairs(got, pinned)
		require.True(t, pinned[assistantCallIdx],
			"the assistant tool call must not be dropped as an orphan when its tool result exists")
		require.True(t, pinned[toolIdx],
			"pinning the assistant tool call must pull in its matching tool result")
	})
}

// ledger: A2/W-A-04#4 非工具消息的恢复结果与本改动前逐字节一致
func TestRestoreMessagesPlainTurnIsUnchanged(t *testing.T) {
	got := restoreMessages(storeMessagesFor(plainLiveTurn()))

	require.Len(t, got, 2)
	require.Equal(t, schema.User, got[0].Role)
	require.Equal(t, "hello", got[0].Content)
	require.Empty(t, got[0].ToolCalls)
	require.Equal(t, schema.Assistant, got[1].Role)
	require.Equal(t, "hi", got[1].Content)
	require.Empty(t, got[1].ToolCalls)
}

// ledger: A2/W-A-04#1 恢复后的历史包含 tool 角色的消息
// ledger: A2/W-A-04#2 每条 tool 消息的 ToolCallID 能在同一历史中找到对应的 assistant ToolCalls
//
// applySessionRevertSnapshot (ws_seam.go) is the SECOND call site of the same
// restore defect: fork/reconnect (loadSession) and both restore_turn snapshot
// branches route through it, not through handleRestoreSession. It used to
// carry its own independent role-mapping copy — a binary user/assistant split
// with an Assistant-leaning default, dropping ToolCallID/ToolName/ToolArgs —
// which was the exact same defect restoreMessages exists to fix, just
// duplicated instead of shared. This test exercises applySessionRevertSnapshot
// directly (not restoreMessages) so a regression that reintroduces a private
// copy of the mapping in ws_seam.go — rather than calling restoreMessages —
// is caught here even if ws_handlers.go stays correct.
func TestApplySessionRevertSnapshotKeepsToolTurn(t *testing.T) {
	cs := &connSession{}
	snap := store.SessionRevertSnapshot{
		Meta:     store.SessionSummary{ID: "sess-1", Turns: 1},
		Messages: storedTurn(),
	}

	applySessionRevertSnapshot(cs, snap)

	require.Equal(t, "sess-1", cs.sessionID)
	require.Len(t, cs.history, 4)
	require.Equal(t, schema.User, cs.history[0].Role)
	require.Equal(t, schema.Assistant, cs.history[1].Role)
	require.Len(t, cs.history[1].ToolCalls, 1, "the assistant's tool call must survive the snapshot restore path")
	require.Equal(t, schema.Tool, cs.history[2].Role,
		"a tool row restored via applySessionRevertSnapshot must not fall back to the Assistant-leaning default")
	require.Equal(t, "call_1", cs.history[2].ToolCallID)
	require.Equal(t, schema.Assistant, cs.history[3].Role)
}
