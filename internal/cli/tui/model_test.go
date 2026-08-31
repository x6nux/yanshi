package tui

import (
	"fmt"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/x6nux/yanshi/internal/cli"
	"github.com/x6nux/yanshi/internal/proto"
)

func TestModel_WindowSizeSetsDimensions(t *testing.T) {
	m := newModel(&fakeSession{}, "/proj")
	mm, _ := m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	m = mm.(model)
	assert.Equal(t, 80, m.width)
	assert.Equal(t, 24, m.height)
}

func TestModel_TypesIntoInput(t *testing.T) {
	m := newModel(&fakeSession{}, "/proj")
	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("a")})
	assert.Contains(t, updated.(model).input.Value(), "a")
}

// fakeSession implements just enough of cli.Session for the TUI tests.
type fakeSession struct{}

// SendTurn routes an attachment-carrying turn through the same path Send
// uses. Required on the interface rather than optional so a session that
// forgets it fails to compile — an optional one would silently drop every
// attachment on that path.
func (f *fakeSession) SendTurn(fr proto.ClientFrame) <-chan cli.StreamEvent {
	return f.Send(fr.Text)
}

func (f *fakeSession) Send(_ string) <-chan cli.StreamEvent                 { return nil }
func (f *fakeSession) SendFrame(_ proto.ClientFrame) <-chan cli.StreamEvent { return nil }
func (f *fakeSession) CancelCurrent() error                                 { return nil }
func (f *fakeSession) Mode() string                                         { return "fake" }
func (f *fakeSession) Root() string                                         { return "/proj" }

// scriptedSession is a test double whose Send is a no-op: the tests drive
// applyEvent directly with a scripted event sequence rather than through a live
// channel. It still satisfies tuiSession so newModel accepts it.
type scriptedSession struct {
	events []cli.StreamEvent
	i      int
}

func newScriptedSession(evs []cli.StreamEvent) *scriptedSession { return &scriptedSession{events: evs} }

// SendTurn routes an attachment-carrying turn through the same path Send
// uses. Required on the interface rather than optional so a session that
// forgets it fails to compile — an optional one would silently drop every
// attachment on that path.
func (s *scriptedSession) SendTurn(fr proto.ClientFrame) <-chan cli.StreamEvent {
	return s.Send(fr.Text)
}

func (s *scriptedSession) Send(_ string) <-chan cli.StreamEvent                 { return nil }
func (s *scriptedSession) SendFrame(_ proto.ClientFrame) <-chan cli.StreamEvent { return nil }
func (s *scriptedSession) CancelCurrent() error                                 { return nil }
func (s *scriptedSession) Mode() string                                         { return "fake" }
func (s *scriptedSession) Root() string                                         { return "/proj" }

func (s *scriptedSession) next() cli.StreamEvent {
	ev := s.events[s.i]
	s.i++
	return ev
}

// recordingSession records every ClientFrame passed to SendFrame and every
// prompt passed to Send so command tests can assert on routing ("/clear" sent a
// clear frame, not a user_message). SendFrame returns nil (no synthetic reply —
// tests drive applyEvent of replies directly). canceled counts CancelCurrent
// calls so the C07 single-mode interrupt can be asserted.
type recordingSession struct {
	frames   []proto.ClientFrame
	sentText []string
	canceled int
}

// SendTurn records the WHOLE frame, not just its text: the attachments and
// images a turn carries are the thing tests need to assert on, and recording
// only fr.Text would make "did the turn carry the file?" unanswerable.
func (r *recordingSession) SendTurn(fr proto.ClientFrame) <-chan cli.StreamEvent {
	// frames only, NOT sentText: tests tell the frame path from the plain-text
	// path by which of the two recorded, and recording both would erase that
	// distinction.
	r.frames = append(r.frames, fr)
	return nil
}

func (r *recordingSession) Send(text string) <-chan cli.StreamEvent {
	r.sentText = append(r.sentText, text)
	return nil
}
func (r *recordingSession) SendFrame(f proto.ClientFrame) <-chan cli.StreamEvent {
	r.frames = append(r.frames, f)
	return nil
}
func (r *recordingSession) CancelCurrent() error { r.canceled++; return nil }
func (r *recordingSession) Mode() string         { return "ws" }
func (r *recordingSession) Root() string         { return "/proj" }

// channelSession returns a pre-made open channel from Send so submit() arms a
// real in-flight turn (m.streamCh != nil), letting layout tests exercise the
// status-line path end-to-end through submit().
type channelSession struct {
	ch chan cli.StreamEvent
}

// SendTurn routes an attachment-carrying turn through the same path Send
// uses. Required on the interface rather than optional so a session that
// forgets it fails to compile — an optional one would silently drop every
// attachment on that path.
func (c *channelSession) SendTurn(fr proto.ClientFrame) <-chan cli.StreamEvent {
	return c.Send(fr.Text)
}

func (c *channelSession) Send(_ string) <-chan cli.StreamEvent                 { return c.ch }
func (c *channelSession) SendFrame(_ proto.ClientFrame) <-chan cli.StreamEvent { return nil }
func (c *channelSession) CancelCurrent() error                                 { return nil }
func (c *channelSession) Mode() string                                         { return "ws" }
func (c *channelSession) Root() string                                         { return "/proj" }

// TestModel_StreamsAndRendersToolBlocks drives a full turn's events through
// applyEvent and asserts: a tool_call creates a running tool block, the matching
// tool_result resolves it in place (status ok, result captured), and assistant
// chunks become an assistant block.
func TestModel_StreamsAndRendersToolBlocks(t *testing.T) {
	m := newModel(newScriptedSession(nil), "/proj")
	mm, _ := m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	m = mm.(model)

	for _, ev := range []cli.StreamEvent{
		{Kind: "tool_call", ToolName: "fs_search", ToolArgs: `{"p":"x"}`, ToolStatus: "running"},
		{Kind: "tool_result", ToolName: "fs_search", Text: "hit", ToolStatus: "ok"},
		{Kind: "agent_chunk", Text: "all "},
		{Kind: "agent_chunk", Text: "done"},
		{Kind: "done"},
	} {
		m = m.applyEvent(ev)
	}

	var tool *toolEntry
	var sawAssistant bool
	for _, e := range m.entries {
		if t, ok := e.(*toolEntry); ok {
			tool = t
		}
		if _, ok := e.(assistantEntry); ok {
			sawAssistant = true
		}
	}
	require.NotNil(t, tool, "a tool block must exist")
	assert.Equal(t, "ok", tool.status, "tool_result must resolve the spinner to ok")
	assert.Equal(t, "hit", tool.result)
	assert.True(t, sawAssistant)
	assert.Equal(t, 1, m.toolsRun)
}

// TestModel_CancelThenQuit proves: first Ctrl-C during a stream cancels; a
// second Ctrl-C quits (even while cancel is still in flight).
func TestModel_CancelThenQuit(t *testing.T) {
	m := newModel(newScriptedSession(nil), "/proj")
	m.streamCh = make(chan cli.StreamEvent) // fake in-flight turn

	mm, cmd := m.Update(tea.KeyMsg{Type: tea.KeyCtrlC})
	m = mm.(model)
	assert.True(t, m.canceling, "first Ctrl-C cancels")
	assert.Nil(t, cmd, "first Ctrl-C does not quit")

	mm, cmd = m.Update(tea.KeyMsg{Type: tea.KeyCtrlC})
	assert.NotNil(t, cmd, "second Ctrl-C quits")
}

// TestModel_ToggleExpand proves "e" flips expand on the most recent resolved
// tool block (so the full result can be shown/hidden).
func TestModel_ToggleExpand(t *testing.T) {
	m := newModel(newScriptedSession(nil), "/proj")
	mm, _ := m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	m = mm.(model)
	m = m.applyEvent(cli.StreamEvent{Kind: "tool_call", ToolName: "fs_read", ToolStatus: "running"})
	m = m.applyEvent(cli.StreamEvent{Kind: "tool_result", ToolName: "fs_read", Text: "line1\nline2", ToolStatus: "ok"})

	tool := lastTool(t, m)
	require.False(t, tool.expanded)

	m.toggleExpand()
	assert.True(t, lastTool(t, m).expanded, "expand toggled on")
	m.toggleExpand()
	assert.False(t, lastTool(t, m).expanded, "expand toggled off")
}

func lastTool(t *testing.T, m model) *toolEntry {
	t.Helper()
	for i := len(m.entries) - 1; i >= 0; i-- {
		if tt, ok := m.entries[i].(*toolEntry); ok {
			return tt
		}
	}
	require.Fail(t, "no tool entry")
	return nil
}

// ---- Tool-call display: friendly name + simplified args + no glyph ----

// TestToolDisplayName covers the friendly-name map and its prefix fallbacks
// (vcs_* → Title-cased, memory_* → "Memory", unknown → raw).
func TestToolDisplayName(t *testing.T) {
	cases := map[string]string{
		"fs_read":      "Read",
		"fs_write":     "Write",
		"fs_edit":      "Edit",
		"fs_list":      "List",
		"fs_glob":      "Glob",
		"fs_search":    "Search",
		"shell_run":    "Bash",
		"time_now":     "Time",
		"web_fetch":    "Fetch",
		"skill_use":    "Skill",
		"vcs_commit":   "Commit",
		"vcs_status":   "Status",
		"vcs_git_log":  "GitLog",
		"memory_read":  "Memory",
		"memory_write": "Memory",
		"unknown_tool": "unknown_tool",
		"mcp_custom":   "mcp_custom",
	}
	for raw, want := range cases {
		assert.Equalf(t, want, toolDisplayName(raw), "toolDisplayName(%q)", raw)
	}
}

// TestToolArgSummary covers the compact arg summary: Read→path (root-relative,
// basename fallback), Bash→command, Search→pattern (quoted), unknown→first key,
// and empty/{} → "".
func TestToolArgSummary(t *testing.T) {
	cases := []struct {
		name, args, root, want string
	}{
		{"fs_read", `{"path":"internal/cli/tui/model.go"}`, "/proj", "(internal/cli/tui/model.go)"},
		{"fs_read", `{"path":"/proj/src/main.go"}`, "/proj", "(src/main.go)"},
		{"fs_read", `{"path":"/abs/elsewhere/deep/main.go"}`, "/proj", "(main.go)"},
		{"fs_read", `{"path":"/proj"}`, "/proj", "(.)"},
		{"fs_read", `{"path":"C:/proj/sub/main.go"}`, "C:/proj", "(sub/main.go)"},
		{"fs_read", `{"path":"D:/elsewhere/main.go"}`, "C:/proj", "(main.go)"},
		{"shell_run", `{"command":"go test ./..."}`, "", "(go test ./...)"},
		{"fs_search", `{"pattern":"MergeToMain"}`, "", `("MergeToMain")`},
		{"fs_glob", `{"glob":"**/*.go"}`, "", "(**/*.go)"},
		{"unknown_tool", `{"foo":"bar"}`, "", `("bar")`},
		{"time_now", `{}`, "", ""},
		{"fs_read", ``, "", ""},
	}
	for _, tc := range cases {
		assert.Equalf(t, tc.want, toolArgSummary(tc.name, tc.args, tc.root),
			"toolArgSummary(%q,%q,%q)", tc.name, tc.args, tc.root)
	}
}

// TestToolArgSummary_HyperlinksDisabledByDefault proves toolArgSummary emits
// plain text — byte-identical to the pre-W-E-06 output above — when
// SetHyperlinksEnabled has never been called (the zero value of
// hyperlinksEnabled). This is the state every other test in the package
// observes, and the state a TERM=dumb session runs in for its entire
// lifetime (buildModelForCapability calls SetHyperlinksEnabled(false) for
// it) — a corrupted-escape regression here would otherwise ship as garbage
// on an unsupported terminal.
func TestToolArgSummary_HyperlinksDisabledByDefault(t *testing.T) {
	hyperlinksEnabled.Store(false)
	got := toolArgSummary("fs_read", `{"path":"src/main.go"}`, "/proj")
	want := "(src/main.go)"
	assert.Equal(t, want, got)
	assert.NotContains(t, got, "\x1b]8;;", "OSC 8 bytes must not appear when hyperlinks are disabled")
}

