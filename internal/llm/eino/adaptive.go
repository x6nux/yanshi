// internal/llm/eino/adaptive.go
//
// AdaptiveModel is the decorator that turns the mechanisms in this package into
// behaviour. It sits between the orchestrator and a provider adapter and owns
// five concerns that all live at the same seam — the moment a request is about
// to leave, and the moment its failure comes back:
//
//	M7  rate limiting        — before the request, so quota is not spent
//	                           discovering the limit.
//	M6  schema sanitization  — on the request, so a gateway that cannot parse
//	                           $ref never sees one.
//	M5  quirk repair         — learned repairs applied up front, new ones
//	                           learned from the failure and applied once.
//	C6  overflow recovery    — a forced compaction and exactly one resend.
//	M10 usage accounting     — on the response, per call rather than per session.
//
// WHY ONE WRAPPER AND NOT FIVE. Each of these needs to both mutate the outgoing
// request and inspect the incoming error, and they interact: an overflow retry
// must re-apply the learned quirks to its shrunk history, a quirk retry must
// re-sanitize the tools, and both must pass the rate limiter again. Five
// independent decorators would either duplicate that ordering or get it subtly
// wrong depending on the order they were composed in — and the composition
// order would live in bootstrap, far from anything that explains it.
//
// THE RETRY BUDGET IS SHARED AND IS EXACTLY ONE. A single call makes at most
// two requests. Quirk repair and overflow recovery do not each get an attempt:
// if the first failure was an overflow and the retry fails on a quirk, that
// second error is returned. Two independent budgets multiply, and the whole
// point of both features is that a failing turn must not become a metered loop.
package eino

import (
	"context"
	"log/slog"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"

	"github.com/x6nux/yanshi/internal/ctxcompact"
)

// SanitizeMode selects when tool schemas are rewritten for portability.
type SanitizeMode string

// Tool-schema sanitization modes.
const (
	// SanitizeAuto sanitizes only for models that have DEMONSTRATED they need
	// it, by rejecting a request with a schema-shaped error (M5's
	// QuirkRejectsToolSchemaRefs). This is the default.
	SanitizeAuto SanitizeMode = "auto"
	// SanitizeAlways sanitizes every request. For a deployment that talks only
	// to a gateway known to be strict, this saves the one failed request per
	// process that auto pays to learn.
	SanitizeAlways SanitizeMode = "always"
	// SanitizeNever disables sanitization even after a schema rejection. An
	// escape hatch for the case where the rewrite itself is the problem.
	SanitizeNever SanitizeMode = "never"
)

// NormalizeSanitizeMode maps an operator-supplied string onto a SanitizeMode,
// defaulting an empty or unrecognised value to SanitizeAuto.
//
// Unrecognised values fall back rather than erroring because this is a
// portability aid: a typo in it must not be able to stop a yanshi that would
// otherwise run.
func NormalizeSanitizeMode(s string) SanitizeMode {
	switch SanitizeMode(s) {
	case SanitizeAlways:
		return SanitizeAlways
	case SanitizeNever:
		return SanitizeNever
	default:
		return SanitizeAuto
	}
}

// AdaptiveConfig configures an AdaptiveModel. Every field is optional; the zero
// value produces a wrapper that only records nothing and passes everything
// through, which is what makes it safe to install unconditionally.
type AdaptiveConfig struct {
	// ModelID is the registry key this wrapper's inner model answers to. It
	// keys the quirk store, the rate limiter and the usage rows, so a wrapper
	// built without one learns and throttles under the empty key — shared with
	// every other unnamed model in the process. Always set it.
	ModelID string
	// Provider is the adapter kind, recorded on usage rows.
	Provider string
	// Quirks is the shared learned-quirk store. nil disables M5 entirely.
	Quirks *QuirkStore
	// Limiter throttles outgoing calls. nil disables M7.
	Limiter *RateLimiter
	// UsageSink persists per-call token usage. nil disables M10.
	UsageSink UsageSink
	// Sanitize selects the M6 policy. The zero value is SanitizeAuto.
	Sanitize SanitizeMode
	// Overflow configures C6. A zero ContextWindow disables overflow recovery.
	Overflow OverflowRecoveryConfig
}

