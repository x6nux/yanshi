package tui

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/x6nux/yanshi/internal/cli"
)

// TestDistillSlashCommandIsRegistered proves /distill is a real, registered
// slash command that sends a distill_memories control frame — not just a
// constructor with no caller (A2/W-A-05: the distillation chain shipped
// complete with zero production callers nine times running before this).
//
// This test lives in internal/cli/tui rather than internal/api/http because
// hasCommand and commandTable are TUI/client concerns; an internal/api/http
// -> internal/cli/tui import would be server->client and GOV1
// (internal/archtest/deps_test.go) rejects it.
//
// ledger: A2/W-A-05#1 /distill 命令在 commandTable 中注册并能触发一次蒸馏
func TestDistillSlashCommandIsRegistered(t *testing.T) {
	require.True(t, hasCommand("distill"), "/distill must be registered in commandTable")

	rec := &recordingSession{}
	m := newModel(rec, "/proj")
	mm, _ := m.runCommand("/distill")
	_ = mm.(model)
	require.Len(t, rec.frames, 1)
	assert.Equal(t, "distill_memories", rec.frames[0].Type)
}

// TestApplyEvent_MemoriesDistilled_RendersAck proves the memories_distilled
// reply is rendered into the transcript (not silently dropped — the frame
// type must be in both isControlReply and model.go's applyEvent switch, or
// it round-trips over the wire but is invisible to the user).
func TestApplyEvent_MemoriesDistilled_RendersAck(t *testing.T) {
	m := newModel(&fakeSession{}, "/proj")
	m = m.applyEvent(cli.StreamEvent{
		Kind: "memories_distilled",
		Text: "distilled: considered 10, merged 3",
	})
	ack, ok := m.entries[len(m.entries)-1].(ackEntry)
	require.True(t, ok)
	assert.Equal(t, "distilled: considered 10, merged 3", ack.text)
}
