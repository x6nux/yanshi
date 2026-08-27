package http

import (
	"testing"

	"github.com/cloudwego/eino/schema"
	"github.com/stretchr/testify/require"

	"github.com/x6nux/yanshi/internal/ctxcompact"
	"github.com/x6nux/yanshi/internal/store"
)

// storedTurn is one persisted ReAct turn: a user ask, an assistant tool call,
// the tool result, and the assistant answer — exactly what the durable log
// holds after a single tool-using exchange.
func storedTurn() []store.Message {
	return []store.Message{
		{Seq: 1, Role: "user", Content: "list the files"},
		{Seq: 2, Role: "assistant", Content: "", ToolCallID: "call_1", ToolName: "fs_list", ToolArgs: `{"path":"."}`},
		{Seq: 3, Role: "tool", Content: "a.go\nb.go", ToolCallID: "call_1", ToolName: "fs_list"},
		{Seq: 4, Role: "assistant", Content: "there are two files"},
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
	got := restoreMessages([]store.Message{
		{Seq: 1, Role: "user", Content: "hello"},
		{Seq: 2, Role: "assistant", Content: "hi"},
	})

	require.Len(t, got, 2)
	require.Equal(t, schema.User, got[0].Role)
	require.Equal(t, "hello", got[0].Content)
	require.Empty(t, got[0].ToolCalls)
	require.Equal(t, schema.Assistant, got[1].Role)
	require.Equal(t, "hi", got[1].Content)
	require.Empty(t, got[1].ToolCalls)
}
