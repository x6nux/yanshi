package tui

import (
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/x6nux/yanshi/internal/cli"
)

// TestCov_Update_MouseNonLeft covers the mouse-button switch default (a non-Left
// button is a no-op return).
func TestCov_Update_MouseNonLeft(t *testing.T) {
	m := newModel(&fakeSession{}, "/proj")
	mm, _ := m.Update(tea.MouseMsg{Button: tea.MouseButtonMiddle, X: 0, Y: 0})
	assert.NotNil(t, mm)
}

// TestCov_Update_ToastTickRearm covers the toast-tick re-arm branch: while
// toasts remain after pruning, Update re-arms the tick. The returned cmd wraps
// a closure (the cover block) that only executes when the cmd fires, so we
// invoke it to cover the closure body.
func TestCov_Update_ToastTickRearm(t *testing.T) {
	m := newModel(&fakeSession{}, "/proj")
	m.pushToast("info", "persists past one tick")
	mm, cmd := m.Update(toastTickMsg{})
	assert.NotNil(t, mm)
	require.NotNil(t, cmd, "tick re-armed while toasts remain")
	msg := cmd() // fire the tick cmd → executes its closure
	_, ok := msg.(toastTickMsg)
	assert.True(t, ok, "the tick closure yields a toastTickMsg")
}

// TestCov_ApplyEvent_StandaloneToolResult covers the standalone tool_result
// branch (a result with no preceding tool_call — e.g. out-of-order delivery).
func TestCov_ApplyEvent_StandaloneToolResult(t *testing.T) {
	m := newModel(&fakeSession{}, "/proj")
	m = m.applyEvent(cli.StreamEvent{Kind: "tool_result", ToolName: "fs_read", Text: "late arrival", ToolStatus: "ok"})
	require.NotEmpty(t, m.entries, "a standalone entry was appended")
}

// TestCov_ApplyEvent_NestedResultSegments covers the nested tool_result summary
// segments: tokens ("Nk") and duration ("> 0"), computed when a child resolves.
func TestCov_ApplyEvent_NestedResultSegments(t *testing.T) {
	m := newModel(&fakeSession{}, "/proj")
	m = m.applyEvent(cli.StreamEvent{Kind: "tool_call", ToolName: "analysis", ToolStatus: "running"})
	m = m.applyEvent(cli.StreamEvent{Kind: "tool_call", ToolName: "fs_read", ToolStatus: "running"}) // child → nestedProgress ◌
	nt := m.lastRunningNestedTool()
	require.NotNil(t, nt)
	nt.nestedTokens = 5000
	nt.startedAt = time.Now().Add(-2 * time.Second)
	m = m.applyEvent(cli.StreamEvent{Kind: "tool_result", ToolName: "fs_read", ToolStatus: "ok"})

	nt2 := m.lastRunningNestedTool()
	require.NotEmpty(t, nt2.nestedProgress)
	line := nt2.nestedProgress[len(nt2.nestedProgress)-1]
	assert.Contains(t, line, "5k", "tokens segment")
	assert.Contains(t, line, "tools", "tool-uses segment")
}

// TestCov_NewProgram covers the program entry point. newModel stores the
// session without invoking it during construction, and tea.NewProgram only
// constructs the program (it starts on Run), so a zero-value *cli.Session is
// sufficient to exercise the entry.
func TestCov_NewProgram(t *testing.T) {
	// NewProgram applies the process's real terminal capability (W-E-01) via
	// ApplyColorProfile, which mutates lipgloss's package-level shared
	// renderer with no auto-detect-mode-restoring API (see
	// capability_test.go's withColorProfile doc comment). Left unrestored,
	// this leaks whatever this test machine's TERM/COLORTERM happen to be
	// into every later test in the package that renders a lipgloss style.
	prev := lipgloss.ColorProfile()
	t.Cleanup(func() { ApplyColorProfile(prev) })
	// W-E-06: NewProgram also calls SetHyperlinksEnabled(cap.AltScreen) from
	// the machine's real detected capability — another process-global side
	// effect this test's real (non-injected) capability detection can leak
	// into later tests exactly like the color profile above.
	prevHyperlinks := hyperlinksEnabled.Load()
	t.Cleanup(func() { hyperlinksEnabled.Store(prevHyperlinks) })

	p := NewProgram(&cli.Session{}, "/proj", Preferences{})
	assert.NotNil(t, p)
}

// TestCov_Update_StreamMsgSpinner covers the streamMsg spinner re-arm: while a
// tool is running, Update appends m.spinner.Tick so the glyph animates.
func TestCov_Update_StreamMsgSpinner(t *testing.T) {
	m := newModel(&fakeSession{}, "/proj")
	m = m.applyEvent(cli.StreamEvent{Kind: "tool_call", ToolName: "shell_run", ToolStatus: "running"})
	mm, _ := m.Update(streamMsg{ev: cli.StreamEvent{Kind: "agent_chunk", Text: "x"}})
	assert.NotNil(t, mm)
}
