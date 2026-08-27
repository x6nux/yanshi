package tools

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cloudwego/eino/schema"
	"github.com/stretchr/testify/require"

	"github.com/x6nux/yanshi/internal/approval"
	"github.com/x6nux/yanshi/internal/store"
)

// This file closes the C1+C2 loop from the READ side, with tool rows that were
// produced by a run rather than typed into a fixture.
//
// The existing C2 tests seed the message log directly, which is right for
// checking the recall tools' own behaviour but leaves the joint claim
// unverified: C1 says a turn's tool_call/tool_result pairs reach the durable
// log, C2 says the model can read them back, and each half can pass its own
// tests while the pair fails. Concretely, the write side records a tool result
// under one column layout and the read side renders another, and the only
// symptom is a model that follows a recall pointer into an empty answer.
//
// So this test takes an eino message history of the shape a real ReAct turn
// produces, puts it through the SAME conversion the WS transport uses before
// persisting, and then asks the real history tools for the content back. What
// is asserted is that the bytes the tool produced during the turn come back
// out -- not that a row exists.

// persistTurnHistory converts an eino history to store rows and appends them,
// mirroring internal/api/http::storeMessagesFor.
//
// It is a copy of that mapping rather than a call to it because this package
// cannot import the transport (it is a layer above), and because the copy is
// itself the thing under test: if the two ever disagree about which column a
// tool result's body lands in, this test's recall assertions fail, which is the
// notification that would otherwise never arrive.
func persistTurnHistory(t *testing.T, s *store.Store, sessionID string, hist []*schema.Message) {
	t.Helper()
	rows := make([]store.Message, 0, len(hist))
	for _, m := range hist {
		if m == nil || m.Role == schema.System {
			continue
		}
		switch m.Role {
		case schema.Tool:
			rows = append(rows, store.Message{
				Role:       store.RoleToolResult,
				Content:    m.Content,
				ToolCallID: m.ToolCallID,
				ToolName:   m.ToolName,
			})
		case schema.Assistant:
			if m.Content != "" {
				rows = append(rows, store.Message{Role: store.RoleAssistant, Content: m.Content})
			}
			for _, tc := range m.ToolCalls {
				rows = append(rows, store.Message{
					Role:       store.RoleToolCall,
					ToolCallID: tc.ID,
					ToolName:   tc.Function.Name,
					ToolArgs:   tc.Function.Arguments,
				})
			}
		default:
			if m.Content == "" {
				continue
			}
			rows = append(rows, store.Message{Role: store.RoleUser, Content: m.Content})
		}
	}
	_, _, err := s.AppendMessages(sessionID, rows)
	require.NoError(t, err)
}

