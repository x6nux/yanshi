package tui

import (
	"testing"

	"github.com/charmbracelet/bubbles/spinner"
	"github.com/stretchr/testify/assert"

	"github.com/x6nux/yanshi/internal/cli"
	"github.com/x6nux/yanshi/internal/proto"
)

func TestSessionsEntry_Render(t *testing.T) {
	out := (&sessionsEntry{}).render(80, spinner.Model{})
	assert.Contains(t, out, "(none)")

	se := &sessionsEntry{sessions: []proto.SessionInfo{
		{ID: "s1", Title: "first", MsgCount: 3},
		{ID: "s2", Title: "", MsgCount: 0},
	}}
	out = se.render(80, spinner.Model{})
	assert.Contains(t, out, "s1")
	assert.Contains(t, out, "first")
	assert.Contains(t, out, "(untitled)")
}

func TestStatsEntry_Render(t *testing.T) {
	// No token usage recorded.
	out := (&statsEntry{}).render(80, spinner.Model{})
	assert.Contains(t, out, "no token usage")

	// Populated histogram with bars + summary.
	se := &statsEntry{sessions: []proto.SessionInfo{
		{ID: "s1", Title: "big", TokensIn: 1000, TokensOut: 500},
		{ID: "s2", Title: "", TokensIn: 100, TokensOut: 0},
		{ID: "s3", Title: "zero", TokensIn: 0, TokensOut: 0}, // skipped (no tokens)
	}}
	out = se.render(80, spinner.Model{})
	assert.Contains(t, out, "big")
	assert.Contains(t, out, "(untitled)")
	assert.Contains(t, out, "summary")
	assert.Contains(t, out, "█", "a bar renders for the top consumer")

	// More than 15 sessions -> truncated to 15.
	var many []proto.SessionInfo
	for i := 0; i < 18; i++ {
		many = append(many, proto.SessionInfo{ID: "x", Title: "t", TokensIn: i + 1})
	}
	out = (&statsEntry{sessions: many}).render(80, spinner.Model{})
	assert.Contains(t, out, "summary")
}

func TestRestoreEntry_Render(t *testing.T) {
	out := (&restoreEntry{sessionID: "s1", count: 5}).render(80, spinner.Model{})
	assert.Contains(t, out, "s1")

	out = (&restoreEntry{sessionID: "s1", err: "not found"}).render(80, spinner.Model{})
	assert.Contains(t, out, "not found")
}

func TestModelsEntry_RenderEmpty(t *testing.T) {
	out := (modelsEntry{}).render(80, spinner.Model{})
	assert.Contains(t, out, "(none configured)")
}

func TestFeaturesEntry_RenderEmpty(t *testing.T) {
	out := (featuresEntry{}).render(80, spinner.Model{})
	assert.Contains(t, out, "(none registered)")
}

func TestStatusEntry_RenderVariants(t *testing.T) {
	// Default model/thinking + no cost.
	out := (&statusEntry{}).render(80, spinner.Model{})
	assert.Contains(t, out, "(default)")
	assert.Contains(t, out, "off")
	assert.Contains(t, out, "N/A")

	// Populated with cache + reasoning tokens + known cost.
	se := &statusEntry{
		model: "gpt", thinking: "high",
		tokensIn: 100, tokensOut: 50, turns: 3,
		cachedTokens: 80, reasoningTokens: 20,
		costUSD: 0.00123, costKnown: true,
	}
	out = se.render(80, spinner.Model{})
	assert.Contains(t, out, "gpt")
	assert.Contains(t, out, "high")
	assert.Contains(t, out, "cache 80")
	assert.Contains(t, out, "think 20")
	assert.Contains(t, out, "$0.001230")
}

func TestJobsEntry_Render(t *testing.T) {
	out := (jobsEntry{}).render(80, spinner.Model{})
	assert.Contains(t, out, "(none)")

	out = (jobsEntry{items: []proto.JobInfo{{ID: "j1", PID: 42, State: "running", Command: "go test"}}}).render(80, spinner.Model{})
	assert.Contains(t, out, "j1")
	assert.Contains(t, out, "go test")
}

// TestEvents_Helpers covers the last*Entry nil paths and anyToolRunning /
// discardLastThinking edge cases.
func TestEvents_Helpers(t *testing.T) {
	m := aeModel()
	assert.Nil(t, m.lastSessionsEntry())
	assert.Nil(t, m.lastStatsEntry())
	assert.Nil(t, m.lastRestoreEntry())
	assert.False(t, m.anyToolRunning())

	// discardLastThinking with no live thinking block is a no-op.
	m.discardLastThinking()

	// anyToolRunning true when a tool is running.
	m = m.applyEvent(cli.StreamEvent{Kind: "tool_call", ToolName: "fs_read", ToolStatus: "running"})
	assert.True(t, m.anyToolRunning())
}
