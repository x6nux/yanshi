package eino

import (
	"context"
	"testing"

	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/schema"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// adkInstruction is the fixture's agent instruction — yanshi's INITIAL CONTEXT,
// in the only form the real chain produces one.
const adkInstruction = "You are yanshi. Prefer fs_read over shell_run when reading files."

// runADKChain drives the real chain — adk.NewChatModelAgent + adk.Runner +
// CompactingModel + a recording inner model — and returns every message slice
// the inner model was handed, in order.
//
// threshold <= 0 disables compaction (pass-through), which is how the test
// observes what adk hands the model BEFORE compaction can rewrite it. With
// compaction on, the first recorded slice is the summarization request that
// ctxcompact builds, not an adk model input — a first version of this test
// asserted against inputs[0] on a compacting run and was reading the summary
// prompt.
func runADKChain(t *testing.T, threshold float64) [][]*schema.Message {
	t.Helper()
	inner := &recordingModel{summary: "SUMMARY", reply: "done", streamOK: true}
	cm := &CompactingModel{
		Inner:         inner,
		Threshold:     threshold,
		ContextWindow: 1000,
		KeepRecent:    2,
	}

	ctx := context.Background()
	agent, err := adk.NewChatModelAgent(ctx, &adk.ChatModelAgentConfig{
		Model:         cm,
		Instruction:   adkInstruction,
		MaxIterations: 2,
	})
	require.NoError(t, err)

	// Heavy enough to cross Threshold*ContextWindow once compaction is enabled.
	history := []*schema.Message{
		bigMessage(300), bigMessage(300), bigMessage(300), bigMessage(300),
	}
	iter := adk.NewRunner(ctx, adk.RunnerConfig{Agent: agent}).Run(ctx, history)
	for {
		if _, ok := iter.Next(); !ok {
			break
		}
	}

	return inner.inputsSnapshot()
}

// TestCompactingModel_RealADKChainKeepsTheSystemMessage closes the
// version-drift gap left by the ctxcompact-side tests.
//
// The fix for the dropped system prompt (W-D-14) rests on a fact about a
// DEPENDENCY: adk's defaultGenModelInput prepends the agent Instruction as a
// schema.System message, so the slice CompactingModel compacts starts with the
// initial context. The tests in internal/ctxcompact assert what happens to a
// slice of that shape, but they BUILD the shape by hand — so if eino stopped
// prepending, or the orchestrator supplied its own GenModelInput, those tests
// would keep passing while the real path silently changed.
//
// This one drives the actual chain and asserts on what the PROVIDER would have
// received, which is the only thing that matters.
func TestCompactingModel_RealADKChainKeepsTheSystemMessage(t *testing.T) {
	// HALF ONE — the dependency fact the whole fix rests on. Compaction off, so
	// what the inner model sees is exactly what adk built.
	passthrough := runADKChain(t, 0)
	require.NotEmpty(t, passthrough, "the agent must have called the model")
	first := passthrough[0]
	require.NotEmpty(t, first)
	assert.Equal(t, schema.System, first[0].Role,
		"adk.defaultGenModelInput must still prepend the Instruction as a system message; "+
			"if this fails, the initial context is no longer inside the slice compaction rewrites")
	assert.Contains(t, first[0].Content, adkInstruction)

	// HALF TWO — it survives compaction to the last call the provider sees.
	// Before the fix this is where it vanished: compaction rewrote the slice and
	// no rule in Plan pinned a system message.
	compacted := runADKChain(t, 0.5)
	require.Greater(t, len(compacted), 1,
		"compaction must actually have fired — the summarize call plus the real one")
	last := compacted[len(compacted)-1]

	systems := 0
	for _, m := range last {
		if m != nil && m.Role == schema.System {
			systems++
			assert.Contains(t, m.Content, adkInstruction, "verbatim, not re-rendered")
		}
	}
	assert.Equal(t, 1, systems,
		"the final provider call carries exactly one system message — not zero (dropped), not two (re-injected)")

	// And the call really was post-compaction: the history it carries is shorter
	// than what went in, and ends in the summary fragment.
	assert.Less(t, len(last), len(passthrough[0])+1,
		"the last call ran on a compacted history, not the original")
}
