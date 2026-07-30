package otelobs

import (
	"context"
	"sync"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"

	obslog "github.com/x6nux/yanshi/internal/observe/log"
)

// instruments holds lazily-initialized metric instruments. Created on first
// use AFTER bootstrap.OTel.Setup has set the global MeterProvider, so the
// instruments are bound to the real provider rather than the no-op default.
// The sync.Once guarantee ensures exactly one creation per process lifetime.
var instruments struct {
	once           sync.Once
	sessionLatency metric.Float64Histogram
	turnLatency    metric.Float64Histogram
	toolLatency    metric.Float64Histogram
	tokenCounter   metric.Int64Counter
	retryCounter   metric.Int64Counter
	errorCounter   metric.Int64Counter
}

func ensureInstruments() {
	instruments.once.Do(func() {
		m := otel.Meter("github.com/x6nux/yanshi")
		var err error
		instruments.sessionLatency, err = m.Float64Histogram("yanshi.session.duration", metric.WithUnit("s"))
		instrumentMust(err)
		instruments.turnLatency, err = m.Float64Histogram("yanshi.turn.duration", metric.WithUnit("s"))
		instrumentMust(err)
		instruments.toolLatency, err = m.Float64Histogram("yanshi.tool.duration", metric.WithUnit("s"))
		instrumentMust(err)
		instruments.tokenCounter, err = m.Int64Counter("yanshi.llm.tokens", metric.WithUnit("{token}"))
		instrumentMust(err)
		instruments.retryCounter, err = m.Int64Counter("yanshi.llm.retry", metric.WithUnit("{attempt}"))
		instrumentMust(err)
		instruments.errorCounter, err = m.Int64Counter("yanshi.operation.errors", metric.WithUnit("{error}"))
		instrumentMust(err)
	})
}

func instrumentMust(err error) {
	if err != nil {
		panic("otelobs: instrument creation failed: " + err.Error())
	}
}

// startOperation is the shared core for StartSession, StartTurn, and StartTool.
// It creates a named span, attaches safe attributes, and returns a context +
// end function. The end function records latency, sets error status (without
// error body), adds error attributes, and is safe to call multiple times.
func startOperation(ctx context.Context, spanName string, kind trace.SpanKind,
	histogram metric.Float64Histogram, attrs ...attribute.KeyValue) (context.Context, func(error)) {
	started := time.Now()
	ctx, span := otel.Tracer("github.com/x6nux/yanshi").Start(ctx, spanName,
		trace.WithSpanKind(kind), trace.WithAttributes(attrs...))
	if sc := span.SpanContext(); sc.IsValid() {
		ids := obslog.IDsFromContext(ctx)
		ids.TraceID = sc.TraceID().String()
		ctx = obslog.WithIDs(ctx, ids)
	}
	var once sync.Once
	return ctx, func(err error) {
		once.Do(func() {
			endAttrs := attrs
			if err != nil {
				errType := obslog.SafeErrorType(err)
				span.SetAttributes(attribute.String("error.type", errType))
				span.SetStatus(codes.Error, "operation failed")
				instruments.errorCounter.Add(ctx, 1, metric.WithAttributes(attribute.String("error.type", errType)))
				endAttrs = append(append([]attribute.KeyValue{}, attrs...), attribute.String("error.type", errType))
			}
			histogram.Record(ctx, time.Since(started).Seconds(), metric.WithAttributes(endAttrs...))
			span.End()
		})
	}
}

// StartSession opens a session-level span. The returned end function
// records latency and finalizes the span before process shutdown.
func StartSession(ctx context.Context) (context.Context, func(error)) {
	ensureInstruments()
	ids := obslog.IDsFromContext(ctx)
	attrs := make([]attribute.KeyValue, 0, 1)
	if ids.SessionID != "" {
		attrs = append(attrs, attribute.String("session.id", ids.SessionID))
	}
	return startOperation(ctx, "agent.session", trace.SpanKindServer, instruments.sessionLatency, attrs...)
}

// SetSessionID attaches or updates the session.id attribute to the current
// span. No-op when sessionID is empty.
func SetSessionID(ctx context.Context, sessionID string) {
	if sessionID == "" {
		return
	}
	trace.SpanFromContext(ctx).SetAttributes(attribute.String("session.id", sessionID))
}

// StartTurn opens a turn-level span. The returned end function records
// latency and finalizes the span. Use defer end() to close at turn boundary.
func StartTurn(ctx context.Context, modelName string) (context.Context, func(error)) {
	ensureInstruments()
	ids := obslog.IDsFromContext(ctx)
	attrs := make([]attribute.KeyValue, 0, 3)
	if ids.SessionID != "" {
		attrs = append(attrs, attribute.String("session.id", ids.SessionID))
	}
	if ids.TurnID != "" {
		attrs = append(attrs, attribute.String("turn.id", ids.TurnID))
	}
	if modelName != "" {
		attrs = append(attrs, attribute.String("model.name", modelName))
	}
	return startOperation(ctx, "agent.turn", trace.SpanKindInternal, instruments.turnLatency, attrs...)
}

// StartTool opens a tool-level span. The returned end function records
// latency and finalizes the span. Use defer end() or goroutine to close at
// tool completion. The span's name is "tool.<toolName>" so OTel systems can
// facet on tool name without scanning attributes.
func StartTool(ctx context.Context, toolName string) (context.Context, func(error)) {
	ensureInstruments()
	return startOperation(ctx, "tool."+toolName, trace.SpanKindInternal, instruments.toolLatency,
		attribute.String("tool.name", toolName))
}

// RecordUsage emits a yanshi.llm.tokens counter for one provider usage.
// Token kinds are "input", "cache_hit", "output", "reasoning" — zero values
// are skipped. Constraint 9: every provider usage is recorded exactly once.
func RecordUsage(ctx context.Context, modelName string, prompt, cached, completion, reasoning int) {
	ensureInstruments()
	if prompt < 0 {
		prompt = 0
	}
	if cached < 0 {
		cached = 0
	}
	if cached > prompt {
		cached = prompt
	}
	if completion < 0 {
		completion = 0
	}
	if reasoning < 0 {
		reasoning = 0
	}
	base := []attribute.KeyValue{attribute.String("model.name", modelName)}
	for _, item := range []struct {
		kind  string
		value int
	}{
		{kind: "input", value: prompt - cached},
		{kind: "cache_hit", value: cached},
		{kind: "output", value: completion},
		{kind: "reasoning", value: reasoning},
	} {
		if item.value == 0 {
			continue
		}
		attrs := append(append([]attribute.KeyValue{}, base...), attribute.String("token.kind", item.kind))
		instruments.tokenCounter.Add(ctx, int64(item.value), metric.WithAttributes(attrs...))
	}
}

// RecordRetry emits a yanshi.llm.retry counter with safe attributes.
// error.type is the concrete error type (%T), not the error body.
// Constraint 6: retry metric only from sleepRetry (single retry point).
func RecordRetry(ctx context.Context, attempt, maxAttempts int, err error) {
	ensureInstruments()
	instruments.retryCounter.Add(ctx, 1, metric.WithAttributes(
		attribute.Int("retry.attempt", attempt),
		attribute.Int("retry.max", maxAttempts),
		attribute.String("error.type", obslog.SafeErrorType(err)),
	))
}
