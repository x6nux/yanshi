package otelobs

import (
	"context"
	"errors"
	"strings"
	"testing"

	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

// TestToolSpanNeverRecordsErrorBody proves the StartTool end function never
// writes the error body into span attributes or events. Constraint 17 / 1 in
// the C4 plan: no prompt, args, error body, or sensitive data in telemetry.
//
// ledger: C4/OBS2#4 脱敏
func TestToolSpanNeverRecordsErrorBody(t *testing.T) {
	recorder := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder))
	otel.SetTracerProvider(tp)
	_, end := StartTool(context.Background(), "shell_run")
	end(errors.New("provider body contains sk-super-secret"))
	spans := recorder.Ended()
	if len(spans) != 1 {
		t.Fatalf("ended spans=%d", len(spans))
	}
	span := spans[0]
	if span.Name() != "tool.shell_run" {
		t.Fatalf("name=%q", span.Name())
	}
	for _, attr := range span.Attributes() {
		if strings.Contains(attr.Value.AsString(), "sk-super-secret") {
			t.Fatalf("secret leaked in attribute: %+v", attr)
		}
	}
	if len(span.Events()) != 0 {
		t.Fatalf("RecordError would leak error text; events=%+v", span.Events())
	}
}