// AdaptiveModel wraps a model.BaseChatModel with the behaviours listed in this
// file's comment.
type AdaptiveModel struct {
	// Inner is the wrapped provider adapter.
	Inner model.BaseChatModel
	cfg   AdaptiveConfig
}

// NewAdaptiveModel wraps inner. A nil inner is a programming error and yields
// nil, so a caller that forgot to build the provider gets a nil-pointer panic
// at its own call site rather than a wrapper that silently answers nothing.
func NewAdaptiveModel(inner model.BaseChatModel, cfg AdaptiveConfig) *AdaptiveModel {
	if inner == nil {
		return nil
	}
	cfg.Sanitize = NormalizeSanitizeMode(string(cfg.Sanitize))
	return &AdaptiveModel{Inner: inner, cfg: cfg}
}

// compile-time interface check.
var _ model.BaseChatModel = (*AdaptiveModel)(nil)

// request is the mutable state of one attempt: the history and the options that
// will be sent. Both are rebuilt (never mutated in place) by every repair, so
// the caller's slices are never touched.
type request struct {
	msgs []*schema.Message
	opts []model.Option
	// sanitized records whether the tool schemas in opts have already been
	// rewritten, so a second repair pass does not sanitize twice (harmless but
	// wasteful) and so the quirk repair can tell whether it has anything left
	// to try.
	sanitized bool
}

// prepare builds the first attempt: learned quirks applied, tools sanitized if
// policy or a learned quirk says so.
func (a *AdaptiveModel) prepare(msgs []*schema.Message, opts []model.Option) request {
	req := request{msgs: msgs, opts: opts}
	if a.cfg.Quirks != nil {
		for _, q := range a.cfg.Quirks.List(a.cfg.ModelID) {
			req = a.applyQuirk(req, q)
		}
	}
	if a.cfg.Sanitize == SanitizeAlways && !req.sanitized {
		req = a.sanitizeTools(req)
	}
	return req
}

// applyQuirk rewrites req according to one learned quirk. Unknown quirks are
// returned unchanged, which cannot happen through QuirkFromError but can
// through a store populated by a future version.
func (a *AdaptiveModel) applyQuirk(req request, q Quirk) request {
	switch q {
	case QuirkNeedsReasoningContent:
		if msgs, changed := applyReasoningContent(req.msgs); changed {
			req.msgs = msgs
		}
	case QuirkRejectsToolSchemaRefs:
		if a.cfg.Sanitize != SanitizeNever && !req.sanitized {
			req = a.sanitizeTools(req)
		}
	case QuirkRejectsSystemRole:
		if msgs, changed := applyNoSystemRole(req.msgs); changed {
			req.msgs = msgs
		}
	}
	return req
}

// sanitizeTools appends a WithTools option carrying the M6-rewritten schemas.
//
// Appending rather than rebuilding the option slice is what makes this safe:
// eino applies options in order and the last WithTools wins, so the original
// option set is left intact for anything else it carries (temperature, tool
// choice, impl-specific options this package does not know about).
func (a *AdaptiveModel) sanitizeTools(req request) request {
	common := model.GetCommonOptions(&model.Options{}, req.opts...)
	if len(common.Tools) == 0 {
		req.sanitized = true // nothing to do, but do not try again
		return req
	}
	clean, err := SanitizeToolInfos(common.Tools)
	if err != nil {
		slog.Warn("tool schema sanitization incomplete", "model", a.cfg.ModelID, "err", err)
	}
	req.opts = append(append([]model.Option{}, req.opts...), model.WithTools(clean))
	req.sanitized = true
	return req
}

