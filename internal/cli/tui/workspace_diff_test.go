package tui

import (
	"strings"
	"testing"

	"github.com/charmbracelet/bubbles/spinner"
	"github.com/muesli/termenv"

	"github.com/x6nux/yanshi/internal/cli"
	"github.com/x6nux/yanshi/internal/proto"
)

// TestCmdDiff_SSERejected mirrors TestRestoreTurnCommand's SSE guard: /diff
// is a control frame with no SSE-side handling (SSE has no persistent
// server-side history to query), so SSE mode must reject before sending
// anything.
func TestCmdDiff_SSERejected(t *testing.T) {
	srec := &sseSession{}
	m := newModel(srec, "/proj")
	mm, _ := m.runCommand("/diff")
	got := mm.(model)
	if len(srec.frames) != 0 {
		t.Fatalf("SSE mode must not send a frame, got %d", len(srec.frames))
	}
	foundErr := false
	for _, e := range got.entries {
		if ee, ok := e.(errorEntry); ok && strings.Contains(ee.text, "WebSocket") {
			foundErr = true
		}
	}
	if !foundErr {
		t.Fatal("expected an errorEntry naming the WebSocket requirement")
	}
}

// TestCmdDiff_SendsListWorkspaceDiffFrame proves the no-arg WS form sends a
// list_workspace_diff control frame.
func TestCmdDiff_SendsListWorkspaceDiffFrame(t *testing.T) {
	rec := &recordingSession{}
	m := newModel(rec, "/proj")
	cmdDiff(m, []string{})
	if len(rec.frames) != 1 {
		t.Fatalf("expected 1 frame sent, got %d", len(rec.frames))
	}
	if rec.frames[0].Type != "list_workspace_diff" {
		t.Errorf("frame.Type = %q, want list_workspace_diff", rec.frames[0].Type)
	}
}

// TestCmdDiff_RejectsArgs proves /diff takes no arguments — it is a plain
// snapshot request, unlike /restore-turn's id/yes forms.
func TestCmdDiff_RejectsArgs(t *testing.T) {
	rec := &recordingSession{}
	m := newModel(rec, "/proj")
	out, _ := cmdDiff(m, []string{"bogus"})
	got := out.(model)
	if len(rec.frames) != 0 {
		t.Fatalf("unexpected args must not send a frame, got %d", len(rec.frames))
	}
	foundUsage := false
	for _, e := range got.entries {
		if ee, ok := e.(errorEntry); ok && strings.Contains(ee.text, "usage: /diff") {
			foundUsage = true
		}
	}
	if !foundUsage {
		t.Fatal("expected a usage errorEntry")
	}
}

// TestApplyEvent_WorkspaceDiffPopulatesEntry proves the workspace_diff reply
// is routed to a workspaceDiffEntry carrying the reply's file list verbatim.
func TestApplyEvent_WorkspaceDiffPopulatesEntry(t *testing.T) {
	rs := &recordingSession{}
	m := newModel(rs, "/proj")
	ev := cli.StreamEvent{
		Kind: "workspace_diff",
		WorkspaceDiff: []proto.WorkspaceDiffFile{
			{Path: "a.go", Op: "modified", OldText: "old", NewText: "new"},
		},
	}
	m = m.applyEvent(ev)
	var found *workspaceDiffEntry
	for _, e := range m.entries {
		if wd, ok := e.(workspaceDiffEntry); ok {
			found = &wd
		}
	}
	if found == nil {
		t.Fatal("workspaceDiffEntry not appended to entries")
	}
	if len(found.files) != 1 || found.files[0].Path != "a.go" {
		t.Fatalf("workspaceDiffEntry.files = %+v, want the reply's file list", found.files)
	}
}

// TestWorkspaceDiffEntry_Render_EmptyList proves the empty-changeset case
// renders an explicit "no pending changes" line, matching seamsEntry's
// empty-list convention rather than silently rendering nothing.
func TestWorkspaceDiffEntry_Render_EmptyList(t *testing.T) {
	e := workspaceDiffEntry{}
	out := e.render(80, spinner.Model{})
	if !strings.Contains(out, "no pending workspace changes") {
		t.Fatalf("empty entry should say so explicitly, got %q", out)
	}
}

// TestWorkspaceDiffEntry_Render_ShowsPerFileDiff is the literal fulfillment
// of W-E-13's "复用 W-E-02 的渲染" acceptance criterion: for each file the
// entry must feed difflib.Compute(OldText, NewText) into renderColoredDiff
// (the W-E-02 function), so the reply shows the actual +/- diff content —
// not just a bare file-name listing.
func TestWorkspaceDiffEntry_Render_ShowsPerFileDiff(t *testing.T) {
	e := workspaceDiffEntry{files: []proto.WorkspaceDiffFile{
		{Path: "a.go", Op: "modified", OldText: "line one\nline two", NewText: "line one\nline TWO"},
		{Path: "b.go", Op: "added", OldText: "", NewText: "brand new"},
		{Path: "c.go", Op: "deleted", OldText: "gone now", NewText: ""},
	}}
	out := e.render(80, spinner.Model{})
	for _, want := range []string{"a.go", "b.go", "c.go"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing path %q:\n%s", want, out)
		}
	}
	if !strings.Contains(out, "-line two") || !strings.Contains(out, "+line TWO") {
		t.Errorf("modified file must show a real diff (old/new lines), got:\n%s", out)
	}
	if !strings.Contains(out, "+brand new") {
		t.Errorf("added file must render as an all-insert diff, got:\n%s", out)
	}
	if !strings.Contains(out, "-gone now") {
		t.Errorf("deleted file must render as an all-delete diff, got:\n%s", out)
	}
}

// TestWorkspaceDiffEntry_Render_AsciiSuppressesColor is the mandatory
// negative control for W-E-13's new /diff rendering path: it must not be a
// 4th hand-rolled-ANSI bypass (E1 found three; W-E-02 proved renderColoredDiff
// itself is not a fourth). workspaceDiffEntry.render delegates entirely to
// renderColoredDiff, so under termenv.Ascii it must emit zero escape bytes,
// with a positive control proving the same input DOES emit escapes under a
// color-capable profile (so the zero count is the profile suppressing color,
// not the function never emitting any).
func TestWorkspaceDiffEntry_Render_AsciiSuppressesColor(t *testing.T) {
	e := workspaceDiffEntry{files: []proto.WorkspaceDiffFile{
		{Path: "a.go", Op: "modified", OldText: "a\nb\nc", NewText: "a\nx\nc"},
	}}

	withColorProfile(t, termenv.Ascii)
	out := e.render(80, spinner.Model{})
	if strings.ContainsRune(out, '\x1b') {
		t.Fatalf("Ascii profile: workspaceDiffEntry.render output still contains an ANSI escape byte: %q", out)
	}

	withColorProfile(t, termenv.ANSI256)
	colored := e.render(80, spinner.Model{})
	if !strings.ContainsRune(colored, '\x1b') {
		t.Fatalf("ANSI256 profile: expected workspaceDiffEntry.render to emit ANSI escapes, got %q", colored)
	}
}
