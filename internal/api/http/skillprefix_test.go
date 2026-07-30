package http

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestResolveQuery_SkillInjection(t *testing.T) {
	reg := writeSkillFile(t, t.TempDir(), "hi",
		"---\nname: hi\ndescription: greeting skill\n---\n# Hi\nSay hi.")

	q, errMsg := resolveQuery(reg, "/skill hi build it")
	require.Empty(t, errMsg)
	assert.Contains(t, q, "Say hi.")
	assert.Contains(t, q, "Task: build it")
}

func TestResolveQuery_UnknownSkill(t *testing.T) {
	reg := writeSkillFile(t, t.TempDir(), "hi",
		"---\nname: hi\ndescription: greeting skill\n---\n# Hi\nSay hi.")
	_, errMsg := resolveQuery(reg, "/skill nope")
	assert.Contains(t, errMsg, `unknown skill`)
}

func TestResolveQuery_PlainMessage(t *testing.T) {
	q, errMsg := resolveQuery(nil, "just a normal message")
	require.Empty(t, errMsg)
	assert.Equal(t, "just a normal message", q)
}
