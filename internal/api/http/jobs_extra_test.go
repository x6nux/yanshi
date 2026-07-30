package http

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	"github.com/x6nux/yanshi/internal/proto"
	"github.com/x6nux/yanshi/internal/shell"
)

// TestUnixOrZeroPtr tests the unixOrZeroPtr helper function.
func TestUnixOrZeroPtr(t *testing.T) {
	// nil pointer -> 0
	assert.Equal(t, int64(0), unixOrZeroPtr(nil))

	// non-nil pointer -> unix timestamp
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	result := unixOrZeroPtr(&now)
	assert.Equal(t, now.Unix(), result)

	// zero time -> works
	zero := time.Time{}
	result = unixOrZeroPtr(&zero)
	assert.Equal(t, int64(0), result)
}

// TestJobInfo tests the jobInfo converter function.
func TestJobInfo(t *testing.T) {
	now := time.Now()
	job := shell.Job{
		ID:        "job-1",
		SessionID: "sess-1",
		Command:   "echo hello",
		State:     shell.StateRunning,
		Output:    "hello\n",
		Error:     "",
		ExitCode:  0,
		PID:       1234,
		StartedAt: now,
		EndedAt:   nil,
	}
	info := jobInfo(job)
	assert.Equal(t, "job-1", info.ID)
	assert.Equal(t, "echo hello", info.Command)
	assert.Equal(t, string(shell.StateRunning), info.State)
	assert.Equal(t, "hello\n", info.Output)
	assert.Equal(t, 0, info.ExitCode)
	assert.Equal(t, 1234, info.PID)
	assert.Equal(t, proto.UnixOrZero(now), info.StartedAt)
	assert.Equal(t, int64(0), info.EndedAt)

	// With EndAt set.
	ended := time.Now()
	job.EndedAt = &ended
	info = jobInfo(job)
	assert.Equal(t, ended.Unix(), info.EndedAt)
}
