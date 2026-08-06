package log

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
)

func TestNewTraceIDAndTurnID(t *testing.T) {
	tid := NewTraceID()
	// 16 bytes → 32 lowercase hex chars. Failure path (empty) is not
	// deterministically reachable, so assert the happy path only.
	if len(tid) != 32 {
		t.Fatalf("trace id len=%d want 32: %q", len(tid), tid)
	}
	for _, c := range tid {
		if !strings.ContainsRune("0123456789abcdef", c) {
			t.Fatalf("non-hex trace id %q", tid)
		}
	}
	turn := NewTurnID()
	// 8 bytes → 16 lowercase hex chars.
	if len(turn) != 16 {
		t.Fatalf("turn id len=%d want 16: %q", len(turn), turn)
	}
}

// TestNewIDsDegradeOnEntropyFailure forces the RNG to fail and asserts both
// helpers return "" rather than panicking or emitting a partial/zeroed id.
func TestNewIDsDegradeOnEntropyFailure(t *testing.T) {
	prev := readRandom
	t.Cleanup(func() { readRandom = prev })
	readRandom = func([]byte) (int, error) { return 0, errors.New("rng unavailable") }
	if id := NewTraceID(); id != "" {
		t.Fatalf("trace id must be empty on entropy failure, got %q", id)
	}
	if id := NewTurnID(); id != "" {
		t.Fatalf("turn id must be empty on entropy failure, got %q", id)
	}
}

func TestWithIDsNilContextAndProgressiveMerge(t *testing.T) {
	// nil ctx is tolerated and promoted to Background.
	ctx := WithIDs(nil, IDs{TraceID: "tr"})
	if got := IDsFromContext(ctx); got.TraceID != "tr" {
		t.Fatalf("nil ctx must still bind: %+v", got)
	}
	// nil ctx to IDsFromContext returns the zero value.
	if IDsFromContext(nil) != (IDs{}) {
		t.Fatal("nil ctx must return zero IDs")
	}
	// Progressive merges: only non-empty fields overlay prior values.
	ctx = context.Background()
	ctx = WithIDs(ctx, IDs{SessionID: "s1", Tool: "fs_read"})
	ctx = WithIDs(ctx, IDs{TurnID: "t1"})
	ids := IDsFromContext(ctx)
	if ids.SessionID != "s1" || ids.TurnID != "t1" || ids.Tool != "fs_read" {
		t.Fatalf("merge lost a field: %+v", ids)
	}
}

// TestParseLevelAllVariants.
//
// ledger: C4/OBS1#3 级别可配
func TestParseLevelAllVariants(t *testing.T) {
	cases := map[string]slog.Level{
		"debug":    slog.LevelDebug,
		"DEBUG":    slog.LevelDebug,
		"warn":     slog.LevelWarn,
		"warning":  slog.LevelWarn,
		"error":    slog.LevelError,
		"":         slog.LevelInfo,
		"  Info ":  slog.LevelInfo,
		"nonsense": slog.LevelInfo,
	}
	for in, want := range cases {
		if got := ParseLevel(in); got != want {
			t.Errorf("ParseLevel(%q)=%v want %v", in, got, want)
		}
	}
}

func TestNewTextFormatEmitsKeyValuePairs(t *testing.T) {
	var buf bytes.Buffer
	logger := New(Config{Writer: &buf, Format: "text", Level: "debug"})
	logger.Info("hello", "count", 42)
	got := buf.String()
	if !strings.Contains(got, "hello") || !strings.Contains(got, "count=42") {
		t.Fatalf("text handler output missing fields: %s", got)
	}
}

func TestNewDefaultsWriterToStderrWhenNil(t *testing.T) {
	// Nil Writer must fall back without panicking; we only assert a logger is
	// produced (writing to os.Stderr is a side effect we do not capture here).
	logger := New(Config{Format: "json", Level: "info"})
	if logger == nil {
		t.Fatal("New must return a non-nil logger with nil Writer")
	}
}

func TestSetupInstallsAsDefault(t *testing.T) {
	prev := slog.Default()
	t.Cleanup(func() { slog.SetDefault(prev) })
	var buf bytes.Buffer
	logger := Setup(Config{Writer: &buf, Format: "json", Level: "info"})
	if slog.Default() != logger {
		t.Fatal("Setup must install the returned logger as slog.Default")
	}
	slog.Info("via-default")
	if !strings.Contains(buf.String(), "via-default") {
		t.Fatalf("slog.Default did not route through Setup logger: %s", buf.String())
	}
}

func TestWarnErrNoopsOnNilAndLogsOnRealError(t *testing.T) {
	prev := slog.Default()
	t.Cleanup(func() { slog.SetDefault(prev) })
	var buf bytes.Buffer
	Setup(Config{Writer: &buf, Format: "json", Level: "warn"})

	// nil error must short-circuit before logging.
	WarnErr(context.Background(), "should-skip", nil)
	if buf.Len() != 0 {
		t.Fatalf("nil err must not produce output: %s", buf.String())
	}

	// A real error logs the message and the safe error_type but never the body.
	WarnErr(context.Background(), "failed", errors.New("sk-body"),
		slog.String("component", "uploader"))
	got := buf.String()
	for _, want := range []string{`"msg":"failed"`, "error_type", `"component":"uploader"`} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %s in: %s", want, got)
		}
	}
	if strings.Contains(got, "sk-body") {
		t.Fatalf("error body leaked into log: %s", got)
	}
}

