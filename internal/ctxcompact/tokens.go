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
// Content, ReasoningContent, all three multimodal part slices, and all
// ToolCalls (name + arguments + id), plus fixed structural overheads for the
// message envelope and each tool call.
//
// Tool-call ARGUMENTS are estimated as their own run rather than concatenated
// with the name and id: the JSON blob is punctuation-dense and the identifiers
// are opaque, and estimateTextTokens charges those at different rates. Summing
// the three lengths first and dividing once (what the old form did) applies one
// blended rate to all three and loses exactly that distinction.
//
// The multimodal slices are priced by part, never by payload length: see
// imageTokens. Counting them at zero — which this function did until W-A-01 —
// let a pasted screenshot weigh 8 tokens and kept the compaction gate shut
// until the provider answered 400.
func estimateMessageTokens(m *schema.Message) int {
	if m == nil {
		return 0
	}
	n := estimateTextTokens(m.Content) + perMessageOverhead
	n += estimateTextTokens(m.ReasoningContent)
	n += estimateChatParts(m.MultiContent)
	n += estimateInputParts(m.UserInputMultiContent)
	n += estimateOutputParts(m.AssistantGenMultiContent)
	for _, tc := range m.ToolCalls {
		n += estimateTextTokens(tc.Function.Name)
		n += estimateTextTokens(tc.Function.Arguments)
		n += estimateTextTokens(tc.ID)
		n += perToolCallOverhead
	}
	return n
}

// estimateInputParts prices schema.Message.UserInputMultiContent.
//
// Text parts go through the same structural estimator as Content; media parts
// get their fixed tier cost. A part carrying both (the schema permits it) is
// charged for both.
func estimateInputParts(parts []schema.MessageInputPart) int {
	n := 0
	for _, p := range parts {
		n += estimateTextTokens(p.Text)
		switch {
		case p.Image != nil:
			n += imageTokens(p.Image.Detail)
		case p.Audio != nil, p.Video != nil, p.File != nil:
			n += opaquePartTokens
		}
	}
	return n
}

// estimateChatParts prices the deprecated schema.Message.MultiContent.
//
// Deprecated upstream, but internal/ctxcompact/redact.go still walks it, so a
// message carrying it is still a message this package will hand to a provider.
// Pricing one field and not the other would leave exactly the hole this change
// closes.
func estimateChatParts(parts []schema.ChatMessagePart) int {
	n := 0
	for _, p := range parts {
		n += estimateTextTokens(p.Text)
		switch {
		case p.ImageURL != nil:
			n += imageTokens(p.ImageURL.Detail)
		case p.AudioURL != nil, p.VideoURL != nil, p.FileURL != nil:
			n += opaquePartTokens
		}
	}
	return n
}

// estimateOutputParts prices schema.Message.AssistantGenMultiContent.
//
// Model-generated media counts against the window exactly like user-supplied
// media: it is replayed in the next request's history.
//
// Unlike MessageInputPart, schema.MessageOutputPart has no File field (model
// output has no notion of a file attachment) and MessageOutputImage carries no
// Detail tier, so an output image is always priced at imageTokensHigh.
func estimateOutputParts(parts []schema.MessageOutputPart) int {
	n := 0
	for _, p := range parts {
		n += estimateTextTokens(p.Text)
		switch {
		case p.Image != nil:
			n += imageTokensHigh
		case p.Audio != nil, p.Video != nil:
			n += opaquePartTokens
		}
	}
	return n
}
