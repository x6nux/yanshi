package log

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
)

func TestHandlerRedactsDirectBoundNestedAndErrorAttrs(t *testing.T) {
	var buf bytes.Buffer
	logger := New(Config{Writer: &buf, Format: "json", Level: "debug"})
	logger = logger.With("api_key", "sk-ant-bound")
	logger.Info("request",
		"prompt", "private prompt",
		slog.Group("request", "tool_args", `{"path":"C:/secret"}`),
		"err", errors.New("Bearer hidden-token"),
	)
	got := buf.String()
	for _, secret := range []string{"sk-ant-bound", "private prompt", "C:/secret", "hidden-token"} {
		if strings.Contains(got, secret) {
			t.Fatalf("log leaked %q: %s", secret, got)
		}
	}
	if strings.Count(got, Redacted) < 4 {
		t.Fatalf("expected every sensitive value to be redacted: %s", got)
	}
}

func TestHandlerAddsCorrelationIDsFromContext(t *testing.T) {
	var buf bytes.Buffer
	logger := New(Config{Writer: &buf, Format: "json", Level: "info"})
	ctx := WithIDs(context.Background(), IDs{
		TraceID:   "0123456789abcdef0123456789abcdef",
		SessionID: "session-1",
		TurnID:    "turn-2",
		Tool:      "fs_read",
	})
	logger.InfoContext(ctx, "correlated")
	got := buf.String()
	for _, want := range []string{`"trace_id":"0123456789abcdef0123456789abcdef"`, `"session_id":"session-1"`, `"turn_id":"turn-2"`, `"tool":"fs_read"`} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %s in %s", want, got)
		}
	}
}

func TestParseLevelAndSafeErrorType(t *testing.T) {
	if got := ParseLevel("warn"); got != slog.LevelWarn {
		t.Fatalf("ParseLevel(warn) = %v", got)
	}
	if got := SafeErrorType(errors.New("api_key=secret")); got != "*errors.errorString" {
		t.Fatalf("SafeErrorType = %q", got)
	}
	if got := SafeErrorType(nil); got != "" {
		t.Fatalf("SafeErrorType(nil) = %q", got)
	}
}
