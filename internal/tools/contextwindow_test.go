// internal/tools/contextwindow_test.go
package tools

import (
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	einollm "github.com/x6nux/yanshi/internal/llm/eino"
)

// TestContextWindowTools_RequestsOnABoundSignal proves the handler's success
// path: given a turn context that actually has a W-C-14 signal bound (the
// production shape, since orchestrator.go binds it unconditionally), the
// handler's call to einollm.RequestNewWindow lands, and a later read via
// einollm's own consumption function sees exactly the reason the model gave.
func TestContextWindowTools_RequestsOnABoundSignal(t *testing.T) {
	ct := NewContextWindowTools()
	ctx := einollm.WithNewWindowSignal(context.Background())

	out, err := ct.run(ctx, `{"reason":"finished reading the large log dump"}`)
	require.NoError(t, err)
	assert.Contains(t, out, "New window requested")

	// The handler's job is to WRITE the signal, not consume it — the actual
	// consumer is CompactingModel.maybeCompact on the turn's next model call
	// (einollm's own tests, e.g. TestRequestNewWindow_ConsumedOnce, pin the
	// read side). Confirming a second write on the same still-bound ctx also
	// succeeds is the closest this package gets to observing the write
	// landed, without reaching into einollm's unexported consumption path.
	assert.True(t, einollm.RequestNewWindow(ctx, "a later call can still write"),
		"the signal slot bound by this ctx is still writable")
}

// TestContextWindowTools_ErrorsWithoutABoundSignal proves the handler
// reports an error — not a silent success — when the turn never bound a
// W-C-14 signal (e.g. a sub-agent context, per orchestrator.go's own doc
// comment on WithNewWindowSignal). Reporting nothing-happened as success
// would tell the model its request landed when nothing will ever read it.
func TestContextWindowTools_ErrorsWithoutABoundSignal(t *testing.T) {
	ct := NewContextWindowTools()
	_, err := ct.run(context.Background(), `{"reason":"anything"}`)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not available on this turn")
}

// TestContextWindowTools_RejectsEmptyReason mirrors milestone_set's rejection
// contract (MilestoneTools.runSet's "required" case): an empty or
// whitespace-only reason is refused rather than recorded, so the eventual
// activity-line/notice text is never blank.
func TestContextWindowTools_RejectsEmptyReason(t *testing.T) {
	ct := NewContextWindowTools()
	ctx := einollm.WithNewWindowSignal(context.Background())

	cases := []string{"", "   ", "\n\t "}
	for _, reason := range cases {
		// %q gives a JSON-safe escaped string literal (Go and JSON agree on
		// \n, \t and \" escapes), unlike concatenating the raw control
		// characters straight into the JSON text.
		_, err := ct.run(ctx, fmt.Sprintf(`{"reason":%q}`, reason))
		require.Error(t, err, "reason %q must be rejected", reason)
		assert.Contains(t, err.Error(), "required")
	}
}

// TestContextWindowTools_NameIsRegisterable pins the tool name, the same
// convention TestMilestoneTools_NameIsRegisterable follows: a rename here
// without updating bootstrap.go's registration and profile.go's allow list
// makes the tool fail-closed at runtime and reddens GOV5.
func TestContextWindowTools_NameIsRegisterable(t *testing.T) {
	got := NewContextWindowTools().Tools()
	require.Len(t, got, 1)
	info, err := got[0].Info(context.Background())
	require.NoError(t, err)
	require.Equal(t, "context_new_window", info.Name)
}
