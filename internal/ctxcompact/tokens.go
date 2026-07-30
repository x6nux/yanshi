package ctxcompact

import "github.com/cloudwego/eino/schema"

// EstimateTokens sums a chars/4 + per-field-overhead heuristic across msgs.
// It counts Content, ReasoningContent, and every ToolCall's name/arguments/id
// — the prior len(Content)/4+8 estimate badly undercounted tool-heavy ReAct
// loops, so the threshold gate never fired until too late (bug⑤). This is only
// used to gate WHEN to compact and to pick chunk boundaries; it never feeds
// billing.
func EstimateTokens(msgs []*schema.Message) int {
	n := 0
	for _, m := range msgs {
		n += estimateMessageTokens(m)
	}
	return n
}

// estimateMessageTokens returns the per-message token estimate, accounting for
// Content, ReasoningContent, and all ToolCalls (name + arguments + id). The
// per-message overhead of 8 tokens approximates role/structural framing.
func estimateMessageTokens(m *schema.Message) int {
	if m == nil {
		return 0
	}
	n := len(m.Content)/4 + 8 // base content + per-message overhead
	if m.ReasoningContent != "" {
		n += len(m.ReasoningContent) / 4
	}
	for _, tc := range m.ToolCalls {
		// name + arguments(JSON string) + id, each ~chars/4, plus structural overhead
		n += (len(tc.Function.Name)+len(tc.Function.Arguments)+len(tc.ID))/4 + 16
	}
	return n
}
