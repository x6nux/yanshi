// internal/ctxcompact/options.go
package ctxcompact

import (
	"context"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
)

// ModelSummarizer is the narrow contract RunSummary needs from a chat model:
// just Generate + Stream. It mirrors model.BaseChatModel's two methods exactly
// (the compile-time assertion in options_test.go proves any BaseChatModel
// satisfies it — real remote model, einollm.FakeModel, or the CompactingModel's
// inner). Defining it at the consumer (this package) follows Go's "accept
// interfaces" idiom and pins the minimal surface the core depends on, rather
// than naming the full BaseChatModel type throughout.
type ModelSummarizer interface {
	Generate(ctx context.Context, msgs []*schema.Message, opts ...model.Option) (*schema.Message, error)
	Stream(ctx context.Context, msgs []*schema.Message, opts ...model.Option) (*schema.StreamReader[*schema.Message], error)
}

// PlanOpts configures the pure Plan function (no IO).
type PlanOpts struct {
	// KeepRecent is the number of trailing messages kept verbatim — Plan pins
	// the last 2*KeepRecent messages regardless of role (the tail may include
	// tool messages, not strictly user/assistant pairs). EnforceToolCallPairs
	// then repairs any pair severed by the tail cut.
	KeepRecent int
}

// PlanResult is the pinned/summarize split Plan produces.
type PlanResult struct {
	// PinnedIndices are ASCENDING message indices kept verbatim. Assemble
	// iterates them in order to preserve original history order, so producers
	// must keep them sorted ascending.
	PinnedIndices []int
	// SummarizeIndices are the remaining indices (also ascending) whose content
	// is folded into the summary.
	SummarizeIndices []int
	WorkingSetPaths  []string
}

// RunOpts configures Run / RunSummary (carries the summary-model window).
type RunOpts struct {
	// ModelWindow is the summary model's context window (tokens), used to pick
	// single-vs-chunked and chunk boundaries. From provider.context_window.
	ModelWindow int
	// ChunkThreshold is the fraction of ModelWindow at which RunSummary switches
	// from single cache-aligned call to carry-style chunking. The config layer
	// (Task 13) defaults a zero value to 0.9; HERE, a zero value disables the
	// chunking guard — RunSummary falls back to the full ModelWindow as the
	// chunk budget.
	ChunkThreshold float64
	// SummaryWordLimit caps the summary length the model is asked for.
	SummaryWordLimit int

	// CoveredSeq is the persisted-message sequence range the summarized
	// messages came from. It is quoted to the model so it can cite pointers
	// inside it, and it is the fallback attribution for a summary item whose
	// own [seq:…] pointer the model omitted.
	//
	// THE ZERO VALUE IS "UNKNOWN", NOT "STARTS AT ZERO", and the instruction
	// omits the range clause entirely for it. That matters because the seq
	// numbers only exist once the history has been persisted: the mid-turn
	// path summarizes messages that are still in flight and has no seqs to
	// offer, while the pre-turn path is reading a stored session and does.
	// Quoting 0-0 to the first would invite the model to cite a range that
	// resolves to nothing, which is worse than citing nothing at all — a
	// pointer that looks recoverable and is not costs a wasted history_read
	// and teaches the model the pointers are noise.
	CoveredSeq SeqRef

	// OutputReserve is the number of tokens held back from ModelWindow for the
	// model's REPLY. Every budget in this package is computed against
	// ModelWindow minus this value; see DefaultOutputReserve for why a window
	// spent entirely on input is a request that cannot be answered. 0 selects
	// DefaultOutputReserve; the applied value is always clamped to at most half
	// the window (effectiveReserve).
	//
	// Set it from the provider's configured max_tokens when known.
	OutputReserve int

	// Redactor, when non-nil, strips registered secrets from the copy of the
	// history handed to the summary model, and from the summary that comes
	// back. The PINNED messages are never touched — see redactForSummary for
	// the boundary and why it is drawn there. nil disables redaction, which is
	// the pre-C11 behaviour.
	Redactor Redactor

	// Quality is the gate a candidate summary must clear before Run is willing
	// to replace history with it. The ZERO VALUE DISABLES every check; Run
	// substitutes DefaultQualityPolicy unless DisableQualityGate is set, so a
	// caller that passes no policy still gets the floors.
	Quality QualityPolicy

	// DisableQualityGate turns the summary quality gate off entirely, including
	// the default policy Run would otherwise substitute.
	//
	// It is a separate bool rather than a sentinel value of Quality because
	// "the caller did not configure a policy" and "the caller wants no policy"
	// are different intents that a zero struct cannot distinguish — and the
	// safe reading of the first is to apply the defaults. Making the unsafe
	// choice cost an explicit field keeps it out of reach of a caller who
	// simply forgot to fill Quality in.
	DisableQualityGate bool

	// EvictionMap is C3's in-context directory of previously evicted spans.
	//
	// Run MUTATES it: a successful compaction appends the span it just evicted
	// along with the milestones harvested from the summary, then renders the
	// updated map into the assembled history. It is a POINTER for that reason
	// — the map is session state that must survive across compactions, and a
	// copy would make every compaction the first one, producing a map that
	// only ever describes the most recent eviction.
	//
	// nil disables the map, and that is the correct value for a caller with no
	// persisted sequence numbers (the mid-turn path), because a directory of
	// spans history_read cannot resolve is a directory of dead addresses. Run
	// enforces the same thing independently: without a citable CoveredSeq
	// nothing is recorded even when a map is supplied.
	EvictionMap *EvictionMap

	// EvictionMapBudget bounds the rendered map in characters. 0 selects
	// DefaultEvictionMapBudget.
	EvictionMapBudget int

	// Trigger names the compression path that is running (one of the
	// hooks.go Trigger* constants). It rides the lifecycle events so a hook
	// can tell a pre-turn auto-compact from a manual /compact. The zero
	// value reaches hooks as an empty string — acceptable for a caller that
	// predates the lifecycle bus, but a NEW path should register itself
	// there rather than ship an unnamed trigger.
	Trigger string
}