// recover attempts to repair req in place after err, returning true when
// something actually changed and a resend is therefore worth making.
//
// Returning false on "nothing changed" is rule 1 of both C6 and M5: a resend of
// a byte-identical request reproduces the same failure and costs another
// charge. The two repairs are tried in a fixed order — overflow first, because
// an over-long prompt can produce an error whose text happens to mention a
// schema keyword (providers echo the rejected body), and shrinking is the
// correct response to that.
func (a *AdaptiveModel) recover(ctx context.Context, req *request, err error) (Quirk, bool) {
	if IsContextOverflow(err) {
		msgs, shrank := forceCompact(ctx, req.msgs, a.Inner, a.cfg.Overflow)
		if !shrank {
			return "", false
		}
		slog.Warn("context overflow: forced compaction, retrying once",
			"model", a.cfg.ModelID,
			"messages_before", len(req.msgs),
			"messages_after", len(msgs))
		req.msgs = msgs
		return "", true
	}
	if a.cfg.Quirks == nil {
		return "", false
	}
	q, ok := QuirkFromError(err)
	if !ok {
		return "", false
	}
	before := *req
	repaired := a.applyQuirk(*req, q)
	if !requestChanged(before, repaired) {
		return "", false
	}
	*req = repaired
	return q, true
}

// requestChanged reports whether a repair produced a materially different
// request. It compares the message slice header and the sanitized flag rather
// than deep-comparing content, which is sufficient because every repair in this
// package either builds a new slice or sets the flag — and a repair that did
// neither had nothing to do.
func requestChanged(before, after request) bool {
	if before.sanitized != after.sanitized {
		return true
	}
	if len(before.msgs) != len(after.msgs) {
		return true
	}
	for i := range before.msgs {
		if before.msgs[i] != after.msgs[i] {
			return true
		}
	}
	return false
}

// Generate throttles, sends, and on failure repairs and resends at most once.
func (a *AdaptiveModel) Generate(ctx context.Context, msgs []*schema.Message, opts ...model.Option) (
	*schema.Message, error) {
	req := a.prepare(msgs, opts)
	if err := a.cfg.Limiter.Wait(ctx, a.cfg.ModelID); err != nil {
		return nil, err
	}
	out, err := a.Inner.Generate(ctx, req.msgs, req.opts...)
	if err == nil {
		a.record(ctx, out)
		return out, nil
	}
	firstErr := err
	sent := req.msgs
	before := len(sent)
	q, ok := a.recover(ctx, &req, firstErr)
	if !ok {
		return nil, firstErr
	}
	if waitErr := a.cfg.Limiter.Wait(ctx, a.cfg.ModelID); waitErr != nil {
		return nil, waitErr
	}
	out, err = a.Inner.Generate(ctx, req.msgs, req.opts...)
	if err != nil {
		return nil, a.retryError(firstErr, err, before, len(req.msgs), sent, req.msgs)
	}
	a.learn(q, firstErr)
	a.record(ctx, out)
	return out, nil
}

// Stream throttles, opens the stream, and — because a provider rejection
// surfaces on the first Recv rather than from Stream itself — peeks one item
// before handing the reader back, so the same repair-and-resend-once policy
// applies to streaming.
//
// A caller that never retries sees a reader indistinguishable from the
// provider's: openAndPeek replays the peeked item. See streamretry.go.
func (a *AdaptiveModel) Stream(ctx context.Context, msgs []*schema.Message, opts ...model.Option) (
	*schema.StreamReader[*schema.Message], error) {
	req := a.prepare(msgs, opts)
	if err := a.cfg.Limiter.Wait(ctx, a.cfg.ModelID); err != nil {
		return nil, err
	}
	open := func(c context.Context) (*schema.StreamReader[*schema.Message], error) {
		return a.Inner.Stream(c, req.msgs, req.opts...)
	}
	sr, err := openAndPeek(ctx, open)
	if err == nil {
		return a.recordStream(ctx, sr), nil
	}
	firstErr := err
	sent := req.msgs
	before := len(sent)
	q, ok := a.recover(ctx, &req, firstErr)
	if !ok {
		return nil, firstErr
	}
	if waitErr := a.cfg.Limiter.Wait(ctx, a.cfg.ModelID); waitErr != nil {
		return nil, waitErr
	}
	sr, err = openAndPeek(ctx, open)
	if err != nil {
		return nil, a.retryError(firstErr, err, before, len(req.msgs), sent, req.msgs)
	}
	a.learn(q, firstErr)
	return a.recordStream(ctx, sr), nil
}

