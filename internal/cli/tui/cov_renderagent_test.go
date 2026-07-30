package tui

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	"github.com/x6nux/yanshi/internal/proto"
)

// TestRenderAgent_ProgressColoring covers the e.progress coloring block: lines
// prefixed "✓ " render green, "✗ " red, and other non-empty lines get a spinner
// prefix. This block runs only when e.progress (tool_chunk Text) is non-empty,
// which the existing nestedProgress tests do not exercise.
func TestRenderAgent_ProgressColoring(t *testing.T) {
	e := &toolEntry{
		name:   "workflow_start",
		args:   `{"goal":"x"}`,
		status: "running",
		nested: true,
		progress: []string{
			"✓ task one done\n",
			"✗ task two failed\n",
			"→ running task three\n",
		},
	}
	out := e.render(80, newSpinner())
	assert.Contains(t, out, "task one done")
	assert.Contains(t, out, "task two failed")
	assert.Contains(t, out, "running task three")
}

// TestRenderAgent_OverwriteHeadWindow covers the progressOverwrite head-window
// path: running + progressOverwrite renders the FIRST 10 lines (headLines), not
// the last 10 (tailLines).
func TestRenderAgent_OverwriteHeadWindow(t *testing.T) {
	var prog []string
	for i := 0; i < 15; i++ {
		prog = append(prog, "agent-"+string(rune('A'+i))+"\n")
	}
	e := &toolEntry{
		name:              "workflow_start",
		status:            "running",
		nested:            true,
		progressOverwrite: true,
		progress:          prog,
	}
	out := e.render(80, newSpinner())
	// Head window: first 10 lines (agent-A .. agent-J) are visible.
	assert.Contains(t, out, "agent-A", "head window shows the top (running) agents")
	assert.NotContains(t, out, "agent-O", "head window drops the tail beyond 10 lines")
}

// TestRenderAgent_NestedTextHiddenInCollapsedRunning is the regression test for
// the "agent response floods the transcript" bug: while a nested agent tool is
// RUNNING, the sub-agent's streamed model output (nestedText — its free-form
// prose answer) must NOT render in the collapsed view. The default transcript
// shows only the sub-agent's TOOL CALLS (nestedProgress) + workflow panel
// (progress); the prose is reserved for ctrl+o expand (see
// TestRenderAgent_NestedTextShownInExpandedRunning). Without this guard a
// chatty analysis agent dumps its whole markdown answer inline.
func TestRenderAgent_NestedTextHiddenInCollapsedRunning(t *testing.T) {
	e := &toolEntry{
		name:       "analysis",
		status:     "running",
		nested:     true,
		nestedText: "The analysis concludes X is safe.",
	}
	out := e.render(80, newSpinner())
	assert.NotContains(t, out, "analysis concludes",
		"collapsed running must NOT dump the sub-agent's prose response")
}

// TestRenderAgent_NestedTextShownInExpandedRunning proves the prose suppressed
// by TestRenderAgent_NestedTextHiddenInCollapsedRunning is still reachable via
// ctrl+o expand — the full text is hidden by default, not discarded.
func TestRenderAgent_NestedTextShownInExpandedRunning(t *testing.T) {
	e := &toolEntry{
		name:       "analysis",
		status:     "running",
		nested:     true,
		nestedText: "The analysis concludes X is safe.",
		expanded:   true,
	}
	out := e.render(80, newSpinner())
	assert.Contains(t, out, "analysis concludes",
		"expanded running must surface the sub-agent's prose")
}

// TestRenderAgent_RunningExpanded covers the running + expanded path (full body,
// no tail cap).
func TestRenderAgent_RunningExpanded(t *testing.T) {
	prog := []string{"step one\n", "step two\n", "step three\n"}
	e := &toolEntry{
		name:     "analysis",
		status:   "running",
		nested:   true,
		progress: prog,
		expanded: true,
	}
	out := e.render(80, newSpinner())
	assert.Contains(t, out, "step one")
	assert.Contains(t, out, "step three")
}

// TestRenderAgent_DoneExpandedWithBody covers the done + expanded + non-empty
// body path (full body PLUS the summary).
func TestRenderAgent_DoneExpandedWithBody(t *testing.T) {
	e := &toolEntry{
		name:           "analysis",
		status:         "ok",
		nested:         true,
		nestedProgress: []string{"did thing one", "did thing two"},
		nestedToolUses: 2,
		expanded:       true,
		startedAt:      time.Now().Add(-2 * time.Minute),
		endedAt:        time.Now(),
	}
	out := e.render(80, newSpinner())
	assert.Contains(t, out, "did thing one")
	assert.Contains(t, out, "2 tool uses")
}

// TestRenderAgent_EmptyBodyRunningReturnsHeader covers the running + empty body
// fast return (header only, no panel).
func TestRenderAgent_EmptyBodyRunningReturnsHeader(t *testing.T) {
	e := &toolEntry{name: "analysis", status: "running", nested: true}
	out := e.render(80, newSpinner())
	// Just the header; no trailing panel.
	assert.NotEmpty(t, out)
	assert.True(t, strings.Count(out, "\n") <= 3, "header-only render is short")
}

