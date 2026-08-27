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

// skillUseCtx builds a context whose profile allows skill_use.
func skillUseCtx() context.Context {
	return WithProfile(context.Background(), guard.PermissionProfile{
		Tools: guard.ToolsPerm{Allow: []string{"skill_use"}},
	})
}

// writeSkillPack creates root/<name>/SKILL.md with the given frontmatter.
func writeSkillPack(t *testing.T, root, name, frontmatter, body string) {
	t.Helper()
	dir := filepath.Join(root, name)
	require.NoError(t, os.MkdirAll(dir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "SKILL.md"),
		[]byte("---\n"+frontmatter+"\n---\n"+body), 0o644))
}

// withSkillLookPath points skills.LookPath at a PATH the test controls, so the
// outcome does not depend on what the CI runner happens to have installed.
func withSkillLookPath(t *testing.T, present ...string) {
	t.Helper()
	set := map[string]bool{}
	for _, p := range present {
		set[p] = true
	}
	orig := skills.LookPath
	skills.LookPath = func(name string) (string, error) {
		if set[name] {
			return "/fake/bin/" + name, nil
		}
		return "", os.ErrNotExist
	}
	t.Cleanup(func() { skills.LookPath = orig })
}

// TestSkillUse_RefusesSkillWithMissingRequirements is the consumer half of T5:
// hiding the skill from MetaPrompt stops the model CHOOSING it, and this stops
// it LOADING one by a name it already had — from an earlier turn, from a
// prompt baked before the binary was uninstalled (MetaPrompt is snapshotted at
// orchestrator construction, FN3), or from a user typing it.
//
// The refusal is a result, not a Go error: the model must be able to read
// which programs are missing and re-plan within the same turn.
func TestSkillUse_RefusesSkillWithMissingRequirements(t *testing.T) {
	withSkillLookPath(t) // nothing installed
	root := t.TempDir()
	writeSkillPack(t, root, "needs-tool",
		"name: needs-tool\ndescription: d\nrequires:\n  - bin: ast-grep\n  - bin: rg",
		"SECRET BODY THAT MUST NOT LEAK")

	reg, err := skills.NewLoader(skills.Builtin(root)).Load()
	require.NoError(t, err)

	out, err := NewSkillUseTool(reg).InvokableRun(skillUseCtx(), `{"name":"needs-tool"}`)
	require.NoError(t, err, "a missing dependency must not abort the turn")
	assert.Contains(t, out, "ast-grep")
	assert.Contains(t, out, "rg")
	assert.NotContains(t, out, "SECRET BODY",
		"the body must not be delivered when the skill cannot run")
}

// TestSkillUse_LoadsWhenRequirementsAreSatisfied is the negative control: the
// refusal above must be caused by the missing binary, not by the presence of a
// requires: block. Without this, a bug that refused every declaring skill
// would pass the test above.
func TestSkillUse_LoadsWhenRequirementsAreSatisfied(t *testing.T) {
	withSkillLookPath(t, "ast-grep", "rg")
	root := t.TempDir()
	writeSkillPack(t, root, "needs-tool",
		"name: needs-tool\ndescription: d\nrequires:\n  - bin: ast-grep\n  - bin: rg",
		"THE BODY")

	reg, err := skills.NewLoader(skills.Builtin(root)).Load()
	require.NoError(t, err)

	out, err := NewSkillUseTool(reg).InvokableRun(skillUseCtx(), `{"name":"needs-tool"}`)
	require.NoError(t, err)
	assert.Contains(t, out, "THE BODY")
}

// TestSkillUse_UndeclaredSkillIsUnaffected proves the overwhelmingly common
// pack — one that declares nothing — is untouched by this feature. It is the
// regression that would have broken every existing skill in the repo.
func TestSkillUse_UndeclaredSkillIsUnaffected(t *testing.T) {
	withSkillLookPath(t) // nothing installed at all
	root := t.TempDir()
	writeSkillPack(t, root, "plain", "name: plain\ndescription: d", "PLAIN BODY")

	reg, err := skills.NewLoader(skills.Builtin(root)).Load()
	require.NoError(t, err)

	out, err := NewSkillUseTool(reg).InvokableRun(skillUseCtx(), `{"name":"plain"}`)
	require.NoError(t, err)
	assert.Contains(t, out, "PLAIN BODY")
}

// TestSkillUse_DisabledStillTakesPrecedence proves the new check did not
// displace the existing one: an explicitly disabled skill is still refused for
// being disabled, whatever its requirements say.
func TestSkillUse_DisabledStillTakesPrecedence(t *testing.T) {
	withSkillLookPath(t, "gh")
	root := t.TempDir()
	writeSkillPack(t, root, "off", "name: off\ndescription: d\nrequires:\n  - bin: gh", "BODY")
	require.NoError(t, os.WriteFile(filepath.Join(root, "off", ".disabled"), nil, 0o644))

	reg, err := skills.NewLoader(skills.Builtin(root)).Load()
	require.NoError(t, err)

	out, err := NewSkillUseTool(reg).InvokableRun(skillUseCtx(), `{"name":"off"}`)
	require.NoError(t, err)
	assert.Contains(t, out, "disabled")
	assert.NotContains(t, out, "BODY")
}
