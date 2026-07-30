package registry

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/x6nux/yanshi/internal/guard"
)

func TestRegistry_RegisterAndGet(t *testing.T) {
	r := New()
	r.Register(Entry{Name: "search", Kind: KindLocal, Capabilities: []string{"web_*"}})
	got, ok := r.Get("search")
	require.True(t, ok)
	assert.Equal(t, "search", got.Name)
	assert.Equal(t, KindLocal, got.Kind)
	_, ok = r.Get("nope")
	assert.False(t, ok)
}

func TestRegistry_ByCapability(t *testing.T) {
	r := New()
	r.Register(Entry{Name: "search", Kind: KindLocal, Capabilities: []string{"web_*"}})
	r.Register(Entry{Name: "mem", Kind: KindLocal, Capabilities: []string{"memory_*"}})
	got := r.ByCapability("web_*")
	require.Len(t, got, 1)
	assert.Equal(t, "search", got[0].Name)
}

func TestRegisterExternal(t *testing.T) {
	r := New()
	prof := guard.PermissionProfile{
		Tools: guard.ToolsPerm{Allow: []string{"coding.*"}},
	}
	RegisterExternal(r, "claudecode", "Claude Code via ACP", []string{"coding.*"}, prof)

	got, ok := r.Get("claudecode")
	require.True(t, ok)
	assert.Equal(t, "claudecode", got.Name)
	assert.Equal(t, KindExternal, got.Kind)
	assert.Equal(t, "Claude Code via ACP", got.Description)
	assert.Equal(t, []string{"coding.*"}, got.Capabilities)

	// Should appear in ByCapability results.
	matches := r.ByCapability("coding.*")
	assert.Len(t, matches, 1)
	assert.Equal(t, "claudecode", matches[0].Name)
}
