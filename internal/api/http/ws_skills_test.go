package http

import (
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/x6nux/yanshi/internal/agent/orchestrator"
	einollm "github.com/x6nux/yanshi/internal/llm/eino"
	"github.com/x6nux/yanshi/internal/proto"
	"github.com/x6nux/yanshi/internal/skills"
)

func writeWSSkill(t *testing.T, root, name string) {
	t.Helper()
	dir := filepath.Join(root, name)
	require.NoError(t, os.MkdirAll(dir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(
		"---\nname: "+name+"\ndescription: "+name+" desc\n---\n# "+name+"\n"), 0o644))
}

func skillFrameMap(f proto.ServerFrame) map[string]proto.SkillInfo {
	out := make(map[string]proto.SkillInfo, len(f.Skills))
	for _, sk := range f.Skills {
		out[sk.Name] = sk
	}
	return out
}

// TestChatWS_Skills_AllRootsInstallDisableUninstall is the FN1/FN6 handler-
// level regression: bootstrap-equivalent original Loader survives every Reload,
// while canonical actions and Enabled state cross the real WS protocol.
func TestChatWS_Skills_AllRootsInstallDisableUninstall(t *testing.T) {
	builtinRoot := t.TempDir()
	userRoot := t.TempDir()
	pluginRoot := t.TempDir()
	writeWSSkill(t, builtinRoot, "built")
	writeWSSkill(t, userRoot, "personal")
	writeWSSkill(t, pluginRoot, "plug")

	loader := skills.NewLoader(
		skills.Builtin(builtinRoot),
		skills.User(userRoot),
		skills.Plugin("demo", pluginRoot),
	)
	reg, err := loader.Load()
	require.NoError(t, err)
	fixtureRoot, err := filepath.Abs(filepath.Join("..", "..", "skills", "fixtures"))
	require.NoError(t, err)

	o, err := orchestrator.New(orchestrator.Config{
		Model: einollm.NewFakeModel([]string{"ok"}, nil),
	})
	require.NoError(t, err)
	srv := New(Config{
		Token:          "t",
		SkillsRegistry: reg,
		SkillsLoader:   loader,
		SkillsDstRoot:  userRoot,
		SkillsCloner:   &skills.CloneStub{AsRemote: fixtureRoot},
	})
	srv.ChatWS(o, nil, reg)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	c := dial(t, dialWSURL(t, ts))
	defer c.Close()

	require.NoError(t, c.WriteJSON(proto.NewListSkills()))
	listed := readFrame(t, c)
	require.Equal(t, "skills_list", listed.Type)
	byName := skillFrameMap(listed)
	for _, name := range []string{"built", "personal", "plug"} {
		_, ok := byName[name]
		assert.True(t, ok, "initial all-roots list missing %s", name)
	}

	require.NoError(t, c.WriteJSON(proto.NewInstallSkill("github:fake-remote/evil-scripts")))
	ack := readFrame(t, c)
	require.Equal(t, "skill_ack", ack.Type)
	assert.Equal(t, "installed", ack.Action)
	assert.Empty(t, ack.Text)
	require.NotNil(t, ack.Skill)
	assert.Equal(t, "evil-scripts", ack.Skill.Name)
	assert.Equal(t, "user", ack.Skill.Source)

	require.NoError(t, c.WriteJSON(proto.NewListSkills()))
	listed = readFrame(t, c)
	byName = skillFrameMap(listed)
	for _, name := range []string{"built", "personal", "plug", "evil-scripts"} {
		_, ok := byName[name]
		assert.True(t, ok, "post-install all-roots list missing %s", name)
	}

	require.NoError(t, c.WriteJSON(proto.NewDisableSkill("evil-scripts")))
	ack = readFrame(t, c)
	assert.Equal(t, "skill_ack", ack.Type)
	assert.Equal(t, "disabled", ack.Action, "must use explicit canonical map")
	assert.Empty(t, ack.Text)
	require.NotNil(t, ack.Skill)
	assert.False(t, ack.Skill.Enabled)

	require.NoError(t, c.WriteJSON(proto.NewUninstallSkill("evil-scripts")))
	ack = readFrame(t, c)
	assert.Equal(t, "uninstalled", ack.Action)
	assert.Empty(t, ack.Text)

	require.NoError(t, c.WriteJSON(proto.NewListSkills()))
	listed = readFrame(t, c)
	byName = skillFrameMap(listed)
	_, stillInstalled := byName["evil-scripts"]
	assert.False(t, stillInstalled)
	for _, name := range []string{"built", "personal", "plug"} {
		_, ok := byName[name]
		assert.True(t, ok, "post-uninstall all-roots list missing %s", name)
	}
}
