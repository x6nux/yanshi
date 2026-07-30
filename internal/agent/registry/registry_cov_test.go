package registry

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestCov_RuntimeChildCtx_LiveParent covers the branch where a non-empty
// parentID maps to a live runtime entry: the parent's ctx is returned so the
// child inherits the parent's cancellation lineage.
func TestCov_RuntimeChildCtx_LiveParent(t *testing.T) {
	m := newTestManager(t, 4)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	m.runtime["parent-1"] = &runtimeAgent{ctx: ctx, done: make(chan struct{})}
	assert.Same(t, ctx, m.runtimeChildCtx("parent-1"))
}
