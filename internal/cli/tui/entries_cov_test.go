package tui

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

// This file lifts entries.go coverage by exercising the render branches the
// existing suites leave cold: the expanded/edge variants of renderTail,
// renderAgent, renderNormal, renderDiff, renderColoredDiff, and firstLine.

// TestCov_RenderTailFinishedExpanded covers renderTail's finished + expanded
// path: a resolved shell_run whose JSON output is rendered in full (not the
// 3-line collapsed tail).
func TestCov_RenderTailFinishedExpanded(t *testing.T) {
	e := &toolEntry{
		name:     "shell_run",
		args:     `{"command":"echo"}`,
		root:     ".",
		status:   "ok",
		result:   `{"output":"line one\nline two\nline three","exit":0,"duration_ms":5}`,
		expanded: true,
	}
	out := e.render(80, newSpinner())
	assert.Contains(t, out, "line one", "expanded finished tail shows the full output")
	assert.Contains(t, out, "line three")
}

// TestCov_RenderAgentRunningExpandedEmptyBody covers renderAgent's running +
// expanded + empty-body fast return (header only, no panel) — a pure-reasoning
// child inspected via ctrl+o before it has produced anything.
func TestCov_RenderAgentRunningExpandedEmptyBody(t *testing.T) {
	e := &toolEntry{name: "analysis", status: "running", nested: true, expanded: true}
	out := e.render(80, newSpinner())
	assert.NotEmpty(t, out)
	// Header only: no indented body panel lines.
	assert.True(t, strings.Count(out, "\n") <= 3, "empty expanded running is header-only")
}

// TestCov_RenderNormalExpandedResult covers renderNormal's expanded + non-empty
// result path (full ⎿ result instead of the truncated one-liner).
func TestCov_RenderNormalExpandedResult(t *testing.T) {
	e := &toolEntry{
		name:     "memory_save",
		args:     `{"k":"v"}`,
		root:     ".",
		status:   "ok",
		result:   "saved to the long-term store\nwith detail",
		expanded: true,
	}
	out := e.render(80, newSpinner())
	assert.Contains(t, out, "saved to the long-term store")
	assert.Contains(t, out, "with detail", "expanded renders the full multi-line result")
}

// TestCov_RenderDiffRunning covers renderDiff's running fast path (header +
// blank line, before any old/new to diff).
func TestCov_RenderDiffRunning(t *testing.T) {
	e := &toolEntry{
		name:   "fs_edit",
		args:   `{"old_string":"a","new_string":"b"}`,
		root:   ".",
		status: "running",
	}
	out := e.render(80, newSpinner())
	assert.Contains(t, out, "Edit")
	// Running: no diff footprint line yet.
	assert.NotContains(t, out, "行")
}

// TestCov_RenderDiffFSEditBadArgsFallsBack covers renderDiff's fs_edit branch
// when args lack old_string/new_string — it falls back to renderNormal so a
// malformed frame still surfaces a result line instead of an empty block.
func TestCov_RenderDiffFSEditBadArgsFallsBack(t *testing.T) {
	e := &toolEntry{
		name:   "fs_edit",
		args:   `{"path":"foo.go"}`, // no old_string/new_string
		root:   ".",
		status: "ok",
		result: "edit applied",
	}
	out := e.render(80, newSpinner())
	assert.Contains(t, out, "⎿", "bad-args fallback renders a normal result line")
	assert.Contains(t, out, "edit applied")
}

// TestCov_RenderDiffFSEditNoChangeCollapsed covers renderDiff's collapsed
// fs_edit footprint path: identical old/new yields a "+0 -0 行" footprint.
// (The separate `diff == ""` branch is unreachable here — parseEditStrings
// rejects both-empty args, and unifiedDiff only returns "" when ops is empty,
// which needs both inputs empty — so identical non-empty strings still produce
// a zero-change context diff rather than an empty one.)
func TestCov_RenderDiffFSEditNoChangeCollapsed(t *testing.T) {
	e := &toolEntry{
		name:   "fs_edit",
		args:   `{"old_string":"same","new_string":"same"}`,
		root:   ".",
		status: "ok",
	}
	out := e.render(80, newSpinner())
	assert.Contains(t, out, "+0 -0 行", "identical old/new shows a zero-change footprint")
}

// TestCov_RenderDiffFSWriteExpanded covers the fs_write expanded path: a
// "wrote N lines" footprint plus a first-line preview of the content.
func TestCov_RenderDiffFSWriteExpanded(t *testing.T) {
	e := &toolEntry{
		name:     "fs_write",
		args:     `{"content":"package main\n\nfunc main() {}"}`,
		root:     ".",
		status:   "ok",
		expanded: true,
	}
	out := e.render(80, newSpinner())
	assert.Contains(t, out, "wrote 3 lines")
	assert.Contains(t, out, "package main", "expanded shows the first-line preview")
}

// TestCov_RenderAgentActivityJoinsProgressAndNested covers the activity-join
// separator branch: when a single-agent tool (progressOverwrite=false) has BOTH
// nestedProgress (per-tool lines from SubAgentEmit) and progress (tool_chunk
// Text from bindSubAgentProgress), the two are joined with a newline.
func TestCov_RenderAgentActivityJoinsProgressAndNested(t *testing.T) {
	e := &toolEntry{
		name:           "analysis",
		status:         "running",
		nested:         true,
		nestedProgress: []string{"Agent(Read) ◌"},
		progress:       []string{"Read(foo.go)\n"},
	}
	out := e.render(80, newSpinner())
	assert.Contains(t, out, "Agent(Read)", "nestedProgress line present")
	assert.Contains(t, out, "Read(foo.go)", "progress line present")
}

// TestCov_RenderColoredDiffBlankLine covers the empty-line branch of
// renderColoredDiff: a blank context line renders as just the 4-space indent.
func TestCov_RenderColoredDiffBlankLine(t *testing.T) {
	out := renderColoredDiff("a\n\n+b")
	// The blank middle line becomes a bare indent line; the +b line is present.
	assert.Contains(t, out, "+b")
	assert.True(t, strings.Contains(out, "    "), "blank diff line is padded")
}

// TestCov_FirstLineMultiline covers firstLine's newline branch (returns only
// the first line of a multi-line string).
func TestCov_FirstLineMultiline(t *testing.T) {
	assert.Equal(t, "first", firstLine("first\nsecond\nthird"))
	assert.Equal(t, "first", firstLine("   first\nsecond"))
	// Single-line / all-whitespace still safe.
	assert.Equal(t, "only", firstLine("only"))
	assert.Equal(t, "", firstLine("   "))
}
