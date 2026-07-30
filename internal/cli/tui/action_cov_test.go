package tui

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestCov_CollectActions_InvokeClosures covers every action-source closure
// (command / mode / model / theme) by invoking each collected item's Action.
// m.models is populated so the model source contributes at least one item.
func TestCov_CollectActions_InvokeClosures(t *testing.T) {
	m := newModel(&fakeSession{}, "/proj")
	m.models = []string{"gpt-4"}
	items := m.collectActions()
	require.NotEmpty(t, items)
	// Invoke every item's Action — each is a value-receiver closure, so the
	// calls are independent and must not panic.
	for _, it := range items {
		_, _ = it.Action(m)
	}
}

// TestCov_ActionMove_EmptyItems covers the n==0 no-op guard (avoids a divide-by-
// zero when the palette is open but empty).
func TestCov_ActionMove_EmptyItems(t *testing.T) {
	m := newModel(&fakeSession{}, "/proj")
	m.action = &actionState{visible: true} // empty items
	m.actionMove(1)
	assert.Equal(t, 0, m.action.cursor, "no-op when items is empty")
}

// TestCov_ActionPopup_ScrollWindow covers the cursor>=maxRows scroll branch
// (the popup window starts past the first row so the cursor stays visible).
func TestCov_ActionPopup_ScrollWindow(t *testing.T) {
	m := newModel(&fakeSession{}, "/proj")
	items := make([]actionItem, 12)
	for i := range items {
		items[i] = actionItem{Label: "x", Source: "command", Hint: "h"}
	}
	m.action = &actionState{visible: true, cursor: 10, items: items}
	out := m.actionPopup()
	assert.Contains(t, out, "Actions")
}
