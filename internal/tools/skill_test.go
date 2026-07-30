package tools

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/x6nux/yanshi/internal/guard"
	"github.com/x6nux/yanshi/internal/skills"
)

func TestSkillUse_ReturnsBody(t *testing.T) {
	root := t.TempDir()
	d := filepath.Join(root, "greet")
	require.NoError(t, os.MkdirAll(d, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(d, "SKILL.md"),
		[]byte("---\nname: greet\ndescription: hi\n---\n# Greet\nWave."), 0o644))

	reg, err := skills.NewLoader(skills.Builtin(root)).Load()
	require.NoError(t, err)
	su := NewSkillUseTool(reg)

	ctx := WithProfile(context.Background(), guard.PermissionProfile{
		Tools: guard.ToolsPerm{Allow: []string{"skill_*"}},
	})
	out, err := runTool(ctx, su, `{"name":"greet"}`)
	require.NoError(t, err)
	assert.Contains(t, out, "# Greet")
}

func TestSkillUse_UnknownSkill(t *testing.T) {
	reg, _ := skills.NewLoader(skills.Builtin(t.TempDir())).Load()
	su := NewSkillUseTool(reg)
	ctx := WithProfile(context.Background(), guard.PermissionProfile{
		Tools: guard.ToolsPerm{Allow: []string{"skill_*"}},
	})
	out, err := runTool(ctx, su, `{"name":"nope"}`)
	require.NoError(t, err, "operational error must surface as a result, not a Go error")
	assert.Contains(t, out, "✗")
}

func TestSkillUse_NoProfile(t *testing.T) {
	reg, _ := skills.NewLoader(skills.Builtin(t.TempDir())).Load()
	su := NewSkillUseTool(reg)
	// No permission profile bound: fail-closed.
	out, err := runTool(context.Background(), su, `{"name":"nope"}`)
	require.NoError(t, err, "permission denial must surface as a result, not a Go error")
	assert.Contains(t, out, "✗")
	assert.Contains(t, out, "permission denied")
}

func TestSkillUse_DeniedByProfile(t *testing.T) {
	root := t.TempDir()
	d := filepath.Join(root, "greet")
	require.NoError(t, os.MkdirAll(d, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(d, "SKILL.md"),
		[]byte("---\nname: greet\ndescription: hi\n---\n# Greet\nWave."), 0o644))
	reg, err := skills.NewLoader(skills.Builtin(root)).Load()
	require.NoError(t, err)
	su := NewSkillUseTool(reg)

	// Profile allows only fs_*: skill_use must be denied.
	ctx := WithProfile(context.Background(), guard.PermissionProfile{
		Tools: guard.ToolsPerm{Allow: []string{"fs_*"}},
	})
	out, err := runTool(ctx, su, `{"name":"greet"}`)
	require.NoError(t, err, "permission denial must surface as a result, not a Go error")
	assert.Contains(t, out, "✗")
	assert.Contains(t, out, "permission denied")
}

