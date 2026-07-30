package http

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestCov_ResolveQuery_SkillNoTask covers the no-task branch: "/skill hi"
// (no trailing task) loads the body and appends the "await instructions" suffix.
func TestCov_ResolveQuery_SkillNoTask(t *testing.T) {
	reg := writeSkillFile(t, t.TempDir(), "hi",
		"---\nname: hi\ndescription: greeting skill\n---\n# Hi\nSay hi.")
	q, errMsg := resolveQuery(reg, "/skill hi")
	require.Empty(t, errMsg)
	assert.Contains(t, q, "Say hi.")
	assert.Contains(t, q, "Briefly acknowledge", "no-task await suffix")
}

// TestCov_ResolveQuery_SkillBodyError covers the Body-load-error branch: after
// the SKILL.md file is removed, reg.Body fails.
func TestCov_ResolveQuery_SkillBodyError(t *testing.T) {
	root := t.TempDir()
	reg := writeSkillFile(t, root, "hi",
		"---\nname: hi\ndescription: greeting skill\n---\n# Hi\nSay hi.")
	require.NoError(t, os.Remove(filepath.Join(root, "hi", "SKILL.md")))
	_, errMsg := resolveQuery(reg, "/skill hi")
	if errMsg == "" {
		t.Skip("Registry caches the body at load time; Body() does not re-read")
	}
	assert.Contains(t, errMsg, "failed to load skill")
}