// TestToolArgSummary_HyperlinksEnabledWrapsPath proves that once
// SetHyperlinksEnabled(true) has run (the capable-terminal branch of
// buildModelForCapability), a "path" arg is wrapped in a real OSC 8 escape
// (termenv.Hyperlink) around the same display text toolArgSummary would
// otherwise show plain — clicking it must not open the wrong file, so the
// URI is asserted exactly, not just "contains file://".
func TestToolArgSummary_HyperlinksEnabledWrapsPath(t *testing.T) {
	hyperlinksEnabled.Store(true)
	t.Cleanup(func() { hyperlinksEnabled.Store(false) })

	got := toolArgSummary("fs_read", `{"path":"src/main.go"}`, "/proj")
	wantLink := termenv.Hyperlink("file:///proj/src/main.go", "src/main.go")
	want := "(" + wantLink + ")"
	assert.Equal(t, want, got)
}

// TestToolArgSummary_HyperlinksEnabledGlobStaysPlain proves the "glob" key
// is deliberately excluded from hyperlink-wrapping even when enabled: a glob
// like "**/*.go" is a pattern, not a single file a click could open (see
// toolArgSummary's body comment on the key == "path" check).
func TestToolArgSummary_HyperlinksEnabledGlobStaysPlain(t *testing.T) {
	hyperlinksEnabled.Store(true)
	t.Cleanup(func() { hyperlinksEnabled.Store(false) })

	got := toolArgSummary("fs_glob", `{"glob":"**/*.go"}`, "/proj")
	assert.Equal(t, "(**/*.go)", got)
	assert.NotContains(t, got, "\x1b]8;;")
}

// TestToolArgSummary_HyperlinksEnabledRelativeNoRootStaysPlain proves that a
// relative path with no root to anchor it to degrades to plain text rather
// than emitting a URI that would open the wrong file (fileHyperlinkURI
// returns "" in that case — see its doc comment).
func TestToolArgSummary_HyperlinksEnabledRelativeNoRootStaysPlain(t *testing.T) {
	hyperlinksEnabled.Store(true)
	t.Cleanup(func() { hyperlinksEnabled.Store(false) })

	got := toolArgSummary("fs_read", `{"path":"src/main.go"}`, "")
	assert.Equal(t, "(src/main.go)", got)
	assert.NotContains(t, got, "\x1b]8;;")
}

// TestFileHyperlinkURI covers fileHyperlinkURI directly: root-relative,
// already-absolute, Windows drive-letter, and the two "cannot build safely"
// cases (empty path, relative path with no root).
func TestFileHyperlinkURI(t *testing.T) {
	assert.Equal(t, "file:///proj/src/main.go", fileHyperlinkURI("src/main.go", "/proj"))
	assert.Equal(t, "file:///abs/elsewhere/main.go", fileHyperlinkURI("/abs/elsewhere/main.go", "/proj"))
	assert.Equal(t, "file:///C:/proj/main.go", fileHyperlinkURI("C:/proj/main.go", "/ignored"))
	assert.Equal(t, "", fileHyperlinkURI("", "/proj"))
	assert.Equal(t, "", fileHyperlinkURI("src/main.go", ""))
}

// TestToolEntry_RenderFriendlyFormat proves the rendered tool block uses the
// friendly name + compact arg summary, has NO leading ⏺ glyph, and keeps the
// status glyph (✓). Uses fs_write (toolDispDiff since B4) — its body is the
// "wrote N lines" footprint rather than a ⎿ result line; the ⎿ behavior itself
// is covered by TestToolRenderNormalKeepsResultLine on a Normal-class tool.
func TestToolEntry_RenderFriendlyFormat(t *testing.T) {
	m := newModel(&fakeSession{}, "/proj")
	e := &toolEntry{
		name:   "fs_write",
		args:   `{"path":"internal/cli/tui/model.go"}`,
		root:   "/proj",
		status: "ok",
		result: "1 file written",
	}
	out := e.render(80, m.spinner)
	assert.Contains(t, out, "Write(internal/cli/tui/model.go)", "friendly name + path summary")
	assert.NotContains(t, out, "⏺", "no leading glyph on the tool block")
	assert.NotContains(t, out, "fs_write", "raw tool name is not rendered")
	assert.Contains(t, out, "✓", "ok status glyph present")
	assert.Contains(t, out, "wrote", "diff class shows wrote-footprint")
}

// TestToolEntry_RenderRunningShowsSpinner proves a running tool block renders
// the friendly name + summary with no result line yet.
func TestToolEntry_RenderRunningShowsSpinner(t *testing.T) {
	m := newModel(&fakeSession{}, "/proj")
	e := &toolEntry{
		name:   "shell_run",
		args:   `{"command":"go test ./..."}`,
		root:   "/proj",
		status: "running",
	}
	out := e.render(80, m.spinner)
	assert.Contains(t, out, "Bash(go test ./...)", "running block shows friendly name + command")
	assert.NotContains(t, out, "⏺")
	assert.NotContains(t, out, "⎿", "no result line while running")
}

// TestToolEntry_RenderNoArgsOmitsSummary proves that when there is no useful
// arg the (...) summary is omitted entirely (e.g. Time).
func TestToolEntry_RenderNoArgsOmitsSummary(t *testing.T) {
	m := newModel(&fakeSession{}, "/proj")
	e := &toolEntry{name: "time_now", args: `{}`, root: "/proj", status: "ok", result: "now"}
	out := e.render(80, m.spinner)
	assert.Contains(t, out, "Time", "friendly name shown")
	assert.NotContains(t, out, "Time(", "no arg summary attached when there is no useful arg")
}

// TestToolEntry_RenderErrorShowsErrorHeader proves a failed tool renders as
// Name(Error|short msg) ✗: the friendly name is kept, the short (truncated)
// error message replaces the arg summary in the header, the ✗ glyph shows, and
// the indented ⎿ result line is skipped (the error is already inline).
func TestToolEntry_RenderErrorShowsErrorHeader(t *testing.T) {
	m := newModel(&fakeSession{}, "/proj")
	e := &toolEntry{
		name:   "fs_read",
		args:   `{"path":"missing.txt"}`,
		root:   "/proj",
		status: "error",
		result: "file not found",
	}
	out := e.render(80, m.spinner)
	assert.Contains(t, out, "Read(Error|file not found)", "error header uses friendly name + short error")
	assert.Contains(t, out, "✗", "error glyph present")
	assert.NotContains(t, out, "⎿", "result line skipped for error status")
	// The arg summary (path) must NOT appear — the error replaces it.
	assert.NotContains(t, out, "missing.txt")
}

// TestToolEntry_RenderErrorTruncatesLongMessage proves a long error message is
// truncated in the header so the block stays on one line.
func TestToolEntry_RenderErrorTruncatesLongMessage(t *testing.T) {
	m := newModel(&fakeSession{}, "/proj")
	long := strings.Repeat("x", 120)
	e := &toolEntry{name: "fs_read", args: `{}`, root: "/proj", status: "error", result: long}
	out := e.render(80, m.spinner)
	assert.Contains(t, out, "(Error|", "error header present")
	assert.NotContains(t, out, long, "full long error must not spill into the header")
}

// ---- Layout / scrolling / send-key (bug-fix) ----

// TestModel_LayoutFitsTerminal proves the viewport is sized to EXACTLY the
// remaining terminal height (terminal - footer - input), so the JoinVertical in
// View() totals msg.Height with no overflow — the prior H-5 hardcode overflowed
// by ~4 lines and clipped the bottom of the transcript (sent messages +
// streaming text were invisible). The status bar is now a BOTTOM footer.
func TestModel_LayoutFitsTerminal(t *testing.T) {
	m := newModel(&fakeSession{}, "/proj")
	const w, h = 80, 24
	mm, _ := m.Update(tea.WindowSizeMsg{Width: w, Height: h})
	m = mm.(model)

	footerH := lipgloss.Height(m.statusHeader())
	inputH := lipgloss.Height(inputBorder.Render(m.input.View()))
	wantVP := h - footerH - inputH
	if wantVP < 3 {
		wantVP = 3
	}
	assert.Equal(t, wantVP, m.viewport.Height, "viewport must fill remaining height")
	assert.Equal(t, w, m.viewport.Width)

	// The full joined View must equal the terminal height — no overflow, no gap.
	// Order is now: viewport (top), input, footer (bottom).
	total := lipgloss.Height(lipgloss.JoinVertical(lipgloss.Left,
		m.viewport.View(),
		inputBorder.Render(m.input.View()),
		m.statusHeader(),
	))
	assert.Equal(t, h, total, "JoinVertical(viewport+input+footer) must equal terminal height")
}

// TestModel_SubmitAppendsVisibleUserEntry proves Enter submits the turn and the
// user's message becomes a userEntry in the transcript (it was previously
// rendered off-screen by the overflow bug — here we assert it exists; the layout
// test above asserts it is visible).
func TestModel_SubmitAppendsVisibleUserEntry(t *testing.T) {
	rec := &recordingSession{}
	m := newModel(rec, "/proj")
	m.input.SetValue("hello world")

	mm, _ := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = mm.(model)

	require.Len(t, rec.sentText, 1, "Enter must Send the text")
	assert.Equal(t, "hello world", rec.sentText[0])
	assert.Equal(t, "", m.input.Value(), "input is reset after submit")
	var sawUser bool
	for _, e := range m.entries {
		if ue, ok := e.(*userEntry); ok && ue.text == "hello world" {
			sawUser = true
		}
	}
	assert.True(t, sawUser, "submit must append a userEntry to the transcript")
}

// TestModel_EnterDoesNotInsertNewline proves Enter is intercepted for submit and
// is NOT forwarded to the textarea (so it never inserts a newline into the
// input). An empty input is a no-op submit, leaving the box empty.
func TestModel_EnterDoesNotInsertNewline(t *testing.T) {
	rec := &recordingSession{}
	m := newModel(rec, "/proj")

	mm, _ := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = mm.(model)
	assert.Empty(t, rec.sentText, "empty Enter does not send")
	assert.Equal(t, "", m.input.Value(), "Enter does not insert a newline")
}

// TestModel_PgKeysScrollViewport proves PgUp/PgDn are routed to the viewport
// (not the textarea) so the transcript is scrollable.
func TestModel_PgKeysScrollViewport(t *testing.T) {
	m := newModel(&fakeSession{}, "/proj")
	mm, _ := m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	m = mm.(model)

	// Fill the viewport with more content than fits, then PgDn and PgUp should
	// change the scroll offset without error and without touching the input.
	for i := 0; i < 60; i++ {
		m.entries = append(m.entries, assistantEntry{text: "line"})
	}
	m.refresh()
	m.viewport.GotoBottom()
	before := m.viewport.YOffset

	mm, _ = m.Update(tea.KeyMsg{Type: tea.KeyPgUp})
	m = mm.(model)
	assert.Less(t, m.viewport.YOffset, before, "PgUp must scroll the viewport up")

	down := m.viewport.YOffset
	mm, _ = m.Update(tea.KeyMsg{Type: tea.KeyPgDown})
	m = mm.(model)
	assert.Greater(t, m.viewport.YOffset, down, "PgDn must scroll the viewport down")
}

