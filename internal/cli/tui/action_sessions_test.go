package tui

import (
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/x6nux/yanshi/internal/cli"
	"github.com/x6nux/yanshi/internal/proto"
)

// TestActionPaletteOffersSessions closes the one gap UX1 had left.
//
// Ctrl+K's toggle, fuzzy ranking, Esc handling and 10-row window were all real;
// collectActions just never collected a session. Its own comment said
// "session + tool/MCP source DEFERRED" while the acceptance criterion asks for
// 命令/模式/模型/会话 -- so the palette was three quarters of its specification
// and the code said so out loud.
//
// Selecting the entry has to actually restore, not merely type the command:
// an action item that fills the prompt and waits is indistinguishable from
// tab-completion, and the palette's other three sources all execute.
func TestActionPaletteOffersSessions(t *testing.T) {
	rec := &recordingSession{}
	m := newModel(rec, "/proj")
	m = m.applyEvent(cli.StreamEvent{Kind: "sessions", Sessions: []proto.SessionInfo{
		{ID: "sess-1", Title: "refactor the parser", MsgCount: 12},
		{ID: "sess-2", Title: "chase a flaky test", MsgCount: 3},
	}})

	var found *actionItem
	for i, it := range m.collectActions() {
		if it.Source == "session" {
			found = &m.collectActions()[i]
			break
		}
	}
	if found == nil {
		t.Fatal("the action palette has no session source: Ctrl+K offers commands, " +
			"modes, models and themes, and the acceptance criterion asks for sessions too")
	}
	if found.Label == "" {
		t.Error("a session item with no label cannot be searched for")
	}

	before := len(rec.frames)
	mm, _ := found.Action(m)
	_ = mm.(model)
	if len(rec.frames) == before {
		t.Fatal("selecting a session sent no frame: the item must restore the session, " +
			"not just type its id into the prompt")
	}
	last := rec.frames[len(rec.frames)-1]
	if last.Type != "restore_session" {
		t.Errorf("selecting a session sent %q, want restore_session", last.Type)
	}
}

// TestActionPaletteRefreshesSessionsOnOpen pins the async half.
//
// The model source works this way already: opening the palette fires a fresh
// list_models even when the cache is warm, because a long-lived session's list
// goes stale. Sessions have exactly the same problem -- a session created in
// another window is invisible until something refetches -- and the palette is
// the one place a user would expect to find it.
func TestActionPaletteRefreshesSessionsOnOpen(t *testing.T) {
	rec := &recordingSession{}
	m := newModel(rec, "/proj")
	mm, _, handled := m.handleKeyMsg(tea.KeyMsg{Type: tea.KeyCtrlK})
	if !handled {
		t.Fatal("Ctrl+K did not open the palette")
	}
	_ = mm

	var sawSessionList bool
	for _, f := range rec.frames {
		if f.Type == "session_list" {
			sawSessionList = true
		}
	}
	if !sawSessionList {
		t.Error("opening the palette did not refresh the session list: a session " +
			"created in another window would never appear")
	}
}
