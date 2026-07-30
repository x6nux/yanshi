package goalloop

import (
	"sync"

	"github.com/cloudwego/eino/schema"
)

// Usage is the token consumption of one or more model calls. It mirrors the
// subset of orchestrator.TurnUsage that the goal loop needs for budget
// accounting; defining it locally keeps goalloop from depending on the
// orchestrator package (dependency direction stays inward).
type Usage struct {
	PromptTokens     int
	CompletionTokens int
	TotalTokens      int
}

// Total returns the canonical token count for budget checks. Providers that
// report a non-zero TotalTokens use it directly; providers that omit it (some
// gateways leave it 0) fall back to prompt+completion so the budget still
// bites.
func (u Usage) Total() int {
	if u.TotalTokens > 0 {
		return u.TotalTokens
	}
	return u.PromptTokens + u.CompletionTokens
}

// UsageSink is the shared, concurrency-safe accumulator for goal-loop token
// accounting. One sink is wired into every LLM-calling component (planner,
// intent/quality evaluator, LLMTierer) and into the Loop, so every distinct
// model.Generate call is counted exactly once. Add SUMS per call (it does not
// overwrite): different components make independent Generate calls, each with
// its own usage, so summing across calls is correct — this is also what makes
// parent/child accounting non-duplicating (each call reports into the shared
// sink once, never twice).
type UsageSink struct {
	mu sync.Mutex
	u  Usage
}

// Add sums o into the accumulator. Safe for concurrent use.
func (s *UsageSink) Add(o Usage) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.u.PromptTokens += o.PromptTokens
	s.u.CompletionTokens += o.CompletionTokens
	s.u.TotalTokens += o.TotalTokens
}

// Snapshot returns a copy of the accumulated usage. Callers may mutate the
// returned value freely; it is detached from the sink.
func (s *UsageSink) Snapshot() Usage {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.u
}

// usageFromMeta maps a model response's ResponseMeta.Usage into a Usage. A nil
// meta (no usage reported — e.g. FakeModel, or a provider that omits it) yields
// the zero Usage, so adding it is a no-op.
func usageFromMeta(meta *schema.ResponseMeta) Usage {
	if meta == nil || meta.Usage == nil {
		return Usage{}
	}
	return Usage{
		PromptTokens:     meta.Usage.PromptTokens,
		CompletionTokens: meta.Usage.CompletionTokens,
		TotalTokens:      meta.Usage.TotalTokens,
	}
}

// addUsage records msg's model-call usage into sink. It is nil-safe on both
// arguments so that LLM-calling components can call it unconditionally right
// after model.Generate without sprinkling nil checks at every call site (DRY).
func addUsage(sink *UsageSink, msg *schema.Message) {
	if sink == nil || msg == nil {
		return
	}
	sink.Add(usageFromMeta(msg.ResponseMeta))
}
