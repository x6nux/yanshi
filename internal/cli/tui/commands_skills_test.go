package tui

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/x6nux/yanshi/internal/cli"
	"github.com/x6nux/yanshi/internal/proto"
)

// TestCommand_Skills_SendsListSkills proves /skills sends a list_skills frame.
//
// ledger: C3/E03#4 模型可 load 匹配技能
func TestCommand_Skills_SendsListSkills(t *testing.T) {
	rec := &recordingSession{}
	m := newModel(rec, "/proj")
	mm, _ := m.runCommand("/skills")
	_ = mm.(model)
	require.Len(t, rec.frames, 1)
	assert.Equal(t, "list_skills", rec.frames[0].Type)
}

// TestCommand_SkillInstall_SendsInstallSkill proves /skill install github:o/r
// sends an install_skill frame.
func TestCommand_SkillInstall_SendsInstallSkill(t *testing.T) {
	rec := &recordingSession{}
	m := newModel(rec, "/proj")
	mm, _ := m.runCommand("/skill install github:owner/repo")
	_ = mm.(model)
	require.Len(t, rec.frames, 1)
	assert.Equal(t, "install_skill", rec.frames[0].Type)
	assert.Equal(t, "github:owner/repo", rec.frames[0].Source)
}

// TestCommand_SkillTrust_SendsTrustSkill proves /skill trust foo sends a
// trust_skill frame.
func TestCommand_SkillTrust_SendsTrustSkill(t *testing.T) {
	rec := &recordingSession{}
	m := newModel(rec, "/proj")
	mm, _ := m.runCommand("/skill trust foo")
	_ = mm.(model)
	require.Len(t, rec.frames, 1)
	assert.Equal(t, "trust_skill", rec.frames[0].Type)
	assert.Equal(t, "foo", rec.frames[0].Name)
}

// TestCommand_Skill_RejectsUnknownSubcommand proves /skill foo (unknown
// subcommand) renders a local error.
func TestCommand_Skill_RejectsUnknownSubcommand(t *testing.T) {
	rec := &recordingSession{}
	m := newModel(rec, "/proj")
	mm, _ := m.runCommand("/skill frobnicate")
	m = mm.(model)
	assert.Empty(t, rec.frames, "unknown subcommand must not send a frame")
}

// TestApplyEvent_SkillAck_RendersEntry proves skill_ack pulls name only from
// Action/Text/Skill; there is no StreamEvent.Name (CB4).
func TestApplyEvent_SkillAck_RendersEntry(t *testing.T) {
	m := newModel(&fakeSession{}, "/proj")
	before := len(m.entries)
	m = m.applyEvent(cli.StreamEvent{
		Kind:   "skill_ack",
		Action: "installed",
		Skill:  &proto.SkillInfo{Name: "my-skill", Enabled: true},
	})
	require.Len(t, m.entries, before+1)
	ack, ok := m.entries[len(m.entries)-1].(ackEntry)
	require.True(t, ok)
	assert.Contains(t, ack.text, "my-skill")
	assert.Contains(t, ack.text, "installed")
	assert.Contains(t, ack.text, "restart", "baked MetaPrompt requires honest restart hint (FN3)")
}

func TestApplyEvent_SkillAck_ErrorUsesText(t *testing.T) {
	m := newModel(&fakeSession{}, "/proj")
	m = m.applyEvent(cli.StreamEvent{Kind: "skill_ack", Action: "enabled", Text: "boom"})
	_, ok := m.entries[len(m.entries)-1].(errorEntry)
	assert.True(t, ok, "non-empty Text is a mutation error, must render as errorEntry")
}
