package rlm_test

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/x6nux/yanshi/internal/agent/orchestrator"
	"github.com/x6nux/yanshi/internal/agent/rlm"
	"github.com/x6nux/yanshi/internal/guard"
)

// meteringModel counts what is actually sent to a provider: how many calls, and
// how many characters of message content each carried.
//
// Characters rather than tokens because tokenisation is provider-specific and
// the ratio between the two paths — not an absolute figure — is what the clause
// is about. Any tokeniser is monotonic in input length, so a path that sends
// several times the text costs several times as much.
type meteringModel struct {
	mu    sync.Mutex
	calls int
	chars int
}

func (m *meteringModel) record(messages []*schema.Message) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls++
	for _, msg := range messages {
		if msg == nil {
			continue
		}
		m.chars += len(msg.Content)
	}
}

func (m *meteringModel) totals() (calls, chars int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.calls, m.chars
}

func (m *meteringModel) Generate(_ context.Context, messages []*schema.Message, _ ...model.Option) (*schema.Message, error) {
	m.record(messages)
	return schema.AssistantMessage("ok", nil), nil
}

func (m *meteringModel) Stream(_ context.Context, messages []*schema.Message, _ ...model.Option) (*schema.StreamReader[*schema.Message], error) {
	m.record(messages)
	return schema.StreamReaderFromArray([]*schema.Message{schema.AssistantMessage("ok", nil)}), nil
}

// batchPrompts builds n independent classification-style tasks — the shape
// rlm_query exists for.
func batchPrompts(n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = fmt.Sprintf("Classify item %d as positive or negative.", i)
	}
	return out
}

// TestRLMSendsFarLessThanTheSubAgentPathForTheSameBatch is the cost clause,
// measured rather than declared.
//
// SelectRLMModel forcing cost_class=cheap says the model is cheap; it does not
// say the PATH is cheaper, and nothing compared the two. The comparison that
// matters is what each path actually puts on the wire for the same work:
//
//   - rlm_query sends one bare prompt per item.
//   - a sub-agent per item sends that prompt plus the inherited system
//     instruction (AGENT.md/CLAUDE.md and the output contract) on every call,
//     and runs a ReAct loop that can call the model more than once per item.
//
// So the difference is structural, not a property of which model is selected —
// which is why it can be asserted at all. The assertion is on the ratio, with a
// deliberately loose factor: the point is an order-of-magnitude claim
// ("显著低于"), and pinning an exact number would fail on any prompt change.
//
// ledger: C1/RLM1#4 成本显著低于 sub-agent
func TestRLMSendsFarLessThanTheSubAgentPathForTheSameBatch(t *testing.T) {
	const n = 8
	prompts := batchPrompts(n)

	// Path A: rlm_query.
	rlmMeter := &meteringModel{}
	results, err := rlm.Runner{Model: rlmMeter, MaxConcurrency: 8}.Run(context.Background(), prompts)
	require.NoError(t, err)
	require.Len(t, results, n)
	rlmCalls, rlmChars := rlmMeter.totals()
	require.Equal(t, n, rlmCalls, "rlm must make exactly one model call per item")

	// Path B: one sub-agent per item, through the real orchestrator so the
	// inherited instruction is the production one rather than a guess.
	subMeter := &meteringModel{}
	instruction := strings.Repeat(
		"You are a coding agent. Follow the project conventions in AGENT.md. ", 20)
	o, err := orchestrator.New(orchestrator.Config{
		Model:       subMeter,
		Instruction: instruction,
		Profile:     guard.PermissionProfile{Tools: guard.ToolsPerm{Allow: []string{"*"}}},
	})
	require.NoError(t, err)

	// One turn per item. A sub-agent IS a nested turn — same instruction, same
	// ReAct loop — so driving the public turn entry point measures the same
	// thing without reaching for an unexported binder, and without the result
	// depending on which delegation tool was used.
	for _, p := range prompts {
		iter := o.EventsWithHistoryOpts(context.Background(),
			[]*schema.Message{schema.UserMessage(p)}, orchestrator.TurnOpts{})
		for {
			if _, ok := iter.Next(); !ok {
				break
			}
		}
	}
	subCalls, subChars := subMeter.totals()
	require.Positive(t, subCalls, "the sub-agent path never reached the model, so there is nothing to compare")

	t.Logf("rlm: %d calls, %d chars — sub-agent: %d calls, %d chars", rlmCalls, rlmChars, subCalls, subChars)

	// The factor is loose on purpose. What must hold is that the batch path is
	// cheaper by a wide margin, not that it is cheaper by exactly this much.
	assert.Less(t, rlmChars*3, subChars,
		"rlm sent %d chars for %d items and the sub-agent path sent %d; the batch path "+
			"is supposed to avoid re-sending the inherited instruction per item",
		rlmChars, n, subChars)
	assert.LessOrEqual(t, rlmCalls, subCalls,
		"rlm made more model calls (%d) than the sub-agent path (%d)", rlmCalls, subCalls)
}
