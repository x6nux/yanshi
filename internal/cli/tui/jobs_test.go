package tui

import (
	"testing"

	"github.com/charmbracelet/bubbles/spinner"
	"github.com/x6nux/yanshi/internal/cli"
	"github.com/x6nux/yanshi/internal/proto"
)

// TestJobsEntryRendersNoneAndItems (Task 22): the jobsEntry renderer handles
// both the empty list (renders "(none)") and a populated list (renders id,
// pid, state, command per line). Both branches must not panic and must
// produce stable output for the TUI to assert against.
func TestJobsEntryRendersNoneAndItems(t *testing.T) {
	empty := jobsEntry{items: nil}
	if got := empty.render(0, spinner.Model{}); got == "" {
		t.Fatal("empty jobsEntry must render something")
	}

	populated := jobsEntry{items: []proto.JobInfo{
		{ID: "j-1", SessionID: "s-1", Command: "go test", State: "running", PID: 7},
		{ID: "j-2", SessionID: "s-2", Command: "echo hi", State: "exited", PID: 8, ExitCode: 0},
	}}
	got := populated.render(0, spinner.Model{})
	for _, want := range []string{"j-1", "j-2", "go test", "echo hi", "running", "exited"} {
		if !containsString(got, want) {
			t.Fatalf("jobsEntry missing %q in:\n%s", want, got)
		}
	}
}

// TestApplyEventJobsRoutesToJobsEntry (Task 22): applyEvent must route a
// "jobs" StreamEvent to a jobsEntry so /jobs actually renders. The test
// builds a minimal model, applies a jobs event, and asserts the last entry is
// a jobsEntry with the expected items.
func TestApplyEventJobsRoutesToJobsEntry(t *testing.T) {
	rec := &recordingSession{}
	m := newModel(rec, "/proj")
	items := []proto.JobInfo{{ID: "j-1", SessionID: "s-1", Command: "go test", State: "running", PID: 7}}
	m = m.applyEvent(cli.StreamEvent{Kind: "jobs", Jobs: items})
	if len(m.entries) == 0 {
		t.Fatal("jobs event must produce an entry")
	}
	last := m.entries[len(m.entries)-1]
	je, ok := last.(jobsEntry)
	if !ok {
		t.Fatalf("last entry must be jobsEntry, got %T", last)
	}
	if len(je.items) != 1 || je.items[0].ID != "j-1" {
		t.Fatalf("jobsEntry items not propagated: %#v", je.items)
	}
}

// TestApplyEventJobEventRoutesToSummary (Task 22): applyEvent must route a
// "job_event" StreamEvent to a summaryEntry so /jobs read/stdin/cancel acks
// render as a one-line audit.
func TestApplyEventJobEventRoutesToSummary(t *testing.T) {
	rec := &recordingSession{}
	m := newModel(rec, "/proj")
	m = m.applyEvent(cli.StreamEvent{Kind: "job_event", ID: "j-1", ToolStatus: "running", Text: "hello"})
	if len(m.entries) == 0 {
		t.Fatal("job_event must produce an entry")
	}
	last := m.entries[len(m.entries)-1]
	se, ok := last.(summaryEntry)
	if !ok {
		t.Fatalf("last entry must be summaryEntry, got %T", last)
	}
	if se.text == "" {
		t.Fatalf("summaryEntry text must be non-empty")
	}
}

func containsString(haystack, needle string) bool {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}
