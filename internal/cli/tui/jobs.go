package tui

import (
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/bubbles/spinner"
	"github.com/x6nux/yanshi/internal/proto"
)

// cmdJobs: /jobs                       → list all shell jobs
//
//	/jobs read <id> [max]      → read job output (max bytes, default 4096)
//	/jobs stdin <id> <text...> → write stdin to a job
//	/jobs cancel <id>          → cancel a job
//
// (Task 22). The list reply arrives as a "jobs" StreamEvent that applyEvent
// routes to jobsEntry; the read reply arrives as a "job_event" StreamEvent
// rendered as a one-line summary.
func cmdJobs(m model, args []string) (tea.Model, tea.Cmd) {
	if len(args) == 0 {
		return m.sendControlFrame(proto.NewJobsList())
	}
	switch args[0] {
	case "read":
		if len(args) < 2 {
			m.entries = append(m.entries, errorEntry{text: "usage: /jobs read <id> [max-bytes]"})
			m.refresh()
			m.viewport.GotoBottom()
			return m, nil
		}
		max := 0
		if len(args) >= 3 {
			fmt.Sscanf(args[2], "%d", &max)
		}
		return m.sendControlFrame(proto.NewJobRead(args[1], max))
	case "stdin":
		if len(args) < 3 {
			m.entries = append(m.entries, errorEntry{text: "usage: /jobs stdin <id> <text>"})
			m.refresh()
			m.viewport.GotoBottom()
			return m, nil
		}
		return m.sendControlFrame(proto.NewJobWrite(args[1], strings.Join(args[2:], " ")))
	case "cancel":
		if len(args) < 2 {
			m.entries = append(m.entries, errorEntry{text: "usage: /jobs cancel <id>"})
			m.refresh()
			m.viewport.GotoBottom()
			return m, nil
		}
		return m.sendControlFrame(proto.NewJobCancel(args[1]))
	}
	m.entries = append(m.entries, errorEntry{text: "usage: /jobs [read <id> [max]|stdin <id> <text>|cancel <id>]"})
	m.refresh()
	m.viewport.GotoBottom()
	return m, nil
}

// jobsEntry renders a /jobs list reply: a header followed by one line per job
// (id, pid, state, command). Empty list renders as "(none)" so the user sees
// an explicit confirmation rather than a blank block.
type jobsEntry struct{ items []proto.JobInfo }

func (e jobsEntry) render(_ int, _ spinner.Model) string {
	if len(e.items) == 0 {
		return doneSummaryStyle.Render("Jobs\n  (none)") + "\n"
	}
	var b strings.Builder
	b.WriteString("Jobs\n")
	for _, item := range e.items {
		fmt.Fprintf(&b, "  %s  pid=%d  %s  %s\n", item.ID, item.PID, item.State, item.Command)
	}
	return doneSummaryStyle.Render(strings.TrimSuffix(b.String(), "\n")) + "\n"
}