// retryError picks the error to surface when the single retry also failed.
//
// An overflow that survived a real shrink is wrapped with both TOKEN counts,
// because "we compacted from N to M tokens and it still did not fit" is a
// different operator action (raise context_window, or find the indivisible
// segment) from "compaction did nothing". Every other second failure is
// returned bare: the provider's own words are more useful than any wrapper's
// summary of them.
//
// The counts are re-measured here rather than taken from the message-slice
// lengths the callers happen to have. Those lengths were passed in once and
// rendered into a message reading "26 → 16 tokens" — a real observed output, in
// which both numbers were message counts and neither was a token count. It is
// worse than no number: no model has a 16-token window, so an operator reading
// it would conclude compaction had destroyed the history, and the "raise
// context_window" decision the message exists to inform would be made against
// two numbers three orders of magnitude too small.
//
// msgsBefore/msgsAfter are still the shrink DETECTOR, because that is what they
// are reliable for: they say whether recover() replaced the slice at all.
func (a *AdaptiveModel) retryError(firstErr, secondErr error, msgsBefore, msgsAfter int,
	before, after []*schema.Message) error {
	if IsContextOverflow(firstErr) && msgsAfter != msgsBefore {
		return &overflowRetryError{
			Before: ctxcompact.EstimateTokens(before),
			After:  ctxcompact.EstimateTokens(after),
			Err:    secondErr,
		}
	}
	return secondErr
}

// learn records a quirk after the repaired request SUCCEEDED. A zero quirk (the
// overflow path) records nothing — an overflow is a property of one history,
// not of the model.
func (a *AdaptiveModel) learn(q Quirk, evidence error) {
	if q == "" || a.cfg.Quirks == nil {
		return
	}
	msg := ""
	if evidence != nil {
		msg = evidence.Error()
	}
	a.cfg.Quirks.Learn(a.cfg.ModelID, q, msg)
}

// record writes one usage row for a completed non-streaming call.
func (a *AdaptiveModel) record(ctx context.Context, msg *schema.Message) {
	if a.cfg.UsageSink == nil {
		return
	}
	rec, ok := usageFromMessage(msg)
	if !ok {
		return
	}
	rec.Model, rec.Provider = a.cfg.ModelID, a.cfg.Provider
	recordUsage(ctx, a.cfg.UsageSink, rec)
}

// recordStream returns a reader that forwards sr unchanged and writes ONE usage
// row when the stream ends.
//
// One row, from the LAST delta that carried usage, because that is how the
// providers report it: usage arrives on a final chunk (OpenAI with
// stream_options, Anthropic's message_delta), and earlier chunks either omit it
// or carry a running total. Summing would multiply a single call's tokens by
// the number of chunks that mentioned them.
func (a *AdaptiveModel) recordStream(ctx context.Context, sr *schema.StreamReader[*schema.Message]) *schema.StreamReader[*schema.Message] {
	if a.cfg.UsageSink == nil {
		return sr
	}
	out, ow := schema.Pipe[*schema.Message](1)
	go func() {
		defer func() {
			ow.Close()
			sr.Close()
		}()
		var last UsageRecord
		var seen bool
		for {
			msg, err := sr.Recv()
			if err != nil {
				if seen {
					last.Model, last.Provider = a.cfg.ModelID, a.cfg.Provider
					recordUsage(ctx, a.cfg.UsageSink, last)
				}
				if !isEOF(err) {
					ow.Send(nil, err)
				}
				return
			}
			if rec, ok := usageFromMessage(msg); ok {
				last, seen = rec, true
			}
			if ow.Send(msg, nil) {
				return
			}
		}
	}()
	return out
}
