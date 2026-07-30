package proto

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/x6nux/yanshi/internal/task/work"
)

// TestNewSessionList covers the NewSessionList constructor (frame.go:165) which
// had no caller in tests — it builds a session_list control frame.
func TestNewSessionList(t *testing.T) {
	f := NewSessionList()
	assert.Equal(t, "session_list", f.Type)
}

// TestUnixOrZero covers both arms of unixOrZero (frame.go:653-657) and the
// exported UnixOrZero wrapper (frame.go:662): a zero time serializes as 0, a
// real time as its Unix epoch seconds. The WS handler relies on this so an
// unset ExpiresAt does not surface a negative platform value.
func TestUnixOrZero(t *testing.T) {
	// Zero time → 0 (not the negative value time.Time{}.Unix() yields on some
	// platforms).
	assert.Equal(t, int64(0), UnixOrZero(time.Time{}))
	assert.Equal(t, int64(0), unixOrZero(time.Time{}))

	// Known instant → Unix seconds.
	ts := time.Unix(1_700_000_000, 0).UTC()
	assert.Equal(t, int64(1_700_000_000), UnixOrZero(ts))
	assert.Equal(t, int64(1_700_000_000), unixOrZero(ts))
}

// TestNewTaskUpdate_NonNil covers frame.go:729: NewTaskUpdate with a real task
// builds a task_update frame carrying the snapshot and the task's ID. (The nil
// branch is already covered by TestNewPlanUpdateNilRowsIsNonNilEmptyChecklist.)
func TestNewTaskUpdate_NonNil(t *testing.T) {
	task := &work.WorkTask{ID: "wt-42", Title: "ship it"}
	f := NewTaskUpdate(task)
	assert.Equal(t, "task_update", f.Type)
	assert.Equal(t, "wt-42", f.TaskID)
	if f.Task == nil {
		t.Fatal("Task snapshot must be carried on the frame")
	}
}