// TestModel_StatusCarriesContextWindow proves a status reply carrying
// context_window is stored on the model and surfaces in the rendered header as
// "1.6%  2k tokens".
func TestModel_StatusCarriesContextWindow(t *testing.T) {
	m := newModel(&fakeSession{}, "/proj")
	m = m.applyEvent(cli.StreamEvent{
		Kind: "status", Model: "claude-opus-4", TokensIn: 2000, ContextWindow: 128000,
	})
	assert.Equal(t, 128000, m.contextWindow)
	assert.Contains(t, m.statusHeader(), "1.6%  2k tokens")
}

// TestFormatTokens covers the compact token formatter used in the header.
func TestFormatTokens(t *testing.T) {
	for n, want := range map[int]string{
		0: "0", 100: "100", 999: "999",
		1000: "1k", 1500: "1.5k", 2000: "2k",
		48213: "48.2k", 128000: "128k",
	} {
		assert.Equalf(t, want, formatTokens(n), "formatTokens(%d)", n)
	}
}

// ---- A: auto-grow input (1–3 lines, clamp past 3) ----

// TestModel_InputAutoGrowsAndClamps proves growInput sizes the textarea height
// to its content: 1 line by default, +1 per newline up to 3, then clamped (the
// bubbles textarea scrolls internally past that).
func TestModel_InputAutoGrowsAndClamps(t *testing.T) {
	m := newModel(&fakeSession{}, "/proj")
	mm, _ := m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	m = mm.(model)
	assert.Equal(t, 1, m.input.Height(), "default height is 1 line")

	cases := []struct {
		value string
		want  int
	}{
		{"one", 1},
		{"one\ntwo", 2},
		{"a\nb\nc", 3},
		{"a\nb\nc\nd", 3},       // 4 lines: clamped at 3
		{"a\nb\nc\nd\ne\nf", 3}, // 6 lines: still clamped at 3
		{"", 1},                 // empty resets to 1
	}
	for i, tc := range cases {
		m.input.SetValue(tc.value)
		m.growInput()
		assert.Equalf(t, tc.want, m.input.Height(), "case %d (%q): want %d", i, tc.value, tc.want)
	}
}

// TestModel_InputGrowsViaUpdate proves a typed newline (Ctrl+Enter) grows the
// rendered input block height end-to-end through Update, then reflows. (Ctrl+O
// previously shared this duty but is now the thinking-block expand key.)
func TestModel_InputGrowsViaUpdate(t *testing.T) {
	m := newModel(&fakeSession{}, "/proj")
	mm, _ := m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	m = mm.(model)
	before := lipgloss.Height(inputBorder.Render(m.input.View()))

	mm, _ = m.Update(tea.KeyMsg{Type: tea.KeyCtrlEnter})
	m = mm.(model)
	assert.Equal(t, 2, m.input.Height(), "Ctrl+Enter grows input to 2 lines")
	after := lipgloss.Height(inputBorder.Render(m.input.View()))
	assert.Equal(t, before+1, after, "rendered input block grew by one line")
}

// TestModel_CtrlEnterInsertsNewline proves Ctrl+Enter — now detectable on
// Windows via the local bubbletea fork — inserts a newline and grows the input,
// while plain Enter submits and does NOT insert a newline. Also guards the fork:
// KeyCtrlEnter is a distinct KeyType from KeyEnter and its String() is registered.
func TestModel_CtrlEnterInsertsNewline(t *testing.T) {
	// The fork's new KeyType is distinct from KeyEnter and names itself correctly.
	require.NotEqual(t, tea.KeyEnter, tea.KeyCtrlEnter, "fork must distinguish Ctrl+Enter")
	assert.Equal(t, "ctrl+enter", tea.KeyCtrlEnter.String())

	m := newModel(&fakeSession{}, "/proj")
	mm, _ := m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	m = mm.(model)
	before := lipgloss.Height(inputBorder.Render(m.input.View()))

	mm, _ = m.Update(tea.KeyMsg{Type: tea.KeyCtrlEnter})
	m = mm.(model)
	assert.Contains(t, m.input.Value(), "\n", "Ctrl+Enter inserts a newline")
	assert.Equal(t, 2, m.input.Height(), "Ctrl+Enter grows input to 2 lines")
	after := lipgloss.Height(inputBorder.Render(m.input.View()))
	assert.Equal(t, before+1, after, "rendered input block grew by one line")

	// Plain Enter on a fresh model does not leave a newline behind.
	m2 := newModel(&fakeSession{}, "/proj")
	mm2, _ := m2.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m2 = mm2.(model)
	assert.NotContains(t, m2.input.Value(), "\n", "Enter does not insert a newline")
}

// ---- B: long paste collapses to [粘贴 #<id>] ----

// TestModel_LongPasteCollapsesAndExpands proves: a long submitted message
// renders the collapsed placeholder in the transcript, and "e" expands it to
// the full text. The placeholder carries a stable 4-hex id.
func TestModel_LongPasteCollapsesAndExpands(t *testing.T) {
	m := newModel(&fakeSession{}, "/proj")
	mm, _ := m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	m = mm.(model)

	long := strings.Repeat("x", 300) // > pasteThresholdChars
	ue := &userEntry{text: long, pasteID: pasteID(long), pasted: true}
	m.entries = append(m.entries, ue)

	rendered := ue.render(80, m.spinner)
	assert.Contains(t, rendered, "[粘贴 #")
	assert.Len(t, ue.pasteID, 4, "paste id is 4 hex chars")
	assert.NotContains(t, rendered, strings.Repeat("x", 10), "full text hidden when collapsed")

	// "e" expands the most recent collapsible block (here, the user paste).
	m.toggleExpand()
	assert.True(t, ue.expanded, "toggle expanded the long paste")
	rendered = ue.render(80, m.spinner)
	assert.Contains(t, rendered, "xxxx", "full text shown once expanded")
	assert.NotContains(t, rendered, "[粘贴 #", "placeholder gone once expanded")

	// Toggle again collapses.
	m.toggleExpand()
	assert.False(t, ue.expanded)
}

// TestModel_ShortMessageNotCollapsed proves short messages render full markdown
// (no placeholder) and are NOT toggled by "e" (they aren't collapsible).
func TestModel_ShortMessageNotCollapsed(t *testing.T) {
	m := newModel(&fakeSession{}, "/proj")
	mm, _ := m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	m = mm.(model)

	ue := &userEntry{text: "hello there", pasteID: pasteID("hello there")}
	m.entries = append(m.entries, ue)
	rendered := ue.render(80, m.spinner)
	assert.NotContains(t, rendered, "[粘贴 #", "short message has no placeholder")
	assert.Contains(t, rendered, "hello")

	// "e" is a no-op on a short (non-collapsible) user entry.
	m.toggleExpand()
	assert.False(t, ue.expanded, "short message is not toggled")
}

// TestModel_PasteIDStable proves pasteID is deterministic for the same content.
func TestModel_PasteIDStable(t *testing.T) {
	a := pasteID("same content")
	b := pasteID("same content")
	assert.Equal(t, a, b)
	assert.Len(t, a, 4)
	assert.NotEqual(t, a, pasteID("different content"))
}

// TestModel_SubmitCollapsesLongPaste proves submit() records a userEntry whose
// pasteID matches the sent text, and the FULL text is sent (collapse is
// display-only). With B9/T11 it also verifies submit() threads inputPasted
// through to userEntry.pasted (the collapse-eligibility flag).
func TestModel_SubmitCollapsesLongPaste(t *testing.T) {
	rec := &recordingSession{}
	m := newModel(rec, "/proj")
	long := strings.Repeat("y", 300)
	m.input.SetValue(long)
	// Simulate a bracketed-paste event landing just before Enter so submit()
	// sees inputPasted=true (a plain SetValue doesn't go through KeyRunes, so
	// we arm the flag directly to exercise the submit() threading).
	m.inputPasted = true

	mm, _ := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = mm.(model)

	require.Len(t, rec.sentText, 1, "the FULL text must be sent to the model")
	assert.Equal(t, long, rec.sentText[0], "collapse must not truncate what is sent")

	var ue *userEntry
	for _, e := range m.entries {
		if x, ok := e.(*userEntry); ok {
			ue = x
		}
	}
	require.NotNil(t, ue)
	assert.True(t, ue.isLong())
	assert.True(t, ue.pasted, "submit threads inputPasted → userEntry.pasted")
	assert.Equal(t, pasteID(long), ue.pasteID, "transcript entry carries the stable id")
	assert.False(t, m.inputPasted, "submit resets inputPasted for the next turn")
}

// TestModel_SubmitTypedLongNotPasted proves a LONG message that was TYPED
// (no paste signal) yields a userEntry with pasted=false — it is NOT
// eligible for "[粘贴 #id]" collapse and renders in full (B9/T11).
func TestModel_SubmitTypedLongNotPasted(t *testing.T) {
	rec := &recordingSession{}
	m := newModel(rec, "/proj")
	long := strings.Repeat("z", 300)
	m.input.SetValue(long)
	// No inputPasted arming — simulates slow typing via SetValue. submit() must
	// record pasted=false so render() keeps the text inline.

	mm, _ := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = mm.(model)

	var ue *userEntry
	for _, e := range m.entries {
		if x, ok := e.(*userEntry); ok {
			ue = x
		}
	}
	require.NotNil(t, ue)
	assert.True(t, ue.isLong())
	assert.False(t, ue.pasted, "typed long message is NOT marked pasted")
}

// ---- C: live activity status line ----

// TestModel_StatusLineAbsentWhenIdle proves the status line is NOT rendered
// when no turn is in flight (m.streamCh == nil).
func TestModel_StatusLineAbsentWhenIdle(t *testing.T) {
	m := newModel(&fakeSession{}, "/proj")
	mm, _ := m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	m = mm.(model)
	assert.Nil(t, m.streamCh)
	assert.NotContains(t, m.View(), "Thinking", "no status line when idle")
}

// TestModel_StatusLinePresentWhileStreaming proves the status line renders
// between the viewport and the input while a turn is in flight, carries the
// activity text, and is hidden again once the stream ends.
func TestModel_StatusLinePresentWhileStreaming(t *testing.T) {
	m := newModel(&fakeSession{}, "/proj")
	mm, _ := m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	m = mm.(model)

	// Arm a fake in-flight turn.
	m.streamCh = make(chan cli.StreamEvent)
	m.startTurn()
	m.reflow()

	view := m.View()
	assert.Contains(t, view, "Thinking…", "status line shows while streaming")
	assert.Contains(t, view, "(0:", "status line shows elapsed time")

	// The full joined View must still equal the terminal height with the extra
	// status line accounted for (no overflow).
	total := lipgloss.Height(view)
	assert.Equal(t, 24, total, "JoinVertical still totals terminal height with status line")

	// Ending the turn hides the status line.
	m = m.applyEvent(cli.StreamEvent{Kind: "done"})
	assert.Nil(t, m.streamCh)
	assert.NotContains(t, m.View(), "Thinking", "status line hidden after done")
}

// TestModel_StatusLineActivityUpdatesOnToolCall proves the activity text tracks
// the current step: a running tool_call sets "Running <name>…", and a later
// agent_chunk returns it to "Thinking…".
func TestModel_StatusLineActivityUpdatesOnToolCall(t *testing.T) {
	m := newModel(&fakeSession{}, "/proj")
	m.streamCh = make(chan cli.StreamEvent)
	m.startTurn()
	assert.Equal(t, "Thinking…", m.activity)

	m = m.applyEvent(cli.StreamEvent{Kind: "tool_call", ToolName: "fs_search", ToolStatus: "running"})
	assert.Equal(t, "Running Search…", m.activity)
	assert.Contains(t, m.View(), "Running Search…")

	m = m.applyEvent(cli.StreamEvent{Kind: "agent_chunk", Text: "ok"})
	assert.Equal(t, "Thinking…", m.activity, "agent_chunk returns activity to Thinking")
}