// TestRenderAgent_NestedThoughtHiddenInCollapsedRunning proves the sub-agent's
// reasoning (nestedThought) is ALSO suppressed in the collapsed running view,
// not just its prose answer (nestedText). The collapsed view is a tool-call log
// only; a pure-reasoning sub-agent (no tool calls yet) renders just its header
// with the spinner — the activity line elsewhere signals "running". The
// reasoning remains reachable via ctrl+o expand (covered by
// TestToolRenderAgentShowsNestedThought and TestToolRenderAgentProgressPreferredOverThought).
func TestRenderAgent_NestedThoughtHiddenInCollapsedRunning(t *testing.T) {
	e := &toolEntry{
		name:          "analysis",
		status:        "running",
		nested:        true,
		nestedThought: "reasoning about the problem",
	}
	out := e.render(80, newSpinner())
	assert.NotContains(t, out, "reasoning about",
		"collapsed running must NOT show the sub-agent's reasoning")
}

// TestRenderAgent_DoneCollapsedHidesNestedText is a regression guard for the
// DONE path: a resolved (ok) nested agent must not leak its prose answer into
// the collapsed transcript either — only the per-tool lines + done summary
// show. (The running path is covered by
// TestRenderAgent_NestedTextHiddenInCollapsedRunning.)
func TestRenderAgent_DoneCollapsedHidesNestedText(t *testing.T) {
	e := &toolEntry{
		name:           "analysis",
		status:         "ok",
		nested:         true,
		nestedProgress: []string{"Agent(Read) 1 tools"},
		nestedToolUses: 1,
		nestedText:     "The analysis concludes X is safe.",
	}
	out := e.render(80, newSpinner())
	assert.NotContains(t, out, "analysis concludes",
		"collapsed done must NOT dump the sub-agent's prose response")
	assert.Contains(t, out, "Agent(Read)",
		"collapsed done still shows the per-tool activity line")
	assert.Contains(t, out, "Done",
		"collapsed done still shows the summary")
}

// TestRenderAgent_WorkflowPanelSuppressesNestedProgress is the regression test
// for the workflow "raw tool calls flood the panel" bug. In workflow mode the
// sub-agents' raw tool_call frames are forwarded via SubAgentEmit and would
// land in nestedProgress as unattributed "Agent(List) ◌" lines — one per child
// call, with no hint of WHICH sub-agent made them. That duplicates (and
// clutters) the workflow's own per-agent panel (e.progress, e.g.
// "Agent-A1(List) 1 tools …"). When the workflow panel is active
// (progressOverwrite=true) the collapsed RUNNING view must show ONLY the panel;
// the raw per-tool lines stay reachable via ctrl+o expand.
func TestRenderAgent_WorkflowPanelSuppressesNestedProgress(t *testing.T) {
	e := &toolEntry{
		name:              "analysis",
		status:            "running",
		nested:            true,
		progressOverwrite: true,
		progress:          []string{"Agent-A1(List) 1 tools 0 B 0s\n"},
		nestedProgress:    []string{"Agent(List) ◌", "Agent(Glob) ◌"},
	}
	out := e.render(80, newSpinner())
	assert.Contains(t, out, "Agent-A1(List)",
		"the workflow per-agent panel must stay visible")
	assert.NotContains(t, out, "Agent(List) ◌",
		"raw per-tool nestedProgress must be suppressed when the panel is active")
	assert.NotContains(t, out, "Agent(Glob) ◌",
		"raw per-tool nestedProgress must be suppressed when the panel is active")
}

// TestRenderAgent_WorkflowDoneSuppressesNestedProgress covers the DONE path of
// the same rule: a resolved workflow must not list the raw per-tool lines
// either — only the done summary (the panel conveys the per-agent shape while
// running; the summary conveys it once finished).
func TestRenderAgent_WorkflowDoneSuppressesNestedProgress(t *testing.T) {
	e := &toolEntry{
		name:              "analysis",
		status:            "ok",
		nested:            true,
		progressOverwrite: true,
		nestedProgress:    []string{"Agent(List) ◌"},
		nestedAgentsDone:  5,
		nestedAgentsTotal: 5,
	}
	out := e.render(80, newSpinner())
	assert.NotContains(t, out, "Agent(List) ◌",
		"raw per-tool lines must be suppressed in the workflow done view")
	assert.Contains(t, out, "Done",
		"the done summary must still render")
}

// ---- mcpStatusEntry render ----

func TestMcpStatusEntry_Render(t *testing.T) {
	// Empty -> "(none configured)".
	out := mcpStatusEntry{}.render(80, newSpinner())
	assert.Contains(t, out, "(none configured)")

	// Populated servers with each status marker + an error line.
	e := mcpStatusEntry{servers: []proto.MCPServerStatus{
		{Name: "ready-srv", Transport: "stdio", ToolCount: 3, Status: "ready"},
		{Name: "failed-srv", Transport: "http", ToolCount: 0, Status: "failed", Error: "conn refused"},
		{Name: "starting-srv", Transport: "stdio", ToolCount: 1, Status: "starting"},
		{Name: "other-srv", Transport: "stdio", ToolCount: 2, Status: "unknown"},
	}}
	out = e.render(80, newSpinner())
	assert.Contains(t, out, "ready-srv")
	assert.Contains(t, out, "failed-srv")
	assert.Contains(t, out, "conn refused")
	assert.Contains(t, out, "starting-srv")
	assert.Contains(t, out, "other-srv")
}