func TestLooksSensitiveValue(t *testing.T) {
	sensitive := []string{
		"sk-ant-12345",
		"Bearer abc.def.ghi",
		"xoxb-12345",
		"xoxp-67890",
		"https://svc/?api_key=hunter2",
		"raw\nauthorization: basic dXNlcg==",
	}
	for _, v := range sensitive {
		if !looksSensitiveValue(v) {
			t.Errorf("expected sensitive: %q", v)
		}
	}
	plain := []string{"", "just a sentence", "model-name", "stable"}
	for _, v := range plain {
		if looksSensitiveValue(v) {
			t.Errorf("plain value flagged sensitive: %q", v)
		}
	}
}

// secretStringer implements fmt.Stringer so fmt.Sprint yields a sensitive
// prefix. slog stores it as KindAny (a non-primitive struct), exercising the
// Any+stringer arm of redactAttr that error-typing alone does not reach.
type secretStringer struct{}

func (secretStringer) String() string { return "sk-leaked" }

func TestRedactAttrBranches(t *testing.T) {
	// Sensitive key redacts regardless of the underlying value.
	if r := redactAttr(slog.String("password", "plain")); r.Value.String() != Redacted {
		t.Fatalf("sensitive key not redacted: %+v", r)
	}
	// Plain string value passes through unchanged.
	if r := redactAttr(slog.String("count", "42")); r.Value.String() != "42" {
		t.Fatalf("plain string altered: %+v", r)
	}
	// String value whose content looks sensitive (sk- prefix) is redacted.
	if r := redactAttr(slog.String("note", "sk-abc")); r.Value.String() != Redacted {
		t.Fatalf("sensitive-looking string not redacted: %+v", r)
	}
	// KindAny error is redacted.
	if r := redactAttr(slog.Any("err", errors.New("boom"))); r.Value.String() != Redacted {
		t.Fatalf("error any not redacted: %+v", r)
	}
	// KindAny stringer whose Sprint is sensitive is redacted.
	if r := redactAttr(slog.Any("body", secretStringer{})); r.Value.String() != Redacted {
		t.Fatalf("sensitive any not redacted: %+v", r)
	}
	// KindAny non-sensitive value passes through (kind preserved, not
	// rewritten to a redacted String). A struct is genuinely KindAny.
	type point struct{ X, Y int }
	if r := redactAttr(slog.Any("p", point{1, 2})); r.Value.Kind() != slog.KindAny {
		t.Fatalf("non-sensitive any kind lost: %+v", r)
	}
	// Group keeps its kind; nested sensitive child is redacted.
	g := redactAttr(slog.Group("request", slog.String("api_key", "sk-x"), slog.Int("n", 1)))
	if g.Value.Kind() != slog.KindGroup {
		t.Fatalf("group kind lost: %+v", g)
	}
	var sawRedacted, sawInt bool
	for _, child := range g.Value.Group() {
		if child.Key == "api_key" && child.Value.String() == Redacted {
			sawRedacted = true
		}
		if child.Key == "n" && child.Value.Int64() == 1 {
			sawInt = true
		}
	}
	if !sawRedacted || !sawInt {
		t.Fatalf("group children not handled: %+v", g.Value.Group())
	}
	// Non-string, non-any, non-group attr (int) passes through unchanged.
	if r := redactAttr(slog.Int("port", 8080)); r.Value.Int64() != 8080 {
		t.Fatalf("int attr altered: %+v", r)
	}
}

// TestWithGroupPreservesRedaction covers WithGroup (previously 0%): a logger
// derived through WithGroup+WithAttrs must still redact pre-bound secrets.
func TestWithGroupPreservesRedaction(t *testing.T) {
	var buf bytes.Buffer
	logger := New(Config{Writer: &buf, Format: "json", Level: "debug"})
	logger = logger.WithGroup("req").With("api_key", "sk-z")
	logger.Info("call", "ok", "value")
	got := buf.String()
	if strings.Contains(got, "sk-z") {
		t.Fatalf("WithAttrs-bound secret leaked via WithGroup logger: %s", got)
	}
	if !strings.Contains(got, Redacted) {
		t.Fatalf("expected a redaction marker: %s", got)
	}
}

func TestRedactAttrsMapsAll(t *testing.T) {
	in := []slog.Attr{slog.String("password", "p"), slog.Int("n", 1)}
	out := redactAttrs(in)
	if len(out) != 2 || out[0].Value.String() != Redacted || out[1].Value.Int64() != 1 {
		t.Fatalf("redactAttrs mishandled: %+v", out)
	}
}

// guard intentionally removed — fmt is no longer imported.
