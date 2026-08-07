package http

import (
	"context"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/x6nux/yanshi/internal/agent/orchestrator"
	"github.com/x6nux/yanshi/internal/guard"
	einollm "github.com/x6nux/yanshi/internal/llm/eino"
	"github.com/x6nux/yanshi/internal/proto"
	"github.com/x6nux/yanshi/internal/skills"
	"github.com/x6nux/yanshi/internal/tools"
)

// TestChatWS_GetStatus proves get_status returns a status frame.
func TestChatWS_GetStatus(t *testing.T) {
	o, err := orchestrator.New(orchestrator.Config{Model: einollm.NewFakeModel([]string{"x"}, nil)})
	require.NoError(t, err)
	srv := New(Config{Token: "t"})
	srv.ChatWS(o, nil, nil)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	c := dial(t, dialWSURL(t, ts))
	defer c.Close()

	require.NoError(t, c.WriteJSON(proto.NewGetStatus()))
	f := readFrame(t, c)
	assert.Equal(t, "status", f.Type)
}

// TestChatWS_FeaturesList proves features_list returns a features_reply even
// when no registry is configured.
func TestChatWS_FeaturesList(t *testing.T) {
	o, err := orchestrator.New(orchestrator.Config{Model: einollm.NewFakeModel([]string{"x"}, nil)})
	require.NoError(t, err)
	srv := New(Config{Token: "t"})
	srv.ChatWS(o, nil, nil)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	c := dial(t, dialWSURL(t, ts))
	defer c.Close()

	require.NoError(t, c.WriteJSON(proto.NewFeaturesList()))
	f := readFrame(t, c)
	assert.Equal(t, "features", f.Type)
}

// TestChatWS_PermissionsList proves permissions_list returns permissions even
// with a nil approval manager (empty list).
func TestChatWS_PermissionsList(t *testing.T) {
	o, err := orchestrator.New(orchestrator.Config{Model: einollm.NewFakeModel([]string{"x"}, nil)})
	require.NoError(t, err)
	srv := New(Config{Token: "t"})
	srv.ChatWS(o, nil, nil)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	c := dial(t, dialWSURL(t, ts))
	defer c.Close()

	require.NoError(t, c.WriteJSON(proto.NewListPermissions()))
	f := readFrame(t, c)
	assert.Equal(t, "permissions", f.Type)
	assert.Empty(t, f.Permissions)
}

// TestChatWS_ListSkillsNoRegistry proves list_skills returns an empty list
// when the skills registry is nil.
func TestChatWS_ListSkillsNoRegistry(t *testing.T) {
	o, err := orchestrator.New(orchestrator.Config{Model: einollm.NewFakeModel([]string{"x"}, nil)})
	require.NoError(t, err)
	srv := New(Config{Token: "t"})
	srv.ChatWS(o, nil, nil)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	c := dial(t, dialWSURL(t, ts))
	defer c.Close()

	require.NoError(t, c.WriteJSON(proto.NewListSkills()))
	f := readFrame(t, c)
	assert.Equal(t, "skills_list", f.Type)
	assert.Empty(t, f.Skills)
}

// TestChatWS_InstallSkillDisabled proves install_skill returns a skill_ack
// with an error text when skills loader/dstRoot are not configured.
func TestChatWS_InstallSkillDisabled(t *testing.T) {
	o, err := orchestrator.New(orchestrator.Config{Model: einollm.NewFakeModel([]string{"x"}, nil)})
	require.NoError(t, err)
	srv := New(Config{Token: "t"})
	srv.ChatWS(o, nil, nil)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	c := dial(t, dialWSURL(t, ts))
	defer c.Close()

	require.NoError(t, c.WriteJSON(proto.NewInstallSkill("some-source")))
	f := readFrame(t, c)
	assert.Equal(t, "skill_ack", f.Type)
	assert.Contains(t, f.Text, "disabled")
}

// TestChatWS_PermissionRevokeNoManager proves permission_revoke returns an
// error when the approval manager is nil.
func TestChatWS_PermissionRevokeNoManager(t *testing.T) {
	o, err := orchestrator.New(orchestrator.Config{Model: einollm.NewFakeModel([]string{"x"}, nil)})
	require.NoError(t, err)
	srv := New(Config{Token: "t"})
	srv.ChatWS(o, nil, nil)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	c := dial(t, dialWSURL(t, ts))
	defer c.Close()

	require.NoError(t, c.WriteJSON(proto.NewRevokePermission("rule-1")))
	f := readFrame(t, c)
	assert.Equal(t, "error", f.Type)
}

