package ctxcompact

import "github.com/cloudwego/eino/schema"

// EstimateTokens sums a per-message token estimate across msgs.
//
// The per-character work is done by estimateTextTokens, a STRUCTURAL estimator
// (word / opaque / punctuation runs and CJK runes, each at its own measured
// density) rather than the chars/4 rule this used to apply. chars/4 undercounts
// Chinese by up to 4x and JSON tool arguments by ~25%, so the compaction gate
// opened long after the window was actually full — see estimateTextTokens for
// the measured error band of both forms and for why the replacement is biased
// to overcount.
//
// This number gates WHEN to compact and where to cut chunks; it never feeds
// billing. TokenCountingMode reports how it was produced.
func EstimateTokens(msgs []*schema.Message) int {
	n := 0
	for _, m := range msgs {
		n += estimateMessageTokens(m)
	}
	return n
}

// estimateMessageTokens returns the per-message token estimate, accounting for
// Content, ReasoningContent, and all ToolCalls (name + arguments + id), plus
// fixed structural overheads for the message envelope and each tool call.
//
// Tool-call ARGUMENTS are estimated as their own run rather than concatenated
// with the name and id: the JSON blob is punctuation-dense and the identifiers
// are opaque, and estimateTextTokens charges those at different rates. Summing
// the three lengths first and dividing once (what the old form did) applies one
// blended rate to all three and loses exactly that distinction.
func estimateMessageTokens(m *schema.Message) int {
	if m == nil {
		return 0
	}
	n := estimateTextTokens(m.Content) + perMessageOverhead
	n += estimateTextTokens(m.ReasoningContent)
	for _, tc := range m.ToolCalls {
		n += estimateTextTokens(tc.Function.Name)
		n += estimateTextTokens(tc.Function.Arguments)
		n += estimateTextTokens(tc.ID)
		n += perToolCallOverhead
	}
	return n
}
