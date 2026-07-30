package tools

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/x6nux/yanshi/internal/task/work"
)

// TestTaskContext_SiblingIsolation: two sibling contexts derived from the same
// parent must not leak thread/plan/manager state to each other.
func TestTaskContext_SiblingIsolation(t *testing.T) {
	parent := context.Background()
	cA := WithThreadLink(parent, "th-A", "tn-A")
	cA = WithPlanMode(cA, true)
	cB := WithThreadLink(parent, "th-B", "tn-B")
	cB = WithPlanMode(cB, false)

	a, ok := ThreadLinkFromContext(cA)
	require.True(t, ok)
	assert.Equal(t, "th-A", a.ThreadID)
	assert.True(t, PlanModeActive(cA))

	b, ok := ThreadLinkFromContext(cB)
	require.True(t, ok)
	assert.Equal(t, "th-B", b.ThreadID)
	assert.False(t, PlanModeActive(cB))

	// parent itself has no values
	_, ok = ThreadLinkFromContext(parent)
	assert.False(t, ok)
	assert.False(t, PlanModeActive(parent))
}

// TestTaskManager_NilDoesNotOverride: WithTaskManager(nil) returns the parent
// context unchanged so a nil Manager never shadows a real one set higher up.
func TestTaskManager_NilDoesNotOverride(t *testing.T) {
	fake := work.NewFakeManager()
	parent := WithTaskManager(context.Background(), fake)
	// child passes nil → parent's fake survives
	child := WithTaskManager(parent, nil)
	got, ok := TaskManagerFromContext(child)
	require.True(t, ok)
	require.NotNil(t, got)
	// same pointer identity is not required (interface) but fake is the one set
	assert.NotNil(t, got)

	// empty parent → false
	empty, ok := TaskManagerFromContext(context.Background())
	assert.False(t, ok)
	assert.Nil(t, empty)
}

// TestWorkEventCallback_RoundTrip: a registered callback receives the exact
// work.Event pushed by EmitWorkEvent; EmitWorkEvent with no callback bound
// (fresh ctx) is a no-op and does not panic.
func TestWorkEventCallback_RoundTrip(t *testing.T) {
	// fresh ctx → no callback → no panic, no side effect
	empty := context.Background()
	EmitWorkEvent(empty, work.Event{Kind: work.EventTaskUpdate})

	var seen []work.Event
	cb := WorkEventCallback(func(e work.Event) {
		seen = append(seen, e)
	})
	ctx := WithWorkEventCallback(empty, cb)
	EmitWorkEvent(ctx, work.Event{Kind: work.EventTaskUpdate, TaskID: "wt-1"})
	EmitWorkEvent(ctx, work.Event{Kind: work.EventChecklistUpdate, TaskID: "wt-2"})

	require.Len(t, seen, 2)
	assert.Equal(t, work.EventTaskUpdate, seen[0].Kind)
	assert.Equal(t, "wt-1", seen[0].TaskID)
	assert.Equal(t, work.EventChecklistUpdate, seen[1].Kind)

	// WithWorkEventCallback(ctx, nil) 是 no-op：保留 ctx 原有的 callback
	ctx2 := WithWorkEventCallback(ctx, nil)
	EmitWorkEvent(ctx2, work.Event{Kind: work.EventPlanUpdate})
	require.Len(t, seen, 3)
	assert.Equal(t, work.EventPlanUpdate, seen[2].Kind)
}

// TestPlanModeDefault: zero-value context returns false.
func TestPlanModeDefault(t *testing.T) {
	assert.False(t, PlanModeActive(context.Background()))
}

// TestThreadLinkZeroValue: missing thread link returns ok=false.
func TestThreadLinkZeroValue(t *testing.T) {
	_, ok := ThreadLinkFromContext(context.Background())
	assert.False(t, ok)
}