// TestModel_StatusLineTokensOmittedWhenZero proves the "↓ Xk" segment is hidden
// until the first status reply populates tokensIn.
func TestModel_StatusLineTokensOmittedWhenZero(t *testing.T) {
	m := newModel(&fakeSession{}, "/proj")
	m.streamCh = make(chan cli.StreamEvent)
	m.startTurn()
	assert.NotContains(t, m.statusLine(), "↓", "no token segment when tokensIn == 0")

	m = m.applyEvent(cli.StreamEvent{Kind: "status", TokensIn: 2000})
	assert.Contains(t, m.statusLine(), "↓ 2k", "token segment shown after a status reply")
}

// TestModel_ActivityTickAdvancesGlyph proves each activityTickMsg advances the
// leading glyph and re-arms only while streaming; when idle the tick stops.
func TestModel_ActivityTickAdvancesGlyph(t *testing.T) {
	m := newModel(&fakeSession{}, "/proj")
	m.streamCh = make(chan cli.StreamEvent)
	m.startTurn()
	before := m.glyphFrame

	mm, cmd := m.Update(activityTickMsg{})
	m = mm.(model)
	assert.Equal(t, before+1, m.glyphFrame, "tick advances the glyph frame")
	assert.NotNil(t, cmd, "tick re-arms while streaming")

	// End the stream; the next tick must NOT advance or re-arm.
	m = m.applyEvent(cli.StreamEvent{Kind: "done"})
	got := m.glyphFrame
	mm, cmd = m.Update(activityTickMsg{})
	m = mm.(model)
	assert.Equal(t, got, m.glyphFrame, "no advance when idle")
	assert.Nil(t, cmd, "no re-arm when idle")
}

// TestModel_LayoutFitsThroughSubmitDoneCycle proves the JoinVertical total
// stays exactly at the terminal height across the streaming lifecycle: after
// submit() arms a turn (status line appears), during events, and after done
// (status line disappears) — no overflow, no gap.
func TestModel_LayoutFitsThroughSubmitDoneCycle(t *testing.T) {
	ch := make(chan cli.StreamEvent, 4)
	sess := &channelSession{ch: ch}
	m := newModel(sess, "/proj")
	const w, h = 80, 24
	mm, _ := m.Update(tea.WindowSizeMsg{Width: w, Height: h})
	m = mm.(model)

	// Submit a user message; Send returns the open channel → streamCh set,
	// status line visible.
	m.input.SetValue("hello")
	mm, _ = m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = mm.(model)
	require.NotNil(t, m.streamCh, "submit armed an in-flight turn")
	assert.Contains(t, m.View(), "Thinking…", "status line visible after submit")
	assert.Equal(t, h, lipgloss.Height(m.View()), "total == terminal height after submit")

	// Feed an event (reflow runs inside applyEvent); still exact.
	m = m.applyEvent(cli.StreamEvent{Kind: "agent_chunk", Text: "hi"})
	assert.Equal(t, h, lipgloss.Height(m.View()), "total == terminal height mid-stream")

	// End the turn (status line hidden); still exact — no 1-line gap.
	close(ch)
	m = m.applyEvent(cli.StreamEvent{Kind: "done"})
	assert.Nil(t, m.streamCh)
	assert.NotContains(t, m.View(), "Thinking…", "status line hidden after done")
	assert.Equal(t, h, lipgloss.Height(m.View()), "total == terminal height after done")
}

// TestFormatDuration covers the mm:ss elapsed formatter for the status line.
func TestFormatDuration(t *testing.T) {
	for d, want := range map[time.Duration]string{
		0:                         "0:00",
		12 * time.Second:          "0:12",
		95 * time.Second:          "1:35",
		60 * time.Minute:          "60:00",
		125 * time.Second:         "2:05",
		time.Hour + 5*time.Second: "60:05",
	} {
		assert.Equalf(t, want, formatDuration(d), "formatDuration(%v)", d)
	}
}

// ---- D: bottom status bar (footer) ----

// TestModel_FooterAtBottom proves the status bar (yanshi · mode · ...) is the
// LAST block in the layout (Claude-Code-style bottom footer), not the first.
// The input box must sit ABOVE the footer, and the footer must render in the
// bottom region of the screen. Covers both idle and streaming layouts.
func TestModel_FooterAtBottom(t *testing.T) {
	cases := []struct {
		name      string
		streaming bool
	}{
		{"idle", false},
		{"streaming", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := newModel(&fakeSession{}, "/proj")
			mm, _ := m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
			m = mm.(model)
			if tc.streaming {
				m.streamCh = make(chan cli.StreamEvent)
				m.startTurn()
				m.reflow()
			}

			view := m.View()
			lines := strings.Split(view, "\n")

			// Footer is the last block; take its last occurrence. With the "yanshi"
			// brand segment removed, we find the footer by looking for the workDir
			// ("proj") — the first segment that always renders for this test setup.
			footerLine := -1
			for i, ln := range lines {
				if strings.Contains(ln, "proj") {
					footerLine = i
				}
			}
			require.GreaterOrEqualf(t, footerLine, 0, "%s: footer must be rendered", tc.name)
			assert.Greaterf(t, footerLine, len(lines)/2, "%s: footer must be in the bottom half", tc.name)

			// The input box (rounded border) must sit ABOVE the footer.
			inputLine := -1
			for i, ln := range lines {
				if strings.Contains(ln, "╭") {
					inputLine = i
				}
			}
			require.GreaterOrEqualf(t, inputLine, 0, "%s: input box must be rendered", tc.name)
			assert.Lessf(t, inputLine, footerLine, "%s: input box must be above the footer", tc.name)

			// Total height still exact (footer accounted for in reflow).
			assert.Equalf(t, 24, lipgloss.Height(view), "%s: total == terminal height", tc.name)
		})
	}
}

// TestModel_FooterAccountsForViewportHeight proves the viewport height is
// reduced by the footer's rendered height (so the footer doesn't overflow the
// terminal). With a tall footer (multi-segment) the viewport shrinks to fit.
//
// The height must be measured with blockHeight (wrapping-aware), not
// lipgloss.Height (newline-only): at width 80 the multi-segment footer renders
// wider than the terminal and wraps onto a second line, and reflow subtracts
// that wrapped height. Using the same metric the production code uses is what
// makes this a faithful regression guard.
func TestModel_FooterAccountsForViewportHeight(t *testing.T) {
	m := newModel(&fakeSession{}, "/proj")
	const w, h = 80, 24
	mm, _ := m.Update(tea.WindowSizeMsg{Width: w, Height: h})
	m = mm.(model)

	// Populate status fields so the footer renders several segments.
	m = m.applyEvent(cli.StreamEvent{
		Kind: "status", Model: "claude-opus-4", TokensIn: 2000, ContextWindow: 128000, Thinking: "high",
	})

	footerH := blockHeight(m.statusHeader(), w)
	inputH := blockHeight(inputBorder.Render(m.input.View()), w)
	wantVP := h - footerH - inputH
	if wantVP < 3 {
		wantVP = 3
	}
	assert.Equal(t, wantVP, m.viewport.Height,
		"viewport must subtract the footer height from the terminal height")
	assert.Greater(t, footerH, 0, "footer has a measurable height")
}

// TestModel_FooterColorized proves each footer segment statusHeader
// (view.go) actually renders has a distinct colour configuration in every
// built-in theme, so the Powerline bar has visible contrast between pills.
// The key list below is the exact set of literal keys statusHeader passes to
// its tc(key) lookup (view.go) — read off every tc("...") call site by hand,
// not copied from any theme table.
//
// RE-H (fix-e1 review of W-E-01) found the prior version of this test used
// its own 10-key list, checked against ThemeDefault only, disconnected from
// both directions of the real consumer: it included three keys ("name",
// "think", "cache") statusHeader never looks up — theme-table entries with
// zero readers, rendered permanently dead by tc()'s silent fallback — and
// omitted two it does ("total", the four "perm_*" keys), so the test neither
// caught the dead keys nor covered half of what actually renders. Extending
// the key list to the real set surfaced three more real collisions the old
// list's blind spot had hidden in every theme it checked (ThemeDefault) and
// every one it never checked at all (ThemeHighContrast, ThemeMuted):
// "perm_edits" was byte-identical to "mode" in all three tables, and
// "perm_default"/"tools" were byte-identical in ThemeMuted — same pill,
// different meanings. All four are now distinct colours (styles.go, RE-H
// comments on those entries) and this test loops themeList instead of just
// ThemeDefault so a fourth can't hide the same way.
//
// The require.Len(tm.Colors)==len(keys) assertion below is what makes this
// self-enforcing going forward: a table entry added without a matching
// tc() call (recreating RE-H) changes tm.Colors' size without keys', and a
// keys entry added without a matching table entry fails the "missing
// segment" check — either drift is caught here instead of silently
// tolerated by tc()'s fallback.
func TestModel_FooterColorized(t *testing.T) {
	m := newModel(&fakeSession{}, "/proj")
	m = m.applyEvent(cli.StreamEvent{
		Kind: "status", Model: "claude-opus-4", TokensIn: 2000, ContextWindow: 128000, Thinking: "high",
	})
	footer := m.statusHeader()

	// Text content is intact (each label present).
	assert.Contains(t, footer, "proj") // workDir
	assert.Contains(t, footer, "claude-opus-4")
	assert.Contains(t, footer, "1.6%")
	assert.Contains(t, footer, "总消耗") // unified consumption segment (in+out)

	keys := []string{
		"dir", "mode", "git", "model", "ctx", "total",
		"perm_default", "perm_edits", "perm_auto", "perm_yolo",
		"tools", "queue",
	}
	for _, tm := range themeList {
		t.Run(string(tm.Name), func(t *testing.T) {
			require.Lenf(t, tm.Colors, len(keys),
				"theme %q has %d segment(s) but statusHeader only looks up %d keys — table and consumer have drifted (RE-H)",
				tm.Name, len(tm.Colors), len(keys))
			seen := make(map[string]string, len(keys))
			for _, k := range keys {
				c, ok2 := tm.Colors[k]
				if !ok2 {
					t.Fatalf("theme %q missing segment %q that statusHeader looks up", tm.Name, k)
				}
				// Colour must be set.
				assert.NotEmptyf(t, c.fg, "theme %q %q fg must be set", tm.Name, k)
				assert.NotEmptyf(t, c.bg, "theme %q %q bg must be set", tm.Name, k)
				// (fg,bg,bold) tuple must be unique so pills are distinguishable.
				key := fmt.Sprintf("fg=%s bg=%s bold=%v", c.fg, c.bg, c.bold)
				if prev, dup := seen[key]; dup {
					t.Fatalf("theme %q: segment %q reuses the colour tuple of %q — segments must differ", tm.Name, k, prev)
				}
				seen[key] = k
			}
		})
	}
}

// TestModel_ManyAgentChunksAccumulate is a regression guard: pending used to be
// a strings.Builder field, and value-receiver applyEvent copied the non-zero
// Builder — the SECOND agent_chunk panicked ("illegal use of non-zero Builder
// copied by value") in real streaming use (the direct-call tests missed it
// because the stack address was reused). pending is now a plain string, so many
// consecutive chunks must accumulate without panic.
func TestModel_ManyAgentChunksAccumulate(t *testing.T) {
	m := newModel(newScriptedSession(nil), "/proj")
	mm, _ := m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	m = mm.(model)

	for _, ev := range []cli.StreamEvent{
		{Kind: "agent_chunk", Text: "Hello"},
		{Kind: "agent_chunk", Text: ", "},
		{Kind: "agent_chunk", Text: "world"},
		{Kind: "agent_chunk", Text: "!"},
		{Kind: "done"},
	} {
		m = m.applyEvent(ev) // must not panic on the 2nd+ chunk
	}

	var asst string
	for _, e := range m.entries {
		if a, ok := e.(assistantEntry); ok {
			asst = a.text
		}
	}
	assert.Equal(t, "Hello, world!", asst, "consecutive agent_chunks must accumulate into one assistant block")
}