// TestChatWS_ListSeamsNoVCS proves list_seams returns empty list when VCS is
// not configured.
func TestChatWS_ListSeamsNoVCS(t *testing.T) {
	o, err := orchestrator.New(orchestrator.Config{Model: einollm.NewFakeModel([]string{"x"}, nil)})
	require.NoError(t, err)
	srv := New(Config{Token: "t"})
	srv.ChatWS(o, nil, nil)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	c := dial(t, dialWSURL(t, ts))
	defer c.Close()

	require.NoError(t, c.WriteJSON(proto.NewListSeams()))
	f := readFrame(t, c)
	assert.Equal(t, "seams", f.Type)
	assert.Empty(t, f.Seams)
}

// TestChatWS_RestoreTurnNoVCS proves restore_turn returns an error when VCS
// is not configured.
func TestChatWS_RestoreTurnNoVCS(t *testing.T) {
	o, err := orchestrator.New(orchestrator.Config{Model: einollm.NewFakeModel([]string{"x"}, nil)})
	require.NoError(t, err)
	srv := New(Config{Token: "t"})
	srv.ChatWS(o, nil, nil)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	c := dial(t, dialWSURL(t, ts))
	defer c.Close()

	require.NoError(t, c.WriteJSON(proto.NewRestoreTurn("seam-1", "head-abc")))
	f := readFrame(t, c)
	assert.Equal(t, "error", f.Type)
}

// TestChatWS_InvalidFrame proves a malformed client frame (empty type) produces
// an error response via the default case.
func TestChatWS_InvalidFrame(t *testing.T) {
	o, err := orchestrator.New(orchestrator.Config{Model: einollm.NewFakeModel([]string{"x"}, nil)})
	require.NoError(t, err)
	srv := New(Config{Token: "t"})
	srv.ChatWS(o, nil, nil)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	c := dial(t, dialWSURL(t, ts))
	defer c.Close()

	require.NoError(t, c.WriteJSON(proto.ClientFrame{}))
	f := readFrame(t, c)
	assert.Equal(t, "error", f.Type)
}

// TestShortHash tests the shortHash helper function.
func TestShortHash(t *testing.T) {
	assert.Equal(t, "abc", shortHash("abc"))
	assert.Equal(t, "12345678", shortHash("12345678"))
	assert.Equal(t, "12345678", shortHash("123456789abcdef0"))
	assert.Equal(t, "", shortHash(""))
}

// TestSkillInfoNil proves skillInfo returns nil for a nil skill pointer.
func TestSkillInfoNil(t *testing.T) {
	assert.Nil(t, skillInfo(nil))
}

// TestSkillInfoValid proves skillInfo converts a valid skill to SkillInfo.
func TestSkillInfoValid(t *testing.T) {
	sk := &skills.Skill{
		Name:        "test-skill",
		Description: "A test skill",
		Source:      "user",
		Enabled:     true,
		Trusted:     false,
	}
	info := skillInfo(sk)
	require.NotNil(t, info)
	assert.Equal(t, "test-skill", info.Name)
	assert.Equal(t, "A test skill", info.Description)
	assert.Equal(t, "user", info.Source)
	assert.True(t, info.Enabled)
	assert.False(t, info.Trusted)
}

// TestAuthorizeControlAction tests the authorizeControlAction helper function.
// It verifies that the function correctly rejects unauthorized control actions.
func TestAuthorizeControlAction(t *testing.T) {
	// Create an orchestrator with an empty profile (no tools allowed).
	o, err := orchestrator.New(orchestrator.Config{
		Model:   einollm.NewFakeModel([]string{"x"}, nil),
		Profile: guard.PermissionProfile{}, // empty = HardDeny
	})
	require.NoError(t, err)
	srv := New(Config{Token: "t"})
	srv.ChatWS(o, nil, nil)

	// authorizeControlAction with an empty profile should deny everything.
	err = srv.authorizeControlAction(context.Background(), "", nil, "shell_run")
	require.Error(t, err)
}

// TestResolvePermissionMode_PlansDenies tests that Plan mode unconditionally
// denies write operations without prompting.
func TestResolvePermissionMode_PlanDenies(t *testing.T) {
	cs := &connSession{perm: &permModeState{}}
	cs.perm.set(guard.ModePlan)
	d, ok := resolvePermissionMode(context.Background(), cs, nil, tools.PermissionRequest{
		Tool: "fs_write",
		Args: `{"path":"/etc/config"}`,
	})
	assert.True(t, ok)
	assert.Equal(t, tools.PermissionDeny, d)
}

// TestResolvePermissionMode_YoloAlwaysAllows tests that YOLO mode always
// allows any non-force/non-approval tool call.
func TestResolvePermissionMode_YoloAlwaysAllows(t *testing.T) {
	cs := &connSession{perm: &permModeState{}}
	cs.perm.set(guard.ModeYOLO)
	d, ok := resolvePermissionMode(context.Background(), cs, nil, tools.PermissionRequest{
		Tool: "shell_run",
		Args: `{"command":"echo hi"}`,
	})
	assert.True(t, ok)
	assert.Equal(t, tools.PermissionAllow, d)
}
