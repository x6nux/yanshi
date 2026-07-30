package tools

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestWorkRootRoundTrip(t *testing.T) {
	ctx := context.Background()
	assert.Equal(t, "", WorkRootFromContext(ctx), "unbound ctx yields empty root")

	ctx = WithWorkRoot(ctx, "/some/proj")
	assert.Equal(t, "/some/proj", WorkRootFromContext(ctx), "bound root round-trips")
}

func TestWorkRootEmptyAllowed(t *testing.T) {
	// An empty root is stored verbatim (not conflated with "unbound");
	// spillIfTooLong maps "" → ".". This keeps WithWorkRoot callable
	// unconditionally.
	ctx := WithWorkRoot(context.Background(), "")
	assert.Equal(t, "", WorkRootFromContext(ctx))
}
