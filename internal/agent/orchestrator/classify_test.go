package orchestrator

import (
	"testing"

	"github.com/cloudwego/eino/schema"

	"github.com/x6nux/yanshi/internal/proto"
)

// TestUsageExtractsCacheAndReasoning proves ClassifyEventsWithUsage pulls the
// new CachedTokens / ReasoningTokens / TotalTokens fields out of a model
// response's ResponseMeta.Usage (where the OpenAI acl wrapper records them) and
// into TurnUsage, alongside the existing PromptTokens / CompletionTokens.
//
// The OpenAI acl wrapper (eino-ext/libs/acl/openai chat_model.go:771,1215,1238)
// populates schema.TokenUsage on each assistant message —
//   - PromptTokenDetails.CachedTokens (prompt cache hits)
//   - CompletionTokensDetails.ReasoningTokens (reasoning model spend)
//   - TotalTokens
//
// so ClassifyEventsWithUsage must surface them. This test feeds a fake iterator
// one assistant message carrying all of the above and asserts each lands on the
// accumulator. Failure here means the extraction in TurnUsage.set is missing a
// field that /cost (Spec B8) needs.
func TestUsageExtractsCacheAndReasoning(t *testing.T) {
	msg := &schema.Message{
		Role:    schema.Assistant,
		Content: "hi",
		ResponseMeta: &schema.ResponseMeta{
			FinishReason: "stop",
			Usage: &schema.TokenUsage{
				PromptTokens:     100,
				PromptTokenDetails: schema.PromptTokenDetails{CachedTokens: 40},
				CompletionTokens: 20,
				CompletionTokensDetails: schema.CompletionTokensDetails{ReasoningTokens: 8},
				TotalTokens:     120,
			},
		},
	}
	iter := newFakeEventIter(t, []*schema.Message{msg})

	var u TurnUsage
	ClassifyEventsWithUsage(iter, &u, func(proto.ServerFrame) {})

	if u.PromptTokens != 100 || u.CompletionTokens != 20 ||
		u.CachedTokens != 40 || u.ReasoningTokens != 8 || u.TotalTokens != 120 {
		t.Fatalf("usage 提取错: %+v", u)
	}
}