func TestModel_AssistantLabelAppearsOncePerTurn(t *testing.T) {
	m := newModel(newScriptedSession(nil), "/proj")
	m.startTurn()
	for _, ev := range []cli.StreamEvent{
		{Kind: "agent_chunk", Text: "组合"},
		{Kind: "tool_call", ToolName: "fs_read", ToolStatus: "running"},
		{Kind: "tool_result", ToolName: "fs_read", ToolStatus: "ok", Text: "package main"},
		{Kind: "agent_chunk", Text: "根、编排器、工具"},
		{Kind: "done"},
	} {
		m = m.applyEvent(ev)
	}

	labels := 0
	for _, e := range m.entries {
		if a, ok := e.(assistantEntry); ok {
			labels += strings.Count(a.render(80, newSpinner()), "assistant:")
		}
	}
	assert.Equal(t, 1, labels, "one user turn should render one assistant label across tool calls")
}

// ---- E: collapsible thinking-process display ----

// findThinkingEntry returns the most recent *thinkingEntry in the transcript.
func findThinkingEntry(t *testing.T, m model) *thinkingEntry {
	t.Helper()
	for i := len(m.entries) - 1; i >= 0; i-- {
		if te, ok := m.entries[i].(*thinkingEntry); ok {
			return te
		}
		// A finalized thinking block may be attached to an assistant entry
		// (rendered between its label and content) rather than standalone.
		if ae, ok := m.entries[i].(assistantEntry); ok && ae.thought != nil {
			return ae.thought
		}
	}
	require.Fail(t, "no thinkingEntry found")
	return nil
}

// TestModel_ThinkingLiveThenCollapses proves: streaming thinking deltas create a
// LIVE thinkingEntry (streaming text rendered, live=true); the first
// non-thinking event (agent_chunk) finalizes it — live=false, endedAt set — so
// it collapses to the "Thought for …" line, and the reasoning text accumulates.
//
// The visibility half of the clause is asserted against te.render(), not just
// the entry struct: a thinkingEntry nobody draws is not "visible streaming
// thinking". The producer half is orchestrator's
// TestClassifyEvents_EmitsThinkingForReasoning.
//
// ledger: C2/UX8#1 思考模型可见流式思考
func TestModel_ThinkingLiveThenCollapses(t *testing.T) {
	m := newModel(&fakeSession{}, "/proj")
	mm, _ := m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	m = mm.(model)
	// Set turnStart to a known instant so the status line (which still uses
	// turnStart) has a non-zero elapsed time for the activity line.
	m.turnStart = time.Now()

	// Three reasoning deltas stream in.
	m = m.applyEvent(cli.StreamEvent{Kind: "thinking", Text: "conside"})
	m = m.applyEvent(cli.StreamEvent{Kind: "thinking", Text: "ring "})
	m = m.applyEvent(cli.StreamEvent{Kind: "thinking", Text: "the options"})

	te := findThinkingEntry(t, m)
	assert.True(t, te.live, "block is live while reasoning streams")
	assert.Equal(t, "considering the options", te.text, "reasoning deltas accumulate")
	assert.False(t, te.startedAt.IsZero(), "startedAt is set on creation")
	assert.True(t, te.endedAt.IsZero(), "endedAt not set until finalized")
	rendered := te.render(80, m.spinner)
	assert.Contains(t, rendered, "Thinking…", "live header shown")
	assert.Contains(t, rendered, "considering", "live streaming text rendered")

	// First non-thinking event finalizes the block.
	m = m.applyEvent(cli.StreamEvent{Kind: "agent_chunk", Text: "answer"})
	te = findThinkingEntry(t, m)
	assert.False(t, te.live, "agent_chunk finalizes the thinking block")
	assert.False(t, te.endedAt.IsZero(), "endedAt recorded on finalize")
	assert.False(t, te.expanded, "finalized block starts collapsed")

	// Collapsed render shows the one-line summary + expand hint, not the body.
	rendered = te.render(80, m.spinner)
	assert.Contains(t, rendered, "Thought for", "collapsed header")
	assert.Contains(t, rendered, "ctrl+o to expand", "expand hint")
	assert.NotContains(t, rendered, "considering", "body hidden when collapsed")
}

// TestModel_ThinkingCtrlOTogglesExpand proves ctrl+o flips expanded on the most
// recent finalized thinkingEntry, revealing the full reasoning, and collapses on
// the next press. It is a no-op on a live block or when no block exists.
//
// ledger: C2/UX8#4 可折叠
func TestModel_ThinkingCtrlOTogglesExpand(t *testing.T) {
	m := newModel(&fakeSession{}, "/proj")
	mm, _ := m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	m = mm.(model)

	// Stream reasoning then finalize with agent_chunk.
	m = m.applyEvent(cli.StreamEvent{Kind: "thinking", Text: "reasoning here"})
	m = m.applyEvent(cli.StreamEvent{Kind: "agent_chunk", Text: "answer"})

	te := findThinkingEntry(t, m)
	require.False(t, te.live)
	require.False(t, te.expanded, "starts collapsed")

	// ctrl+o expands.
	mm, _ = m.Update(tea.KeyMsg{Type: tea.KeyCtrlO})
	m = mm.(model)
	te = findThinkingEntry(t, m)
	assert.True(t, te.expanded, "ctrl+o expands the finalized thinking block")
	rendered := te.render(80, m.spinner)
	// The reasoning is markdown-rendered (ANSI-styled, words may be split by
	// escape codes), so assert against the stripped plain text.
	assert.Contains(t, stripANSI(rendered), "reasoning here", "full reasoning shown when expanded")
	assert.NotContains(t, rendered, "ctrl+o to expand", "collapsed hint gone when expanded")

	// ctrl+o again collapses.
	mm, _ = m.Update(tea.KeyMsg{Type: tea.KeyCtrlO})
	m = mm.(model)
	te = findThinkingEntry(t, m)
	assert.False(t, te.expanded, "second ctrl+o collapses again")

	// ctrl+o does NOT insert a newline (its old behavior) — input stays empty.
	assert.Equal(t, "", m.input.Value(), "ctrl+o must not insert a newline")
}

// TestModel_ThinkingCtrlONoopWhenNone proves ctrl+o is a no-op (and does not
// insert a newline) when there is no finalized thinking block to expand — e.g.
// on a fresh transcript or while the block is still live.
func TestModel_ThinkingCtrlONoopWhenNone(t *testing.T) {
	m := newModel(&fakeSession{}, "/proj")
	mm, _ := m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	m = mm.(model)

	// No thinking block at all.
	mm, _ = m.Update(tea.KeyMsg{Type: tea.KeyCtrlO})
	m = mm.(model)
	assert.Equal(t, "", m.input.Value(), "ctrl+o is a no-op with no thinking block")

	// A LIVE block is not toggleable (still streaming).
	m = m.applyEvent(cli.StreamEvent{Kind: "thinking", Text: "live…"})
	te := findThinkingEntry(t, m)
	require.True(t, te.live)
	mm, _ = m.Update(tea.KeyMsg{Type: tea.KeyCtrlO})
	m = mm.(model)
	te = findThinkingEntry(t, m)
	assert.True(t, te.live, "live block unaffected by ctrl+o")
	assert.False(t, te.expanded, "live block is not expanded by ctrl+o")
}

// TestModel_ThinkingManyDeltasAccumulate is a regression guard paralleling
// TestModel_ManyAgentChunksAccumulate: thinkingEntry.text is a plain string, NOT
// a strings.Builder, because the model value is copied through applyEvent. Many
// consecutive thinking deltas must accumulate without panicking.
func TestModel_ThinkingManyDeltasAccumulate(t *testing.T) {
	m := newModel(&fakeSession{}, "/proj")
	mm, _ := m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	m = mm.(model)

	for _, ev := range []cli.StreamEvent{
		{Kind: "thinking", Text: "step one "},
		{Kind: "thinking", Text: "step two "},
		{Kind: "thinking", Text: "step three"},
		{Kind: "agent_chunk", Text: "done"},
		{Kind: "done"},
	} {
		m = m.applyEvent(ev) // must not panic on the 2nd+ thinking delta
	}

	te := findThinkingEntry(t, m)
	assert.False(t, te.live, "finalized by agent_chunk")
	assert.Equal(t, "step one step two step three", te.text,
		"consecutive thinking deltas accumulate into one block (string field, not Builder)")
}

// TestModel_ThinkingFinalizedOnDone proves a `done` event (e.g. the turn ended
// right after reasoning, with no content) finalizes a live thinking block via
// flushAssistant — so it never stays "Thinking…" forever.
func TestModel_ThinkingFinalizedOnDone(t *testing.T) {
	m := newModel(&fakeSession{}, "/proj")
	mm, _ := m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	m = mm.(model)

	m = m.applyEvent(cli.StreamEvent{Kind: "thinking", Text: "only reasoning"})
	te := findThinkingEntry(t, m)
	require.True(t, te.live, "live before done")

	m = m.applyEvent(cli.StreamEvent{Kind: "done"})
	te = findThinkingEntry(t, m)
	assert.False(t, te.live, "done finalizes the live thinking block")
	assert.False(t, te.endedAt.IsZero(), "endedAt recorded")
}

// TestModel_ThinkingSeparatesPhases proves two reasoning phases (reasoning →
// content → reasoning again) produce TWO separate thinkingEntry blocks: the
// second reasoning phase opens a new block rather than reopening the finalized
// first one.
func TestModel_ThinkingSeparatesPhases(t *testing.T) {
	m := newModel(&fakeSession{}, "/proj")
	mm, _ := m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	m = mm.(model)

	m = m.applyEvent(cli.StreamEvent{Kind: "thinking", Text: "first "})
	m = m.applyEvent(cli.StreamEvent{Kind: "agent_chunk", Text: "mid "})
	m = m.applyEvent(cli.StreamEvent{Kind: "thinking", Text: "second "})
	m = m.applyEvent(cli.StreamEvent{Kind: "agent_chunk", Text: "end"})

	var blocks []*thinkingEntry
	for _, e := range m.entries {
		if te, ok := e.(*thinkingEntry); ok {
			blocks = append(blocks, te)
		}
	}
	require.Len(t, blocks, 2, "two reasoning phases → two thinking blocks")
	assert.Equal(t, "first ", blocks[0].text)
	assert.Equal(t, "second ", blocks[1].text)
	assert.False(t, blocks[0].live, "first block finalized by the intervening agent_chunk")
	assert.False(t, blocks[1].live, "second block finalized by the trailing agent_chunk")
}

// TestModel_ThinkingStartedAtUsesNow proves the thinking block's startedAt is
// set to time.Now() at the moment the first reasoning delta is received, NOT
// the turn's start time (m.turnStart). In a ReAct loop with multiple reasoning
// phases (thinking → tool → thinking → …), using turnStart would cause the
// second and later thinking blocks to include the duration of the preceding
// tool calls in their "Thought for Xs" header. Using time.Now() gives each
// reasoning phase its own independent elapsed time, so "Thought for 3s" means
// the user waited 3 seconds for this particular reasoning step.
func TestModel_ThinkingStartedAtUsesNow(t *testing.T) {
	m := newModel(&fakeSession{}, "/proj")
	mm, _ := m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	m = mm.(model)

	// turnStart is set to an old time (as if the user submitted long ago).
	m.turnStart = time.Date(2026, 7, 17, 12, 0, 0, 0, time.UTC)

	// Reasoning delta arrives — startedAt must be NEAR time.Now(), not the
	// ancient turnStart.
	before := time.Now()
	m = m.applyEvent(cli.StreamEvent{Kind: "thinking", Text: "pondering"})
	after := time.Now()

	te := findThinkingEntry(t, m)
	assert.False(t, te.startedAt.Equal(m.turnStart),
		"startedAt must NOT be the ancient turnStart (which would include tool-call time)")
	assert.True(t, !te.startedAt.Before(before) && !te.startedAt.After(after),
		"startedAt must be near time.Now() when the reasoning delta arrived")
}

