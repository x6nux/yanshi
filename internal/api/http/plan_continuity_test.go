package http

import (
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/cloudwego/eino/schema"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/x6nux/yanshi/internal/agent/orchestrator"
	"github.com/x6nux/yanshi/internal/guard"
	einollm "github.com/x6nux/yanshi/internal/llm/eino"
	"github.com/x6nux/yanshi/internal/proto"
)

// TestPlanThenExecuteKeepsTheHistory covers the confirm-and-switch clause.
//
// /plan-off has a TUI test that the mode flips back. Nothing checked the thing
// the clause is actually about: that the planning turn and the execution turn
// that follows share one conversation. If the mode switch reset cs.history, the
// model would start executing with no memory of the plan it just wrote — and
// every symptom of that is downstream and silent (a worse answer, not an
// error).
//
// FakeModel in Echo mode makes the history observable through the ONLY channel
// a test has here: the reply is the concatenation of every input message, so
// text from the planning turn appearing in the execution turn's answer means it
// was still in the history the model received.
//
// ledger: A2/G05#3 确认后切执行且历史连续
func TestPlanThenExecuteKeepsTheHistory(t *testing.T) {
	const planMarker = "PLAN_TURN_MARKER"

	fm := &einollm.FakeModel{Echo: true}
	o, err := orchestrator.New(orchestrator.Config{
		Model:   fm,
		Profile: guard.PermissionProfile{Tools: guard.ToolsPerm{Allow: []string{"*"}}},
	})
	require.NoError(t, err)

	s := New(Config{Token: "t"})
	s.ChatWS(o, nil, nil)
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	c := dial(t, "ws"+strings.TrimPrefix(ts.URL, "http")+"/api/v1/chat/ws")
	defer c.Close()

	// Turn 1, in plan mode.
	require.NoError(t, c.WriteJSON(proto.NewSetMode("plan", 0)))
	drainUntil(t, c, "status")
	require.NoError(t, c.WriteJSON(proto.NewUserMessage("draft a plan: "+planMarker)))
	drainUntil(t, c, "done")

	// Confirm: back to the ordinary mode.
	require.NoError(t, c.WriteJSON(proto.NewSetMode("default", 0)))
	drainUntil(t, c, "status")

	// Turn 2, executing.
	require.NoError(t, c.WriteJSON(proto.NewUserMessage("now do it")))
	var reply strings.Builder
	for {
		f := readFrame(t, c)
		if f.Type == "agent_chunk" {
			reply.WriteString(f.Text)
		}
		if f.Type == "done" {
			break
		}
	}

	assert.Contains(t, reply.String(), planMarker,
		"the execution turn did not see the planning turn: switching out of plan mode "+
			"dropped the conversation, so the model executes with no memory of the plan "+
			"it just wrote")
	assert.Contains(t, reply.String(), "now do it",
		"the execution turn's own message is missing from what the model received")
}

// TestPlanModeSwitchDoesNotReplayAsANewSession is the other half.
//
// A history that is "continuous" by being replayed from scratch each turn would
// satisfy the assertion above while doubling the context every turn. The
// planning message must appear ONCE.
//
// ledger: A2/G05#3 确认后切执行且历史连续
func TestPlanModeSwitchDoesNotReplayAsANewSession(t *testing.T) {
	const planMarker = "UNIQUE_PLAN_LINE"

	fm := einollm.NewFakeModelWithMessages([]*schema.Message{
		schema.AssistantMessage("ok", nil),
	}, nil)
	fm.RecordMessages = true
	fm.Repeat = true // the same reply every turn; RecordMessages keeps the latest input

	o, err := orchestrator.New(orchestrator.Config{
		Model:   fm,
		Profile: guard.PermissionProfile{Tools: guard.ToolsPerm{Allow: []string{"*"}}},
	})
	require.NoError(t, err)

	s := New(Config{Token: "t"})
	s.ChatWS(o, nil, nil)
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	c := dial(t, "ws"+strings.TrimPrefix(ts.URL, "http")+"/api/v1/chat/ws")
	defer c.Close()

	require.NoError(t, c.WriteJSON(proto.NewSetMode("plan", 0)))
	drainUntil(t, c, "status")
	require.NoError(t, c.WriteJSON(proto.NewUserMessage(planMarker)))
	drainUntil(t, c, "done")

	require.NoError(t, c.WriteJSON(proto.NewSetMode("default", 0)))
	drainUntil(t, c, "status")
	require.NoError(t, c.WriteJSON(proto.NewUserMessage("execute")))
	drainUntil(t, c, "done")

	var seen int
	for _, m := range fm.ReceivedMessages {
		if m != nil && strings.Contains(m.Content, planMarker) {
			seen++
		}
	}
	assert.Equal(t, 1, seen,
		"the planning message appears %d times in the execution turn's history; a mode "+
			"switch that re-appends the transcript doubles the context every turn", seen)
}
