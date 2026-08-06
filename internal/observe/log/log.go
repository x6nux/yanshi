// Package log configures yanshi's process-wide structured logger. It injects
// correlation IDs from context and redacts sensitive values before any handler
// can serialize them.
package log

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"time"
)

// Config controls the process logger. Zero values mean info/json/os.Stderr.
type Config struct {
	Level  string
	Format string
	Writer io.Writer
}

// IDs are safe correlation fields. They deliberately exclude prompts, tool
// arguments, commands, paths, hosts, and provider error text.
type IDs struct {
	TraceID   string
	SessionID string
	TurnID    string
	Tool      string
}

type idsKey struct{}

// readRandom is the entropy source for NewTraceID/NewTurnID. It is a
// package-level indirection solely so tests can drive the entropy-failure
// degradation path (return ""), which crypto/rand practically never hits in
// production. Production always uses crypto/rand.Read.
var readRandom = rand.Read

// WithIDs merges IDs with an existing context value.
func WithIDs(ctx context.Context, ids IDs) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	current := IDsFromContext(ctx)
	if ids.TraceID != "" {
		current.TraceID = ids.TraceID
	}
	if ids.SessionID != "" {
		current.SessionID = ids.SessionID
	}
	if ids.TurnID != "" {
		current.TurnID = ids.TurnID
	}
	if ids.Tool != "" {
		current.Tool = ids.Tool
	}
	return context.WithValue(ctx, idsKey{}, current)
}

// IDsFromContext returns the bound correlation fields.
func IDsFromContext(ctx context.Context) IDs {
	if ctx == nil {
		return IDs{}
	}
	ids, _ := ctx.Value(idsKey{}).(IDs)
	return ids
}

// NewTraceID returns a 16-byte lowercase hex identifier compatible in length
// with an OpenTelemetry trace ID. Entropy failure degrades to an empty ID.
func NewTraceID() string {
	var raw [16]byte
	if _, err := readRandom(raw[:]); err != nil {
		return ""
	}
	return hex.EncodeToString(raw[:])
}

// NewTurnID returns an independent 8-byte correlation identifier.
func NewTurnID() string {
	var raw [8]byte
	if _, err := readRandom(raw[:]); err != nil {
		return ""
	}
	return hex.EncodeToString(raw[:])
}

// ParseLevel maps user configuration to slog levels; unknown values are info.
func ParseLevel(level string) slog.Level {
	switch strings.ToLower(strings.TrimSpace(level)) {
	case "debug":
		return slog.LevelDebug
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

type redactHandler struct {
	inner slog.Handler
	// sample may be nil, which disables sampling entirely. Shared by
	// WithAttrs/WithGroup derivatives on purpose: the budget belongs to the
	// message, and a logger that re-derives itself per request would otherwise
	// get a fresh budget each time and never be throttled at all.
	sample *sampler
}

func (h *redactHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.inner.Enabled(ctx, level)
}

func (h *redactHandler) Handle(ctx context.Context, record slog.Record) error {
	suppressed := 0
	if h.sample != nil {
		ok, n := h.sample.allow(record.Level, record.Message)
		if !ok {
			return nil
		}
		suppressed = n
	}
	clean := slog.NewRecord(record.Time, record.Level, record.Message, record.PC)
	if suppressed > 0 {
		// Report the gap rather than closing over it silently: a reader who
		// cannot tell throttling from absence will draw the wrong conclusion
		// about how often something happened.
		clean.AddAttrs(slog.Int("suppressed", suppressed))
	}
	record.Attrs(func(attr slog.Attr) bool {
		clean.AddAttrs(redactAttr(attr))
		return true
	})
	ids := IDsFromContext(ctx)
	if ids.TraceID != "" {
		clean.AddAttrs(slog.String("trace_id", ids.TraceID))
	}
	if ids.SessionID != "" {
		clean.AddAttrs(slog.String("session_id", ids.SessionID))
	}
	if ids.TurnID != "" {
		clean.AddAttrs(slog.String("turn_id", ids.TurnID))
	}
	if ids.Tool != "" {
		clean.AddAttrs(slog.String("tool", ids.Tool))
	}
	return h.inner.Handle(ctx, clean)
}

func (h *redactHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &redactHandler{inner: h.inner.WithAttrs(redactAttrs(attrs)), sample: h.sample}
}

func (h *redactHandler) WithGroup(name string) slog.Handler {
	return &redactHandler{inner: h.inner.WithGroup(name), sample: h.sample}
}

// New creates a redacting structured logger.
func New(cfg Config) *slog.Logger {
	writer := cfg.Writer
	if writer == nil {
		writer = os.Stderr
	}
	opts := &slog.HandlerOptions{Level: ParseLevel(cfg.Level)}
	var inner slog.Handler
	if strings.EqualFold(cfg.Format, "text") {
		inner = slog.NewTextHandler(writer, opts)
	} else {
		inner = slog.NewJSONHandler(writer, opts)
	}
	// 20 copies of a message per 10s window: enough that a burst is visible in
	// full, low enough that a tight loop cannot bury the rest of the log.
	// WARN and above bypass this entirely (see sampler).
	return slog.New(&redactHandler{inner: inner, sample: newSampler(20, 10*time.Second)})
}

// Setup installs the redacting logger as slog.Default.
func Setup(cfg Config) *slog.Logger {
	logger := New(cfg)
	slog.SetDefault(logger)
	return logger
}

// SafeErrorType returns only the concrete type, never err.Error(). Provider
// errors often contain response bodies, prompts, or credentials.
func SafeErrorType(err error) string {
	if err == nil {
		return ""
	}
	return fmt.Sprintf("%T", err)
}

// WarnErr logs a diagnostic without serializing the error body.
func WarnErr(ctx context.Context, message string, err error, attrs ...slog.Attr) {
	if err == nil {
		return
	}
	attrs = append(attrs, slog.String("error_type", SafeErrorType(err)))
	slog.LogAttrs(ctx, slog.LevelWarn, message, attrs...)
}
