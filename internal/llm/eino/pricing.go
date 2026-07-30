package eino

import "fmt"

// ModelPricing is the per-million-token USD price for one model. CacheHitPerM
// is the discount for prompt-cache hits (Anthropic cache_read ~0.1x input).
type ModelPricing struct {
	InputPerM    float64
	CacheHitPerM float64
	OutputPerM   float64
}

// Usage is the minimum projection of orchestrator.TurnUsage that pricing
// needs. eino is a leaf package and must not import orchestrator; bootstrap
// and the WS handler perform the field mapping.
type Usage struct {
	Prompt     int
	Cached     int
	Completion int
	Reasoning  int
}

// DefaultPricing returns the fallback table. Prices reflect Anthropic's
// publicly published pricing (claude-api skill, cached 2026-07-21). Operators
// override with config Pricing.Overrides; MergePricing layers overrides on
// top of this table. The exact model IDs are authoritative; do not append
// date suffixes or fabricate legacy IDs.
func DefaultPricing() map[string]ModelPricing {
	return map[string]ModelPricing{
		"claude-fable-5":    {InputPerM: 10, CacheHitPerM: 1, OutputPerM: 50},
		"claude-mythos-5":   {InputPerM: 10, CacheHitPerM: 1, OutputPerM: 50},
		"claude-opus-4-8":   {InputPerM: 5, CacheHitPerM: 0.5, OutputPerM: 25},
		"claude-opus-4-7":   {InputPerM: 5, CacheHitPerM: 0.5, OutputPerM: 25},
		"claude-opus-4-6":   {InputPerM: 5, CacheHitPerM: 0.5, OutputPerM: 25},
		"claude-sonnet-5":   {InputPerM: 3, CacheHitPerM: 0.3, OutputPerM: 15},
		"claude-sonnet-4-6": {InputPerM: 3, CacheHitPerM: 0.3, OutputPerM: 15},
		"claude-haiku-4-5":  {InputPerM: 1, CacheHitPerM: 0.1, OutputPerM: 5},
	}
}

// MergePricing returns a new table that combines base and overlay. Overlay
// entries win on key collision. The base map is never mutated so callers can
// pass DefaultPricing() and still see the same values on the next call.
func MergePricing(base, overlay map[string]ModelPricing) map[string]ModelPricing {
	merged := make(map[string]ModelPricing, len(base)+len(overlay))
	for key, price := range base {
		merged[key] = price
	}
	for key, price := range overlay {
		merged[key] = price
	}
	return merged
}

// CostOK returns the dollar estimate for one usage and whether the model is
// known. Unknown models always return (0, false) so callers render N/A,
// never $0.00 for something they could not price.
func CostOK(tab map[string]ModelPricing, model string, u Usage) (float64, bool) {
	price, ok := tab[model]
	if !ok {
		return 0, false
	}
	return computeCost(price, u), true
}

// computeCost splits the prompt into cache hits and misses so cache_read
// tokens are billed at the discounted rate. Cached tokens are clamped to the
// prompt size as defense-in-depth against providers reporting inconsistent
// totals. Reasoning tokens are intentionally NOT billed separately because
// Anthropic prices them as output today; if that changes, add a
// ReasoningPerM field to ModelPricing instead of branching here.
func computeCost(price ModelPricing, u Usage) float64 {
	cached := clampNonNeg(u.Cached)
	if cached > u.Prompt {
		cached = u.Prompt
	}
	miss := u.Prompt - cached
	return float64(cached)/1_000_000*price.CacheHitPerM +
		float64(miss)/1_000_000*price.InputPerM +
		float64(clampNonNeg(u.Completion))/1_000_000*price.OutputPerM
}

// clampNonNeg guards against a provider returning negative usage counts
// (seen in some streaming edge cases) — they would otherwise reduce the bill.
func clampNonNeg(n int) int {
	if n < 0 {
		return 0
	}
	return n
}

// BilledUsage holds the per-session cumulative billable tokens. Each provider
// usage contributes its own (Prompt-Cached, Cached, Completion); we never
// reuse the WS handler's overwrite-semantics tokensIn for accounting (that
// field reports current-context-size, not running spend).
type BilledUsage struct {
	InputTokens  int
	CachedTokens int
	OutputTokens int
}

// Ledger accumulates billable tokens across a session. Each Add sees one
// provider usage event (including the judge call) and adds its non-cached
// input, cache hit, and output contributions. Same-session model switches
// are handled by the caller: the caller picks the right pricing table per
// Add, or treats the Ledger as cross-model aggregated spend.
type Ledger struct {
	Billed BilledUsage
}

// Add folds one provider usage into the ledger. Cached tokens are clamped to
// Prompt as defense-in-depth (some providers report more cache_read than
// prompt tokens in edge cases).
func (l *Ledger) Add(u Usage) {
	cached := clampNonNeg(u.Cached)
	if cached > u.Prompt {
		cached = u.Prompt
	}
	miss := u.Prompt - cached
	l.Billed.InputTokens += miss
	l.Billed.CachedTokens += cached
	l.Billed.OutputTokens += clampNonNeg(u.Completion)
}

// Cost returns the accumulated dollar cost and known flag for the session.
// When the model is unknown to the table, returns (0, false) so the caller
// renders "N/A" instead of "$0.00".
func (l *Ledger) Cost(tab map[string]ModelPricing, model string) (float64, bool) {
	price, ok := tab[model]
	if !ok {
		return 0, false
	}
	return float64(l.Billed.InputTokens)/1_000_000*price.InputPerM +
		float64(l.Billed.CachedTokens)/1_000_000*price.CacheHitPerM +
		float64(l.Billed.OutputTokens)/1_000_000*price.OutputPerM, true
}

// FormatCost renders a dollar estimate. When known is false the renderer
// always returns "N/A" so users can distinguish unknown pricing from a
// $0.00 session. Known-and-zero renders as "$0.0000" (constraint 11).
func FormatCost(cost float64, known bool) string {
	if !known || cost < 0 {
		return "N/A"
	}
	switch {
	case cost == 0:
		return "$0.0000"
	case cost < 0.0001:
		return "<$0.0001"
	case cost < 0.01:
		return fmt.Sprintf("$%.4f", cost)
	case cost < 1:
		return fmt.Sprintf("$%.3f", cost)
	default:
		return fmt.Sprintf("$%.2f", cost)
	}
}