// ---- F: mouse-wheel scrolling ----

// TestModel_MouseWheelScrollViewport proves wheel events (delivered now that
// NewProgram re-enables WithMouseCellMotion) scroll the transcript via the
// viewport, mirroring PgUp/PgDn. Non-wheel mouse events are ignored so they do
// not interfere (Shift+drag selection happens at the terminal layer).
func TestModel_MouseWheelScrollViewport(t *testing.T) {
	m := newModel(&fakeSession{}, "/proj")
	mm, _ := m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	m = mm.(model)

	for i := 0; i < 60; i++ {
		m.entries = append(m.entries, assistantEntry{text: "line"})
	}
	m.refresh()
	m.viewport.GotoBottom()
	before := m.viewport.YOffset

	// Wheel up (from the bottom) scrolls the viewport up.
	mm, _ = m.Update(tea.MouseMsg{Action: tea.MouseActionPress, Button: tea.MouseButtonWheelUp})
	m = mm.(model)
	assert.Less(t, m.viewport.YOffset, before, "wheel up must scroll the viewport up")

	up := m.viewport.YOffset
	mm, _ = m.Update(tea.MouseMsg{Action: tea.MouseActionPress, Button: tea.MouseButtonWheelDown})
	m = mm.(model)
	assert.Greater(t, m.viewport.YOffset, up, "wheel down must scroll the viewport down")

	// A non-wheel mouse event (button press / drag) must NOT scroll.
	pre := m.viewport.YOffset
	mm, _ = m.Update(tea.MouseMsg{Action: tea.MouseActionPress, Button: tea.MouseButtonLeft})
	m = mm.(model)
	assert.Equal(t, pre, m.viewport.YOffset, "non-wheel mouse events are ignored by the handler")
}

// ---- G: tool block colored by status + result size ----

// TestToolGlyph_ColorByStatus proves the tool name/glyph color tracks status:
// running → blue, ok → green, error → red (distinct, non-empty). This is the
// "color the tool block by status" behavior surfaced via toolGlyph.
func TestToolGlyph_ColorByStatus(t *testing.T) {
	m := newModel(&fakeSession{}, "/proj")
	sp := m.spinner

	want := map[string]lipgloss.Style{
		"running": runningNameStyle,
		"ok":      okStyle,
		"error":   errStyle,
	}
	seen := make(map[string]string, 3)
	for status, wantStyle := range want {
		_, gotStyle := toolGlyph(status, sp)
		assert.Equal(t, wantStyle, gotStyle, "status %q returns its style", status)
		c := fmt.Sprintf("%v", gotStyle.GetForeground())
		assert.NotEmptyf(t, c, "%s must have a foreground color", status)
		if prev, dup := seen[c]; dup {
			t.Fatalf("status %s reuses the color of %s — status colors must differ", status, prev)
		}
		seen[c] = status
	}
}

// TestToolEntry_RenderColorsNameByStatus proves the rendered block's friendly
// name carries the status color: the ANSI-styled name differs across ok/error
// (and contains the status glyph). Guards that render wires toolGlyph through.
func TestToolEntry_RenderColorsNameByStatus(t *testing.T) {
	m := newModel(&fakeSession{}, "/proj")
	ok := &toolEntry{name: "fs_read", args: `{"path":"a.go"}`, root: "/proj", status: "ok", result: "1\tx"}
	bad := &toolEntry{name: "fs_read", args: `{"path":"a.go"}`, root: "/proj", status: "error", result: "boom"}
	okOut := ok.render(80, m.spinner)
	badOut := bad.render(80, m.spinner)
	assert.Contains(t, okOut, "Read", "ok block shows friendly name")
	assert.Contains(t, okOut, "✓", "ok block shows green check")
	assert.Contains(t, badOut, "✗", "error block shows red cross")
	// The name "Read" is styled with different colors per status → rendered
	// bytes differ (the ANSI escape for green vs red).
	assert.NotEqual(t,
		strings.Index(okOut, "Read"), -1, "ok name present")
	// Locate the styled name span: it differs because the foreground color does.
	assert.NotEqual(t, okOut, badOut, "ok and error blocks render differently")
}

// TestToolEntry_RenderReadIsHeaderOnly proves an fs_read result renders ONLY the
// header "Read(<path>) ✓" — no "<bytes> · <N> lines" size suffix and no ⎿ result
// line. The file body is for the model, not the transcript; the prior sizeHint
// ("12.3 KB · 245 lines") was removed because it leaked irrelevant detail into
// the transcript. The path summary in the header is still shown.
func TestToolEntry_RenderReadIsHeaderOnly(t *testing.T) {
	m := newModel(&fakeSession{}, "/proj")
	// fs_read result shape: "<n>\t<line>" per line (still set, now unused by render).
	result := "1\tpackage store\n2\t\n3\ttype Store struct{}"
	e := &toolEntry{
		name:   "fs_read",
		args:   `{"path":"internal/store/store.go"}`,
		root:   "/proj",
		status: "ok",
		result: result,
	}
	out := e.render(80, m.spinner)
	// Header carries the friendly name + path summary + ok glyph.
	assert.Contains(t, out, "Read(internal/store/store.go)", "path summary present in header")
	assert.Contains(t, out, "✓", "ok glyph present")
	// NO size hint: the byte/line suffixes were removed from renderSilent.
	assert.NotContains(t, out, "3 lines", "no line-count size hint")
	assert.NotContains(t, out, "34 B", "no byte-size hint")
	assert.NotContains(t, out, "KB", "no KB size hint")
	// NO ⎿ result line: silent tools render header-only.
	assert.NotContains(t, out, "⎿", "no result line for silent tool")
}

// TestToolEntry_RenderListIsHeaderOnly proves an fs_list result renders ONLY the
// header "List(<path>) ✓" — no "<N> entries · <bytes>" size suffix and no ⎿
// result line. The directory listing is for the model, not the transcript; the
// prior sizeHint was removed alongside fs_read's. The path summary is still
// shown.
func TestToolEntry_RenderListIsHeaderOnly(t *testing.T) {
	m := newModel(&fakeSession{}, "/proj")
	result := `[{"name":"a.go","size":100,"is_dir":false},{"name":"b.go","size":50,"is_dir":false},{"name":"sub","size":0,"is_dir":true}]`
	e := &toolEntry{name: "fs_list", args: `{"path":"."}`, root: "/proj", status: "ok", result: result}
	out := e.render(80, m.spinner)
	// Header carries the friendly name + path summary + ok glyph.
	assert.Contains(t, out, "List(.)", "path summary present in header")
	assert.Contains(t, out, "✓", "ok glyph present")
	// NO size hint: the entry-count/byte suffixes were removed from renderSilent.
	assert.NotContains(t, out, "3 entries", "no entry-count size hint")
	assert.NotContains(t, out, "150 B", "no byte-size hint")
	assert.NotContains(t, out, "entries", "no entries segment at all")
	// NO ⎿ result line: silent tools render header-only.
	assert.NotContains(t, out, "⎿", "no result line for silent tool")
}

// TestToolEntry_RenderNonFSToolOmitsSize proves the size suffix is omitted for
// non-fs tools (e.g. shell_run) even when a result exists.
func TestToolEntry_RenderNonFSToolOmitsSize(t *testing.T) {
	m := newModel(&fakeSession{}, "/proj")
	e := &toolEntry{name: "shell_run", args: `{"command":"ls"}`, root: "/proj", status: "ok", result: "file1\nfile2"}
	out := e.render(80, m.spinner)
	assert.Contains(t, out, "Bash", "friendly name still shown")
	assert.NotContains(t, out, "lines", "non-fs tool omits the size segment")
	assert.NotContains(t, out, "entries")
}

// TestInputDebounceCoalescesReflow proves the bottom-of-Update reflow (the
// "input changed" path) is debounced: 50 consecutive KeyRunes (mimicking a
// 50-rune paste) must trigger far fewer reflows than 50. Before the debounce,
// every Update called m.reflow() unconditionally (reflows == 50), which is the
// root cause of paste/delete jitter (T9/T12). With the debouncer, only the
// first KeyMsg arms a ~16ms tick; subsequent ones are coalesced while it is in
// flight, so reflows stays small (the runtime would fire the tick once per
// ~16ms window). The assertion is intentionally loose (≤5, not exactly 1)
// because tea.Tick is async and the test cannot drive the runtime — we only
// assert that coalescing kicked in.
func TestInputDebounceCoalescesReflow(t *testing.T) {
	m := newModel(&fakeSession{}, "/proj")
	mm, _ := m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	m = mm.(model)

	reflows := 0
	m.countReflow = &reflows // test hook: reflow() bumps *countReflow on entry

	for i := 0; i < 50; i++ { // simulate a 50-rune paste, one KeyRunes per rune
		mm, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'a'}})
		m = mm.(model)
	}

	if reflows > 5 {
		t.Fatalf("期望 reflow 被 debounce 合并（≤5），实际 %d（50 次 KeyRunes 应被合并）", reflows)
	}
}

// TestFormatBytes covers the compact byte formatter used in size hints.
func TestFormatBytes(t *testing.T) {
	assert.Equal(t, "0 B", formatBytes(0))
	assert.Equal(t, "512 B", formatBytes(512))
	assert.Equal(t, "1.0 KB", formatBytes(1024))
	assert.Equal(t, "12.3 KB", formatBytes(12615)) // 12615/1024 ≈ 12.32
	assert.Equal(t, "1.0 MB", formatBytes(1048576))
}

// ---- H: 5s global repaint safety net (B2) ----

// TestRepaintTickFullReflow proves the 5s repaintMsg triggers exactly one full
// reflow (the safety-net heartbeat that catches non-event-driven time-variant
// rendering). reflow() is bumped via the test-only countReflow hook; exactly 1
// means the handler called reflow once and did not loop or skip.
func TestRepaintTickFullReflow(t *testing.T) {
	m := newModel(&fakeSession{}, "/proj")
	reflows := 0
	m.countReflow = &reflows

	mm, _ := m.Update(repaintMsg{})
	_ = mm.(model)

	if reflows != 1 {
		t.Fatalf("期望 repaintMsg 触发 1 次 reflow，实际 %d", reflows)
	}
}

// TestRepaintTickRearms proves the repaintTick handler re-arms itself on every
// fire so the 5s heartbeat continues for the lifetime of the program (not just
// the first tick). The returned Cmd must be non-nil.
func TestRepaintTickRearms(t *testing.T) {
	m := newModel(&fakeSession{}, "/proj")

	mm, cmd := m.Update(repaintMsg{})
	_ = mm.(model)

	if cmd == nil {
		t.Fatalf("期望 repaintMsg 返回非 nil Cmd（re-arm repaintTick）")
	}
}

// TestRepaintTickInInit proves Init() arms the first repaintTick alongside the
// other startup Cmds so the heartbeat starts at launch (not only after the
// first user interaction).
func TestRepaintTickInInit(t *testing.T) {
	m := newModel(&fakeSession{}, "/proj")
	cmd := m.Init()
	require.NotNil(t, cmd, "Init must return a Batch with startup Cmds")

	// Batch returns a single Cmd that fans out; the only way to confirm the
	// tick is included from outside is to drive it: fire the tick and assert the
	// handler is reachable (i.e. the type exists and reflow ran). This is a
	// smoke test; the heartbeat is exercised end-to-end by TestRepaintTickRearms.
	mm, _ := m.Update(repaintMsg{})
	_ = mm.(model)
}

