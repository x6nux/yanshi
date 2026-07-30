package batch

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestCov_SpawnWithRetry_ZeroRetries covers the "retries exhausted" branch:
// with retries=0 the spawn loop never runs, lastErr stays nil, and the
// synthesized "retries exhausted" error is returned (mgr/runner unused).
func TestCov_SpawnWithRetry_ZeroRetries(t *testing.T) {
	_, err := spawnWithRetry(context.Background(), nil, nil, 0, 0)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "retries exhausted")
}
