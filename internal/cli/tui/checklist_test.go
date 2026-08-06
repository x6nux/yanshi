package tui

import (
	"strings"
	"testing"

	"github.com/charmbracelet/bubbles/spinner"

	"github.com/x6nux/yanshi/internal/cli"
	"github.com/x6nux/yanshi/internal/task/work"
)

func sampleChecklist() work.Checklist {
	return work.Checklist{Items: []work.ChecklistItem{
		{ID: 1, Content: "read the spec", Status: work.ChecklistDone},
		{ID: 2, Content: "write the parser", Status: work.ChecklistInProgress},
		{ID: 3, Content: "wire it up", Status: work.ChecklistPending},
	}}
}

// TestPlanUpdateFrameReachesTheTranscript is the streaming clause.
//
// Every hop had a test — update_plan wrote to the store, the tool emitted a
// work.Event, the WS layer turned it into a plan_update frame, proto round-trips
// it — and the TUI's frame switch had no branch for it, so the frame arrived and
// was dropped. A per-hop test suite that never checks the last hop reports full
// coverage of a feature the user cannot see.
//
// ledger: A2/G05#2 计划可流式更新
func TestPlanUpdateFrameReachesTheTranscript(t *testing.T) {
	list := sampleChecklist()
	for _, kind := range []string{"plan_update", "checklist_update"} {
		t.Run(kind, func(t *testing.T) {
			m := newTestModel(t)
			before := len(m.entries)
			m = m.applyEvent(cli.StreamEvent{Kind: kind, ID: "task-1", Checklist: &list})
			if len(m.entries) == before {
				t.Fatalf("a %s frame produced no transcript entry: the update never reaches the user", kind)
			}
			out := m.entries[len(m.entries)-1].render(80, spinner.Model{})
			for _, want := range []string{"read the spec", "write the parser", "wire it up"} {
				if !strings.Contains(out, want) {
					t.Errorf("rendered block omits %q:\n%s", want, out)
				}
			}
			// The in-progress step has to be distinguishable with styling
			// stripped, which is what a non-TTY and a pasted transcript give.
			if !strings.Contains(out, "[~] write the parser") {
				t.Errorf("the running step carries no plain-text marker:\n%s", out)
			}
			if !strings.Contains(out, "[x] read the spec") || !strings.Contains(out, "[ ] wire it up") {
				t.Errorf("done and pending steps are not distinguishable:\n%s", out)
			}
		})
	}
}

// TestChecklistRenderDistinguishesReplaceFromPatch pins the one thing the two
// frame types differ on.
//
// ledger: A2/G05#2 计划可流式更新
func TestChecklistRenderDistinguishesReplaceFromPatch(t *testing.T) {
	list := sampleChecklist()
	replaced := checklistEntry{taskID: "t", list: list, replaced: true}.render(80, spinner.Model{})
	patched := checklistEntry{taskID: "t", list: list, replaced: false}.render(80, spinner.Model{})
	if replaced == patched {
		t.Error("a replaced plan and a patched checklist render identically; the operator " +
			"cannot tell a rewrite from an addition")
	}
	if !strings.Contains(replaced, "plan") || !strings.Contains(patched, "checklist") {
		t.Errorf("headers do not name what happened:\n%q\n%q", replaced, patched)
	}
}

// TestChecklistUnknownStatusIsNotRenderedAsDone guards the vocabulary.
//
// A status the renderer does not know must not fall through to the done glyph:
// showing a step as finished when it is not is the one wrong answer here.
func TestChecklistUnknownStatusIsNotRenderedAsDone(t *testing.T) {
	e := checklistEntry{list: work.Checklist{Items: []work.ChecklistItem{
		{ID: 1, Content: "mystery", Status: work.ChecklistItemStatus("blocked")},
	}}}
	out := e.render(80, spinner.Model{})
	if strings.Contains(out, "[x] mystery") {
		t.Errorf("an unknown status rendered as done:\n%s", out)
	}
	if !strings.Contains(out, "blocked") {
		t.Errorf("the unknown status value is not shown, so nobody learns the vocabulary drifted:\n%s", out)
	}
}