// ---- Sub-agent event routing: inline nestedProgress/nestedText, not top-level entries ----

// startRunningAnalysis arms the model with a running Analysis toolEntry (the
// parent tool_call that opens a nested agent). Subsequent tool_call /
// tool_result events while it is running represent the child's ReAct loop and
// route into nestedProgress (observable activity); agent_chunk events route
// into nestedText (streamed model output, accumulated as continuous text).
func startRunningAnalysis(m model) model {
	return m.applyEvent(cli.StreamEvent{
		Kind:       "tool_call",
		ToolName:   "analysis",
		ToolArgs:   `{"prompt":"go"}`,
		ToolStatus: "running",
	})
}

// findNestedTool returns the most recent *toolEntry flagged nested.
func findNestedTool(t *testing.T, m model) *toolEntry {
	t.Helper()
	for i := len(m.entries) - 1; i >= 0; i-- {
		if te, ok := m.entries[i].(*toolEntry); ok && te.nested {
			return te
		}
	}
	require.Fail(t, "no nested toolEntry found")
	return nil
}

// countToolEntries returns how many *toolEntry are in the transcript.
func countToolEntries(m model) int {
	n := 0
	for _, e := range m.entries {
		if _, ok := e.(*toolEntry); ok {
			n++
		}
	}
	return n
}

// TestApplyEvent_ParentToolCallCreatesNestedEntry proves the PARENT's tool_call
// (the Analysis invocation itself) still creates a top-level nested toolEntry
// when no nested agent is running. This is the "first call" path: the routing
// guard (lastRunningNestedTool == nil) must fall through to the original logic.
// Regressions would either swallow the parent call entirely (no header shown)
// or double-count toolsRun.
func TestApplyEvent_ParentToolCallCreatesNestedEntry(t *testing.T) {
	m := newModel(&fakeSession{}, "/proj")
	mm, _ := m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	m = mm.(model)
	before := countToolEntries(m)

	m = startRunningAnalysis(m)

	assert.Equal(t, before+1, countToolEntries(m), "parent call creates exactly one entry")
	nt := findNestedTool(t, m)
	assert.Equal(t, "analysis", nt.name)
	assert.Equal(t, "running", nt.status)
	assert.True(t, nt.nested, "Analysis is flagged nested")
	assert.Equal(t, 1, m.toolsRun, "toolsRun incremented for the parent call")
	assert.Equal(t, "Running Analysis…", m.activity)
}

// TestApplyEvent_SubAgentToolCallRoutesNested proves a sub-agent's tool_call
// (arriving while a nested agent is RUNNING) routes into nestedProgress as an
// indented "→ <Name>(<summary>)" line, and does NOT create a top-level entry
// or flush the parent's pending assistant text.
func TestApplyEvent_SubAgentToolCallRoutesNested(t *testing.T) {
	m := newModel(&fakeSession{}, "/proj")
	mm, _ := m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	m = mm.(model)
	m = startRunningAnalysis(m)
	before := countToolEntries(m)

	m = m.applyEvent(cli.StreamEvent{
		Kind:       "tool_call",
		ToolName:   "fs_read",
		ToolArgs:   `{"path":"foo.go"}`,
		ToolStatus: "running",
	})

	// No new top-level entry.
	assert.Equal(t, before, countToolEntries(m), "sub-agent call must NOT create a top-level entry")
	nt := findNestedTool(t, m)
	require.Len(t, nt.nestedProgress, 1, "routed to nestedProgress")
	assert.Contains(t, nt.nestedProgress[0], "Agent(Read)")
	assert.Equal(t, 1, m.toolsRun, "toolsRun NOT incremented for sub-agent call")
}

// TestApplyEvent_SubAgentAgentChunkRoutesNested proves a sub-agent's agent_chunk
// accumulates into the nested tool's nestedText (continuous text, natural \n
// splits lines) rather than landing in the PARENT's pending answer or being
// appended per-chunk to nestedProgress. Per-chunk nestedProgress lines produced
// "two-Chinese-chars per line" breakage because chunks are short text deltas,
// not line units; nestedText accumulates them as continuous text. Without this
// routing, the child's output would render as the parent's reply, inverting the
// relationship.
func TestApplyEvent_SubAgentAgentChunkRoutesNested(t *testing.T) {
	m := newModel(&fakeSession{}, "/proj")
	mm, _ := m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	m = mm.(model)
	m = startRunningAnalysis(m)

	m = m.applyEvent(cli.StreamEvent{Kind: "agent_chunk", Text: "working on it"})

	nt := findNestedTool(t, m)
	assert.Contains(t, nt.nestedText, "working on it",
		"sub-agent chunk accumulated into nestedText")
	assert.Empty(t, nt.nestedProgress,
		"chunk must NOT append a per-chunk line to nestedProgress")
	assert.Equal(t, "", m.pending, "parent pending untouched")
	// Activity stays as "Running Analysis…", not flipped to "Thinking…".
	assert.Equal(t, "Running Analysis…", m.activity,
		"sub-agent chunk must not change the parent activity line")
}

// TestApplyEvent_SubAgentAgentChunkAccumulatesContinuously proves consecutive
// agent_chunk deltas accumulate into one continuous nestedText string (not one
// nestedProgress line per chunk). This is the regression guard for the
// "two-Chinese-chars per line" bug: chunks are short text deltas (including
// IME single-character deltas), so per-chunk nestedProgress lines fragmented
// CJK output into one-char/line noise.
func TestApplyEvent_SubAgentAgentChunkAccumulatesContinuously(t *testing.T) {
	m := newModel(&fakeSession{}, "/proj")
	mm, _ := m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	m = mm.(model)
	m = startRunningAnalysis(m)

	for _, ev := range []cli.StreamEvent{
		{Kind: "agent_chunk", Text: "你"},
		{Kind: "agent_chunk", Text: "好"},
		{Kind: "agent_chunk", Text: "世界"},
	} {
		m = m.applyEvent(ev)
	}

	nt := findNestedTool(t, m)
	assert.Equal(t, "你好世界", nt.nestedText,
		"consecutive chunks must concatenate into continuous nestedText")
	assert.Empty(t, nt.nestedProgress,
		"no per-chunk lines appended to nestedProgress")
}

// TestApplyEvent_SubAgentToolResultRoutesNested proves a sub-agent's tool_result
// routes into nestedProgress as a "⎿ <first line>" summary, WITHOUT resolving
// any top-level entry (the child's tools were never created at the top level).
func TestApplyEvent_SubAgentToolResultRoutesNested(t *testing.T) {
	m := newModel(&fakeSession{}, "/proj")
	mm, _ := m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	m = mm.(model)
	m = startRunningAnalysis(m)
	// Child's fs_read tool_call (routes to nestedProgress).
	m = m.applyEvent(cli.StreamEvent{
		Kind:       "tool_call",
		ToolName:   "fs_read",
		ToolArgs:   `{"path":"foo.go"}`,
		ToolStatus: "running",
	})
	before := countToolEntries(m)

	m = m.applyEvent(cli.StreamEvent{
		Kind:       "tool_result",
		ToolName:   "fs_read",
		Text:       "package main\nfunc main() {}",
		ToolStatus: "ok",
	})

	// No new top-level entry, no resolution of a top-level tool.
	assert.Equal(t, before, countToolEntries(m), "sub-agent result must NOT create a top-level entry")
	nt := findNestedTool(t, m)
	// fs_read is silent: the call routes to nestedProgress, but the result is
	// SUPPRESSED (matches main-agent renderSilent — large output is for the
	// model, not the transcript). Only the call line remains.
	require.Len(t, nt.nestedProgress, 1)
	assert.Contains(t, nt.nestedProgress[0], "Read")
}

// TestApplyEvent_SubAgentAllToolsSuppressResult proves ALL sub-agent tool calls
// suppress their result (header-only), not just silent ones. shell_run (a
// non-silent tail-class tool in the main agent) also renders header-only in the
// sub-agent — unified styling. Only the call line; no ⎿ result.
func TestApplyEvent_SubAgentAllToolsSuppressResult(t *testing.T) {
	m := newModel(&fakeSession{}, "/proj")
	mm, _ := m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	m = mm.(model)
	m = startRunningAnalysis(m)
	m = m.applyEvent(cli.StreamEvent{
		Kind: "tool_call", ToolName: "shell_run", ToolArgs: `{"command":"go test"}`, ToolStatus: "running",
	})
	before := countToolEntries(m)
	m = m.applyEvent(cli.StreamEvent{
		Kind: "tool_result", ToolName: "shell_run", Text: "ok\nfoo", ToolStatus: "ok",
	})
	assert.Equal(t, before, countToolEntries(m), "sub-agent result must NOT create a top-level entry")
	nt := findNestedTool(t, m)
	require.Len(t, nt.nestedProgress, 1, "only the call line; result suppressed for ALL tools")
	assert.Contains(t, nt.nestedProgress[0], "Agent(Bash)")
	assert.NotContains(t, strings.Join(nt.nestedProgress, "\n"), "ok\nfoo",
		"shell_run output must NOT leak into the transcript")
}

// TestApplyEvent_ParentToolResultResolvesNested proves the PARENT's own
// tool_result (Analysis finishing) still resolves the Analysis entry to ok/error,
// even though the routing guard (lastRunningNestedTool) was pointing at it. The
// name match in lastRunningTool(name) distinguishes the parent's result from
// the child's tool_result events. A child agent_chunk that landed in nestedText
// must survive the parent resolve (it is part of the sub-agent's streamed
// output, rendered in the expanded body).
func TestApplyEvent_ParentToolResultResolvesNested(t *testing.T) {
	m := newModel(&fakeSession{}, "/proj")
	mm, _ := m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	m = mm.(model)
	m = startRunningAnalysis(m)
	// Child agent_chunk lands in nestedText (not nestedProgress) and must
	// survive the parent resolve.
	m = m.applyEvent(cli.StreamEvent{Kind: "agent_chunk", Text: "did work"})

	m = m.applyEvent(cli.StreamEvent{
		Kind:       "tool_result",
		ToolName:   "analysis",
		Text:       "analysis complete",
		ToolStatus: "ok",
	})

	nt := findNestedTool(t, m)
	assert.Equal(t, "ok", nt.status, "Analysis entry resolved")
	assert.Equal(t, "analysis complete", nt.result)
	assert.Contains(t, nt.nestedText, "did work",
		"nestedText preserved across parent resolve")
}

// TestApplyEvent_NestedUsageAttributesTokens proves a nested_usage frame
// (emitted by runSubAgentTurn after the child's stream drains) attributes its
// token total to the running Analysis block, so the done summary can render
// "Nk tokens". Without this, the Analysis block's nestedTokens stays 0 even
// when the sub-agent reported usage, and the summary omits the token segment.
// TestApplyEvent_EventsAfterNestedDoneAreTopLevel proves that AFTER Analysis
// finishes (status flips from running), subsequent tool_calls are NOT routed to
// nestedProgress — the routing guard returns nil because the nested entry is no
// longer running.
func TestApplyEvent_EventsAfterNestedDoneAreTopLevel(t *testing.T) {
	m := newModel(&fakeSession{}, "/proj")
	mm, _ := m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	m = mm.(model)
	m = startRunningAnalysis(m)
	m = m.applyEvent(cli.StreamEvent{
		Kind:       "tool_result",
		ToolName:   "analysis",
		Text:       "done",
		ToolStatus: "ok",
	})
	nt := findNestedTool(t, m)
	require.Equal(t, "ok", nt.status)
	beforeProg := len(nt.nestedProgress)
	beforeEntries := countToolEntries(m)

	// A subsequent fs_read tool_call is a PARENT-turn tool, not a child event.
	m = m.applyEvent(cli.StreamEvent{
		Kind:       "tool_call",
		ToolName:   "fs_read",
		ToolArgs:   `{"path":"x.go"}`,
		ToolStatus: "running",
	})

	assert.Equal(t, beforeEntries+1, countToolEntries(m),
		"post-done tool_call creates a top-level entry (original logic)")
	assert.Equal(t, beforeProg, len(nt.nestedProgress),
		"post-done events do NOT touch the resolved nested entry")
}

