package tui

import (
	"bufio"
	"fmt"
	"os"
	"strings"
	"unicode/utf8"

	tea "github.com/charmbracelet/bubbletea"
)

const logsTailLines = 80

// cmdLogs tails the structured log file (set on the status frame as log_path)
// and renders the last logsTailLines lines as a transcript entry. When log_path
// is empty, logs go to stderr — the command reports that instead of guessing a
// path. This keeps the TUI self-contained: runtime logs never corrupt the
// alt-screen render, but the user can still inspect them without leaving the app.
func cmdLogs(m model, _ []string) (tea.Model, tea.Cmd) {
	if m.logPath == "" {
		m.entries = append(m.entries, summaryEntry{
			text: "logs: writing to stderr (no file configured); run `yanshi exec` or `yanshi chat --no-tui` to see them",
		})
		return m, nil
	}
	lines, err := tailFile(m.logPath, logsTailLines)
	if err != nil {
		m.entries = append(m.entries, errorEntry{
			text: fmt.Sprintf("logs: cannot read %s: %v", m.logPath, err),
		})
		return m, nil
	}
	if len(lines) == 0 {
		m.entries = append(m.entries, summaryEntry{
			text: fmt.Sprintf("logs: %s is empty (no log lines yet)", m.logPath),
		})
		return m, nil
	}
	header := fmt.Sprintf("── logs: %s (last %d lines) ──", m.logPath, len(lines))
	m.entries = append(m.entries, summaryEntry{text: header + "\n" + strings.Join(lines, "\n")})
	return m, nil
}

// tailFile reads the last n lines of path. It scans the whole file keeping a
// rolling ring of the most recent n lines — log files are bounded by the
// process lifetime, so a full scan is acceptable and avoids seeking math that
// breaks on CRLF / multi-byte UTF-8.
func tailFile(path string, n int) ([]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	ring := make([]string, 0, n+1)
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		line := sc.Text()
		if !utf8.ValidString(line) {
			line = strings.ToValidUTF8(line, "?")
		}
		ring = append(ring, line)
		if len(ring) > n {
			ring = ring[1:]
		}
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	return ring, nil
}