// qualityPolicy returns the policy Run applies: none when the gate is
// explicitly disabled, the caller's when non-zero, else DefaultQualityPolicy.
func (o RunOpts) qualityPolicy() QualityPolicy {
	if o.DisableQualityGate {
		return QualityPolicy{}
	}
	if !o.Quality.enabled() {
		return DefaultQualityPolicy
	}
	return o.Quality
}

// Result is what Run returns on success.
type Result struct {
	// Messages is the compacted history: pinned messages verbatim in original
	// order, followed by a sentinel-prefixed user-role summary at the tail.
	Messages     []*schema.Message
	TokensBefore int
	TokensAfter  int

	// Fold reports what the C5 tool-result fold pass did: how many results it
	// examined and how many it rewrote at each tier. Zero when folding did not
	// fire, which is the common case — see FoldToolResults.
	//
	// It is reported rather than merely applied because a compaction that
	// shrank the history by folding is a materially different event from one
	// that shrank it by summarizing, and an operator reading a log needs to
	// tell them apart: the first says the window is full of recoverable tool
	// output, the second says it is full of conversation.
	Fold FoldStats

	// Overflow is non-nil when Messages STILL exceeds the input budget
	// (ModelWindow less the output reserve) after compaction. It is a
	// *ContextOverflowError, so errors.Is(res.Overflow, ErrContextOverflow)
	// matches and the token numbers are available on the concrete type.
	//
	// It is a FIELD rather than a second error return because compaction
	// succeeded: Messages is a valid, strictly smaller history and is the best
	// thing the caller has. Overflow says "this still will not fit", which is
	// a fact about the send, and the send is not Run's decision. A caller that
	// forwards regardless is doing what the code did before C9; a caller that
	// refuses now has a typed reason to refuse with, instead of a provider 400.
	Overflow error

	// Fallback is true when Run could not obtain a summary (RunSummary
	// returned an error — model call exhausted, quota, overflow, whatever the
	// underlying provider chain gave up on) and instead discarded the
	// summarize-set history outright, keeping only the pinned messages
	// (W-C-04). This is a token-budget-style fallback, not an error: the
	// caller still gets a valid, strictly smaller history back, exactly like
	// the ordinary summarization path, so callers that only checked `err`
	// before W-C-04 keep working unchanged (see maybeCompact's err!=nil ||
	// TokensAfter>=TokensBefore check, which already treats this result as a
	// success because TokensAfter<TokensBefore holds structurally).
	//
	// It is NOT set by the len(plan.SummarizeIndices)==0 shortcut in Run — that
	// branch means "Plan found nothing worth summarizing", a normal outcome on
	// any short turn, not a model failure. Conflating the two would spam a
	// "fallback" notice on every ordinary compaction.
	Fallback bool
}