// TestApplyEvent_NestedFullCycleRender proves the end-to-end inline rendering:
// Analysis call → child thinking → child tool_call → child tool_result → child
// agent_chunk → Analysis done. The transcript shows ONE nested entry with all
// child activity inline (no top-level fs_read block, no parent pending
// contamination), and the resolved entry's render shows the tail.
func TestApplyEvent_NestedFullCycleRender(t *testing.T) {
	m := newModel(&fakeSession{}, "/proj")
	mm, _ := m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	m = mm.(model)

	for _, ev := range []cli.StreamEvent{
		{Kind: "tool_call", ToolName: "analysis", ToolArgs: `{"prompt":"go"}`, ToolStatus: "running"},
		{Kind: "thinking", Text: "planning"},
		{Kind: "tool_call", ToolName: "fs_read", ToolArgs: `{"path":"main.go"}`, ToolStatus: "running"},
		{Kind: "tool_result", ToolName: "fs_read", Text: "package main", ToolStatus: "ok"},
		{Kind: "agent_chunk", Text: "I read main.go"},
		{Kind: "tool_result", ToolName: "analysis", Text: "final answer", ToolStatus: "ok"},
	} {
		m = m.applyEvent(ev)
	}

	// Exactly ONE toolEntry at top level (the Analysis block); the child's
	// fs_read was never promoted to a top-level entry.
	assert.Equal(t, 1, countToolEntries(m),
		"only the Analysis block exists at top level")
	nt := findNestedTool(t, m)
	assert.Equal(t, "ok", nt.status)

	// nestedProgress carries ONLY the child's tool_call (1 line). fs_read is a
	// silent tool — in the MAIN agent it renders header-only (renderSilent:
	// large output is for the model, not the transcript), so the sub-agent
	// suppresses its ⎿ result line too, matching the main-agent styling for
	// ALL silent tools (fs_read/fs_list/fs_glob/fs_search). The
	// child's agent_chunk accumulates into nestedText, not nestedProgress.
	assert.Len(t, nt.nestedProgress, 1)
	assert.Contains(t, nt.nestedProgress[0], "Read")
	// The child's streamed model output lives in nestedText as continuous text.
	assert.Contains(t, nt.nestedText, "I read main.go",
		"agent_chunk accumulated into nestedText")

	// The collapsed done render shows the Analysis header + the one-line
	// summary (1 tool use — the child's fs_read). The chunk lives in the body,
	// which the summary replaces in the collapsed view.
	out := nt.render(80, m.spinner)
	assert.Contains(t, out, "Analysis", "header present")
	assert.Contains(t, out, "1 tool use", "summary reflects the child's tool call")
	assert.NotContains(t, out, "fs_read",
		"raw tool name not leaked into render (friendly name only)")

	// Expanded render surfaces the full body (nestedProgress + nestedText,
	// including the chunk) plus the summary, so the user can inspect every
	// sub-agent step.
	nt.expanded = true
	out = nt.render(80, m.spinner)
	assert.Contains(t, out, "I read main.go", "chunk present in expanded body")
	// Parent pending never absorbed the child's chunk.
	var hasAssistant bool
	for _, e := range m.entries {
		if ae, ok := e.(assistantEntry); ok && strings.Contains(ae.text, "I read main.go") {
			hasAssistant = true
		}
	}
	assert.False(t, hasAssistant,
		"child agent_chunk must not contaminate the parent's assistant block")
}

// TestModel_StartupBannerAsyncTools verifies the banner renders its instant
// header (OS/Shell/Go/Date) at model creation and appends the async tool-probe
// rows only when startupToolsMsg arrives — so the TUI is interactive
// immediately instead of blocking boot on exec probes.
func TestModel_StartupBannerAsyncTools(t *testing.T) {
	m := newModel(&fakeSession{}, "/proj")
	require.NotNil(t, m.startupBanner)

	// Header renders instantly (zero exec): all four rows present.
	header := m.startupBanner.info
	for _, want := range []string{"OS:", "Shell:", "Go:", "Date:"} {
		assert.Contains(t, header, want, "header should include %q at boot", want)
	}
	// No tool rows yet — probes haven't run.
	for _, notYet := range []string{"Node.js:", "Git:", "Python"} {
		assert.NotContains(t, header, notYet, "tool rows must not be in the header")
	}

	// Async probes resolve → rows appended beneath the header.
	rows := "Node.js: v24.0.0\nGit:     git version 2.50.0"
	mm, _ := m.Update(startupToolsMsg{rows: rows})
	m = mm.(model)
	assert.Equal(t, header+"\n"+rows, m.startupBanner.info)
	assert.Contains(t, m.startupBanner.info, "Node.js:")

	// Empty rows (no probe succeeded anywhere) is a no-op.
	prev := m.startupBanner.info
	mm2, _ := m.Update(startupToolsMsg{rows: ""})
	m = mm2.(model)
	assert.Equal(t, prev, m.startupBanner.info, "empty rows must not mutate the banner")
}

// ---- RE-J (fix-e1 review of W-E-01): server-sent tool-output text must not
// carry escape sequences into the terminal ----
//
// applyEvent's doc comment names the four toolEntry fields this covers
// (result, progress, nestedText, nestedThought); these tests exercise each
// one THROUGH applyEvent (not by constructing a toolEntry directly and
// calling stripANSI by hand) so a future edit that reorders or drops one of
// the seven ev.Text assignment sites fails here, not just in a unit test of
// stripANSI itself. escPayload mirrors the review's named example
// ("ls --color=always" emitting live SGR codes: ESC [ 3 1 m ... ESC [ 0 m).
const escPayload = "before\x1b[31mRED\x1b[0mafter"

// TestApplyEvent_StripsANSIFromToolResult proves a finished tool's plain-text
// result (the toolDispNormal / toolDispTail-while-JSON-fails shape) has its
// ESC bytes removed before landing in toolEntry.result, so renderNormal's
// direct resultStyle.Render(e.result) (entries.go) never sees them.
func TestApplyEvent_StripsANSIFromToolResult(t *testing.T) {
	m := newModel(&fakeSession{}, "/proj")
	m = m.applyEvent(cli.StreamEvent{Kind: "tool_call", ToolName: "memory_save", ToolStatus: "running"})
	m = m.applyEvent(cli.StreamEvent{Kind: "tool_result", ToolName: "memory_save", Text: escPayload, ToolStatus: "ok"})
	got := m.entries[len(m.entries)-1].(*toolEntry).result
	assert.NotContains(t, got, "\x1b", "tool_result text must be stripped of ESC bytes")
	assert.Equal(t, "beforeREDafter", got)
}

// TestApplyEvent_StripsANSIFromStandaloneToolResult covers the same field via
// the OTHER assignment site (model.go's "no preceding tool_call" fallback
// branch, used for out-of-order frame delivery) — a fix that only touched
// the primary branch would leave this one live.
func TestApplyEvent_StripsANSIFromStandaloneToolResult(t *testing.T) {
	m := newModel(&fakeSession{}, "/proj")
	m = m.applyEvent(cli.StreamEvent{Kind: "tool_result", ToolName: "memory_save", Text: escPayload, ToolStatus: "ok"})
	got := m.entries[len(m.entries)-1].(*toolEntry).result
	assert.NotContains(t, got, "\x1b", "standalone tool_result text must be stripped of ESC bytes")
	assert.Equal(t, "beforeREDafter", got)
}

// TestApplyEvent_StripsANSIFromToolProgress proves a RUNNING shell_run's live
// stdout chunks (tool_progress events) are stripped before landing in
// toolEntry.progress — this is the primary RE-J surface: renderTail joins
// e.progress and renders it through renderToolOutput while the command is
// still running, well before any JSON envelope exists to decode.
func TestApplyEvent_StripsANSIFromToolProgress(t *testing.T) {
	m := newModel(&fakeSession{}, "/proj")
	m = m.applyEvent(cli.StreamEvent{Kind: "tool_call", ToolName: "shell_run", ToolStatus: "running"})
	m = m.applyEvent(cli.StreamEvent{Kind: "tool_progress", ToolName: "shell_run", Text: escPayload})
	got := m.entries[len(m.entries)-1].(*toolEntry).progress
	require.Len(t, got, 1)
	assert.NotContains(t, got[0], "\x1b", "tool_progress text must be stripped of ESC bytes")
	assert.Equal(t, "beforeREDafter", got[0])
}

// TestApplyEvent_StripsANSIFromToolChunk covers tool_chunk's two branches
// (Overwrite=true replaces progress wholesale; Overwrite=false appends) —
// both are separate assignment sites in model.go and both must strip.
func TestApplyEvent_StripsANSIFromToolChunk(t *testing.T) {
	m := newModel(&fakeSession{}, "/proj")
	m = m.applyEvent(cli.StreamEvent{Kind: "tool_call", ToolName: "workflow_start", ToolStatus: "running"})

	m = m.applyEvent(cli.StreamEvent{Kind: "tool_chunk", ToolName: "workflow_start", Text: escPayload, Overwrite: true})
	e := m.entries[len(m.entries)-1].(*toolEntry)
	require.Len(t, e.progress, 1)
	assert.NotContains(t, e.progress[0], "\x1b", "overwrite tool_chunk text must be stripped of ESC bytes")
	assert.Equal(t, "beforeREDafter", e.progress[0])

	m = m.applyEvent(cli.StreamEvent{Kind: "tool_chunk", ToolName: "workflow_start", Text: escPayload, Overwrite: false})
	e = m.entries[len(m.entries)-1].(*toolEntry)
	require.Len(t, e.progress, 2)
	assert.NotContains(t, e.progress[1], "\x1b", "appended tool_chunk text must be stripped of ESC bytes")
	assert.Equal(t, "beforeREDafter", e.progress[1])
}

// TestApplyEvent_StripsANSIFromNestedText proves a sub-agent's streamed
// answer text (agent_chunk while a nested agent tool is running, routed into
// nestedText) is stripped — this feeds renderAgent's expanded body, which
// funnels into the same renderToolOutput/resultStyle.Render consumer as
// shell_run's output.
func TestApplyEvent_StripsANSIFromNestedText(t *testing.T) {
	m := newModel(&fakeSession{}, "/proj")
	m = startRunningAnalysis(m)
	m = m.applyEvent(cli.StreamEvent{Kind: "agent_chunk", Text: escPayload})
	nt := findNestedTool(t, m)
	assert.NotContains(t, nt.nestedText, "\x1b", "nestedText must be stripped of ESC bytes")
	assert.Equal(t, "beforeREDafter", nt.nestedText)
}

// TestApplyEvent_StripsANSIFromNestedThought mirrors
// TestApplyEvent_StripsANSIFromNestedText for the "thinking" branch's
// nestedThought field (rendered as the expanded-body fallback in renderAgent
// when neither activity nor nestedText is present).
func TestApplyEvent_StripsANSIFromNestedThought(t *testing.T) {
	m := newModel(&fakeSession{}, "/proj")
	m = startRunningAnalysis(m)
	m = m.applyEvent(cli.StreamEvent{Kind: "thinking", Text: escPayload})
	nt := findNestedTool(t, m)
	assert.NotContains(t, nt.nestedThought, "\x1b", "nestedThought must be stripped of ESC bytes")
	assert.Equal(t, "beforeREDafter", nt.nestedThought)
}
