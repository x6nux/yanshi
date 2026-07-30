package registry

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestStatusConstants(t *testing.T) {
	assert.Equal(t, Status("running"), StatusRunning)
	assert.Equal(t, Status("completed"), StatusCompleted)
	assert.Equal(t, Status("failed"), StatusFailed)
	assert.Equal(t, Status("cancelled"), StatusCancelled)
	assert.Equal(t, Status("interrupted"), StatusInterrupted)
	assert.Equal(t, Status("pending"), StatusPending)
}

func TestStatusTerminal(t *testing.T) {
	assert.True(t, StatusCompleted.Terminal())
	assert.True(t, StatusFailed.Terminal())
	assert.True(t, StatusCancelled.Terminal())
	assert.True(t, StatusInterrupted.Terminal())
	assert.False(t, StatusRunning.Terminal())
	assert.False(t, StatusPending.Terminal())
	assert.False(t, Status("").Terminal())
}

func TestUsageAdd(t *testing.T) {
	a := Usage{PromptTokens: 100, CompletionTokens: 20, TotalTokens: 120, ModelCalls: 1}
	b := Usage{PromptTokens: 50, CompletionTokens: 10, TotalTokens: 60, ModelCalls: 2}
	c := a.Add(b)
	assert.Equal(t, int64(150), c.PromptTokens)
	assert.Equal(t, int64(30), c.CompletionTokens)
	assert.Equal(t, int64(180), c.TotalTokens)
	assert.Equal(t, int64(3), c.ModelCalls)
}

func TestSpawnErrCapErrorAs(t *testing.T) {
	err := &SpawnErrCap{Cap: 10}
	var target *SpawnErrCap
	require.True(t, errors.As(err, &target))
	assert.Equal(t, 10, target.Cap)
}

func TestSpawnErrCapError(t *testing.T) {
	err := &SpawnErrCap{Cap: 5}
	assert.Contains(t, err.Error(), "5")
}

func TestRunnerFunc(t *testing.T) {
	var r Runner = RunnerFunc(func(ctx context.Context, agentID, assignment string) (string, error) {
		return "done", nil
	})
	out, err := r.Run(nil, "ag-1", "task")
	require.NoError(t, err)
	assert.Equal(t, "done", out)
}

func TestWaitOptsDefaults(t *testing.T) {
	var opts WaitOpts
	assert.Zero(t, opts.Timeout)
}

func TestCloneRecord(t *testing.T) {
	rec := Record{
		ID: "ag-1", Status: StatusRunning, Prompt: "scan",
		AllowedTools: []string{"fs_read", "shell_run"},
		Usage:        Usage{PromptTokens: 10},
		StartedAt:    time.Now(),
	}
	clone := cloneRecord(rec)
	assert.Equal(t, rec, clone)
	clone.Status = StatusCompleted
	assert.Equal(t, StatusRunning, rec.Status)
}

func TestListResultFields(t *testing.T) {
	lr := ListResult{
		Agents:  []Record{{ID: "a"}},
		Running: 1,
		Limit:   10,
	}
	assert.Len(t, lr.Agents, 1)
	assert.Equal(t, 1, lr.Running)
	assert.Equal(t, 10, lr.Limit)
}
