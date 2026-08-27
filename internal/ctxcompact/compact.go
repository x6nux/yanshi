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

// Options carries the compaction settings the transport-facing entry points
// (MaybeCompact / ForceCompact) cannot express in their positional parameters.
//
// It exists because those two signatures hard-code `RunOpts{ModelWindow: …,
// ChunkThreshold: 0.9}` and have no seam for anything else. That is not a
// cosmetic limitation: a Redactor that cannot be passed in is a redactor that
// never runs, so C11 would have shipped as unreachable code with a full test
// suite proving it works when called — which nothing in production does. The
// zero value reproduces the historical behaviour exactly, so adopting this is
// per-caller and never a silent change.
type Options struct {
	// Redactor strips registered secrets from the copy of the history sent to
	// the summary model, and from the summary that comes back. nil disables
	// redaction. Pass the process-wide secrets.Redactor here.
	Redactor Redactor

	// OutputReserve is the tokens held back for the reply when deciding whether
	// the compacted history still overflows. 0 selects DefaultOutputReserve.
	OutputReserve int

	// Quality overrides the summary quality policy. Zero means
	// DefaultQualityPolicy unless DisableQualityGate is set.
	Quality QualityPolicy

	// DisableQualityGate turns the quality gate off entirely.
	DisableQualityGate bool

	// CoveredSeq is the persisted sequence range of the history being
	// compacted. It makes the summary's [seq:lo-hi] pointers RESOLVABLE:
	// history_read(from_seq, to_seq) returns those exact messages, so a claim
	// in the summary can be traced back to the exchange it came from.
	//
	// The zero value means "these messages have no persisted seq" and is the
	// correct value for the mid-turn path, whose messages are still in flight.
	// A pre-turn caller reading a stored session should fill it in; leaving it
	// zero costs the fallback attribution and nothing else, so an unwired
	// caller degrades to the pre-C4 behaviour rather than breaking.
	CoveredSeq SeqRef

	// EvictionMap is the session's C3 map of previously evicted spans, MUTATED
	// by a successful compaction and rendered into the assembled history. nil
	// disables it. See RunOpts.EvictionMap for why it is a pointer and why a
	// caller without persisted seq numbers should leave it nil.
	EvictionMap *EvictionMap

	// EvictionMapBudget bounds the rendered map in characters; 0 selects
	// DefaultEvictionMapBudget.
	EvictionMapBudget int
}

// runOpts builds the RunOpts for a compaction against contextWindow.
// ChunkThreshold stays pinned at 0.9 here, matching what both entry points have
// always passed; docs/compaction.md records that the configured
// compaction.chunk_threshold is not yet wired through, and this function is
// where that wiring will land rather than a second literal elsewhere.
func (o Options) runOpts(contextWindow int) RunOpts {
	return RunOpts{
		ModelWindow:        contextWindow,
		ChunkThreshold:     0.9,
		OutputReserve:      o.OutputReserve,
		Redactor:           o.Redactor,
		Quality:            o.Quality,
		DisableQualityGate: o.DisableQualityGate,
		CoveredSeq:         o.CoveredSeq,
		EvictionMap:        o.EvictionMap,
		EvictionMapBudget:  o.EvictionMapBudget,
	}
}

// MaybeCompact is the PRE-TURN compaction entry (the WS handler calls it before
// a user_message turn). It delegates to Run with a ModelSummarizer wrapping the
// caller-supplied model.BaseChatModel. When compaction fires it returns the new
// history and did=true; on summary failure it returns the original history with
// did=false so the caller keeps it intact (bug⑥ — never an empty summary).
// threshold <= 0, contextWindow <= 0, under-budget, or too-few-messages is a
// no-op returning the original slice with did=false.
//
// It is MaybeCompactWithOptions with zero Options; callers that need redaction
// or a custom quality policy call that directly.
func MaybeCompact(ctx context.Context, msgs []*schema.Message,
	threshold float64, contextWindow, keepRecent int,
	m model.BaseChatModel, onChunk func(string)) ([]*schema.Message, int, int, bool) {

	return MaybeCompactWithOptions(ctx, msgs, threshold, contextWindow, keepRecent, m, onChunk, Options{})
}

// MaybeCompactWithOptions is MaybeCompact with the settings that do not fit its
// positional signature — most importantly the Redactor, without which no secret
// is ever stripped from the summary input.
//
// It returns did=false on a quality rejection exactly as it does on a model
// error: both mean "the history was not replaced", which is the one thing the
// caller must not get wrong. The REASON is dropped at this boundary because the
// four-value return has nowhere to put it; a caller that needs to log which
// rule fired should call Run directly, where the error carries the issues.
func MaybeCompactWithOptions(ctx context.Context, msgs []*schema.Message,
	threshold float64, contextWindow, keepRecent int,
	m model.BaseChatModel, onChunk func(string), opts Options) ([]*schema.Message, int, int, bool) {

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
		opts.runOpts(contextWindow), summarizer{m}, onChunk)
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

	return ForceCompactWithOptions(ctx, msgs, contextWindow, keepRecent, m, onChunk, Options{})
}

// ForceCompactWithOptions is ForceCompact with the settings that do not fit its
// positional signature. See MaybeCompactWithOptions for why the variant exists
// and what happens to a quality rejection here.
func ForceCompactWithOptions(ctx context.Context, msgs []*schema.Message, contextWindow, keepRecent int,
	m model.BaseChatModel, onChunk func(string), opts Options) ([]*schema.Message, int, int, bool) {

	before := EstimateTokens(msgs)
	noop := func() ([]*schema.Message, int, int, bool) { return msgs, before, before, false }
	if contextWindow <= 0 || len(msgs) <= keepRecent*2+1 {
		return noop()
	}
	res, err := Run(ctx, msgs, PlanOpts{KeepRecent: keepRecent},
		opts.runOpts(contextWindow), summarizer{m}, onChunk)
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
