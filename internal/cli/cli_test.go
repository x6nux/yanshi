package cli

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestRun_HeadlessOneTurn exercises Resolve + Send end-to-end without a
// terminal: a fresh root has no lockfile, so the session bootstraps an
// in-process backend (fake model) and runHeadless pumps one query through it.
// The fake model yields one assistant chunk, then the turn ends with done.
func TestRun_HeadlessOneTurn(t *testing.T) {
	dir := t.TempDir()
	opts := Options{FakeModel: true, Root: dir, ConfigPath: writeTestConfig(t, dir)}
	evs, err := runHeadless(context.Background(), opts, []string{"hello"})
	require.NoError(t, err)
	assert.NotEmpty(t, evs)
	assert.Equal(t, "done", evs[len(evs)-1].Kind)
}
