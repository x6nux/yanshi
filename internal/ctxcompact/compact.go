// Package ctxcompact implements automatic context-compaction for the chat
// transports. When the estimated token count of the conversation history reaches
// a configurable fraction of the model's context window, the older turns are
// summarized into a single user+sentinel message while a configurable number of
// recent turns are kept verbatim. The summary is produced via a STREAMING model
// turn, and each assistant text delta is forwarded to an onChunk callback so the
// UI can render the summary being generated.
package ctxcompact

import (
	"context"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
)

// MaybeCompact is the PRE-TURN compaction entry (the WS handler calls it before
// a user_message turn). It delegates to Run with a ModelSummarizer wrapping the
// caller-supplied model.BaseChatModel. When compaction fires it returns the new
// history and did=true; on summary failure it returns the original history with
// did=false so the caller keeps it intact (bug⑥ — never an empty summary).
// threshold <= 0, contextWindow <= 0, under-budget, or too-few-messages is a
// no-op returning the original slice with did=false.
func MaybeCompact(ctx context.Context, msgs []*schema.Message,
	threshold float64, contextWindow, keepRecent int,
	m model.BaseChatModel, onChunk func(string)) ([]*schema.Message, int, int, bool) {

	before := EstimateTokens(msgs)
	noop := func() ([]*schema.Message, int, int, bool) {
		return msgs, before, before, false
	}
	if threshold <= 0 || contextWindow <= 0 {
		return noop()
	}
	if before < int(threshold*float64(contextWindow)) {
		return noop()
	}
	if len(msgs) <= keepRecent*2+1 {
		return noop()
	}

	res, err := Run(ctx, msgs, PlanOpts{KeepRecent: keepRecent},
		RunOpts{ModelWindow: contextWindow, ChunkThreshold: 0.9}, summarizer{m}, onChunk)
	if err != nil || res.TokensAfter >= res.TokensBefore {
		// best-effort: Run failed, OR Plan pinned everything so nothing was
		// actually summarized (TokensAfter unchanged). Either way keep the
		// original history and report did=false — a misleading "compacted"
		// status would fire compact_chunk streams / status frames for nothing.
		return noop()
	}
	return res.Messages, res.TokensBefore, res.TokensAfter, true
}

// ForceCompact is like MaybeCompact but skips the threshold gate — the caller
// already decided to compact (e.g. the manual /compact command). It KEEPS the
// too-few-messages guard and the did-means-actual-shrink check so a tiny or
// already-compacted history still no-ops. contextWindow here is the summary
// model's window (NOT a threshold budget), so it must be a sane window size;
// callers that don't know it should pass the model's real context window.
//
// DRY: the gate `err != nil || res.TokensAfter >= res.TokensBefore` and the
// too-few-messages guard previously appeared in three places (MaybeCompact,
// CompactingModel.maybeCompact, ws.go compactNow). Centralizing the force path
// here keeps the gate logic in one place — callers that need to bypass the
// THRESHOLD gate call this, callers that need the full gate call MaybeCompact.
func ForceCompact(ctx context.Context, msgs []*schema.Message, contextWindow, keepRecent int,
	m model.BaseChatModel, onChunk func(string)) ([]*schema.Message, int, int, bool) {

	before := EstimateTokens(msgs)
	noop := func() ([]*schema.Message, int, int, bool) { return msgs, before, before, false }
	if contextWindow <= 0 || len(msgs) <= keepRecent*2+1 {
		return noop()
	}
	res, err := Run(ctx, msgs, PlanOpts{KeepRecent: keepRecent},
		RunOpts{ModelWindow: contextWindow, ChunkThreshold: 0.9}, summarizer{m}, onChunk)
	if err != nil || res.TokensAfter >= res.TokensBefore {
		return noop()
	}
	return res.Messages, res.TokensBefore, res.TokensAfter, true
}

// summarizer adapts a model.BaseChatModel to ModelSummarizer. They are the same
// shape; the adapter exists so ctxcompact's core depends only on the narrow
// ModelSummarizer contract it defines, not on eino's model package throughout.
type summarizer struct{ m model.BaseChatModel }

func (s summarizer) Generate(ctx context.Context, msgs []*schema.Message, opts ...model.Option) (*schema.Message, error) {
	return s.m.Generate(ctx, msgs, opts...)
}
func (s summarizer) Stream(ctx context.Context, msgs []*schema.Message, opts ...model.Option) (*schema.StreamReader[*schema.Message], error) {
	return s.m.Stream(ctx, msgs, opts...)
}
