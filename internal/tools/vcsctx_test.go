package tools

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestVCSContext_RoundTrip(t *testing.T) {
	scope := VCSScope{RepoID: "repo-1", WorktreeID: "wt-1", Agent: "worker-a"}
	ctx := WithVCS(context.Background(), scope)
	got, ok := VCSScopeFromContext(ctx)
	require.True(t, ok)
	assert.Equal(t, scope, got)
}

func TestVCSContext_Absent(t *testing.T) {
	_, ok := VCSScopeFromContext(context.Background())
	assert.False(t, ok)
}

func TestVCSContext_WithNilVCS(t *testing.T) {
	// A scope with nil VCS is valid (means "tracking not configured"); the
	// fs-tool hook checks sc.VCS != nil before recording.
	scope := VCSScope{RepoID: "repo-1", Agent: "orchestrator"}
	ctx := WithVCS(context.Background(), scope)
	got, ok := VCSScopeFromContext(ctx)
	require.True(t, ok)
	assert.Nil(t, got.VCS)
	assert.Equal(t, "repo-1", got.RepoID)
}
