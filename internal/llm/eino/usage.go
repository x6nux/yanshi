// internal/llm/eino/usage.go
//
// M10: durable token-usage accounting.
//
// Usage has always been counted per SESSION and only per session: the /cost
// line and the persisted session row both accumulate into a running total that
// is overwritten as the session goes. That answers "what has this conversation
// cost" and nothing else. After a goal loop runs overnight across three models,
// "which model burned what, and when" has no answer at all — not because the
// numbers were never seen, but because each one was folded into a sum the
// moment it arrived.
//
// This file is the record-per-call half. The sink is an interface rather than
// the store because internal/llm is a service-layer package and the persistence
// choice belongs to the composition root: bootstrap passes an adapter over
// internal/store, tests pass an in-memory recorder, and a deployment that wants
// no usage history passes nil.
package eino

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/cloudwego/eino/schema"

	obslog "github.com/x6nux/yanshi/internal/observe/log"
)

// UsageRecord is one model call's token accounting.
//
// Cached is broken out from Prompt rather than subtracted from it because the
// two are billed at different rates and the provider reports them that way; a
// consumer that wants the billable prompt computes Prompt-Cached, and one that
// wants cache effectiveness needs both. Reasoning is likewise separate: it is
// billed as output but is not part of the visible completion, so folding it in
// would make "output tokens" disagree with the text the user received.
type UsageRecord struct {
	// TS is when the call completed. Zero means "now" and is filled in by the
	// recorder, so a caller cannot accidentally write rows with no time.
	TS time.Time
	// Provider is the adapter kind that served the call ("openai",
	// "anthropic", "openai-responses"), or "" when unknown.
	Provider string
	// Model is the registry key the turn ran on.
	Model string
	// SessionID attributes the call to a conversation when one is in scope.
	SessionID string
	// PromptTokens / CompletionTokens are the provider's own counts.
	PromptTokens     int
	CompletionTokens int
	// CachedTokens is the prompt-cache hit portion of PromptTokens.
	CachedTokens int
	// ReasoningTokens is the thinking portion of the completion.
	ReasoningTokens int
	// CacheHit is true when the provider reported ANY cached prompt tokens.
	// Stored as its own column so the "how often did caching work" question is
	// answerable without re-deriving it from a token count on every row.
	CacheHit bool
}

// UsageSink persists usage records. Implementations must be safe for
// concurrent use and must not block the caller for long: a call is recorded on
// the model's hot path, and accounting is never allowed to slow a turn down.
type UsageSink interface {
	// RecordUsage persists one record. Returning an error is fine — callers
	// log it and continue, because losing an accounting row must never fail a
	// turn that already succeeded.
	RecordUsage(ctx context.Context, rec UsageRecord) error
}

// usageSessionFrom returns the session id to attribute a usage row to.
//
// It reads the CORRELATION id the transports already bind (obslog.WithIDs, set
// per request in chat.go and per turn in ws.go, and back-filled for every other
// entry point by the orchestrator's ensureTurnIDs) rather than a session key of
// its own.
//
// A dedicated injector was written first and deleted: nothing could have called
// it. The model wrapper is shared across every session in the process (the
// orchestrator caches runners by model pointer), so the id cannot live on the
// wrapper; it has to arrive on the context — and the context already carries
// exactly this value, bound by the same code that would have had to bind a
// second one. GOV6 named the injector as having no call site, which was the
// correct verdict: a second key would have been a field only the tests set.
//
// CONSEQUENCE, and it is real: obslog.WithoutIDs suppresses correlation ids,
// and usage rows written under such a context therefore carry no session. That
// is the honest reading — the operator asked for uncorrelated telemetry — and
// the row itself is still written, so per-model and per-day totals are
// unaffected. Only the per-session join is lost.
func usageSessionFrom(ctx context.Context) string {
	return obslog.IDsFromContext(ctx).SessionID
}

// usageFromMessage projects a response message's provider-reported usage onto
// an UsageRecord, reporting false when the message carries none.
//
// A message with no usage is the normal case for a streaming DELTA, so the
// bool is not an error path: the stream recorder simply keeps the last delta
// that had one.
func usageFromMessage(msg *schema.Message) (UsageRecord, bool) {
	if msg == nil || msg.ResponseMeta == nil || msg.ResponseMeta.Usage == nil {
		return UsageRecord{}, false
	}
	u := msg.ResponseMeta.Usage
	rec := UsageRecord{
		PromptTokens:     u.PromptTokens,
		CompletionTokens: u.CompletionTokens,
	}
	rec.CachedTokens = u.PromptTokenDetails.CachedTokens
	rec.ReasoningTokens = u.CompletionTokensDetails.ReasoningTokens
	rec.CacheHit = rec.CachedTokens > 0
	return rec, true
}

// recordUsage writes rec to sink, filling in the timestamp and the ctx-bound
// session. A nil sink is a no-op so every call site can stay unconditional.
//
// Errors are logged at DEBUG, not returned: this runs after a model call has
// already succeeded, and there is no action the caller could take that would be
// better than keeping the answer it just produced.
func recordUsage(ctx context.Context, sink UsageSink, rec UsageRecord) {
	if sink == nil {
		return
	}
	if rec.TS.IsZero() {
		rec.TS = time.Now()
	}
	if rec.SessionID == "" {
		rec.SessionID = usageSessionFrom(ctx)
	}
	if err := sink.RecordUsage(ctx, rec); err != nil {
		slog.Debug("usage record dropped", "model", rec.Model, "err", err)
	}
}

// MemoryUsageSink is an in-process UsageSink that keeps every record.
//
// It is production code, not a test helper: `--fake-model` and any deployment
// without a store still produce usage, and dropping it on the floor would make
// the accounting path untestable end-to-end in exactly the mode most tests run
// in. Unbounded growth is acceptable for the same reason it is acceptable in a
// fake model — the process it lives in is short-lived — and Records returns a
// copy so a reader cannot race the writer.
type MemoryUsageSink struct {
	mu      sync.Mutex
	records []UsageRecord
}

// RecordUsage appends rec. Never returns an error.
func (m *MemoryUsageSink) RecordUsage(_ context.Context, rec UsageRecord) error {
	m.mu.Lock()
	m.records = append(m.records, rec)
	m.mu.Unlock()
	return nil
}

// Records returns a copy of everything recorded so far.
func (m *MemoryUsageSink) Records() []UsageRecord {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]UsageRecord, len(m.records))
	copy(out, m.records)
	return out
}

var _ UsageSink = (*MemoryUsageSink)(nil)
