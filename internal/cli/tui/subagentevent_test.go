package tui

import (
	"strings"
	"testing"

	"github.com/charmbracelet/bubbles/spinner"
	"github.com/x6nux/yanshi/internal/cli"
)

// TestSubagentEventReachesTheTranscript closes the third instance of the same
// last-hop gap in this chain.
//
// The server has relayed subagent_event since B1 — a per-connection relay on
// WS, the bounded lifecycle relay on SSE — and both transports have a test
// proving the frame goes OUT (internal/api/http::TestChatWS_ForwardsTypedSubagentEvent,
// internal/api/http::TestChatSSE_ForwardsTypedSubagentEventWithSingleWriter).
// Nothing checked that anything received it. StreamEvent had no fields for the
// payload and applyEvent had no branch, so every frame was dropped and a user
// who spawned a sub-agent saw nothing until the parent turn ended.
//
// It matters more now than it did: this session moved agent_spawn, agent_list,
// agent_result, agent_wait, agent_send_input, agent_assign, agent_cancel and
// agent_resume into the factory allow list, so the silence became the default
// experience rather than something an operator opted into.
func TestSubagentEventReachesTheTranscript(t *testing.T) {
	for _, tc := range []struct {
		name string
		ev   cli.StreamEvent
		want []string
	}{
		{
			name: "started",
			ev: cli.StreamEvent{
				Kind: "subagent_event", AgentID: "ag-1", AgentRole: "explore",
				AgentEvent: "started", AgentStatus: "running", Text: "scan the tree",
			},
			want: []string{"ag-1", "explore", "started", "running", "scan the tree"},
		},
		{
			// Role and text are both optional on the wire; the line must still
			// name the agent and what happened to it.
			name: "bare",
			ev: cli.StreamEvent{
				Kind: "subagent_event", AgentID: "ag-2", AgentEvent: "completed",
			},
			want: []string{"ag-2", "completed"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := newTestModel(t)
			before := len(m.entries)
			m = m.applyEvent(tc.ev)
			if len(m.entries) == before {
				t.Fatal("a subagent_event produced no transcript entry: the sub-agent runs invisibly")
			}
			out := m.entries[len(m.entries)-1].render(80, spinner.Model{})
			for _, w := range tc.want {
				if !strings.Contains(out, w) {
					t.Errorf("rendered line omits %q:\n%s", w, out)
				}
			}
		})
	}
}

// TestSubagentEventWithoutAnAgentIDIsIgnored covers the guard. A frame that
// lost its id must not append a line reading "subagent : started", which looks
// like a broken agent rather than a broken frame.
func TestSubagentEventWithoutAnAgentIDIsIgnored(t *testing.T) {
	m := newTestModel(t)
	before := len(m.entries)
	m = m.applyEvent(cli.StreamEvent{Kind: "subagent_event", AgentEvent: "started"})
	if len(m.entries) != before {
		t.Fatalf("an id-less subagent_event appended %d entries", len(m.entries)-before)
	}
}
