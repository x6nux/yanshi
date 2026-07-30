package http

import (
	"testing"

	"github.com/x6nux/yanshi/internal/shell"
)

// TestJobInfoSnakeCaseAndNoFakeTimes (Task 22): the http jobInfo helper MUST
// preserve zero StartedAt/EndedAt rather than substituting time.Now() for
// "human-friendly" output. Stale jobs (Task 17 RestoreJobs) legitimately
// have zero times; faking them would be a lie.
func TestJobInfoSnakeCaseAndNoFakeTimes(t *testing.T) {
	job := shell.Job{ID: "j-1", SessionID: "s-1", Command: "go test", State: shell.StateRunning, PID: 5}
	info := jobInfo(job)
	if info.EndedAt != 0 {
		t.Fatalf("EndedAt must stay zero, got %d", info.EndedAt)
	}
	if info.StartedAt != 0 {
		t.Fatalf("zero StartedAt must stay zero, got %d", info.StartedAt)
	}
	if info.ID != "j-1" || info.SessionID != "s-1" || info.State != "running" {
		t.Fatalf("fields not propagated: %#v", info)
	}
}