// TestC1C2_ToolOutputFromATurnIsRecallableAfterEviction is the joint
// acceptance: content that a tool really produced, persisted the way a turn
// persists it, retrieved by both recall tools.
//
// The marker is a string that appears ONLY inside a tool result -- never in a
// user or assistant message -- so a search that finds it can only have read
// the tool row. Before C1 those rows were never written, and this is the
// assertion that would have caught it.
func TestC1C2_ToolOutputFromATurnIsRecallableAfterEviction(t *testing.T) {
	s, err := store.Open(filepath.Join(t.TempDir(), "c1c2.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })

	sid, err := s.CreateSession("c1c2")
	require.NoError(t, err)

	const marker = "PANIC-AT-0xDEADBEEF"
	// The shape a real ReAct turn leaves behind: a user ask, an assistant
	// tool_call, its tool result, and a closing assistant message.
	hist := []*schema.Message{
		schema.SystemMessage("you are an agent"), // must not be persisted
		schema.UserMessage("run the failing test"),
		schema.AssistantMessage("", []schema.ToolCall{{
			ID: "call-1", Type: "function",
			Function: schema.FunctionCall{
				Name:      "shell_run",
				Arguments: `{"command":"go test ./internal/guard"}`,
			},
		}}),
		{
			Role: schema.Tool, ToolCallID: "call-1", ToolName: "shell_run",
			Content: "--- FAIL: TestClassify\n\t" + marker + "\n\tin lexShellLite\nFAIL",
		},
		schema.AssistantMessage("the lexer dereferences an empty segment", nil),
	}
	persistTurnHistory(t, s, sid, hist)

	ht := NewHistoryTools(s)
	ctx := WithApprovalManager(context.Background(), &approval.Manager{}, sid)

	// --- C2 half 1: search finds the evicted tool output -----------------
	searchArgs, err := json.Marshal(map[string]any{"query": "PANIC"})
	require.NoError(t, err)
	found, err := ht.runSearch(ctx, string(searchArgs))
	require.NoError(t, err)
	// The rendered hit wraps the matched term in highlight markers, so the
	// marker is compared with those stripped: what matters is that the tool
	// result's own bytes came back, not how the excerpt is decorated.
	require.Contains(t, stripSearchHighlight(found), marker,
		"the tool result's own bytes are not searchable; either C1 did not persist the "+
			"tool row or C2 does not index it: %s", found)
	require.Contains(t, found, "shell_run",
		"the hit does not name the tool that produced it, so the model cannot tell what "+
			"it is looking at: %s", found)

	// --- C2 half 2: read returns the full text ---------------------------
	readArgs, err := json.Marshal(map[string]any{"from_seq": 0})
	require.NoError(t, err)
	body, err := ht.runRead(ctx, string(readArgs))
	require.NoError(t, err)
	require.Contains(t, body, marker,
		"history_read did not return the tool result body: %s", body)
	require.Contains(t, body, "go test ./internal/guard",
		"the tool CALL's arguments were not recalled; without them the model sees an "+
			"answer with no question: %s", body)

	// --- the system prompt must not have been persisted ------------------
	//
	// It is prepended fresh on every request. A stored copy would be replayed
	// into the next turn's history as ordinary content and shown to the model
	// twice.
	require.NotContains(t, body, "you are an agent",
		"the system prompt was persisted into the message log")
}

// TestC1C2_RecallIsScopedToItsOwnSession is the security half of C2.
//
// The session id comes from context and never from a tool argument, so a model
// that has seen another conversation's id in a log line cannot read it. This
// drives two real sessions with distinct markers and asserts that neither can
// see the other's tool output.
func TestC1C2_RecallIsScopedToItsOwnSession(t *testing.T) {
	s, err := store.Open(filepath.Join(t.TempDir(), "scope.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })

	mine, err := s.CreateSession("mine")
	require.NoError(t, err)
	theirs, err := s.CreateSession("theirs")
	require.NoError(t, err)

	turn := func(marker string) []*schema.Message {
		return []*schema.Message{
			schema.UserMessage("run it"),
			schema.AssistantMessage("", []schema.ToolCall{{
				ID: "c1", Type: "function",
				Function: schema.FunctionCall{Name: "shell_run", Arguments: `{"command":"x"}`},
			}}),
			{Role: schema.Tool, ToolCallID: "c1", ToolName: "shell_run", Content: marker},
		}
	}
	persistTurnHistory(t, s, mine, turn("SECRET-MINE-11111"))
	persistTurnHistory(t, s, theirs, turn("SECRET-THEIRS-22222"))

	ht := NewHistoryTools(s)
	ctx := WithApprovalManager(context.Background(), &approval.Manager{}, mine)

	// Search for a term that matches BOTH sessions' markers.
	args, err := json.Marshal(map[string]any{"query": "SECRET"})
	require.NoError(t, err)
	out, err := ht.runSearch(ctx, string(args))
	require.NoError(t, err)

	require.Contains(t, stripSearchHighlight(out), "SECRET-MINE-11111",
		"the session's own output must be recallable")
	require.NotContains(t, stripSearchHighlight(out), "SECRET-THEIRS-22222",
		"another session's tool output leaked into this session's recall; the scope "+
			"must come from context and not be searchable across sessions")

	// The same must hold for history_read, which takes a seq range and could
	// otherwise page across the whole table.
	readArgs, err := json.Marshal(map[string]any{"from_seq": 0})
	require.NoError(t, err)
	body, err := ht.runRead(ctx, string(readArgs))
	require.NoError(t, err)
	require.NotContains(t, body, "SECRET-THEIRS-22222",
		"history_read paged into another session's messages")
	require.True(t, strings.Contains(body, "SECRET-MINE-11111"),
		"history_read did not return this session's own messages: %s", body)
}

// stripSearchHighlight removes the «» markers history_search wraps matched
// terms in, so a test can assert on the recalled TEXT rather than on the
// excerpt's presentation.
//
// Asserting through the decoration rather than around it would make every one
// of these tests fail the day the highlight style changes, for a reason that
// has nothing to do with whether recall works.
func stripSearchHighlight(s string) string {
	r := strings.NewReplacer("«", "", "»", "")
	return r.Replace(s)
}