// TestSkillUse_RejectsDisabledSkill proves the skill_use tool refuses a
// disabled skill with an operational tool result. Reuses runTool (BQ3).
func TestSkillUse_RejectsDisabledSkill(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "gated")
	require.NoError(t, os.MkdirAll(dir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(
		"---\nname: gated\ndescription: gated-desc\n---\n# gated\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, ".disabled"), nil, 0o644))

	loader := skills.NewLoader(skills.Builtin(root))
	r, err := loader.Load()
	require.NoError(t, err)
	su := NewSkillUseTool(r)
	ctx := WithProfile(context.Background(), guard.PermissionProfile{
		Tools: guard.ToolsPerm{Allow: []string{"*"}},
	})
	out, err := runTool(ctx, su, `{"name":"gated"}`)
	require.NoError(t, err, "GuardedTool operational failure should be a result, not a Go error")
	assert.Contains(t, out, "disabled")
}

// TestSkillUse_BadArgs covers the ParseArgs error branch at skill.go:25-27.
// Invalid JSON args surface as an operational error result.
func TestSkillUse_BadArgs(t *testing.T) {
	reg, _ := skills.NewLoader(skills.Builtin(t.TempDir())).Load()
	su := NewSkillUseTool(reg)
	ctx := WithProfile(context.Background(), guard.PermissionProfile{
		Tools: guard.ToolsPerm{Allow: []string{"skill_*"}},
	})
	out, err := runTool(ctx, su, `{not-json}`)
	require.NoError(t, err, "ParseArgs failure must surface as a result, not a Go error")
	assert.Contains(t, out, "✗")
}

// TestSkillUse_BodyReadError covers the reg.Body error branch at skill.go:36-38.
// Delete SKILL.md after Load so Get succeeds but Body re-reads disk and fails.
func TestSkillUse_BodyReadError(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "ghost")
	require.NoError(t, os.MkdirAll(dir, 0o755))
	skillFile := filepath.Join(dir, "SKILL.md")
	require.NoError(t, os.WriteFile(skillFile, []byte(
		"---\nname: ghost\ndescription: gone\n---\n# ghost\n"), 0o644))

	reg, err := skills.NewLoader(skills.Builtin(root)).Load()
	require.NoError(t, err)
	su := NewSkillUseTool(reg)
	ctx := WithProfile(context.Background(), guard.PermissionProfile{
		Tools: guard.ToolsPerm{Allow: []string{"skill_*"}},
	})
	// Remove the skill body after Load so Body re-reads disk → ReadFile fails.
	require.NoError(t, os.Remove(skillFile))
	out, err := runTool(ctx, su, `{"name":"ghost"}`)
	require.NoError(t, err, "Body read failure must surface as a result, not a Go error")
	assert.Contains(t, out, "✗")
}

func assertSentinelAbsent(t *testing.T, path, stage string) {
	t.Helper()
	_, err := os.Stat(path)
	if !os.IsNotExist(err) {
		t.Fatalf("%s: skill script executed or sentinel stat failed: %v", stage, err)
	}
}

// TestInstalledSkill_FullLifecycleNeverExecutesScripts covers the actual E03
// path: Install → Load → skill_use → Trust → Disable → Reload → Enable →
// Reload → ReadFile. The fixture carries forged remote markers and a script
// that would create YANSHI_SKILL_SENTINEL if anything executed it.
func TestInstalledSkill_FullLifecycleNeverExecutesScripts(t *testing.T) {
	sentinel := filepath.Join(t.TempDir(), "skill-script-ran")
	t.Setenv("YANSHI_SKILL_SENTINEL", sentinel)
	fixtureRoot, err := filepath.Abs(filepath.Join("..", "skills", "fixtures"))
	require.NoError(t, err)
	userRoot := t.TempDir()

	name, err := skills.Install("github:fake-remote/evil-scripts", userRoot,
		&skills.CloneStub{AsRemote: fixtureRoot})
	require.NoError(t, err)
	require.Equal(t, "evil-scripts", name)
	assertSentinelAbsent(t, sentinel, "after Install")

	loader := skills.NewLoader(skills.User(userRoot))
	reg, err := loader.Load()
	require.NoError(t, err)
	s, ok := reg.Get(name)
	require.True(t, ok)
	assert.True(t, s.Enabled, "remote .disabled must be purged so default-enabled applies")
	assert.False(t, s.Trusted, "remote .trusted must be purged")
	assertSentinelAbsent(t, sentinel, "after Load")

	ctx := WithProfile(context.Background(), guard.PermissionProfile{
		Tools: guard.ToolsPerm{Allow: []string{"skill_*"}},
	})
	su := NewSkillUseTool(reg)
	out, err := runTool(ctx, su, `{"name":"evil-scripts"}`)
	require.NoError(t, err)
	assert.Contains(t, out, "# Evil Scripts")
	assertSentinelAbsent(t, sentinel, "after skill_use")

	require.NoError(t, reg.Trust(name))
	require.NoError(t, reg.Disable(name))
	require.NoError(t, reg.Reload(loader))
	s, ok = reg.Get(name)
	require.True(t, ok)
	assert.False(t, s.Enabled, "local .disabled must survive Reload")
	assert.True(t, s.Trusted, "local .trusted must survive Reload")
	out, err = runTool(ctx, su, `{"name":"evil-scripts"}`)
	require.NoError(t, err)
	assert.Contains(t, out, "disabled")
	assertSentinelAbsent(t, sentinel, "after Trust+Disable+Reload")

	require.NoError(t, reg.Enable(name))
	require.NoError(t, reg.Reload(loader))
	s, ok = reg.Get(name)
	require.True(t, ok)
	assert.True(t, s.Enabled)
	assert.True(t, s.Trusted)
	body, err := reg.ReadFile(s, "scripts/evil.sh")
	require.NoError(t, err)
	assert.Contains(t, body, "YANSHI_SKILL_SENTINEL")
	assertSentinelAbsent(t, sentinel, "after Enable+Reload+ReadFile")
}
