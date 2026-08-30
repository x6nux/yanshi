// internal/agent/orchestrator/compaction_redact_test.go
//
// Wiring test for C11 on the MID-TURN compaction path.
//
// The pre-turn (WS/SSE) and mid-turn (ReAct iteration) paths reach the same
// compaction core through completely different plumbing, so wiring one proves
// nothing about the other. Both must carry the redactor: a tool_result holding
// an API key is folded into a summary, and a summary is a PINNED message, so
// the key is re-sent to the provider on every subsequent turn long after the
// tool_result it came from was compacted away.

package orchestrator

import (
	"testing"

	"github.com/stretchr/testify/require"

	einollm "github.com/x6nux/yanshi/internal/llm/eino"
	"github.com/x6nux/yanshi/internal/secrets"
)

// TestWrapCompactionForwardsTheRedactor pins that CompactionConfig.Redactor
// reaches the CompactingModel that actually runs mid-turn compaction. Dropping
// the assignment in wrapCompaction leaves a config field with no reader --
// compiling, looking wired, and redacting nothing.
func TestWrapCompactionForwardsTheRedactor(t *testing.T) {
	red := secrets.NewRedactor()
	red.Register("sk-live-must-not-leak")

	cc := CompactionConfig{
		Threshold:     0.8,
		ContextWindow: 128000,
		KeepRecent:    4,
		Redactor:      red,
	}

	wrapped := wrapCompaction(einollm.NewFakeModel(nil, nil), cc, 128000, 0, nil)
	cm, ok := wrapped.(*einollm.CompactingModel)
	require.True(t, ok, "compaction must be enabled")

	require.NotNil(t, cm.Redactor,
		"mid-turn compaction dropped the redactor: secrets in the summarized "+
			"history would be sent to the summary model and pinned into the summary")
	require.Equal(t, "token=[REDACTED]", cm.Redactor.Redact("token=sk-live-must-not-leak"),
		"the forwarded redactor must be the configured one, with its registry intact")
}

// TestWrapCompactionWithoutRedactorLeavesItNil pins that the no-secrets case
// stays on the historical code path. A nil here must remain a nil INTERFACE:
// ctxcompact skips redaction by comparing the interface against nil, and
// (*secrets.Redactor).Redact locks a field of its receiver, so a typed-nil
// would panic mid-turn instead of disabling redaction.
func TestWrapCompactionWithoutRedactorLeavesItNil(t *testing.T) {
	cc := CompactionConfig{Threshold: 0.8, ContextWindow: 128000, KeepRecent: 4}

	wrapped := wrapCompaction(einollm.NewFakeModel(nil, nil), cc, 128000, 0, nil)
	cm, ok := wrapped.(*einollm.CompactingModel)
	require.True(t, ok, "compaction must be enabled")

	require.Nil(t, cm.Redactor,
		"an unset Redactor must stay a nil interface so ctxcompact's nil guard fires")
}
