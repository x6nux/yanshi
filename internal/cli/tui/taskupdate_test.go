package tui

import (
	"strings"
	"testing"

	"github.com/charmbracelet/bubbles/spinner"
	"github.com/x6nux/yanshi/internal/cli"
	"github.com/x6nux/yanshi/internal/task/work"
)

// TestTaskUpdateFrameReachesTheTranscript is the last hop of the durable-task
// chain.
//
// The server now broadcasts every durable transition, and the WS backend
// already carried ServerFrame.Task through to StreamEvent — but applyEvent had
// no branch for it, so the frame arrived and was dropped. That is precisely
// the wall plan_update sat behind for the whole of A2 (see
// TestPlanUpdateFrameReachesTheTranscript), one hop further along: state
// correct, persisted, mirrored, broadcast, delivered, invisible.
func TestTaskUpdateFrameReachesTheTranscript(t *testing.T) {
	for _, tc := range []struct {
		name string
		task *work.WorkTask
		want []string
	}{
		{
			name: "titled",
			task: &work.WorkTask{ID: "wt-1", Title: "build the parser", Status: work.StatusRunning},
			want: []string{"wt-1", "build the parser", "running"},
		},
		{
			// A broadcast reads the row back from the store, so a title is
			// normally present — but a frame that lost it must still name the
			// task and its status rather than rendering a bare colon.
			name: "untitled",
			task: &work.WorkTask{ID: "wt-2", Status: work.StatusCompleted},
			want: []string{"wt-2", "completed"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := newTestModel(t)
			before := len(m.entries)
			m = m.applyEvent(cli.StreamEvent{Kind: "task_update", Task: tc.task})
			if len(m.entries) == before {
				t.Fatal("a task_update frame produced no transcript entry: the user still sees the old status")
			}
			out := m.entries[len(m.entries)-1].render(80, spinner.Model{})
			for _, w := range tc.want {
				if !strings.Contains(out, w) {
					t.Errorf("rendered line omits %q:\n%s", w, out)
				}
			}
		})
	}
}

// TestTaskUpdateWithoutATaskIsIgnored covers the nil guard. A frame whose Task
// failed to unmarshal must not panic the TUI, and must not append an entry
// that says nothing — an empty "task :" line reads like a bug in the task, not
// in the frame.
func TestTaskUpdateWithoutATaskIsIgnored(t *testing.T) {
	m := newTestModel(t)
	before := len(m.entries)
	m = m.applyEvent(cli.StreamEvent{Kind: "task_update"})
	if len(m.entries) != before {
		t.Fatalf("a task_update with no payload appended %d entries", len(m.entries)-before)
	}
}
