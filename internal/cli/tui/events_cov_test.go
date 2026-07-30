package tui

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestCov_DiscardLastThinking_ThinkingEntry covers the branch where the last
// entry is a standalone thinkingEntry (removed entirely).
func TestCov_DiscardLastThinking_ThinkingEntry(t *testing.T) {
	m := newModel(&fakeSession{}, "/proj")
	m.entries = []entry{&thinkingEntry{text: "lonely thought"}}
	m.discardLastThinking()
	assert.Empty(t, m.entries, "trailing thinkingEntry removed")
}

// TestCov_ToggleExpand_AssistantThought covers the assistantEntry.thought
// toggle branch (ctrl+o on an assistant block with embedded reasoning).
func TestCov_ToggleExpand_AssistantThought(t *testing.T) {
	m := newModel(&fakeSession{}, "/proj")
	m.entries = []entry{assistantEntry{text: "a", thought: &thinkingEntry{text: "t"}}}
	ok := m.toggleExpand()
	assert.True(t, ok)
	assert.True(t, m.entries[0].(assistantEntry).thought.expanded, "thought expanded toggled")
}

// TestCov_DetachTrailingThoughts_MergeTimestamps covers both timestamp-merge
// branches with overlapping intervals: e1 (position 0) started+ended later,
// e2 (position 1) started earlier but ended latest — so the merge takes the
// earliest startedAt (181-183) AND the latest endedAt (178-180).
func TestCov_DetachTrailingThoughts_MergeTimestamps(t *testing.T) {
	t1 := time.Now()
	t2 := t1.Add(5 * time.Minute) // t1 < t2
	e1 := &thinkingEntry{text: "first", startedAt: t2, endedAt: t2}
	e2 := &thinkingEntry{text: "second", startedAt: t1, endedAt: t1.Add(time.Hour)}
	remaining, merged := detachTrailingThoughts([]entry{e1, e2})
	assert.Empty(t, remaining)
	require.NotNil(t, merged)
	assert.Equal(t, t1, merged.startedAt, "earliest startedAt wins")
	assert.Equal(t, t1.Add(time.Hour), merged.endedAt, "latest endedAt wins")
}
